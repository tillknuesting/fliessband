// Command fliessband is a software factory for Go gRPC microservices.
//
//	fliessband serve            start the web UI (default 127.0.0.1:7070)
//	fliessband run SPEC.yaml    run a spec through the line in the terminal
//	fliessband gen -o DIR SPEC  only render the templates
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/tillknuesting/fliessband/internal/gen"
	"github.com/tillknuesting/fliessband/internal/llm"
	"github.com/tillknuesting/fliessband/internal/pipeline"
	"github.com/tillknuesting/fliessband/internal/server"
	"github.com/tillknuesting/fliessband/internal/spec"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	switch os.Args[1] {
	case "serve":
		err = serveCmd(os.Args[2:])
	case "run":
		err = runCmd(os.Args[2:])
	case "gen":
		err = genCmd(os.Args[2:])
	case "version":
		fmt.Println("fliessband", pipeline.Version)
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `usage:
  fliessband serve [-addr 127.0.0.1:7070] [-data data]   start the web UI
  fliessband run [-llm] [-mutation=true] SPEC.yaml       run a spec through the line
  fliessband gen -o DIR SPEC.yaml                        only render the templates
  fliessband version

environment:
  FLIESSBAND_TOKEN             required to listen beyond localhost; protects the API
  FLIESSBAND_TRUSTED_ORIGINS   comma-separated origins allowed to make writes (proxies)
  FLIESSBAND_LLM_BASE_URL, FLIESSBAND_LLM_API_KEY, FLIESSBAND_LLM_MODEL   the implement stage
  FLIESSBAND_TOOLS             directory with buf, golangci-lint, ... (default .tools/bin)
`)
	os.Exit(2)
}

func toolsDir() string {
	if d := os.Getenv("FLIESSBAND_TOOLS"); d != "" {
		return d
	}
	abs, _ := filepath.Abs(".tools/bin")
	return abs
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func serveCmd(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", envOr("FLIESSBAND_ADDR", "127.0.0.1:7070"), "listen address")
	data := fs.String("data", envOr("FLIESSBAND_DATA", "data"), "directory for runs")
	parallel := fs.Int("parallel", 2, "runs executed at the same time")
	queue := fs.Int("queue", 20, "runs that may wait for a free slot")
	keep := fs.Int("keep", 200, "finished runs kept on disk")
	_ = fs.Parse(args)
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))

	host, _, err := net.SplitHostPort(*addr)
	if err != nil {
		return fmt.Errorf("-addr: %w", err)
	}
	ip := net.ParseIP(host)
	loopback := host == "localhost" || (ip != nil && ip.IsLoopback())
	token := os.Getenv("FLIESSBAND_TOKEN")
	if !loopback && token == "" {
		return fmt.Errorf("refusing to listen on %s without FLIESSBAND_TOKEN: anyone who reaches it could run code on this machine", *addr)
	}

	m, err := pipeline.NewManager(*data, toolsDir(), llm.FromEnv())
	if err != nil {
		return err
	}
	m.SetLimits(*parallel, *queue, *keep)
	var origins []string
	for _, o := range strings.Split(os.Getenv("FLIESSBAND_TRUSTED_ORIGINS"), ",") {
		if o = strings.TrimSpace(o); o != "" {
			origins = append(origins, o)
		}
	}
	h, err := server.New(m, server.Config{SpecsDir: "specs", Token: token, LoopbackOnly: loopback, TrustedOrigins: origins, Log: log})
	if err != nil {
		return err
	}
	srv := &http.Server{Addr: *addr, Handler: h, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: time.Minute,
		IdleTimeout: 2 * time.Minute, MaxHeaderBytes: 64 << 10} // no write timeout: live runs stream for minutes

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	llmInfo := "off"
	if c := m.LLM(); c != nil {
		llmInfo = c.Model + " at " + c.BaseURL
	}
	log.Info("fliessband listening", "url", "http://"+*addr, "version", pipeline.Version, "llm", llmInfo, "auth", token != "",
		"parallel", *parallel, "queue", *queue, "keep", *keep)
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	log.Info("shutting down: canceling active runs")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := m.Shutdown(shutdownCtx); err != nil {
		log.Warn("runs did not stop in time", "err", err)
	}
	if err := srv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	log.Info("stopped")
	return nil
}

