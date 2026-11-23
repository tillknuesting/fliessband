// Package server exposes the factory over HTTP: a JSON API, server-sent
// events for live runs, operational endpoints and the embedded web UI.
//
// The factory runs code, part of it written by an LLM, on the machine it
// serves from, so the HTTP layer is defensive by default: it rejects
// cross-origin writes (CSRF), requests whose Host is not the factory's own
// (DNS rebinding), and, when a token is configured, unauthenticated calls.
package server

import (
	"archive/zip"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tillknuesting/fliessband/internal/gen"
	"github.com/tillknuesting/fliessband/internal/pipeline"
	"github.com/tillknuesting/fliessband/internal/spec"
	"github.com/tillknuesting/fliessband/web"
)

// Config configures the HTTP layer.
type Config struct {
	SpecsDir string
	// Token, when set, is required for the API: as a bearer token or via the
	// session cookie that POST /api/session sets.
	Token string
	// LoopbackOnly restricts the Host header to localhost names, which stops
	// DNS rebinding attacks against a factory bound to 127.0.0.1.
	LoopbackOnly bool
	// TrustedOrigins may make cross-origin writes, e.g. behind a proxy.
	TrustedOrigins []string
	Log            *slog.Logger
}

type api struct {
	m   *pipeline.Manager
	cfg Config

	toolsOnce sync.Once
	tools     []pipeline.Tool

	loginMu   sync.Mutex
	lastFail  time.Time
	sessionID string
}

// New returns the HTTP handler for the factory.
func New(m *pipeline.Manager, cfg Config) (http.Handler, error) {
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.DiscardHandler)
	}
	a := &api{m: m, cfg: cfg}
	if cfg.Token != "" {
		mac := hmac.New(sha256.New, []byte(cfg.Token))
		mac.Write([]byte("fliessband-session-v1"))
		a.sessionID = hex.EncodeToString(mac.Sum(nil))
	}
	go a.toolsOnce.Do(a.loadTools)
	mux := http.NewServeMux()
	static := http.FileServerFS(web.Static)
	if dir := os.Getenv("FLIESSBAND_WEB_DIR"); dir != "" {
		static = http.FileServer(http.Dir(dir)) // dev: edit the UI without rebuilding
	}
	mux.Handle("GET /", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache") // revalidate, so a new build never runs old UI code
		static.ServeHTTP(w, r)
	}))
	mux.HandleFunc("GET /healthz", a.healthz)
	mux.HandleFunc("GET /metrics", a.auth(a.metrics))
	mux.HandleFunc("GET /api/session", a.session)
	mux.HandleFunc("POST /api/session", a.login)
	mux.HandleFunc("GET /api/status", a.auth(a.status))
	mux.HandleFunc("GET /api/examples", a.auth(a.examples))
	mux.HandleFunc("POST /api/validate", a.auth(a.validate))
	mux.HandleFunc("GET /api/runs", a.auth(a.listRuns))
	mux.HandleFunc("POST /api/runs", a.auth(a.startRun))
	mux.HandleFunc("GET /api/runs/{id}", a.auth(a.getRun))
	mux.HandleFunc("POST /api/runs/{id}/cancel", a.auth(a.cancelRun))
	mux.HandleFunc("GET /api/runs/{id}/events", a.auth(a.events))
	mux.HandleFunc("GET /api/runs/{id}/files", a.auth(a.files))
	mux.HandleFunc("GET /api/runs/{id}/file", a.auth(a.file))
	mux.HandleFunc("GET /api/runs/{id}/zip", a.auth(a.zip))
	mux.HandleFunc("GET /api/runs/{id}/sbom", a.auth(a.artifact("sbom.cdx.json", "application/vnd.cyclonedx+json")))
	mux.HandleFunc("GET /api/runs/{id}/provenance", a.auth(a.artifact("provenance.intoto.json", "application/vnd.in-toto+json")))

	csrf := http.NewCrossOriginProtection()
	for _, o := range cfg.TrustedOrigins {
		if err := csrf.AddTrustedOrigin(o); err != nil {
			return nil, fmt.Errorf("trusted origin %q: %w", o, err)
		}
	}
	return a.logging(securityHeaders(a.hostCheck(csrf.Handler(mux)))), nil
}

