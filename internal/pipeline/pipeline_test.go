package pipeline

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"go/format"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tillknuesting/fliessband/internal/gen"
	"github.com/tillknuesting/fliessband/internal/llm"
	"github.com/tillknuesting/fliessband/internal/metrics"
)

// forbiddenCustom imports os/exec: the policy station must stop it before
// anything compiles it.
const forbiddenCustom = `package service

import (
	"context"
	"os/exec"

	pb "example.com/tasks/gen/tasks/v1"
)

func (s *Server) CompleteTask(ctx context.Context, req *pb.CompleteTaskRequest) (*pb.CompleteTaskResponse, error) {
	_ = exec.Command("true").Run()
	return nil, nil
}
`

const brokenCustom = `package service

import (
	"context"

	pb "example.com/tasks/gen/tasks/v1"
)

func (s *Server) CompleteTask(ctx context.Context, req *pb.CompleteTaskRequest) (*pb.CompleteTaskResponse, error) {
	task, err := s.store.UpdateTask(ctx, req.GetId(), func(t *pb.Task) error {
		t.Completed = true // no such field: the build gate must catch this
		return nil
	})
	if err != nil {
		return nil, s.toStatus(ctx, err)
	}
	return &pb.CompleteTaskResponse{Task: task}, nil
}
`

const fixedCustom = `package service

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "example.com/tasks/gen/tasks/v1"
)

const maxIDLen = 200

// CompleteTask marks the task as done and returns it.
func (s *Server) CompleteTask(ctx context.Context, req *pb.CompleteTaskRequest) (*pb.CompleteTaskResponse, error) {
	if req.GetId() == "" {
		return nil, status.Error(codes.InvalidArgument, "id: is required")
	}
	if len(req.GetId()) > maxIDLen {
		return nil, status.Error(codes.InvalidArgument, "id: too long")
	}
	task, err := s.store.UpdateTask(ctx, req.GetId(), func(t *pb.Task) error {
		t.Done = true
		t.UpdateTime = timestamppb.New(s.now())
		return nil
	})
	if err != nil {
		return nil, s.toStatus(ctx, err)
	}
	return &pb.CompleteTaskResponse{Task: task}, nil
}
`

// customTest leaves the id length boundary untested, so the > to >=
// mutant survives and the hardening round has something to do.
const customTest = `package service

import (
	"testing"

	"google.golang.org/grpc/codes"

	pb "example.com/tasks/gen/tasks/v1"
)

func TestCompleteTask(t *testing.T) {
	c, st := newClient(t)
	x := seedTask(t, c, st, 0)
	resp, err := c.CompleteTask(t.Context(), &pb.CompleteTaskRequest{Id: x.GetId()})
	if err != nil {
		t.Fatal(err)
	}
	if !resp.GetTask().GetDone() {
		t.Fatal("task is not done")
	}
	_, err = c.CompleteTask(t.Context(), &pb.CompleteTaskRequest{Id: "missing"})
	wantCode(t, err, codes.NotFound)
	_, err = c.CompleteTask(t.Context(), &pb.CompleteTaskRequest{})
	wantCode(t, err, codes.InvalidArgument)
}
`

const hardenedTest = customTest + `
func TestCompleteTaskIDLength(t *testing.T) {
	c, _ := newClient(t)
	_, err := c.CompleteTask(t.Context(), &pb.CompleteTaskRequest{Id: strings.Repeat("a", 200)})
	wantCode(t, err, codes.NotFound)
	_, err = c.CompleteTask(t.Context(), &pb.CompleteTaskRequest{Id: strings.Repeat("a", 201)})
	wantCode(t, err, codes.InvalidArgument)
}
`

func block(path, body string) string { return "```go path=" + path + "\n" + body + "```\n" }

func tools(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("runs buf and the Go toolchain")
	}
	dir, _ := filepath.Abs("../../.tools/bin")
	if _, err := os.Stat(filepath.Join(dir, "buf")); err != nil {
		t.Skip("run `make tools` first")
	}
	return dir
}

