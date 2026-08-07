package consolidate

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Which errors are worth a second attempt is a cost decision, not a style one.
// Retrying a 401 spends quota to be refused again; not retrying a 503 abandoned
// a 449-session drain because the provider was briefly at 99% CPU.
func TestHTTPErrorTransient(t *testing.T) {
	cases := []struct {
		status int
		want   bool
		why    string
	}{
		{http.StatusTooManyRequests, true, "rate limited: the whole point of waiting"},
		{http.StatusServiceUnavailable, true, "the 503 that stopped the drain"},
		{http.StatusBadGateway, true, "gateway hiccup"},
		{http.StatusGatewayTimeout, true, "upstream slow"},
		{http.StatusInternalServerError, true, "some gateways report overload as 500"},
		{http.StatusUnauthorized, false, "a wrong key is wrong on attempt five"},
		{http.StatusForbidden, false, "not permitted, repeatedly"},
		{http.StatusBadRequest, false, "malformed stays malformed"},
		{http.StatusNotFound, false, "unknown model"},
		{http.StatusPaymentRequired, false, "out of credit; retrying does not add any"},
	}
	for _, c := range cases {
		got := (&httpError{Status: c.status}).Transient()
		if got != c.want {
			t.Errorf("status %d: Transient() = %v, want %v (%s)", c.status, got, c.want, c.why)
		}
	}
}

func TestBackoffGrows(t *testing.T) {
	want := []time.Duration{3 * time.Second, 9 * time.Second, 27 * time.Second, 81 * time.Second}
	for i, w := range want {
		if got := backoff(i + 1); got != w {
			t.Errorf("backoff(%d) = %v, want %v", i+1, got, w)
		}
	}
}

func TestRetryAfterHeader(t *testing.T) {
	t.Run("delay seconds", func(t *testing.T) {
		r := &http.Response{Header: http.Header{"Retry-After": []string{"12"}}}
		if got := retryAfter(r); got != 12*time.Second {
			t.Errorf("got %v, want 12s", got)
		}
	})
	t.Run("absent", func(t *testing.T) {
		if got := retryAfter(&http.Response{Header: http.Header{}}); got != 0 {
			t.Errorf("got %v, want 0", got)
		}
	})
	t.Run("garbage is not a panic", func(t *testing.T) {
		r := &http.Response{Header: http.Header{"Retry-After": []string{"soon"}}}
		if got := retryAfter(r); got != 0 {
			t.Errorf("got %v, want 0", got)
		}
	})
	t.Run("negative is ignored", func(t *testing.T) {
		r := &http.Response{Header: http.Header{"Retry-After": []string{"-5"}}}
		if got := retryAfter(r); got != 0 {
			t.Errorf("got %v, want 0", got)
		}
	})
}

// stubProvider answers with a scripted sequence so the retry loop can be
// observed without a network or a bill.
type stubProvider struct {
	calls int
	errs  []error // one per attempt; nil means success
}

func (s *stubProvider) Name() string { return "stub" }

func (s *stubProvider) Complete(context.Context, Request) (*Response, error) {
	i := s.calls
	s.calls++
	if i < len(s.errs) && s.errs[i] != nil {
		return nil, s.errs[i]
	}
	return &Response{Text: "ok", InputTokens: 1, OutputTokens: 1}, nil
}

func TestCompleteWithRetryStopsOnTerminalErrors(t *testing.T) {
	terminal := []struct {
		name string
		err  error
	}{
		{"unauthorized", &httpError{Status: http.StatusUnauthorized, Body: "Invalid token"}},
		{"bad request", &httpError{Status: http.StatusBadRequest}},
		{"token budget", ErrTokenBudget},
	}
	for _, c := range terminal {
		t.Run(c.name, func(t *testing.T) {
			p := &stubProvider{errs: []error{c.err, c.err, c.err, c.err, c.err}}
			if _, err := completeWithRetry(context.Background(), p, Request{}); err == nil {
				t.Fatal("want an error")
			}
			if p.calls != 1 {
				t.Errorf("calls = %d, want 1 — a terminal error must not be retried", p.calls)
			}
		})
	}
}

