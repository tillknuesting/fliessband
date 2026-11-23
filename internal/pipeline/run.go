// Package pipeline runs a spec through the factory line: generate, check,
// build, test, smoke-test and measure, publishing live events per run.
package pipeline

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/tillknuesting/fliessband/internal/metrics"
	"github.com/tillknuesting/fliessband/internal/spec"
)

// Status of a run or stage.
type Status string

const (
	Pending  Status = "pending"
	Running  Status = "running"
	Passed   Status = "passed"
	Failed   Status = "failed"
	Warning  Status = "warning"
	Skipped  Status = "skipped"
	Canceled Status = "canceled"
)

// StageDef describes a station on the line.
type StageDef struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Tool  string `json:"tool"`
	Gate  bool   `json:"gate"`
	Phase string `json:"phase"`
}

// Stages is the line, in order. Gates stop the line when they fail; the
// other stations measure and report.
var Stages = []StageDef{
	{"spec", "Spec", "fliessband/spec", true, "Contract"},
	{"scaffold", "Scaffold", "templates", true, "Contract"},
	{"protolint", "Proto lint", "buf lint", true, "Contract"},
	{"breaking", "Compatibility", "buf breaking", true, "Contract"},
	{"codegen", "Codegen", "buf generate", true, "Contract"},
	{"implement", "Implement", "LLM", false, "Build"},
	{"policy", "Policy", "fliessband/policy", true, "Build"},
	{"deps", "Dependencies", "go mod tidy", true, "Build"},
	{"build", "Build", "go build", true, "Build"},
	{"vet", "Vet", "go vet", true, "Build"},
	{"lint", "Lint", "golangci-lint, go fix", false, "Build"},
	{"test", "Test", "go test -race", true, "Verify"},
	{"acceptance", "Acceptance", "spec examples", true, "Verify"},
	{"review", "Review", "second-model review", false, "Verify"},
	{"mutation", "Mutation", "fliessband/mutate", false, "Verify"},
	{"fuzz", "Fuzz", "go test -fuzz", false, "Verify"},
	{"vulncheck", "Vulnerabilities", "govulncheck", false, "Verify"},
	{"integration", "Integration", "PostgreSQL in Docker", true, "Run"},
	{"smoke", "Smoke", "grpc health + load", true, "Run"},
	{"sbom", "Supply chain", "CycloneDX, SLSA", false, "Ship"},
	{"measure", "Measure", "fliessband/metrics", false, "Ship"},
}