// TestLLMLine drives the real line with a fake LLM. The first answer
// imports os/exec and tries to overwrite a file it does not own; the
// second does not compile; the third is right but leaves a boundary
// untested; the hardening answer adds the missing test.
func TestLLMLine(t *testing.T) {
	toolsDir := tools(t)
	var mu sync.Mutex
	var prompts []string
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Messages []llm.Message `json:"messages"`
		}
		_ = json.Unmarshal(body, &req)
		mu.Lock()
		prompt := req.Messages[len(req.Messages)-1].Content
		prompts = append(prompts, prompt)
		n := len(prompts)
		mu.Unlock()
		var content string
		switch {
		case strings.Contains(prompt, "mutants (small deliberate faults)"):
			content = block("internal/service/custom_test.go", strings.Replace(hardenedTest, "import (\n", "import (\n\t\"strings\"\n", 1)) +
				block("internal/service/custom.go", "package service // must be rejected: only the test file may change\n")
		case n == 1:
			content = block("internal/service/custom.go", forbiddenCustom) + block("internal/service/custom_test.go", customTest) +
				block("internal/service/service.go", "package service // must be rejected\n")
		case n == 2:
			content = block("internal/service/custom.go", brokenCustom) + block("internal/service/custom_test.go", customTest)
		default:
			content = block("internal/service/custom.go", fixedCustom) + block("internal/service/custom_test.go", customTest)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]string{"role": "assistant", "content": content}}},
			"usage":   map[string]any{"prompt_tokens": 1000, "completion_tokens": 300, "prompt_tokens_details": map[string]int{"cached_tokens": 400}},
		})
	}))
	defer fake.Close()

	src, err := os.ReadFile("../../specs/tasks.yaml")
	if err != nil {
		t.Fatal(err)
	}
	client := &llm.Client{BaseURL: fake.URL, APIKey: "test", Model: "fake-model", HTTP: fake.Client()}
	m, err := NewManager(t.TempDir(), toolsDir, client)
	if err != nil {
		t.Fatal(err)
	}
	r, err := m.Start(src, Options{LLM: true, Mutation: true, Smoke: true})
	if err != nil {
		t.Fatal(err)
	}
	m.Wait(r.ID)

	var got Run
	if err := json.Unmarshal(r.JSON(), &got); err != nil {
		t.Fatal(err)
	}
	for _, st := range got.Stages {
		t.Logf("%-12s %-8s %s", st.ID, st.Status, st.Summary)
	}
	if got.Status != Passed {
		t.Fatalf("run status = %s, want passed", got.Status)
	}
	fx := got.Report.Factory
	if fx.RepairAttempts != 2 || fx.LLMCalls != 4 || fx.FirstPass {
		t.Errorf("repairs %d, calls %d, first pass %v; want 2, 4, false", fx.RepairAttempts, fx.LLMCalls, fx.FirstPass)
	}
	if fx.TokensIn != 4000 || fx.TokensOut != 1200 || fx.TokensCached != 1600 {
		t.Errorf("tokens = %d in (%d cached), %d out", fx.TokensIn, fx.TokensCached, fx.TokensOut)
	}
	var purposes []string
	for _, p := range fx.Prompts {
		purposes = append(purposes, p.Purpose)
		if len(p.SHA256) != 64 {
			t.Errorf("prompt digest %q", p.SHA256)
		}
	}
	if strings.Join(purposes, ",") != "implement,repair,repair,harden" {
		t.Errorf("prompt purposes = %v", purposes)
	}
	if !slices.Contains(fx.RejectedWrites, "internal/service/service.go") || !slices.Contains(fx.RejectedWrites, "internal/service/custom.go") {
		t.Errorf("rejected writes = %v, want service.go (implement) and custom.go (harden)", fx.RejectedWrites)
	}

	// The policy caught os/exec, and the repair prompts carry the failures.
	if !strings.Contains(prompts[1], "os/exec") || !strings.Contains(prompts[1], "policy") {
		t.Errorf("the policy repair prompt does not carry the violation:\n%.800s", prompts[1])
	}
	if !strings.Contains(prompts[2], "Completed") || !strings.Contains(prompts[2], "custom.go") {
		t.Errorf("the build repair prompt does not carry the compiler error:\n%.800s", prompts[2])
	}
	// Visible examples are in the brief; holdouts and the judge are not.
	for i, p := range prompts {
		if !strings.Contains(p, "marks the task as done") {
			t.Errorf("prompt %d lacks the visible example", i)
		}
		if strings.Contains(p, "keeps the other fields") || strings.Contains(p, "by the 3rd") || strings.Contains(p, "acceptanceExamples") {
			t.Errorf("prompt %d leaks a holdout example or the acceptance tests", i)
		}
	}

	acc := got.Report.Acceptance
	if acc == nil || acc.Passed != 3 || acc.Failed != 0 || acc.Holdout != 1 || acc.Pending != 0 {
		t.Errorf("acceptance = %+v, want 3 passed, 1 holdout", acc)
	}
	if c := got.Report.Completeness; c.Implemented != c.Total {
		t.Errorf("completeness = %+v, want every RPC implemented", c)
	}
	mut, h := got.Report.Mutation, fx.Hardening
	if mut == nil || mut.Score == nil || h == nil {
		t.Fatalf("mutation %+v, hardening %+v", mut, h)
	}
	if !h.Kept || h.Survivors == 0 || h.Killed == 0 {
		t.Errorf("hardening = %+v, want kept tests that kill a survivor", h)
	}
	for _, s := range mut.Survivors {
		if s.File == "internal/service/custom.go" && strings.Contains(s.Before, "maxIDLen") {
			t.Errorf("the id length mutant still survives after hardening: %+v", s)
		}
	}

	man, err := gen.LoadManifest(r.ServiceDir())
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{
		"internal/service/custom.go":          gen.OriginLLM,
		"internal/service/custom_test.go":     gen.OriginLLM,
		"internal/service/service.go":         gen.OriginTemplate,
		"internal/service/acceptance_test.go": gen.OriginTemplate,
	} {
		if o := man.Origin(path); o != want {
			t.Errorf("%s origin = %q, want %q", path, o, want)
		}
	}
	b, _ := os.ReadFile(filepath.Join(r.ServiceDir(), "internal/service/service.go"))
	if strings.Contains(string(b), "must be rejected") {
		t.Error("service.go was overwritten by the model")
	}
	b, _ = os.ReadFile(filepath.Join(r.ServiceDir(), "internal/service/custom.go"))
	if !strings.Contains(string(b), "maxIDLen") {
		t.Error("the hardening round changed custom.go")
	}
	prov, err := os.ReadFile(filepath.Join(r.Dir(), "provenance.intoto.json"))
	if err != nil || !strings.Contains(string(prov), fx.Prompts[0].SHA256) || !strings.Contains(string(prov), "https://slsa.dev/provenance/v1") {
		t.Errorf("provenance missing or without the prompt digests: %v", err)
	}
	if _, err := os.Stat(filepath.Join(r.Dir(), "bin")); !os.IsNotExist(err) {
		t.Error("the binary should be removed after the run; the SBOM keeps its digest")
	}
}