func (a *api) loadTools() { a.tools = a.m.Tools() }

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// securityHeaders sets a strict Content-Security-Policy and related headers.
// The UI loads nothing from other origins.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; "+
			"font-src 'self'; connect-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		h.Set("X-Frame-Options", "DENY")
		next.ServeHTTP(w, r)
	})
}

// hostCheck rejects requests addressed to another host name when the
// factory listens on loopback: a web page cannot then rebind its own
// domain to 127.0.0.1 and talk to the factory.
func (a *api) hostCheck(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.cfg.LoopbackOnly {
			host := r.Host
			if h, _, err := net.SplitHostPort(r.Host); err == nil {
				host = h
			}
			host = strings.Trim(host, "[]")
			if host != "localhost" && !strings.HasSuffix(host, ".localhost") && !isLoopbackIP(host) {
				writeErr(w, http.StatusMisdirectedRequest, "this factory only answers to localhost")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func isLoopbackIP(host string) bool {
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// auth requires the token, as a bearer token or as the session cookie.
func (a *api) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if a.cfg.Token == "" || a.authorized(r) {
			next(w, r)
			return
		}
		w.Header().Set("WWW-Authenticate", `Bearer realm="fliessband"`)
		writeErr(w, http.StatusUnauthorized, "sign in with the factory token")
	}
}

func (a *api) authorized(r *http.Request) bool {
	if bearer, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
		return subtle.ConstantTimeCompare([]byte(bearer), []byte(a.cfg.Token)) == 1
	}
	if c, err := r.Cookie("fliessband_session"); err == nil {
		return subtle.ConstantTimeCompare([]byte(c.Value), []byte(a.sessionID)) == 1
	}
	return false
}

func (a *api) session(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]bool{"required": a.cfg.Token != "", "signed_in": a.cfg.Token == "" || a.authorized(r)})
}

// login exchanges the token for an HttpOnly session cookie. Failed
// attempts are slowed down to make guessing impractical.
func (a *api) login(w http.ResponseWriter, r *http.Request) {
	if a.cfg.Token == "" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	var req struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "send {\"token\": \"…\"}")
		return
	}
	a.loginMu.Lock()
	if wait := time.Until(a.lastFail.Add(time.Second)); wait > 0 {
		time.Sleep(wait)
	}
	ok := subtle.ConstantTimeCompare([]byte(req.Token), []byte(a.cfg.Token)) == 1
	if !ok {
		a.lastFail = time.Now()
	}
	a.loginMu.Unlock()
	if !ok {
		a.cfg.Log.Warn("failed sign-in", "remote", r.RemoteAddr)
		writeErr(w, http.StatusUnauthorized, "wrong token")
		return
	}
	//nolint:gosec // G124: Secure is set whenever the request came over HTTPS; plain http://localhost needs it off
	http.SetCookie(w, &http.Cookie{Name: "fliessband_session", Value: a.sessionID, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteStrictMode, Secure: r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https", MaxAge: 7 * 24 * 3600})
	w.WriteHeader(http.StatusNoContent)
}

// statusWriter records the status code; it keeps Flush working for SSE.
type statusWriter struct {
	http.ResponseWriter
	code  int
	bytes int
}

func (s *statusWriter) WriteHeader(code int) {
	s.code = code
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusWriter) Write(b []byte) (int, error) {
	if s.code == 0 {
		s.code = http.StatusOK
	}
	n, err := s.ResponseWriter.Write(b)
	s.bytes += n
	return n, err
}

func (s *statusWriter) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *statusWriter) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// logging writes one structured line per API request.
func (a *api) logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(sw, r)
		if !strings.HasPrefix(r.URL.Path, "/api/") && sw.code < 400 {
			return // static files and probes stay quiet
		}
		level := slog.LevelInfo
		if sw.code >= 500 {
			level = slog.LevelError
		} else if sw.code >= 400 {
			level = slog.LevelWarn
		}
		a.cfg.Log.Log(r.Context(), level, "http", "method", r.Method, "path", r.URL.Path, "status", sw.code,
			"bytes", sw.bytes, "duration_ms", float64(time.Since(start).Microseconds())/1000)
	})
}

