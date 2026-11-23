package pipeline

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tillknuesting/fliessband/internal/llm"
	"github.com/tillknuesting/fliessband/internal/metrics"
	"github.com/tillknuesting/fliessband/internal/spec"
)

// Errors returned by Start.
var (
	ErrQueueFull = errors.New("the factory queue is full; try again when a run finishes")
	ErrClosed    = errors.New("the factory is shutting down")
)

// Manager owns all runs and executes new ones.
type Manager struct {
	dataDir  string
	toolsBin string
	llm      *llm.Client
	goPaths  map[string]string

	keep      int // finished runs kept on disk
	maxQueued int // runs waiting for a slot

	mu      sync.Mutex
	runs    map[string]*Run
	done    map[string]chan struct{}
	cancels map[string]context.CancelFunc
	queued  int
	active  int
	closed  bool
	sem     chan struct{}
	stats   Stats

	dockerMu   sync.Mutex
	dockerSeen time.Time
	dockerInfo dockerInfo

	toolsMu   sync.Mutex
	tools     []Tool
	toolsSeen time.Time
}

// Stats are counters for the factory's /metrics endpoint.
type Stats struct {
	Runs         map[Status]int     `json:"runs"`
	StageSeconds map[string]float64 `json:"stage_seconds"`
	StageCount   map[string]int     `json:"stage_count"`
	StageFailed  map[string]int     `json:"stage_failed"`
	TokensIn     int                `json:"tokens_in"`
	TokensOut    int                `json:"tokens_out"`
	Active       int                `json:"active"`
	Queued       int                `json:"queued"`
	Stored       int                `json:"stored"`
}

// NewManager loads past runs from dataDir/runs. toolsBin is searched for
// tools before PATH. client may be nil (no LLM).
func NewManager(dataDir, toolsBin string, client *llm.Client) (*Manager, error) {
	dataDir, err := filepath.Abs(dataDir)
	if err != nil {
		return nil, err
	}
	m := &Manager{
		dataDir:   dataDir,
		toolsBin:  toolsBin,
		llm:       client,
		keep:      200,
		maxQueued: 20,
		runs:      map[string]*Run{},
		done:      map[string]chan struct{}{},
		cancels:   map[string]context.CancelFunc{},
		sem:       make(chan struct{}, 2),
		stats: Stats{Runs: map[Status]int{}, StageSeconds: map[string]float64{},
			StageCount: map[string]int{}, StageFailed: map[string]int{}},
	}
	if err := os.MkdirAll(filepath.Join(dataDir, "runs"), 0o750); err != nil {
		return nil, err
	}
	m.goPaths = goEnv()
	m.rescan()
	return m, nil
}

// SetLimits configures concurrency, queue length and retention. Call it
// before starting runs.
func (m *Manager) SetLimits(parallel, maxQueued, keep int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if parallel > 0 {
		m.sem = make(chan struct{}, parallel)
	}
	if maxQueued >= 0 {
		m.maxQueued = maxQueued
	}
	if keep > 0 {
		m.keep = keep
	}
}

// rescan loads runs from disk that this process does not know yet, such as
// runs started with "fliessband run" while the UI is up. A run that claims to be
// running but whose process is gone was interrupted, and is marked so.
func (m *Manager) rescan() {
	entries, _ := os.ReadDir(filepath.Join(m.dataDir, "runs"))
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range entries {
		if _, ok := m.runs[e.Name()]; ok || !e.IsDir() {
			continue
		}
		dir := filepath.Join(m.dataDir, "runs", e.Name())
		b, err := os.ReadFile(filepath.Join(dir, "run.json"))
		if err != nil {
			continue
		}
		r := &Run{subs: map[chan Event]struct{}{}}
		if json.Unmarshal(b, r) != nil || r.ID != e.Name() {
			continue
		}
		r.dir = dir
		if r.Status == Running {
			if pid := readPID(dir); pid != os.Getpid() && processAlive(pid) {
				continue // another fliessband process is running it; look again later
			}
			r.interrupt()
			_ = r.save()
		}
		m.runs[r.ID] = r
	}
}