func TestParseFilesRejectsUnownedPaths(t *testing.T) {
	reply := block("internal/service/custom.go", "package service\n") + block("cmd/server/main.go", "package main\n")
	files, rejected := llm.ParseFiles(reply, gen.LLMOwnedFiles)
	if len(files) != 1 || files["internal/service/custom.go"] == "" {
		t.Fatalf("files = %v", files)
	}
	if len(rejected) != 1 || rejected[0] != "cmd/server/main.go" {
		t.Fatalf("rejected = %v", rejected)
	}
}

// Generated code, part of it written by an LLM, must never see credentials.
func TestChildEnvScrubsSecrets(t *testing.T) {
	t.Setenv("FLIESSBAND_LLM_API_KEY", "sk-secret")
	t.Setenv("OPENAI_API_KEY", "sk-secret")
	t.Setenv("GITHUB_TOKEN", "ghp-secret")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "aws-secret")
	t.Setenv("GOPROXY", "https://proxy.golang.org")
	m := &Manager{toolsBin: "/opt/tools", goPaths: map[string]string{"GOCACHE": "/tmp/gocache"}}
	for _, network := range []bool{false, true} {
		env := m.childEnv(network, "EXTRA=1")
		joined := strings.Join(env, "\n")
		if strings.Contains(joined, "secret") {
			t.Fatalf("secret leaked into the child environment:\n%s", joined)
		}
		for _, kv := range env {
			k, _, _ := strings.Cut(kv, "=")
			if isSecretName(k) {
				t.Errorf("credential-like variable %s passed to a child process", k)
			}
		}
		if !strings.Contains(joined, "PATH=/opt/tools") || !strings.Contains(joined, "GOTOOLCHAIN=auto") || !strings.Contains(joined, "EXTRA=1") {
			t.Errorf("child environment misses PATH, GOTOOLCHAIN or extras:\n%s", joined)
		}
		offline := strings.Contains(joined, "GOPROXY=off")
		if offline == network {
			t.Errorf("network=%v but GOPROXY=off is %v", network, offline)
		}
	}
}