func runCmd(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	data := fs.String("data", "data", "directory for runs")
	useLLM := fs.Bool("llm", false, "let the LLM implement custom RPCs")
	model := fs.String("model", "", "LLM model (default from FLIESSBAND_LLM_MODEL)")
	smoke := fs.Bool("smoke", true, "start the binary and load-test it")
	vuln := fs.Bool("vuln", true, "run govulncheck")
	mutation := fs.Bool("mutation", true, "run mutation testing")
	maxMutants := fs.Int("max-mutants", 0, "mutants to run at most (default 150)")
	fuzz := fs.Bool("fuzz", true, "run the fuzz targets")
	fuzzTime := fs.String("fuzztime", "3s", "fuzzing time per target")
	integration := fs.Bool("integration", true, "test the PostgreSQL store in Docker when available")
	jsonOut := fs.Bool("json", false, "print the metrics report as JSON")
	_ = fs.Parse(args)
	if fs.NArg() != 1 {
		usage()
	}
	src, err := os.ReadFile(fs.Arg(0))
	if err != nil {
		return err
	}
	m, err := pipeline.NewManager(*data, toolsDir(), llm.FromEnv())
	if err != nil {
		return err
	}
	r, err := m.Start(src, pipeline.Options{LLM: *useLLM, Model: *model, Smoke: *smoke, Vuln: *vuln, Mutation: *mutation,
		MaxMutants: *maxMutants, Fuzz: *fuzz, FuzzTime: *fuzzTime, Integration: *integration})
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		m.Cancel(r.ID)
	}()
	events, _ := r.Subscribe()
	for ev := range events {
		if ev.Type != "stage" {
			continue
		}
		st := ev.Data.(pipeline.StageState)
		if st.Status == pipeline.Running {
			continue
		}
		mark := map[pipeline.Status]string{pipeline.Passed: "✓", pipeline.Failed: "✗", pipeline.Warning: "!",
			pipeline.Skipped: "–", pipeline.Pending: "↺", pipeline.Canceled: "⊘"}[st.Status]
		fmt.Printf("%s %-16s %7.0f ms  %s\n", mark, st.Name, st.DurationMs, st.Summary)
	}
	m.Wait(r.ID)
	var final pipeline.Run
	if err := json.Unmarshal(r.JSON(), &final); err != nil {
		return err
	}
	fmt.Printf("\nrun %s: %s, service in %s\n", final.ID, final.Status, r.ServiceDir())
	if *jsonOut && final.Report != nil {
		b, _ := json.MarshalIndent(final.Report, "", "  ")
		fmt.Println(string(b))
	}
	if final.Status != pipeline.Passed {
		return fmt.Errorf("run %s", final.Status)
	}
	return nil
}

func genCmd(args []string) error {
	fs := flag.NewFlagSet("gen", flag.ExitOnError)
	out := fs.String("o", "out", "output directory")
	_ = fs.Parse(args)
	if fs.NArg() != 1 {
		usage()
	}
	src, err := os.ReadFile(fs.Arg(0))
	if err != nil {
		return err
	}
	s, problems, err := spec.Parse(src)
	if err != nil {
		return err
	}
	for _, p := range problems {
		fmt.Fprintln(os.Stderr, p)
	}
	if spec.HasErrors(problems) {
		return fmt.Errorf("spec has errors")
	}
	start := time.Now()
	files, err := gen.Generate(s)
	if err != nil {
		return err
	}
	if err := gen.WriteFiles(*out, files); err != nil {
		return err
	}
	fmt.Printf("wrote %d files to %s in %s\n", len(files), *out, time.Since(start).Round(time.Millisecond))
	return gen.NewManifest(src, files).Save(*out)
}