// StageState is the live state of one stage.
type StageState struct {
	StageDef
	Status     Status     `json:"status"`
	Summary    string     `json:"summary"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	DurationMs float64    `json:"duration_ms"`
	Attempt    int        `json:"attempt,omitempty"`
	Log        []string   `json:"log"`
}

// Options control one run.
type Options struct {
	LLM   bool   `json:"llm"`
	Model string `json:"model,omitempty"`
	// Models compete to implement the custom RPCs; more than one starts a
	// tournament. Empty means the factory's configured candidates.
	Models      []string `json:"models,omitempty"`
	Reviewer    string   `json:"reviewer,omitempty"` // reviews the result; empty means the factory's default
	Smoke       bool     `json:"smoke"`
	Vuln        bool     `json:"vuln"`
	Mutation    bool     `json:"mutation"`
	Fuzz        bool     `json:"fuzz"`
	Integration bool     `json:"integration"`
	MaxRepairs  int      `json:"max_repairs"`
	MaxMutants  int      `json:"max_mutants,omitempty"`
	FuzzTime    string   `json:"fuzz_time,omitempty"` // per target, e.g. "3s"
	// Base is the run this one improves on: its implementation is the
	// starting point and its measurements tell the model what to fix.
	Base string `json:"base,omitempty"`
}

// DefaultOptions turn every check on.
func DefaultOptions() Options {
	return Options{Smoke: true, Vuln: true, Mutation: true, Fuzz: true, Integration: true}
}

// Run is one pass of a spec through the line.
type Run struct {
	ID         string          `json:"id"`
	Name       string          `json:"name"`
	CreatedAt  time.Time       `json:"created_at"`
	FinishedAt *time.Time      `json:"finished_at,omitempty"`
	Status     Status          `json:"status"`
	Options    Options         `json:"options"`
	Spec       string          `json:"spec"`
	Problems   []spec.Problem  `json:"problems"`
	Stages     []*StageState   `json:"stages"`
	Report     *metrics.Report `json:"report,omitempty"`

	dir  string
	mu   sync.Mutex
	subs map[chan Event]struct{}
}

// Event is pushed to subscribers while a run executes.
type Event struct {
	Type  string `json:"type"` // snapshot, stage, log, run
	Stage string `json:"stage,omitempty"`
	Line  string `json:"line,omitempty"`
	Data  any    `json:"data,omitempty"`
}

// Summary is the row shown in the run history.
type Summary struct {
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	CreatedAt   time.Time  `json:"created_at"`
	FinishedAt  *time.Time `json:"finished_at,omitempty"`
	Status      Status     `json:"status"`
	LLM         bool       `json:"llm"`
	Score       *float64   `json:"score,omitempty"`
	Grade       string     `json:"grade,omitempty"`
	Coverage    *float64   `json:"coverage,omitempty"`
	MeanCyclo   *float64   `json:"mean_cyclomatic,omitempty"`
	MeanMI      *float64   `json:"mean_mi,omitempty"`
	Code        int        `json:"code_lines"`
	Tests       int        `json:"tests"`
	Findings    int        `json:"findings"`
	RPCs        int        `json:"rpcs"`
	Implemented int        `json:"implemented"`
	DurationMs  float64    `json:"duration_ms"`
	P99Ms       *float64   `json:"p99_ms,omitempty"`
	FailedAt    string     `json:"failed_at,omitempty"`
	Mutation    *float64   `json:"mutation_score,omitempty"`
	Examples    int        `json:"examples"`
	ExamplesOK  int        `json:"examples_passed"`
	CostUSD     float64    `json:"cost_usd,omitempty"`
	Base        string     `json:"base,omitempty"`
	Stages      []Brief    `json:"stages"`
}

// Brief is one station's outcome in a run summary, enough for the floor view
// to show live progress and find bottlenecks across runs.
type Brief struct {
	ID         string     `json:"id"`
	Status     Status     `json:"status"`
	DurationMs float64    `json:"ms"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	Summary    string     `json:"summary,omitempty"`
}

func newRun(id string, src []byte, opts Options, dir string) *Run {
	r := &Run{ID: id, CreatedAt: time.Now().UTC(), Status: Running, Options: opts, Spec: string(src), dir: dir, subs: map[chan Event]struct{}{}}
	for _, d := range Stages {
		r.Stages = append(r.Stages, &StageState{StageDef: d, Status: Pending, Log: []string{}})
	}
	return r
}

func (r *Run) stage(id string) *StageState {
	for _, s := range r.Stages {
		if s.ID == id {
			return s
		}
	}
	panic("unknown stage " + id)
}

// emit runs mutate under the lock and broadcasts ev.
func (r *Run) emit(ev Event, mutate func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if mutate != nil {
		mutate()
	}
	if ev.Type == "" {
		return
	}
	for ch := range r.subs {
		select {
		case ch <- ev:
		default: // slow subscriber: drop it, the client reconnects for a fresh snapshot
			delete(r.subs, ch)
			close(ch)
		}
	}
}

// Subscribe returns a channel that first receives a snapshot of the run and
// then live events. Call the returned function to unsubscribe.
func (r *Run) Subscribe() (<-chan Event, func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	ch := make(chan Event, 4096)
	snap, _ := json.Marshal(r)
	ch <- Event{Type: "snapshot", Data: json.RawMessage(snap)}
	if r.Status == Running {
		r.subs[ch] = struct{}{}
	} else {
		close(ch)
	}
	return ch, func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		if _, ok := r.subs[ch]; ok {
			delete(r.subs, ch)
			close(ch)
		}
	}
}