func TestCancel(t *testing.T) {
	toolsDir := tools(t)
	src, _ := os.ReadFile("../../specs/tasks.yaml")
	m, err := NewManager(t.TempDir(), toolsDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	r, err := m.Start(src, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if !m.Cancel(r.ID) {
		t.Fatal("cancel reported the run as not active")
	}
	m.Wait(r.ID)
	if s := r.status(); s != Canceled {
		t.Fatalf("status = %s, want canceled", s)
	}
	if m.Cancel(r.ID) {
		t.Error("a finished run cannot be canceled")
	}
}

func TestInterruptedRunIsMarked(t *testing.T) {
	dir := t.TempDir()
	run := newRun("20260101-000000-abcd", []byte("name: tasks\n"), Options{}, filepath.Join(dir, "runs", "20260101-000000-abcd"))
	run.stage("spec").Status = Passed
	run.stage("scaffold").Status = Running
	if err := os.MkdirAll(run.Dir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := run.save(); err != nil {
		t.Fatal(err)
	}
	// A pid that does not exist: the process that ran it is gone.
	if err := os.WriteFile(filepath.Join(run.Dir(), "fliessband.pid"), []byte(strconv.Itoa(1<<22+7)), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := NewManager(dir, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	got := m.Get(run.ID)
	if got == nil || got.Status != Failed || got.stage("scaffold").Status != Failed || got.stage("test").Status != Skipped {
		t.Fatalf("interrupted run not marked: %+v", got)
	}
}

func TestQueueLimit(t *testing.T) {
	m, err := NewManager(t.TempDir(), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	m.SetLimits(1, 0, 10)
	m.mu.Lock()
	m.active, m.queued = 1, 0 // the only slot is busy
	m.mu.Unlock()
	if _, err := m.Start([]byte("name: x\n"), Options{}); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("Start with a full queue: %v, want ErrQueueFull", err)
	}
}

// buggyCustom skips input validation: an empty id reaches the store and
// comes back NOT_FOUND instead of INVALID_ARGUMENT. Its own test only
// checks the happy path, so it passes on its own code.
const buggyCustom = `package service

import (
	"context"

	pb "example.com/tasks/gen/tasks/v1"
)

func (s *Server) CompleteTask(ctx context.Context, req *pb.CompleteTaskRequest) (*pb.CompleteTaskResponse, error) {
	task, err := s.store.UpdateTask(ctx, req.GetId(), func(t *pb.Task) error {
		t.Done = true
		return nil
	})
	if err != nil {
		return nil, s.toStatus(ctx, err)
	}
	return &pb.CompleteTaskResponse{Task: task}, nil
}
`

const happyTest = `package service

import (
	"testing"

	pb "example.com/tasks/gen/tasks/v1"
)

func TestCompleteTaskHappy(t *testing.T) {
	c, st := newClient(t)
	x := seedTask(t, c, st, 0)
	resp, err := c.CompleteTask(t.Context(), &pb.CompleteTaskRequest{Id: x.GetId()})
	if err != nil {
		t.Fatal(err)
	}
	if !resp.GetTask().GetDone() {
		t.Fatal("task is not done")
	}
}
`

func TestTournament(t *testing.T) {
	toolsDir := tools(t)
	var mu sync.Mutex
	calls := map[string]int{}
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Model    string        `json:"model"`
			Messages []llm.Message `json:"messages"`
		}
		_ = json.Unmarshal(body, &req)
		mu.Lock()
		calls[req.Model]++
		mu.Unlock()
		var content string
		switch req.Model {
		case "vendor/good":
			content = block("internal/service/custom.go", fixedCustom) + block("internal/service/custom_test.go", customTest)
		case "vendor/buggy":
			content = block("internal/service/custom.go", buggyCustom) + block("internal/service/custom_test.go", happyTest)
		case "vendor/broken":
			content = block("internal/service/custom.go", brokenCustom) + block("internal/service/custom_test.go", customTest)
		case "vendor/slow":
			// A straggler: it never answers before the tournament's deadline.
			select {
			case <-r.Context().Done():
			case <-time.After(time.Minute):
			}
			return
		case "vendor/reviewer":
			content = "```json\n[{\"file\": \"internal/service/custom.go\", \"line\": 18, \"severity\": \"risk\", \"message\": \"UpdateTime uses the server clock.\"}," +
				"{\"file\": \"internal/service/service.go\", \"line\": 1, \"severity\": \"bug\", \"message\": \"not under review, must be dropped\"}]\n```"
		default:
			http.Error(w, "unknown model", http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]string{"role": "assistant", "content": content}}},
			"usage":   map[string]any{"prompt_tokens": 1000, "completion_tokens": 300, "cost": 0.002},
		})
	}))
	defer fake.Close()

	src, err := os.ReadFile("../../specs/tasks.yaml")
	if err != nil {
		t.Fatal(err)
	}
	client := &llm.Client{BaseURL: fake.URL, APIKey: "test", Model: "vendor/good", HTTP: fake.Client()}
	m, err := NewManager(t.TempDir(), toolsDir, client)
	if err != nil {
		t.Fatal(err)
	}
	oldMin, oldFactor := stragglerMin, stragglerFactor
	stragglerMin, stragglerFactor = 2*time.Second, 1
	defer func() { stragglerMin, stragglerFactor = oldMin, oldFactor }()
	r, err := m.Start(src, Options{LLM: true, Models: []string{"vendor/buggy", "vendor/broken", "vendor/good", "vendor/slow"}, Reviewer: "vendor/reviewer"})
	if err != nil {
		t.Fatal(err)
	}
	m.Wait(r.ID)
	var got Run
	if err := json.Unmarshal(r.JSON(), &got); err != nil {
		t.Fatal(err)
	}
	for _, st := range got.Stages {
		t.Logf("%-12s %-8s %s", st.ID, st.Status, st.Summary)
	}
	if got.Status != Passed {
		t.Fatalf("run status = %s, want passed", got.Status)
	}
	fx := got.Report.Factory
	tour := fx.Tournament
	if tour == nil || tour.Winner != "vendor/good" || fx.LLMModel != "vendor/good" {
		t.Fatalf("tournament = %+v, want vendor/good chosen", tour)
	}
	byModel := map[string]metrics.Candidate{}
	for _, c := range tour.Candidates {
		byModel[c.Model] = c
		t.Logf("%-14s %-8s own %s visible %s foreign %s score %.2f", c.Model, c.Status, c.Own, c.Visible, c.Foreign, c.Score)
	}
	if c := byModel["vendor/slow"]; c.Status != "no answer" || !strings.Contains(c.Note, "cut off") {
		t.Errorf("slow candidate = %s (%s), want it cut off after the deadline", c.Status, c.Note)
	}
	if c := byModel["vendor/slow"]; c.Ms > 20_000 {
		t.Errorf("the line waited %.0f ms for the straggler", c.Ms)
	}
	if c := byModel["vendor/broken"]; c.Status != "compile" {
		t.Errorf("broken candidate = %+v, want a compile failure", c)
	}
	if c := byModel["vendor/buggy"]; c.Status != "ran" || c.Own.Passed != 1 || c.Foreign.Passed != 0 || c.Foreign.Total != 1 {
		t.Errorf("buggy candidate = %+v, want own 1/1 and the good model's test failing", c)
	}
	if c := byModel["vendor/good"]; c.Foreign.Passed != 1 || c.Foreign.Total != 1 || !c.Chosen {
		t.Errorf("good candidate = %+v, want the buggy model's test passing and chosen", c)
	}
	rev := fx.Review
	if rev == nil || rev.Model != "vendor/reviewer" || len(rev.Findings) != 1 || rev.Findings[0].Severity != "risk" {
		t.Errorf("review = %+v, want one risk on custom.go", rev)
	}
	if calls["vendor/reviewer"] != 1 || fx.LLMCalls != 5 || fx.CostUSD < 0.0079 { // 4 models, one cut off, and the reviewer
		t.Errorf("calls %v, LLMCalls %d, cost %.4f; want one call per model and $0.008", calls, fx.LLMCalls, fx.CostUSD)
	}
	b, _ := os.ReadFile(filepath.Join(r.ServiceDir(), "internal/service/custom.go"))
	if !strings.Contains(string(b), "maxIDLen") {
		t.Error("the service does not carry the winning implementation")
	}
	// The buggy model lost, but its happy-path test passes on the winner's
	// code, so it is merged into the winner's tests. The broken model's
	// tests never compiled against anything and are not merged.
	if tour.MergedTests != 1 || tour.MergedFrom["vendor/buggy"] != 1 || len(tour.Merged) != 1 || tour.Merged[0] != "vendor/buggy: TestCompleteTaskHappy" {
		t.Errorf("merged %d %v %v (%s), want TestCompleteTaskHappy from vendor/buggy", tour.MergedTests, tour.MergedFrom, tour.Merged, tour.MergeNote)
	}
	b, _ = os.ReadFile(filepath.Join(r.ServiceDir(), "internal/service/custom_test.go"))
	if !strings.Contains(string(b), "func TestCompleteTaskHappy(t *testing.T)") || !strings.Contains(string(b), "func TestCompleteTask(t *testing.T)") {
		t.Errorf("the merged test is not in the final test file:\n%s", b)
	}
	if tr := got.Report.Tests; tr == nil || tr.Failed != 0 || tr.TopLevel < 2 {
		t.Errorf("tests = %+v, want both tests passing in the test station", tr)
	}
}

