package server

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tillknuesting/fliessband/internal/pipeline"
)

func TestSafePath(t *testing.T) {
	root := filepath.FromSlash("/data/runs/x/service")
	for rel, ok := range map[string]bool{
		"go.mod":                 true,
		"internal/service/a.go":  true,
		"../run.json":            false,
		"../../../../etc/passwd": false,
		"internal/../../secret":  false,
		"":                       false,
		"a\x00b":                 false,
	} {
		if _, got := safePath(root, rel); got != ok {
			t.Errorf("safePath(%q) ok = %v, want %v", rel, got, ok)
		}
	}
}

func newTestHandler(t *testing.T, cfg Config) http.Handler {
	t.Helper()
	m, err := pipeline.NewManager(t.TempDir(), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg.SpecsDir = t.TempDir()
	h, err := New(m, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func do(h http.Handler, method, target string, mod func(*http.Request)) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, strings.NewReader(`{"spec":"name: x"}`))
	if mod != nil {
		mod(r)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestSecurityHeadersAndProbes(t *testing.T) {
	h := newTestHandler(t, Config{LoopbackOnly: true})
	w := do(h, "GET", "http://localhost:7070/healthz", nil)
	if w.Code != http.StatusOK || !strings.Contains(w.Header().Get("Content-Security-Policy"), "default-src 'self'") ||
		w.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("healthz: %d, headers %v", w.Code, w.Header())
	}
	if w := do(h, "GET", "http://localhost:7070/metrics", nil); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "fliessband_build_info") {
		t.Fatalf("metrics: %d %s", w.Code, w.Body.String())
	}
	if w := do(h, "GET", "http://localhost:7070/", nil); w.Header().Get("Cache-Control") != "no-cache" {
		t.Errorf("UI assets must be revalidated, Cache-Control = %q", w.Header().Get("Cache-Control"))
	}
}

// A web page that rebinds its own domain to 127.0.0.1 must not reach the API.
func TestHostCheck(t *testing.T) {
	h := newTestHandler(t, Config{LoopbackOnly: true})
	for host, want := range map[string]int{"localhost:7070": 200, "127.0.0.1:7070": 200, "[::1]:7070": 200, "evil.example:7070": 421, "evil.example": 421} {
		r := httptest.NewRequest("GET", "/api/runs", nil)
		r.Host = host
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != want {
			t.Errorf("Host %s: %d, want %d", host, w.Code, want)
		}
	}
}

func TestCrossSiteWritesAreRejected(t *testing.T) {
	h := newTestHandler(t, Config{LoopbackOnly: true})
	w := do(h, "POST", "http://localhost:7070/api/runs", func(r *http.Request) {
		r.Header.Set("Sec-Fetch-Site", "cross-site")
		r.Header.Set("Origin", "https://evil.example")
	})
	if w.Code != http.StatusForbidden {
		t.Fatalf("cross-site POST: %d, want 403", w.Code)
	}
}

func TestTokenAuth(t *testing.T) {
	h := newTestHandler(t, Config{Token: "s3cret"})
	if w := do(h, "GET", "/api/runs", nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("no credentials: %d, want 401", w.Code)
	}
	if w := do(h, "GET", "/api/runs", func(r *http.Request) { r.Header.Set("Authorization", "Bearer wrong") }); w.Code != http.StatusUnauthorized {
		t.Fatalf("wrong bearer: %d, want 401", w.Code)
	}
	if w := do(h, "GET", "/api/runs", func(r *http.Request) { r.Header.Set("Authorization", "Bearer s3cret") }); w.Code != http.StatusOK {
		t.Fatalf("bearer: %d, want 200", w.Code)
	}
	if w := do(h, "GET", "/healthz", nil); w.Code != http.StatusOK {
		t.Fatalf("probes stay open: %d", w.Code)
	}
	login := func(token string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/api/session", strings.NewReader(`{"token":"`+token+`"}`))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if w := login("s3cret"); w.Code != http.StatusNoContent {
		t.Fatalf("login: %d", w.Code)
	} else {
		cookies := w.Result().Cookies()
		if len(cookies) != 1 || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode || cookies[0].Value == "s3cret" {
			t.Fatalf("session cookie: %+v", cookies)
		}
		if w := do(h, "GET", "/api/runs", func(r *http.Request) { r.AddCookie(cookies[0]) }); w.Code != http.StatusOK {
			t.Fatalf("cookie: %d, want 200", w.Code)
		}
	}
}

func TestStatusReportsSandbox(t *testing.T) {
	h := newTestHandler(t, Config{LoopbackOnly: true})
	w := do(h, "GET", "http://localhost:7070/api/status", nil)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"sandbox":{"mode":"none","contained":false`) {
		t.Fatalf("status: %d %s", w.Code, w.Body.String())
	}
}
