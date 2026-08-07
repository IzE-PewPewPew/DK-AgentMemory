package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The viewer must serve its page, not redirect to itself.
//
// http.FileServer redirects any path ending in /index.html to "./" as a
// canonicalisation. A handler that rewrites the mount root to "index.html"
// therefore produces /viewer/ -> 301 -> /viewer/ -> 301, forever: the server
// looks healthy, every endpoint answers, and the one page a human opens is the
// only thing that is broken. Nothing else in the test suite touches static
// files, so this is the only place that would catch it.
func TestViewerServesIndexWithoutRedirectLoop(t *testing.T) {
	h := http.StripPrefix("/viewer/", viewerHandler())

	for _, path := range []string{"/viewer/", "/viewer/index.html"} {
		t.Run(path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			// One canonicalising redirect is allowed, but it must not point
			// back at the URL that produced it.
			if loc := rec.Header().Get("Location"); rec.Code >= 300 && rec.Code < 400 {
				if loc == path || loc == "./" && path == "/viewer/" {
					t.Fatalf("%s redirected to %q, which is itself — an infinite loop", path, loc)
				}
				return
			}

			if rec.Code != http.StatusOK {
				t.Fatalf("got status %d, want 200", rec.Code)
			}
			body := rec.Body.String()
			if !strings.Contains(body, "<title>DevKuong Memories</title>") {
				t.Errorf("response is not the viewer page:\n%.200s", body)
			}
			if !strings.Contains(body, `data-tab="learn"`) {
				t.Error("the Learn tab is missing from the served page")
			}
		})
	}
}

// The page holds an API key in memory, so the response must forbid every
// external origin and every injected script source.
func TestViewerSetsStrictCSP(t *testing.T) {
	rec := httptest.NewRecorder()
	http.StripPrefix("/viewer/", viewerHandler()).
		ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/viewer/", nil))

	csp := rec.Header().Get("Content-Security-Policy")
	for _, want := range []string{"default-src 'none'", "connect-src 'self'", "frame-ancestors 'none'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP missing %q; got %q", want, csp)
		}
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Error("the viewer must not be cached; it renders every memory")
	}
}