// shortBackoff shrinks the retry delay for the duration of a test. Real backoff
// reaches 81 seconds, which is correct in production and intolerable in CI.
func shortBackoff(t *testing.T) {
	t.Helper()
	prev := backoffBase
	backoffBase = time.Millisecond
	t.Cleanup(func() { backoffBase = prev })
}

func TestCompleteWithRetryGivesUpAfterMaxAttempts(t *testing.T) {
	shortBackoff(t)

	p := &stubProvider{}
	for i := 0; i < maxAttempts; i++ {
		p.errs = append(p.errs, &httpError{Status: http.StatusServiceUnavailable, Body: "cpu overloaded"})
	}

	_, err := completeWithRetry(context.Background(), p, Request{})
	if err == nil {
		t.Fatal("want an error")
	}
	if p.calls != maxAttempts {
		t.Errorf("calls = %d, want %d — every transient attempt should be spent", p.calls, maxAttempts)
	}
	if !strings.Contains(err.Error(), "cpu overloaded") {
		t.Errorf("error = %q, want the provider's last message preserved", err)
	}
}

func TestCompleteWithRetrySucceedsAfterTransientFailure(t *testing.T) {
	shortBackoff(t)

	p := &stubProvider{errs: []error{
		&httpError{Status: http.StatusServiceUnavailable, Body: "cpu overloaded"},
		&httpError{Status: http.StatusTooManyRequests, Body: "slow down"},
	}}

	resp, err := completeWithRetry(context.Background(), p, Request{})
	if err != nil {
		t.Fatalf("completeWithRetry: %v", err)
	}
	if resp.Text != "ok" {
		t.Errorf("Text = %q, want ok", resp.Text)
	}
	if p.calls != 3 {
		t.Errorf("calls = %d, want 3 — two transient failures then success", p.calls)
	}
}

// A provider that names its own delay must be obeyed when it asks for longer
// than our backoff would have waited.
func TestRetryAfterOverridesShorterBackoff(t *testing.T) {
	shortBackoff(t)

	p := &stubProvider{errs: []error{
		&httpError{Status: http.StatusTooManyRequests, RetryAfter: 40 * time.Millisecond},
	}}

	start := time.Now()
	if _, err := completeWithRetry(context.Background(), p, Request{}); err != nil {
		t.Fatalf("completeWithRetry: %v", err)
	}
	if waited := time.Since(start); waited < 40*time.Millisecond {
		t.Errorf("waited %v, want at least the 40ms the provider asked for", waited)
	}
}

// The status code has to survive the trip from the HTTP response to the retry
// decision. It used to be flattened into a string, which is why a 503 and a 401
// were treated identically.
func TestPostReturnsTypedHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"message":"system cpu overloaded"}}`))
	}))
	defer srv.Close()

	c := common{client: srv.Client(), endpoint: srv.URL}
	err := c.post(context.Background(), srv.URL, nil, map[string]any{}, &struct{}{})
	if err == nil {
		t.Fatal("want an error")
	}

	var he *httpError
	if !errors.As(err, &he) {
		t.Fatalf("error is %T, want *httpError", err)
	}
	if he.Status != http.StatusServiceUnavailable {
		t.Errorf("Status = %d, want 503", he.Status)
	}
	if !he.Transient() {
		t.Error("Transient() = false for 503")
	}
	if he.RetryAfter != 7*time.Second {
		t.Errorf("RetryAfter = %v, want 7s", he.RetryAfter)
	}
	if !strings.Contains(he.Error(), "cpu overloaded") {
		t.Errorf("Error() = %q, want it to carry the provider's message", he.Error())
	}
}
