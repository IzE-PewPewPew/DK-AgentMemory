package consolidate

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/IzE-PewPewPew/DK-AgentMemory/internal/config"
	"github.com/IzE-PewPewPew/DK-AgentMemory/internal/logx"
)

// An empty key variable must disable consolidation with an explanation, not
// take the server down and not send an empty Bearer token to the provider.
// The 401 that came back from doing the latter said "Invalid token", which
// blames a credential that was correct and merely absent from the process.
func TestMissingAPIKeyDisablesRatherThanFails(t *testing.T) {
	cfg := config.Defaults()
	cfg.Consolidation.Enabled = true
	cfg.Consolidation.LLM.Provider = "openai-compatible"
	cfg.Consolidation.LLM.BaseURL = "https://example.invalid"
	cfg.Consolidation.LLM.Model = "test-model"
	cfg.Consolidation.LLM.APIKeyEnv = "DKM_TEST_KEY_DEFINITELY_UNSET"
	t.Setenv("DKM_TEST_KEY_DEFINITELY_UNSET", "")

	w, err := NewWorker(cfg, nil, nil, logx.Discard())
	if err != nil {
		t.Fatalf("NewWorker returned %v; a missing key must not stop the server", err)
	}
	if w.Enabled() {
		t.Fatal("Enabled() is true with no key; the run would 401 at the provider")
	}
	reason := w.DisabledReason()
	if !strings.Contains(reason, "DKM_TEST_KEY_DEFINITELY_UNSET") {
		t.Errorf("DisabledReason() = %q, want it to name the empty variable", reason)
	}

	if _, err := w.RunNow(context.Background(), nil, false); err == nil {
		t.Error("RunNow succeeded with no provider; want an error")
	}
}

func TestPresentAPIKeyEnables(t *testing.T) {
	cfg := config.Defaults()
	cfg.Consolidation.Enabled = true
	cfg.Consolidation.LLM.Provider = "openai-compatible"
	cfg.Consolidation.LLM.BaseURL = "https://example.invalid"
	cfg.Consolidation.LLM.Model = "test-model"
	cfg.Consolidation.LLM.APIKeyEnv = "DKM_TEST_KEY_SET"
	t.Setenv("DKM_TEST_KEY_SET", "sk-not-a-real-key")

	w, err := NewWorker(cfg, nil, nil, logx.Discard())
	if err != nil {
		t.Fatalf("NewWorker: %v", err)
	}
	if !w.Enabled() {
		t.Fatalf("Enabled() is false with a key present; reason = %q", w.DisabledReason())
	}
	if got := w.DisabledReason(); got != "" {
		t.Errorf("DisabledReason() = %q, want empty when enabled", got)
	}
}

// The drain loop is what turns "consolidate my import" from twenty button
// presses into one, so its stopping condition matters: too eager and it leaves
// sessions behind, too willing and a marking bug bills the operator forever.
func TestDrainLoop(t *testing.T) {
	const batch = 25

	t.Run("stops on a short batch", func(t *testing.T) {
		got := []int{}
		sizes := []int{batch, batch, 7}
		n, err := drainLoop(context.Background(), batch, 100, func(context.Context) (int, error) {
			s := sizes[len(got)]
			got = append(got, s)
			return s, nil
		})
		if err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		if n != 3 {
			t.Errorf("batches = %d, want 3", n)
		}
	})

	t.Run("an empty queue costs one call", func(t *testing.T) {
		calls := 0
		n, err := drainLoop(context.Background(), batch, 100, func(context.Context) (int, error) {
			calls++
			return 0, nil
		})
		if err != nil || n != 1 || calls != 1 {
			t.Fatalf("n=%d calls=%d err=%v, want 1/1/nil", n, calls, err)
		}
	})

	t.Run("a full batch forever stops at the ceiling", func(t *testing.T) {
		// The failure this guards: a bug that never marks sessions summarised
		// would return a full batch on every call. Without the ceiling that is
		// an unbounded number of paid LLM calls.
		calls := 0
		n, err := drainLoop(context.Background(), batch, 10, func(context.Context) (int, error) {
			calls++
			return batch, nil
		})
		if err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		if n != 10 || calls != 10 {
			t.Errorf("n=%d calls=%d, want 10/10", n, calls)
		}
	})

	t.Run("stops on the first error", func(t *testing.T) {
		want := errors.New("provider down")
		calls := 0
		_, err := drainLoop(context.Background(), batch, 100, func(context.Context) (int, error) {
			calls++
			return batch, want
		})
		if !errors.Is(err, want) {
			t.Fatalf("err = %v, want %v", err, want)
		}
		if calls != 1 {
			t.Errorf("calls = %d, want 1 — a dead provider fails the same way twice", calls)
		}
	})

	t.Run("cancellation stops before the next call", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		calls := 0
		_, err := drainLoop(ctx, batch, 100, func(context.Context) (int, error) {
			calls++
			cancel()
			return batch, nil
		})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
		if calls != 1 {
			t.Errorf("calls = %d, want 1", calls)
		}
	})
}