func (a *api) healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "version": pipeline.Version})
}

// metrics serves factory counters in the Prometheus text format.
func (a *api) metrics(w http.ResponseWriter, _ *http.Request) {
	s := a.m.Stats()
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	var b strings.Builder
	metric := func(name, typ, help string) { fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ) }
	metric("fliessband_runs_total", "counter", "Runs finished since start, by status.")
	statuses := make([]string, 0, len(s.Runs))
	for st := range s.Runs {
		statuses = append(statuses, string(st))
	}
	sort.Strings(statuses)
	for _, st := range statuses {
		fmt.Fprintf(&b, "fliessband_runs_total{status=%q} %d\n", st, s.Runs[pipeline.Status(st)])
	}
	metric("fliessband_runs_active", "gauge", "Runs executing now.")
	fmt.Fprintf(&b, "fliessband_runs_active %d\n", s.Active)
	metric("fliessband_runs_queued", "gauge", "Runs waiting for a slot.")
	fmt.Fprintf(&b, "fliessband_runs_queued %d\n", s.Queued)
	metric("fliessband_runs_stored", "gauge", "Runs kept on disk.")
	fmt.Fprintf(&b, "fliessband_runs_stored %d\n", s.Stored)
	stages := make([]string, 0, len(s.StageCount))
	for st := range s.StageCount {
		stages = append(stages, st)
	}
	sort.Strings(stages)
	metric("fliessband_stage_duration_seconds", "summary", "Time spent in each station.")
	for _, st := range stages {
		fmt.Fprintf(&b, "fliessband_stage_duration_seconds_sum{stage=%q} %g\n", st, s.StageSeconds[st])
		fmt.Fprintf(&b, "fliessband_stage_duration_seconds_count{stage=%q} %d\n", st, s.StageCount[st])
	}
	metric("fliessband_stage_failures_total", "counter", "Failed gate stations.")
	for _, st := range stages {
		fmt.Fprintf(&b, "fliessband_stage_failures_total{stage=%q} %d\n", st, s.StageFailed[st])
	}
	metric("fliessband_llm_tokens_total", "counter", "LLM tokens used, by direction.")
	fmt.Fprintf(&b, "fliessband_llm_tokens_total{direction=\"in\"} %d\nfliessband_llm_tokens_total{direction=\"out\"} %d\n", s.TokensIn, s.TokensOut)
	metric("fliessband_build_info", "gauge", "Factory build information.")
	fmt.Fprintf(&b, "fliessband_build_info{version=%q,go=%q} 1\n", pipeline.Version, runtime.Version())
	_, _ = io.WriteString(w, b.String())
}

func (a *api) status(w http.ResponseWriter, _ *http.Request) {
	a.toolsOnce.Do(a.loadTools)
	resp := map[string]any{"tools": a.tools, "llm": nil, "version": pipeline.Version, "auth": a.cfg.Token != ""}
	if c := a.m.LLM(); c != nil {
		resp["llm"] = map[string]string{"model": c.Model, "base_url": c.BaseURL}
	}
	writeJSON(w, http.StatusOK, resp)
}

type example struct {
	Name   string `json:"name"`
	File   string `json:"file"`
	Source string `json:"source"`
}

func (a *api) examples(w http.ResponseWriter, _ *http.Request) {
	entries, _ := os.ReadDir(a.cfg.SpecsDir)
	out := []example{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(a.cfg.SpecsDir, e.Name()))
		if err != nil {
			continue
		}
		out = append(out, example{Name: strings.TrimSuffix(e.Name(), ".yaml"), File: e.Name(), Source: string(b)})
	}
	writeJSON(w, http.StatusOK, out)
}

type validation struct {
	OK       bool           `json:"ok"`
	Error    string         `json:"error,omitempty"`
	Problems []spec.Problem `json:"problems"`
	Service  string         `json:"service,omitempty"`
	RPCs     []string       `json:"rpcs,omitempty"`
	Examples int            `json:"examples"`
	Holdout  int            `json:"holdout"`
	Files    int            `json:"files,omitempty"`
	Proto    string         `json:"proto,omitempty"`
}