func readPID(dir string) int {
	b, err := os.ReadFile(filepath.Join(dir, "fliessband.pid"))
	if err != nil {
		return 0
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	return pid
}

// LLM returns the configured LLM client, or nil.
func (m *Manager) LLM() *llm.Client { return m.llm }

// Start creates a run for the spec source and executes it in the background.
func (m *Manager) Start(src []byte, opts Options) (*Run, error) {
	if opts.MaxRepairs == 0 {
		opts.MaxRepairs = 3
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, ErrClosed
	}
	if m.queued >= m.maxQueued+cap(m.sem)-m.active {
		m.mu.Unlock()
		return nil, ErrQueueFull
	}
	b := make([]byte, 2)
	_, _ = rand.Read(b)
	id := time.Now().Format("20060102-150405") + "-" + hex.EncodeToString(b)
	r := newRun(id, src, opts, filepath.Join(m.dataDir, "runs", id))
	r.Name = guessName(src)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	m.runs[id], m.done[id], m.cancels[id] = r, done, cancel
	m.queued++
	m.mu.Unlock()

	if err := os.MkdirAll(r.ServiceDir(), 0o750); err == nil {
		_ = r.save()
	}
	go func() {
		defer close(done)
		defer cancel()
		select {
		case m.sem <- struct{}{}:
		case <-ctx.Done():
			m.mu.Lock()
			m.queued--
			m.mu.Unlock()
			r.cancelQueued()
			m.finished(r)
			return
		}
		m.mu.Lock()
		m.queued--
		m.active++
		m.mu.Unlock()
		defer func() {
			<-m.sem
			m.mu.Lock()
			m.active--
			m.mu.Unlock()
		}()
		_ = os.WriteFile(filepath.Join(r.Dir(), "fliessband.pid"), []byte(strconv.Itoa(os.Getpid())), 0o600)
		x := &executor{m: m, r: r, src: src, rep: &metrics.Report{Factory: metrics.Factory{StageMs: map[string]float64{}}}}
		x.run(ctx)
		_ = os.Remove(filepath.Join(r.Dir(), "fliessband.pid"))
		_ = os.RemoveAll(filepath.Join(r.Dir(), "bin")) // binaries are large; the SBOM keeps their digest
		m.finished(r)
	}()
	return r, nil
}

// finished records statistics for a completed run and applies retention.
func (m *Manager) finished(r *Run) {
	m.mu.Lock()
	delete(m.cancels, r.ID)
	r.mu.Lock()
	m.stats.Runs[r.Status]++
	for _, st := range r.Stages {
		if st.Status == Pending || st.Status == Skipped {
			continue
		}
		m.stats.StageSeconds[st.ID] += st.DurationMs / 1000
		m.stats.StageCount[st.ID]++
		if st.Status == Failed {
			m.stats.StageFailed[st.ID]++
		}
	}
	if r.Report != nil {
		m.stats.TokensIn += r.Report.Factory.TokensIn
		m.stats.TokensOut += r.Report.Factory.TokensOut
	}
	r.mu.Unlock()
	m.mu.Unlock()
	m.prune()
}

// prune deletes the oldest finished runs beyond the retention limit. The
// newest passed run of each service is always kept: it is the baseline for
// the compatibility check.
func (m *Manager) prune() {
	m.mu.Lock()
	var finished []*Run
	for _, r := range m.runs {
		if r.status() != Running {
			finished = append(finished, r)
		}
	}
	slices.SortFunc(finished, func(a, b *Run) int { return b.CreatedAt.Compare(a.CreatedAt) })
	baseline := map[string]bool{}
	var drop []*Run
	for i, r := range finished {
		keepAsBaseline := r.status() == Passed && !baseline[r.Name]
		if keepAsBaseline {
			baseline[r.Name] = true
		}
		if i >= m.keep && !keepAsBaseline {
			drop = append(drop, r)
			delete(m.runs, r.ID)
			delete(m.done, r.ID)
		}
	}
	m.mu.Unlock()
	for _, r := range drop {
		_ = os.RemoveAll(r.Dir())
	}
}

// Cancel stops a queued or running run. It reports whether the run was active.
func (m *Manager) Cancel(id string) bool {
	m.mu.Lock()
	cancel, ok := m.cancels[id]
	m.mu.Unlock()
	if ok {
		cancel()
	}
	return ok
}

// Shutdown stops accepting runs, cancels the active ones and waits for
// them to record their state, or until ctx ends.
func (m *Manager) Shutdown(ctx context.Context) error {
	m.mu.Lock()
	m.closed = true
	var waits []chan struct{}
	for id, cancel := range m.cancels {
		cancel()
		waits = append(waits, m.done[id])
	}
	m.mu.Unlock()
	for _, ch := range waits {
		select {
		case <-ch:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

// Capacity reports how many lines run at once, how many are busy and how many runs wait.
func (m *Manager) Capacity() (lines, active, queued int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return cap(m.sem), m.active, m.queued
}

// Stats returns a snapshot of the factory counters.
func (m *Manager) Stats() Stats {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := Stats{Runs: map[Status]int{}, StageSeconds: map[string]float64{}, StageCount: map[string]int{}, StageFailed: map[string]int{},
		TokensIn: m.stats.TokensIn, TokensOut: m.stats.TokensOut, Active: m.active, Queued: m.queued, Stored: len(m.runs)}
	for k, v := range m.stats.Runs {
		s.Runs[k] = v
	}
	for k, v := range m.stats.StageSeconds {
		s.StageSeconds[k] = v
	}
	for k, v := range m.stats.StageCount {
		s.StageCount[k] = v
	}
	for k, v := range m.stats.StageFailed {
		s.StageFailed[k] = v
	}
	return s
}

var reName = regexp.MustCompile(`(?m)^name:\s*["']?([A-Za-z0-9-]+)`)

func guessName(src []byte) string {
	if m := reName.FindSubmatch(src); m != nil {
		return strings.ToLower(string(m[1]))
	}
	return "unnamed"
}

// Wait blocks until the run finishes.
func (m *Manager) Wait(id string) {
	m.mu.Lock()
	ch := m.done[id]
	m.mu.Unlock()
	if ch != nil {
		<-ch
	}
}

// Get returns a run by id.
func (m *Manager) Get(id string) *Run {
	m.mu.Lock()
	r := m.runs[id]
	m.mu.Unlock()
	if r == nil {
		m.rescan()
		m.mu.Lock()
		r = m.runs[id]
		m.mu.Unlock()
	}
	return r
}

// List returns run summaries, newest first.
func (m *Manager) List() []Summary {
	m.rescan()
	m.mu.Lock()
	runs := make([]*Run, 0, len(m.runs))
	for _, r := range m.runs {
		runs = append(runs, r)
	}
	m.mu.Unlock()
	out := make([]Summary, 0, len(runs))
	for _, r := range runs {
		out = append(out, r.Summary())
	}
	slices.SortFunc(out, func(a, b Summary) int { return b.CreatedAt.Compare(a.CreatedAt) })
	return out
}

// baseline returns the newest passed run of the named service created
// before the given time, or nil.
func (m *Manager) baseline(name string, before time.Time) *Run {
	m.rescan()
	m.mu.Lock()
	defer m.mu.Unlock()
	var best *Run
	for _, r := range m.runs {
		if r.Name != name || r.status() != Passed || !r.CreatedAt.Before(before) {
			continue
		}
		if _, err := os.Stat(filepath.Join(r.ServiceDir(), "buf.yaml")); err != nil {
			continue
		}
		if best == nil || r.CreatedAt.After(best.CreatedAt) {
			best = r
		}
	}
	return best
}

// basePackage returns the proto package of a run's spec.
func basePackage(r *Run) string {
	s, _, err := spec.Parse([]byte(r.Spec))
	if err != nil {
		return ""
	}
	return s.Package
}

// Tool describes an external tool the line uses.
type Tool struct {
	Name    string `json:"name"`
	Path    string `json:"path"`
	Version string `json:"version"`
	Found   bool   `json:"found"`
}

// Tools reports which tools are available and their versions.
func (m *Manager) Tools() []Tool {
	checks := []struct {
		name string
		args []string
	}{
		{"go", []string{"version"}},
		{"buf", []string{"--version"}},
		{"protoc-gen-go", []string{"--version"}},
		{"protoc-gen-go-grpc", []string{"--version"}},
		{"golangci-lint", []string{"version", "--short"}},
		{"staticcheck", []string{"-version"}},
		{"govulncheck", []string{"-version"}},
		{"docker", []string{"version", "--format", "{{.Client.Version}}"}},
	}
	var out []Tool
	for _, c := range checks {
		t := Tool{Name: c.name}
		if p, ok := m.lookPath(c.name); ok {
			t.Path, t.Found = p, true
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			b, _ := exec.CommandContext(ctx, p, c.args...).CombinedOutput()
			cancel()
			t.Version = firstLine(string(b))
		}
		out = append(out, t)
	}
	return out
}

func (m *Manager) lookPath(name string) (string, bool) {
	if m.toolsBin != "" {
		p := filepath.Join(m.toolsBin, name)
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p, true
		}
	}
	p, err := exec.LookPath(name)
	return p, err == nil
}

func firstLine(s string) string {
	sc := bufio.NewScanner(strings.NewReader(s))
	for sc.Scan() {
		if t := strings.TrimSpace(sc.Text()); t != "" {
			return t
		}
	}
	return ""
}

// cmdResult is the outcome of an external command.
type cmdResult struct {
	stdout, stderr string
	code           int
}

// cmdOpts tune one command.
type cmdOpts struct {
	quiet   bool     // do not stream stdout into the log (JSON output)
	network bool     // allow module downloads
	dir     string   // working directory; default the service directory
	env     []string // extra environment
	timeout time.Duration
}

// command runs a tool in an isolated process group with the scrubbed
// environment, streaming output lines into the stage log.
func (x *executor) command(ctx context.Context, st *StageState, o cmdOpts, name string, args ...string) (cmdResult, error) {
	path, ok := x.m.lookPath(name)
	if !ok {
		return cmdResult{}, fmt.Errorf("%s not found: run `make tools` in the fliessband directory", name)
	}
	if o.timeout == 0 {
		o.timeout = 5 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, o.timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Dir = x.r.ServiceDir()
	if o.dir != "" {
		cmd.Dir = o.dir
	}
	cmd.Env = x.m.childEnv(o.network, o.env...)
	isolate(cmd)
	x.log(st, "$ "+name+" "+strings.Join(args, " "))
	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()
	if err := cmd.Start(); err != nil {
		return cmdResult{}, err
	}
	var wg sync.WaitGroup
	var outB, errB strings.Builder
	pump := func(r io.Reader, b *strings.Builder, stream bool) {
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 1<<20), 8<<20)
		for sc.Scan() {
			line := sc.Text()
			b.WriteString(line)
			b.WriteByte('\n')
			if stream {
				x.log(st, line)
			}
		}
	}
	wg.Go(func() { pump(stdout, &outB, !o.quiet) })
	wg.Go(func() { pump(stderr, &errB, true) })
	wg.Wait()
	err := cmd.Wait()
	res := cmdResult{stdout: outB.String(), stderr: errB.String()}
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return res, fmt.Errorf("%s timed out after %s", name, o.timeout)
	case ctx.Err() != nil:
		return res, errCanceled
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		res.code = exitErr.ExitCode()
		return res, nil
	}
	return res, err
}

var errCanceled = errors.New("canceled")