// JSON returns the run as JSON, taken under the lock.
func (r *Run) JSON() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	b, _ := json.Marshal(r)
	return b
}

// Dir is the run's directory on disk.
func (r *Run) Dir() string { return r.dir }

// ServiceDir is where the generated service lives.
func (r *Run) ServiceDir() string { return filepath.Join(r.dir, "service") }

func (r *Run) save() error {
	b := r.JSON()
	tmp := filepath.Join(r.dir, "run.json.tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(r.dir, "run.json"))
}

// Summary builds the history row.
// Source returns the spec and options the run was started with.
func (r *Run) Source() (string, Options) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.Spec, r.Options
}

func (r *Run) Summary() Summary {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := Summary{ID: r.ID, Name: r.Name, CreatedAt: r.CreatedAt, FinishedAt: r.FinishedAt, Status: r.Status, LLM: r.Options.LLM, Base: r.Options.Base}
	s.Stages = make([]Brief, 0, len(r.Stages))
	for _, st := range r.Stages {
		b := Brief{ID: st.ID, Status: st.Status, DurationMs: st.DurationMs}
		if st.Status == Running {
			b.StartedAt, b.Summary = st.StartedAt, st.Summary
		}
		s.Stages = append(s.Stages, b)
		s.DurationMs += st.DurationMs
		if st.Status == Failed && s.FailedAt == "" {
			s.FailedAt = st.Name
		}
	}
	if rep := r.Report; rep != nil {
		s.Score, s.Grade = &rep.Score.Total, rep.Score.Grade
		if rep.Coverage != nil {
			c := rep.Coverage.ServicePercent()
			s.Coverage = &c
		}
		s.MeanCyclo, s.MeanMI = &rep.Complexity.MeanCyclomatic, &rep.Complexity.MeanMI
		s.Code = rep.Size.ByKind["code"].Code
		if rep.Tests != nil {
			s.Tests = rep.Tests.Total
		}
		s.Findings = rep.Findings.Count()
		s.RPCs, s.Implemented = rep.Completeness.Total, rep.Completeness.Implemented
		if rep.Runtime != nil {
			s.P99Ms = &rep.Runtime.P99Ms
		}
		if rep.Mutation != nil && rep.Mutation.Score != nil {
			s.Mutation = rep.Mutation.Score
		}
		if a := rep.Acceptance; a != nil {
			s.Examples, s.ExamplesOK = len(a.Examples), a.Passed
		}
		s.CostUSD = rep.Factory.CostUSD
	}
	return s
}

func (r *Run) status() Status {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.Status
}

// interrupt marks a run whose process went away while it was running.
func (r *Run) interrupt() {
	now := time.Now().UTC()
	r.Status, r.FinishedAt = Failed, &now
	for _, st := range r.Stages {
		switch st.Status {
		case Running:
			st.Status, st.Summary = Failed, "interrupted: the factory stopped before this station finished"
		case Pending:
			st.Status, st.Summary = Skipped, "not run: the run was interrupted"
		}
	}
}

// cancelQueued marks a run that was canceled before it started.
func (r *Run) cancelQueued() {
	now := time.Now().UTC()
	r.emit(Event{Type: "run", Data: map[string]any{"status": Canceled}}, func() {
		r.Status, r.FinishedAt = Canceled, &now
		for _, st := range r.Stages {
			st.Status, st.Summary = Skipped, "not run: canceled while waiting"
		}
	})
	_ = r.save()
	r.closeSubs()
}

func (r *Run) closeSubs() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for ch := range r.subs {
		close(ch)
		delete(r.subs, ch)
	}
}