func (a *api) validate(w http.ResponseWriter, r *http.Request) {
	src, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	s, problems, err := spec.Parse(src)
	v := validation{Problems: problems}
	if v.Problems == nil {
		v.Problems = []spec.Problem{}
	}
	if err != nil {
		v.Error = err.Error()
		writeJSON(w, http.StatusOK, v)
		return
	}
	v.OK = !spec.HasErrors(problems)
	if v.OK {
		m := gen.NewModel(s)
		v.Service = m.FullServiceName
		for _, e := range s.Entities {
			v.RPCs = append(v.RPCs, e.RPCNames()...)
		}
		for _, mm := range s.Methods {
			v.RPCs = append(v.RPCs, mm.Name)
		}
		for _, ex := range m.Examples {
			v.Examples++
			if ex.Holdout {
				v.Holdout++
			}
		}
		if files, err := gen.Generate(s); err == nil {
			v.Files = len(files)
			for _, f := range files {
				if strings.HasSuffix(f.Path, ".proto") {
					v.Proto = string(f.Data)
				}
			}
		} else {
			v.OK, v.Error = false, err.Error()
		}
	}
	writeJSON(w, http.StatusOK, v)
}

func (a *api) listRuns(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, a.m.List())
}

func (a *api) startRun(w http.ResponseWriter, r *http.Request) {
	req := struct {
		Spec    string           `json:"spec"`
		Options pipeline.Options `json:"options"`
	}{Options: pipeline.DefaultOptions()}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "body must be JSON with spec and options: "+err.Error())
		return
	}
	if strings.TrimSpace(req.Spec) == "" {
		writeErr(w, http.StatusBadRequest, "spec is empty")
		return
	}
	req.Options.MaxRepairs = min(max(req.Options.MaxRepairs, 0), 5)
	req.Options.MaxMutants = min(max(req.Options.MaxMutants, 0), 500)
	run, err := a.m.Start([]byte(req.Spec), req.Options)
	switch {
	case errors.Is(err, pipeline.ErrQueueFull):
		w.Header().Set("Retry-After", "30")
		writeErr(w, http.StatusTooManyRequests, err.Error())
		return
	case errors.Is(err, pipeline.ErrClosed):
		writeErr(w, http.StatusServiceUnavailable, err.Error())
		return
	case err != nil:
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	a.cfg.Log.Info("run started", "run", run.ID, "service", run.Name, "llm", req.Options.LLM)
	writeJSON(w, http.StatusAccepted, map[string]string{"id": run.ID})
}

func (a *api) run(w http.ResponseWriter, r *http.Request) *pipeline.Run {
	run := a.m.Get(r.PathValue("id"))
	if run == nil {
		writeErr(w, http.StatusNotFound, "no run with id "+r.PathValue("id"))
	}
	return run
}

func (a *api) getRun(w http.ResponseWriter, r *http.Request) {
	if run := a.run(w, r); run != nil {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(run.JSON())
	}
}

func (a *api) cancelRun(w http.ResponseWriter, r *http.Request) {
	run := a.run(w, r)
	if run == nil {
		return
	}
	if !a.m.Cancel(run.ID) {
		writeErr(w, http.StatusConflict, "the run is not running")
		return
	}
	a.cfg.Log.Info("run canceled", "run", run.ID)
	w.WriteHeader(http.StatusAccepted)
}