// lenientCustom is fixedCustom without the id length check: it passes every
// gate, but the reviewer calls the missing check a bug.
var lenientCustom = strings.Replace(strings.Replace(fixedCustom, "const maxIDLen = 200\n\n", "", 1),
	"\tif len(req.GetId()) > maxIDLen {\n\t\treturn nil, status.Error(codes.InvalidArgument, \"id: too long\")\n\t}\n", "", 1)

// TestReviewFix drives the review fix: the reviewer reports a bug, the
// implementing model answers once, and the factory keeps the answer only if
// every gate still passes.
func TestReviewFix(t *testing.T) {
	toolsDir := tools(t)
	hardened := strings.Replace(hardenedTest, "import (\n", "import (\n\t\"strings\"\n", 1)
	for _, tc := range []struct {
		name   string
		fix    string // custom.go of the fixing answer
		kept   bool
		reason string // in the note when not kept
	}{
		{name: "kept", fix: fixedCustom, kept: true},
		// The "fix" turns the empty-id error into FAILED_PRECONDITION, which
		// TestCompleteTask catches.
		{name: "reverted", fix: strings.Replace(fixedCustom, `codes.InvalidArgument, "id: is required"`, `codes.FailedPrecondition, "id: is required"`, 1),
			reason: "TestCompleteTask"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var mu sync.Mutex
			var fixPrompts []string
			fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				var req struct {
					Model    string        `json:"model"`
					Messages []llm.Message `json:"messages"`
				}
				_ = json.Unmarshal(body, &req)
				prompt := req.Messages[len(req.Messages)-1].Content
				var content string
				switch {
				case req.Model == "vendor/reviewer":
					content = "```json\n[{\"file\": \"internal/service/custom.go\", \"line\": 18, \"severity\": \"bug\", \"message\": \"Ids longer than 200 characters are not rejected.\"}]\n```"
				case strings.Contains(prompt, "Reviewer findings"):
					mu.Lock()
					fixPrompts = append(fixPrompts, prompt)
					mu.Unlock()
					content = block("internal/service/custom.go", tc.fix) + block("internal/service/custom_test.go", hardened)
				default:
					content = block("internal/service/custom.go", lenientCustom) + block("internal/service/custom_test.go", customTest)
				}
				_ = json.NewEncoder(w).Encode(map[string]any{
					"choices": []any{map[string]any{"message": map[string]string{"role": "assistant", "content": content}}},
					"usage":   map[string]any{"prompt_tokens": 1000, "completion_tokens": 300},
				})
			}))
			defer fake.Close()

			src, err := os.ReadFile("../../specs/tasks.yaml")
			if err != nil {
				t.Fatal(err)
			}
			client := &llm.Client{BaseURL: fake.URL, APIKey: "test", Model: "vendor/lenient", HTTP: fake.Client()}
			m, err := NewManager(t.TempDir(), toolsDir, client)
			if err != nil {
				t.Fatal(err)
			}
			r, err := m.Start(src, Options{LLM: true, Reviewer: "vendor/reviewer"})
			if err != nil {
				t.Fatal(err)
			}
			m.Wait(r.ID)
			var got Run
			if err := json.Unmarshal(r.JSON(), &got); err != nil {
				t.Fatal(err)
			}
			for _, st := range got.Stages {
				t.Logf("%-12s %-8s %s", st.ID, st.Status, st.Summary)
			}
			if got.Status != Passed {
				t.Fatalf("run status = %s, want passed", got.Status)
			}
			fx := got.Report.Factory
			rev := fx.Review
			if rev == nil || !rev.FixAttempted || rev.FixModel != "vendor/lenient" || rev.FixKept != tc.kept {
				t.Fatalf("review = %+v, want a fix attempt by vendor/lenient, kept %v", rev, tc.kept)
			}
			t.Logf("fix note: %s", rev.FixNote)
			var purposes []string
			for _, p := range fx.Prompts {
				purposes = append(purposes, p.Purpose)
			}
			if strings.Join(purposes, ",") != "implement,review,review-fix" || fx.LLMCalls != 3 || fx.TokensIn != 3000 {
				t.Errorf("purposes %v, calls %d, tokens in %d; want implement, review, review-fix", purposes, fx.LLMCalls, fx.TokensIn)
			}
			if len(fixPrompts) != 1 {
				t.Fatalf("%d fix prompts, want exactly one", len(fixPrompts))
			}
			p := fixPrompts[0]
			if !strings.Contains(p, "Ids longer than 200 characters") || !strings.Contains(p, "  18  ") {
				t.Errorf("the fix prompt lacks the finding or its numbered lines:\n%.1500s", p)
			}
			if strings.Contains(p, "keeps the other fields") || strings.Contains(p, "by the 3rd") || strings.Contains(p, "acceptanceExamples") {
				t.Error("the fix prompt leaks a holdout example or the acceptance tests")
			}
			if st := got.stage("test"); st.Status != Passed {
				t.Errorf("test station %s", st.Status)
			}
			if acc := got.Report.Acceptance; acc == nil || acc.Passed != 3 || acc.Failed != 0 {
				t.Errorf("acceptance = %+v, want all 3 examples passing", acc)
			}
			if tr := got.Report.Tests; tr == nil || tr.Failed != 0 {
				t.Errorf("tests = %+v, want no failures", tr)
			}

			impl, _ := os.ReadFile(filepath.Join(r.ServiceDir(), "internal/service/custom.go"))
			tests, _ := os.ReadFile(filepath.Join(r.ServiceDir(), "internal/service/custom_test.go"))
			man, err := gen.LoadManifest(r.ServiceDir())
			if err != nil {
				t.Fatal(err)
			}
			for _, f := range man.Files {
				if f.Path == "internal/service/custom.go" || f.Path == "internal/service/custom_test.go" {
					b, _ := os.ReadFile(filepath.Join(r.ServiceDir(), f.Path))
					if sum := sha256.Sum256(b); hex.EncodeToString(sum[:]) != f.SHA256 {
						t.Errorf("the manifest digest of %s does not match the file", f.Path)
					}
				}
			}
			if tc.kept {
				if rev.FixedFindings != 1 || got.stage("review").Status != Passed {
					t.Errorf("fixed findings %d, review %s; want 1, passed", rev.FixedFindings, got.stage("review").Status)
				}
				if !strings.Contains(string(impl), "maxIDLen") || !strings.Contains(string(tests), "func TestCompleteTaskIDLength") {
					t.Error("the kept fix is not in the service")
				}
				if tr := got.Report.Tests; tr == nil || tr.TopLevel < 2 {
					t.Errorf("tests = %+v, want the report to count the new test", tr)
				}
				return
			}
			if !strings.Contains(rev.FixNote, tc.reason) || got.stage("review").Status != Warning {
				t.Errorf("note %q, review %s; want %q and a warning", rev.FixNote, got.stage("review").Status, tc.reason)
			}
			wantImpl, _ := format.Source([]byte(lenientCustom))
			wantTests, _ := format.Source([]byte(customTest))
			if string(impl) != string(wantImpl) || string(tests) != string(wantTests) {
				t.Errorf("the rejected fix was not reverted exactly:\n%s\n%s", impl, tests)
			}
		})
	}
}