func (a *api) events(w http.ResponseWriter, r *http.Request) {
	run := a.run(w, r)
	if run == nil {
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	ch, unsubscribe := run.Subscribe()
	defer unsubscribe()
	tick := time.NewTicker(15 * time.Second)
	defer tick.Stop()
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				_, _ = fmt.Fprint(w, "event: end\ndata: {}\n\n")
				flusher.Flush()
				return
			}
			b, _ := json.Marshal(ev)
			_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
			flusher.Flush()
		case <-tick.C:
			_, _ = fmt.Fprint(w, ": keep-alive\n\n")
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

type fileEntry struct {
	Path   string `json:"path"`
	Bytes  int64  `json:"bytes"`
	Origin string `json:"origin"`
}

func (a *api) files(w http.ResponseWriter, r *http.Request) {
	run := a.run(w, r)
	if run == nil {
		return
	}
	man, _ := gen.LoadManifest(run.ServiceDir())
	out := []fileEntry{}
	root, err := os.OpenRoot(run.ServiceDir())
	if err != nil {
		writeJSON(w, http.StatusOK, out)
		return
	}
	defer func() { _ = root.Close() }()
	// os.Root confines the walk to the run: generated code cannot point a
	// symlink outside it.
	_ = fs.WalkDir(root.FS(), ".", func(rel string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return nil //nolint:nilerr // list what can be read; skip the rest
		}
		info, err := d.Info()
		if err != nil {
			return nil //nolint:nilerr // the file vanished while listing
		}
		origin := ""
		if man != nil {
			origin = man.Origin(rel)
		}
		if origin == "" {
			origin = "tool"
		}
		out = append(out, fileEntry{Path: rel, Bytes: info.Size(), Origin: origin})
		return nil
	})
	slices.SortFunc(out, func(x, y fileEntry) int { return strings.Compare(x.Path, y.Path) })
	writeJSON(w, http.StatusOK, out)
}

// safePath resolves rel inside root, refusing anything that escapes it.
func safePath(root, rel string) (string, bool) {
	if rel == "" || strings.Contains(rel, "\x00") {
		return "", false
	}
	p := filepath.Join(root, filepath.FromSlash(rel))
	r, err := filepath.Rel(root, p)
	if err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
		return "", false
	}
	return p, true
}

func (a *api) file(w http.ResponseWriter, r *http.Request) {
	run := a.run(w, r)
	if run == nil {
		return
	}
	rel := r.URL.Query().Get("path")
	if _, ok := safePath(run.ServiceDir(), rel); !ok {
		writeErr(w, http.StatusBadRequest, "invalid path")
		return
	}
	root, err := os.OpenRoot(run.ServiceDir())
	if err != nil {
		writeErr(w, http.StatusNotFound, "no such file")
		return
	}
	defer func() { _ = root.Close() }()
	f, err := root.Open(filepath.FromSlash(rel)) // cannot escape the run, even through symlinks
	if err != nil {
		writeErr(w, http.StatusNotFound, "no such file")
		return
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		writeErr(w, http.StatusNotFound, "no such file")
		return
	}
	if info.Size() > 4<<20 {
		writeErr(w, http.StatusRequestEntityTooLarge, "file is larger than 4 MB")
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	http.ServeContent(w, r, info.Name(), info.ModTime(), f)
}

// artifact serves a file the run wrote next to the service, as a download.
func (a *api) artifact(name, contentType string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		run := a.run(w, r)
		if run == nil {
			return
		}
		p := filepath.Join(run.Dir(), name)
		if _, err := os.Stat(p); err != nil {
			writeErr(w, http.StatusNotFound, "this run has no "+name)
			return
		}
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s-%s-%s"`, run.Name, run.ID, name))
		http.ServeFile(w, r, p)
	}
}

func (a *api) zip(w http.ResponseWriter, r *http.Request) {
	run := a.run(w, r)
	if run == nil {
		return
	}
	root, err := os.OpenRoot(run.ServiceDir())
	if err != nil {
		writeErr(w, http.StatusNotFound, "this run has no service")
		return
	}
	defer func() { _ = root.Close() }()
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s-%s.zip"`, run.Name, run.ID))
	zw := zip.NewWriter(w)
	defer func() { _ = zw.Close() }() // a failed close means the client went away
	fsys := root.FS()
	_ = fs.WalkDir(fsys, ".", func(rel string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return nil //nolint:nilerr // zip what can be read; skip the rest
		}
		f, err := zw.Create(run.Name + "/" + rel)
		if err != nil {
			return err
		}
		src, err := fsys.Open(rel)
		if err != nil {
			return nil //nolint:nilerr // zip what can be read; skip the rest
		}
		defer func() { _ = src.Close() }()
		_, err = io.Copy(f, src)
		return err
	})
}
