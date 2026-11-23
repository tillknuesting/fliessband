package pipeline

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/tillknuesting/fliessband/internal/gen"
	"github.com/tillknuesting/fliessband/internal/llm"
	"github.com/tillknuesting/fliessband/internal/metrics"
	"github.com/tillknuesting/fliessband/internal/policy"
	"github.com/tillknuesting/fliessband/internal/security"
	"github.com/tillknuesting/fliessband/internal/spec"
)

// Version is the factory version recorded in SBOMs and provenance.
const Version = "0.2.0"

// executor runs one Run through the line.
type executor struct {
	m         *Manager
	r         *Run
	src       []byte
	spec      *spec.Spec
	model     *gen.Model
	man       *gen.Manifest
	rep       *metrics.Report
	failOut   string // output of the last failing gate, for the repair loop
	failStage string
	llmWrote  bool
	winner    string   // the model whose implementation was chosen
	testJSON  string   // go test -json output of the last test run
	pending   []string // custom methods still stubbed
	db        *postgresDB
	started   time.Time
}

// outcome lets a stage finish as skipped or warning instead of passed/failed.
type outcome struct {
	status Status
	msg    string
}

func (o outcome) Error() string { return o.msg }

func skip(format string, a ...any) error { return outcome{Skipped, fmt.Sprintf(format, a...)} }
func warn(format string, a ...any) error { return outcome{Warning, fmt.Sprintf(format, a...)} }

const maxLogLines = 4000

func (x *executor) log(st *StageState, line string) {
	x.r.emit(Event{Type: "log", Stage: st.ID, Line: line}, func() {
		switch {
		case len(st.Log) < maxLogLines:
			st.Log = append(st.Log, line)
		case len(st.Log) == maxLogLines:
			st.Log = append(st.Log, "… log truncated")
		}
	})
}

func (x *executor) setStage(st *StageState, mutate func(*StageState)) {
	x.r.mu.Lock()
	mutate(st)
	view := *st
	view.Log = nil
	x.r.mu.Unlock()
	x.r.emit(Event{Type: "stage", Stage: st.ID, Data: view}, nil)
}

func (x *executor) summary(st *StageState, format string, a ...any) {
	x.setStage(st, func(s *StageState) { s.Summary = fmt.Sprintf(format, a...) })
}

// do runs one stage. It returns false when a gate fails or the run is canceled.
func (x *executor) do(ctx context.Context, id string, fn func(context.Context, *StageState) error) bool {
	st := x.r.stage(id)
	if ctx.Err() != nil {
		return false
	}
	start := time.Now()
	x.setStage(st, func(s *StageState) {
		s.Status, s.StartedAt, s.Summary = Running, &start, ""
	})
	err := fn(ctx, st)
	if ctx.Err() != nil && err != nil {
		err = errCanceled
	}
	elapsed := float64(time.Since(start).Microseconds()) / 1000
	x.setStage(st, func(s *StageState) {
		s.DurationMs += elapsed
		var o outcome
		switch {
		case err == nil:
			s.Status = Passed
		case errors.Is(err, errCanceled):
			s.Status, s.Summary = Canceled, "canceled"
		case errors.As(err, &o):
			s.Status, s.Summary = o.status, o.msg
		case s.Gate:
			s.Status, s.Summary = Failed, err.Error()
		default:
			s.Status, s.Summary = Warning, err.Error()
		}
	})
	if err != nil {
		var o outcome
		if !errors.As(err, &o) && !errors.Is(err, errCanceled) {
			x.log(st, "✗ "+err.Error())
		}
	}
	x.rep.Factory.StageMs[id] += elapsed
	_ = x.r.save()
	return st.Status != Failed && st.Status != Canceled
}

func (x *executor) run(ctx context.Context) {
	x.started = time.Now()
	defer func() {
		if x.db != nil {
			x.db.stop()
		}
	}()
	ok := x.do(ctx, "spec", x.stageSpec) &&
		x.do(ctx, "scaffold", x.stageScaffold) &&
		x.do(ctx, "protolint", x.stageProtoLint) &&
		x.do(ctx, "breaking", x.stageBreaking) &&
		x.do(ctx, "migrations", x.stageMigrations) &&
		x.do(ctx, "codegen", x.stageCodegen)
	if ok {
		x.do(ctx, "implement", x.stageImplement)
		ok = x.verify(ctx)
	}
	if ok {
		x.do(ctx, "review", x.stageReview)
		if x.r.Options.Mutation {
			x.do(ctx, "mutation", x.stageMutation)
		}
		if x.r.Options.Fuzz {
			x.do(ctx, "fuzz", x.stageFuzz)
		}
		if x.r.Options.Vuln {
			x.do(ctx, "vulncheck", x.stageVuln)
		}
		ok = x.do(ctx, "integration", x.stageIntegration)
		if ok && x.r.Options.Smoke {
			ok = x.do(ctx, "smoke", x.stageSmoke)
		}
		if ok {
			x.do(ctx, "sbom", x.stageSBOM)
		}
	}
	canceled := ctx.Err() != nil
	gates := !canceled
	for _, st := range x.r.Stages {
		if st.Status == Failed {
			gates = false
		}
	}
	for _, st := range x.r.Stages {
		if st.Status == Pending && st.ID != "measure" {
			reason := "turned off for this run"
			switch {
			case canceled:
				reason = "not run: the run was canceled"
			case !gates:
				reason = "not run: an earlier gate failed"
			}
			x.setStage(st, func(s *StageState) { s.Status, s.Summary = Skipped, reason })
		}
	}
	x.rep.Factory.TotalMs = float64(time.Since(x.started).Milliseconds())
	if canceled {
		x.setStage(x.r.stage("measure"), func(s *StageState) { s.Status, s.Summary = Skipped, "not run: the run was canceled" })
	} else {
		x.do(ctx, "measure", func(ctx context.Context, st *StageState) error { return x.stageMeasure(ctx, st, gates) })
	}
	now := time.Now().UTC()
	final := Passed
	switch {
	case canceled:
		final = Canceled
	case !gates:
		final = Failed
	}
	x.r.emit(Event{Type: "run", Data: map[string]any{"status": final}}, func() {
		x.r.Status, x.r.FinishedAt = final, &now
		if !canceled && (x.rep.Size.Files != nil || x.rep.Tests != nil) {
			x.r.Report = x.rep // a canceled run was not measured
		}
	})
	_ = x.r.save()
	x.r.closeSubs()
}

// verify runs the policy, build, vet, lint, test and acceptance stations,
// asking the LLM to repair its own files when a gate fails on them.
func (x *executor) verify(ctx context.Context) bool {
	gates := []string{"policy", "build", "vet", "lint", "test", "acceptance"}
	depsDone := false
	for attempt := 0; ; attempt++ {
		x.failOut, x.failStage = "", ""
		ok := x.do(ctx, "policy", x.stagePolicy)
		if ok && !depsDone {
			// Dependencies resolve once, before the first build; later
			// stations run offline against the module cache.
			ok = x.do(ctx, "deps", x.stageDeps)
			depsDone = ok
		}
		ok = ok && x.do(ctx, "build", x.stageBuild) && x.do(ctx, "vet", x.stageVet)
		if ok {
			x.do(ctx, "lint", x.stageLint)
			ok = x.do(ctx, "test", x.stageTest) && x.do(ctx, "acceptance", x.stageAcceptance)
		}
		if ok {
			x.rep.Factory.FirstPass = attempt == 0
			return true
		}
		if ctx.Err() != nil || !x.canRepair() || attempt >= x.r.Options.MaxRepairs {
			return false
		}
		x.rep.Factory.RepairAttempts = attempt + 1
		if !x.repair(ctx, x.failStage, attempt+1) {
			return false
		}
		for _, id := range gates {
			x.setStage(x.r.stage(id), func(s *StageState) {
				s.Status, s.Summary, s.Attempt = Pending, "", attempt+2
			})
		}
	}
}

// fail records a failing gate's output for the repair loop.
func (x *executor) fail(stage, out string) {
	x.failStage, x.failOut = stage, out
}

// canRepair reports whether the LLM can fix the failure: it must be in
// files the LLM owns, or in acceptance examples it can see.
func (x *executor) canRepair() bool {
	if !x.llmWrote || x.m.llm == nil || x.failStage == "" || x.failOut == "" {
		return false
	}
	if x.failStage == "policy" || x.failStage == "acceptance" {
		return true
	}
	for _, f := range gen.LLMOwnedFiles {
		if strings.Contains(x.failOut, filepath.Base(f)) {
			return true
		}
	}
	return false
}

func (x *executor) stageSpec(_ context.Context, st *StageState) error {
	s, problems, err := spec.Parse(x.src)
	if err != nil {
		return err
	}
	x.r.emit(Event{}, func() {
		x.r.Problems = problems
		x.r.Name = s.Name
	})
	var errs, warns int
	for _, p := range problems {
		x.log(st, p.String())
		if p.Warning {
			warns++
		} else {
			errs++
		}
	}
	if errs > 0 {
		return fmt.Errorf("the spec has %s", fmtCount(errs, "error", "errors"))
	}
	x.spec = s
	x.model = gen.NewModel(s)
	examples := 0
	for _, m := range s.Methods {
		examples += len(m.Examples)
	}
	x.summary(st, "%s, %s, %s, %s", fmtCount(len(s.Entities), "entity", "entities"), fmtCount(s.RPCCount(), "RPC", "RPCs"),
		fmtCount(examples, "example", "examples"), fmtCount(warns, "warning", "warnings"))
	return nil
}

func (x *executor) stageScaffold(_ context.Context, st *StageState) error {
	files, err := gen.Generate(x.spec)
	if err != nil {
		return err
	}
	if err := gen.WriteFiles(x.r.ServiceDir(), files); err != nil {
		return err
	}
	x.man = gen.NewManifest(x.src, files)
	if err := x.man.Save(x.r.ServiceDir()); err != nil {
		return err
	}
	lines := 0
	for _, f := range files {
		n := bytes.Count(f.Data, []byte("\n"))
		lines += n
		x.log(st, fmt.Sprintf("%-48s %5d lines  (%s)", f.Path, n, f.Template))
	}
	x.summary(st, "%d files, %d lines from templates", len(files), lines)
	return nil
}

// bufFindings parses buf's --error-format=json output.
func bufFindings(out, tool string) []metrics.Finding {
	var findings []metrics.Finding
	for _, line := range strings.Split(out, "\n") {
		var f struct {
			Path      string `json:"path"`
			StartLine int    `json:"start_line"`
			Type      string `json:"type"`
			Message   string `json:"message"`
		}
		if json.Unmarshal([]byte(line), &f) == nil && f.Message != "" {
			findings = append(findings, metrics.Finding{Tool: tool, File: "proto/" + f.Path, Line: f.StartLine, Code: f.Type, Message: f.Message})
		}
	}
	return findings
}

func (x *executor) stageProtoLint(ctx context.Context, st *StageState) error {
	res, err := x.command(ctx, st, cmdOpts{quiet: true, timeout: 2 * time.Minute}, "buf", "lint", "--error-format=json")
	if err != nil {
		return err
	}
	findings := bufFindings(res.stdout, "buf lint")
	for _, f := range findings {
		x.log(st, fmt.Sprintf("%s:%d: %s (%s)", f.File, f.Line, f.Message, f.Code))
	}
	x.rep.Findings.BufLint = findings
	if res.code != 0 {
		if len(findings) == 0 {
			return fmt.Errorf("buf lint failed: %s", firstLine(res.stderr))
		}
		return fmt.Errorf("%s against buf STANDARD rules", fmtCount(len(findings), "finding", "findings"))
	}
	x.summary(st, "buf STANDARD rules, no findings")
	return nil
}

// stageBreaking compares the API with the last passing run of the same
// service. A breaking change within one major version fails the line;
// changing the package version (v1 to v2) is how to make one on purpose.
func (x *executor) stageBreaking(ctx context.Context, st *StageState) error {
	c := &metrics.Contract{Package: x.spec.Package}
	x.rep.Contract = c
	base := x.m.baseline(x.spec.Name, x.r.CreatedAt)
	if base == nil {
		c.Note = "first passing run of this service"
		return skip("no earlier passing run of %s to compare with", x.spec.Name)
	}
	if bp := basePackage(base); bp != x.spec.Package {
		c.Note = fmt.Sprintf("package changed from %s to %s", bp, x.spec.Package)
		return skip("the package changed from %s to %s: a new major version may break compatibility", bp, x.spec.Package)
	}
	c.Baseline = base.ID
	res, err := x.command(ctx, st, cmdOpts{quiet: true, timeout: 2 * time.Minute}, "buf", "breaking", "--against", base.ServiceDir(), "--error-format=json")
	if err != nil {
		return err
	}
	c.Changes = bufFindings(res.stdout, "buf breaking")
	for _, f := range c.Changes {
		x.log(st, fmt.Sprintf("%s:%d: %s (%s)", f.File, f.Line, f.Message, f.Code))
	}
	switch {
	case len(c.Changes) > 0:
		return fmt.Errorf("%s against run %s; clients of %s would break. To change the API incompatibly, bump the package version",
			fmtCount(len(c.Changes), "breaking change", "breaking changes"), base.ID, x.spec.Package)
	case res.code != 0:
		return fmt.Errorf("buf breaking failed: %s", firstLine(res.stderr))
	}
	x.summary(st, "wire- and source-compatible with run %s", base.ID)
	return nil
}

func (x *executor) stageCodegen(ctx context.Context, st *StageState) error {
	res, err := x.command(ctx, st, cmdOpts{timeout: 2 * time.Minute}, "buf", "generate")
	if err != nil {
		return err
	}
	if res.code != 0 {
		return fmt.Errorf("buf generate failed: %s", firstLine(res.stderr))
	}
	var n, lines int
	root := x.r.ServiceDir()
	dir, err := os.OpenRoot(root)
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	fsys := dir.FS()
	err = fs.WalkDir(fsys, "gen", func(rel string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		b, err := fs.ReadFile(fsys, rel)
		if err != nil {
			return err
		}
		x.man.Set(rel, gen.OriginBuf, "", b)
		n++
		lines += bytes.Count(b, []byte("\n"))
		x.log(st, fmt.Sprintf("%-48s %5d lines", rel, bytes.Count(b, []byte("\n"))))
		return nil
	})
	if err != nil {
		return err
	}
	if err := x.man.Save(root); err != nil {
		return err
	}
	x.summary(st, "%d files, %d lines from protoc-gen-go and protoc-gen-go-grpc", n, lines)
	return nil
}

// methodBrief describes the custom methods for the LLM, with the visible
// acceptance examples. Holdout examples are left out on purpose.
func (x *executor) methodBrief() string {
	var b strings.Builder
	field := func(fs []spec.Field) string {
		var parts []string
		for _, f := range fs {
			t := f.Type
			if f.Repeated {
				t = "repeated " + t
			}
			parts = append(parts, f.Name+" ("+t+")")
		}
		if len(parts) == 0 {
			return "none"
		}
		return strings.Join(parts, ", ")
	}
	for _, m := range x.spec.Methods {
		fmt.Fprintf(&b, "### %s\n\nRequest fields: %s\nResponse fields: %s\n\n%s\n", m.Name, field(m.Request), field(m.Response), strings.TrimSpace(m.Doc))
		hidden := 0
		var shown []string
		for _, ex := range x.model.Examples {
			if ex.Method != m.Name {
				continue
			}
			if ex.Holdout {
				hidden++
				continue
			}
			line := "- " + ex.Name + ":"
			for _, g := range ex.Given {
				line += " given " + g.Entity + " " + g.JSON + ";"
			}
			line += " request " + ex.Request + ";"
			if ex.Code != "codes.OK" {
				line += " error " + strings.TrimPrefix(ex.Code, "codes.")
			}
			for i, e := range ex.Expect {
				if i > 0 {
					line += ","
				}
				line += " expect " + e.Path + " = " + e.Want
			}
			shown = append(shown, line)
		}
		if len(shown) > 0 {
			fmt.Fprintf(&b, "\nExamples, run as tests you cannot change (${Entity.i.field} refers to a seeded record):\n%s\n", strings.Join(shown, "\n"))
		}
		if hidden > 0 {
			fmt.Fprintf(&b, "%s for this method %s hidden from you.\n", fmtCount(hidden, "more example", "more examples"), map[bool]string{true: "is", false: "are"}[hidden == 1])
		}
		b.WriteString("\n")
	}
	return b.String()
}

func (x *executor) llmContext() (llm.Context, error) {
	root := x.r.ServiceDir()
	read := func(rel string) (string, error) {
		b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		return string(b), err
	}
	in := llm.Context{Files: map[string]string{}, Owned: map[string]string{}, MethodBrief: x.methodBrief()}
	for _, f := range x.man.Files {
		if f.Origin != gen.OriginTemplate && f.Origin != gen.OriginLLM {
			continue
		}
		var err error
		switch {
		case strings.HasSuffix(f.Path, ".proto"):
			in.Proto, err = read(f.Path)
		case slices.Contains(gen.LLMOwnedFiles, f.Path):
			in.Owned[f.Path], err = read(f.Path)
		case f.Path == "internal/service/acceptance_test.go":
			// The judge stays hidden: the brief carries the visible examples.
		case strings.HasPrefix(f.Path, "internal/service/") || f.Path == "internal/store/store.go":
			in.Files[f.Path], err = read(f.Path)
		}
		if err != nil {
			return in, err
		}
	}
	return in, nil
}

// record accounts for one LLM call.
func (x *executor) record(st *StageState, purpose, model string, res llm.Result) {
	f := &x.rep.Factory
	f.LLMCalls++
	f.CostUSD += res.Usage.CostUSD
	f.TokensIn += res.Usage.PromptTokens
	f.TokensOut += res.Usage.CompletionTokens
	f.TokensCached += res.Usage.CachedTokens
	f.Prompts = append(f.Prompts, metrics.PromptRecord{Purpose: purpose, SHA256: res.PromptSHA, Model: model})
	if len(res.Rejected) > 0 {
		f.RejectedWrites = append(f.RejectedWrites, res.Rejected...)
		x.log(st, "rejected writes outside the files the model owns: "+strings.Join(res.Rejected, ", "))
	}
}

func (x *executor) llmModel() string {
	if x.winner != "" {
		return x.winner
	}
	if x.r.Options.Model != "" {
		return x.r.Options.Model
	}
	if x.m.llm != nil {
		return x.m.llm.Model
	}
	return ""
}

// models returns the models that compete to implement the service: the
// run's own choice, else the factory's configured candidates.
func (x *executor) models() []string {
	if len(x.r.Options.Models) > 0 {
		return x.r.Options.Models
	}
	if x.r.Options.Model != "" {
		return []string{x.r.Options.Model}
	}
	return x.m.llm.Candidates()
}

func (x *executor) reviewer() string {
	if x.r.Options.Reviewer != "" {
		return x.r.Options.Reviewer
	}
	return x.m.llm.Reviewer
}

func (x *executor) writeOwned(st *StageState, files map[string]string) error {
	for p, content := range files {
		if err := os.WriteFile(filepath.Join(x.r.ServiceDir(), filepath.FromSlash(p)), []byte(content), 0o644); err != nil { //nolint:gosec // generated source is meant to be shared
			return err
		}
		x.man.Set(p, gen.OriginLLM, "", []byte(content))
		x.log(st, fmt.Sprintf("wrote %s (%d lines)", p, strings.Count(content, "\n")))
	}
	x.llmWrote = true
	return x.man.Save(x.r.ServiceDir())
}

func (x *executor) stageImplement(ctx context.Context, st *StageState) error {
	n := len(x.spec.Methods)
	if n == 0 {
		return skip("no custom RPCs in the spec")
	}
	if x.r.Options.Base != "" {
		if base := x.loadBase(st); base != nil {
			return x.improve(ctx, st, base)
		}
	}
	if !x.r.Options.LLM {
		return skip("LLM off: %s left as stubs", fmtCount(n, "custom RPC", "custom RPCs"))
	}
	if x.m.llm == nil {
		return warn("no LLM configured (set FLIESSBAND_LLM_API_KEY or OPENAI_API_KEY): %s left as stubs", fmtCount(n, "RPC", "RPCs"))
	}
	in, err := x.llmContext()
	if err != nil {
		return err
	}
	models := x.models()
	if len(models) > 1 {
		return x.tournament(ctx, st, models, in)
	}
	x.winner = models[0]
	x.rep.Factory.LLMModel = x.llmModel()
	x.log(st, fmt.Sprintf("asking %s to implement %s", x.llmModel(), fmtCount(n, "RPC", "RPCs")))
	res, err := x.m.llm.Implement(ctx, x.llmModel(), in)
	x.record(st, "implement", x.llmModel(), res)
	if err != nil {
		return warn("LLM call failed, stubs kept: %v", err)
	}
	if err := x.writeOwned(st, res.Files); err != nil {
		return err
	}
	x.summary(st, "%s written by %s, %d tokens in (%d cached), %d out%s", fmtCount(n, "RPC", "RPCs"), x.llmModel(),
		res.Usage.PromptTokens, res.Usage.CachedTokens, res.Usage.CompletionTokens, usd(res.Usage.CostUSD))
	return nil
}

func (x *executor) repair(ctx context.Context, failed string, attempt int) bool {
	st := x.r.stage("implement")
	start := time.Now()
	x.setStage(st, func(s *StageState) { s.Status, s.Attempt = Running, attempt })
	x.log(st, fmt.Sprintf("repair %d/%d: the %s station failed on LLM-owned code, sending the output back", attempt, x.r.Options.MaxRepairs, failed))
	in, err := x.llmContext()
	if err == nil {
		var res llm.Result
		res, err = x.m.llm.Repair(ctx, x.llmModel(), in, failed, x.failOut)
		x.record(st, "repair", x.llmModel(), res)
		if err == nil {
			err = x.writeOwned(st, res.Files)
		}
	}
	elapsed := float64(time.Since(start).Microseconds()) / 1000
	x.rep.Factory.StageMs["implement"] += elapsed
	f := x.rep.Factory
	x.setStage(st, func(s *StageState) {
		s.DurationMs += elapsed
		s.Status = Passed
		s.Summary = fmt.Sprintf("written by %s, %s, %d tokens in, %d out", x.llmModel(),
			fmtCount(attempt, "repair", "repairs"), f.TokensIn, f.TokensOut)
		if err != nil {
			s.Status, s.Summary = Warning, "repair failed: "+err.Error()
		}
	})
	return err == nil
}

func (x *executor) ownedSources() map[string][]byte {
	files := map[string][]byte{}
	for _, p := range gen.LLMOwnedFiles {
		if x.man.Origin(p) != gen.OriginLLM {
			continue
		}
		if b, err := os.ReadFile(filepath.Join(x.r.ServiceDir(), filepath.FromSlash(p))); err == nil {
			files[p] = b
		}
	}
	return files
}

// stagePolicy checks LLM-written files before anything compiles or runs them.
func (x *executor) stagePolicy(_ context.Context, st *StageState) error {
	files := x.ownedSources()
	if len(files) == 0 {
		x.rep.Findings.Policy = nil
		return skip("no LLM-written files to check")
	}
	vs := policy.Check(policy.Config{Module: x.spec.Module, PBPackage: x.model.GoPkgPath}, files)
	var findings []metrics.Finding
	var out strings.Builder
	for _, v := range vs {
		findings = append(findings, metrics.Finding{Tool: "policy", File: v.File, Line: v.Line, Code: v.Rule, Message: v.Message})
		x.log(st, v.String())
		out.WriteString(v.String() + "\n")
	}
	x.rep.Findings.Policy = findings
	if len(vs) > 0 {
		x.fail("policy", out.String())
		return fmt.Errorf("%s in LLM-written files", fmtCount(len(vs), "policy violation", "policy violations"))
	}
	x.summary(st, "%s follow the policy: allowed imports only, no init, panic or directives, tests assert", fmtCount(len(files), "LLM-written file", "LLM-written files"))
	return nil
}

func (x *executor) stageDeps(ctx context.Context, st *StageState) error {
	res, err := x.command(ctx, st, cmdOpts{network: true}, "go", "mod", "tidy")
	if err != nil {
		return err
	}
	if res.code != 0 {
		return fmt.Errorf("go mod tidy failed: %s", firstLine(res.stderr))
	}
	d, i, gv := metrics.ParseGoMod(filepath.Join(x.r.ServiceDir(), "go.mod"))
	x.rep.Build.DirectDeps, x.rep.Build.IndirectDeps, x.rep.Build.GoVersion = d, i, gv
	x.summary(st, "%d direct, %d indirect modules (go %s); later stations run offline", d, i, gv)
	return nil
}

func (x *executor) binary() string { return filepath.Join(x.r.Dir(), "bin", "server") }

func (x *executor) stageBuild(ctx context.Context, st *StageState) error {
	res, err := x.command(ctx, st, cmdOpts{}, "go", "build", "./...")
	if err != nil {
		return err
	}
	if res.code != 0 {
		x.fail("build", res.stderr)
		return fmt.Errorf("compile errors: %s", firstLine(res.stderr))
	}
	res, err = x.command(ctx, st, cmdOpts{}, "go", "build", "-trimpath", "-buildvcs=false", "-o", x.binary(), "./cmd/server")
	if err != nil {
		return err
	}
	if res.code != 0 {
		x.fail("build", res.stderr)
		return fmt.Errorf("building the server binary failed: %s", firstLine(res.stderr))
	}
	fi, err := os.Stat(x.binary())
	if err != nil {
		return err
	}
	x.rep.Build.BinaryBytes = fi.Size()
	x.summary(st, "compiles; server binary %.1f MB", float64(fi.Size())/(1<<20))
	return nil
}

var reVet = regexp.MustCompile(`^(?:\./)?([^\s:]+\.go):(\d+):(?:\d+:)? (.+)$`)

func (x *executor) stageVet(ctx context.Context, st *StageState) error {
	res, err := x.command(ctx, st, cmdOpts{}, "go", "vet", "./...")
	if err != nil {
		return err
	}
	var findings []metrics.Finding
	for _, line := range strings.Split(res.stderr, "\n") {
		if m := reVet.FindStringSubmatch(strings.TrimSpace(line)); m != nil {
			ln, _ := strconv.Atoi(m[2])
			findings = append(findings, metrics.Finding{Tool: "go vet", File: m[1], Line: ln, Message: m[3]})
		}
	}
	x.rep.Findings.Vet = findings
	if res.code != 0 {
		x.fail("vet", res.stderr)
		return fmt.Errorf("%s", fmtCount(max(1, len(findings)), "vet finding", "vet findings"))
	}
	x.summary(st, "no findings")
	return nil
}

func (x *executor) rel(path string) string {
	if r, err := filepath.Rel(x.r.ServiceDir(), path); err == nil && !strings.HasPrefix(r, "..") {
		return filepath.ToSlash(r)
	}
	return filepath.ToSlash(path)
}

// stageLint runs golangci-lint (staticcheck, gosec, errcheck and more, per
// the generated .golangci.yml) and Go 1.26+'s go fix modernizers.
func (x *executor) stageLint(ctx context.Context, st *StageState) error {
	var lint []metrics.Finding
	switch {
	case x.has("golangci-lint"):
		res, err := x.command(ctx, st, cmdOpts{quiet: true}, "golangci-lint", "run", "--output.json.path=stdout", "--show-stats=false", "./...")
		if err != nil {
			return err
		}
		var out struct {
			Issues []struct {
				FromLinter string `json:"FromLinter"`
				Text       string `json:"Text"`
				Pos        struct {
					Filename string `json:"Filename"`
					Line     int    `json:"Line"`
				} `json:"Pos"`
			} `json:"Issues"`
		}
		if err := json.Unmarshal([]byte(firstLine(res.stdout)), &out); err != nil {
			return warn("golangci-lint failed: %s", firstLine(res.stderr+res.stdout))
		}
		for _, is := range out.Issues {
			lint = append(lint, metrics.Finding{Tool: "golangci-lint", File: x.rel(is.Pos.Filename), Line: is.Pos.Line, Code: is.FromLinter, Message: is.Text})
		}
	case x.has("staticcheck"):
		res, err := x.command(ctx, st, cmdOpts{quiet: true}, "staticcheck", "-f", "json", "./...")
		if err != nil {
			return err
		}
		for _, line := range strings.Split(res.stdout, "\n") {
			var f struct {
				Code     string `json:"code"`
				Message  string `json:"message"`
				Location struct {
					File string `json:"file"`
					Line int    `json:"line"`
				} `json:"location"`
			}
			if json.Unmarshal([]byte(line), &f) == nil && f.Message != "" {
				lint = append(lint, metrics.Finding{Tool: "staticcheck", File: x.rel(f.Location.File), Line: f.Location.Line, Code: f.Code, Message: f.Message})
			}
		}
	default:
		return skip("neither golangci-lint nor staticcheck is installed")
	}
	modernize, err := x.modernize(ctx, st)
	if err != nil {
		return err
	}
	for _, f := range append(slices.Clone(lint), modernize...) {
		x.log(st, fmt.Sprintf("%s:%d: %s (%s)", f.File, f.Line, f.Message, f.Code))
	}
	x.rep.Findings.Lint, x.rep.Findings.Modernize = lint, modernize
	if len(lint)+len(modernize) > 0 {
		return warn("%s, %s", fmtCount(len(lint), "lint finding", "lint findings"), fmtCount(len(modernize), "modernization", "modernizations"))
	}
	x.summary(st, "no lint findings; nothing for the go fix modernizers to rewrite")
	return nil
}

var (
	reDiffFile = regexp.MustCompile(`^--- (\S+)`)
	reDiffHunk = regexp.MustCompile(`^@@ -(\d+)`)
)

// modernize runs go fix -diff and turns each hunk into a finding.
func (x *executor) modernize(ctx context.Context, st *StageState) ([]metrics.Finding, error) {
	res, err := x.command(ctx, st, cmdOpts{quiet: true}, "go", "fix", "-diff", "./...")
	if err != nil {
		return nil, err
	}
	var out []metrics.Finding
	file := ""
	var removed string
	for _, line := range strings.Split(res.stdout, "\n") {
		switch {
		case reDiffFile.MatchString(line):
			file = x.rel(reDiffFile.FindStringSubmatch(line)[1])
		case reDiffHunk.MatchString(line) && file != "" && !strings.HasPrefix(file, "gen/"):
			n, _ := strconv.Atoi(reDiffHunk.FindStringSubmatch(line)[1])
			out = append(out, metrics.Finding{Tool: "go fix", File: file, Line: n + 3, Code: "modernize", Message: "can be written in modern Go"})
			removed = ""
		case strings.HasPrefix(line, "-") && len(out) > 0 && removed == "":
			removed = strings.TrimSpace(line[1:])
			out[len(out)-1].Message = "modernize: " + removed
		}
	}
	return out, nil
}

func (x *executor) has(tool string) bool {
	_, ok := x.m.lookPath(tool)
	return ok
}

var reStub = regexp.MustCompile(`fliessband:implement (\w+)`)

// stubs returns the custom methods that still carry a fliessband:implement marker.
func (x *executor) stubs() []string {
	b, err := os.ReadFile(filepath.Join(x.r.ServiceDir(), "internal/service/custom.go"))
	if err != nil {
		return nil
	}
	var out []string
	for _, m := range reStub.FindAllSubmatch(b, -1) {
		out = append(out, string(m[1]))
	}
	return out
}

// skipArgs leaves out acceptance examples of methods that are still stubs.
func (x *executor) skipArgs() []string {
	if len(x.pending) == 0 || len(x.model.Examples) == 0 {
		return nil
	}
	return []string{"-skip", "TestAcceptance/^(" + strings.Join(x.pending, "|") + ")$"}
}

func isAcceptance(test string) bool { return strings.HasPrefix(test, "TestAcceptance") }

func (x *executor) coverPath() string { return filepath.Join(x.r.Dir(), "coverage.out") }

func (x *executor) integrationCoverPath() string {
	return filepath.Join(x.r.Dir(), "coverage-integration.out")
}

// mergedCoverPath joins the unit and integration coverage profiles, so code
// that only the database tests exercise counts as covered.
func (x *executor) mergedCoverPath() string {
	unit, err := os.ReadFile(x.coverPath())
	if err != nil {
		return x.coverPath()
	}
	integ, err := os.ReadFile(x.integrationCoverPath())
	if err != nil {
		return x.coverPath()
	}
	if i := bytes.IndexByte(integ, '\n'); i >= 0 && bytes.HasPrefix(integ, []byte("mode:")) {
		integ = integ[i+1:]
	}
	merged := filepath.Join(x.r.Dir(), "coverage-merged.out")
	if err := os.WriteFile(merged, append(append(unit, '\n'), integ...), 0o600); err != nil {
		return x.coverPath()
	}
	return merged
}

// runTests runs the whole suite once with the race detector and coverage.
// Unit tests are judged here; acceptance results are read from the same
// output by the acceptance station.
func (x *executor) runTests(ctx context.Context, st *StageState) (*metrics.Tests, cmdResult, error) {
	x.pending = x.stubs()
	args := []string{"test", "-race", "-count=1", "-json", "-covermode=atomic", "-coverprofile=" + x.coverPath()}
	args = append(append(args, x.skipArgs()...), "./...")
	res, err := x.command(ctx, st, cmdOpts{quiet: true, timeout: 10 * time.Minute}, "go", args...)
	if err != nil {
		return nil, res, err
	}
	x.testJSON = res.stdout
	tests := metrics.ParseTestJSON(strings.NewReader(res.stdout), x.spec.Module, func(t string) bool { return !isAcceptance(t) })
	return tests, res, nil
}

func (x *executor) stageTest(ctx context.Context, st *StageState) error {
	tests, res, err := x.runTests(ctx, st)
	if err != nil {
		return err
	}
	dec := json.NewDecoder(strings.NewReader(res.stdout))
	for {
		var ev struct{ Action, Test, Output string }
		if dec.Decode(&ev) != nil {
			break
		}
		out := strings.TrimRight(ev.Output, "\n")
		if ev.Action == "output" && ev.Test == "" && out != "" && !strings.HasPrefix(out, "PASS") && !strings.HasPrefix(out, "ok ") {
			x.log(st, out)
		}
	}
	x.rep.Tests = tests
	for _, f := range tests.Failures {
		x.log(st, "--- FAIL "+f.Package+" "+f.Name)
		for _, l := range strings.Split(strings.TrimSpace(f.Output), "\n") {
			x.log(st, "    "+l)
		}
	}
	acceptanceFailed := x.acceptanceFailed()
	switch {
	case tests.Failed > 0:
		var b strings.Builder
		b.WriteString(res.stderr)
		for _, f := range tests.Failures {
			fmt.Fprintf(&b, "--- FAIL: %s (%s)\n%s\n", f.Name, f.Package, f.Output)
		}
		x.fail("test", b.String())
		return fmt.Errorf("%d of %d tests failed", tests.Failed, tests.Total)
	case res.code != 0 && !acceptanceFailed:
		out := packageOutput(res.stdout) + res.stderr
		x.fail("test", out)
		return fmt.Errorf("the test run failed outside a test: %s", lastLine(out))
	}
	cov := ""
	if c, err := metrics.ParseCoverProfile(x.coverPath(), x.spec.Module, x.isGenerated, nil); err == nil {
		cov = fmt.Sprintf(", %.1f%% of service statements covered", c.ServicePercent())
	}
	x.summary(st, "%d tests passed (%d top-level), no leaked goroutines%s", tests.Passed, tests.TopLevel, cov)
	return nil
}

// packageOutput collects output printed outside tests, such as a TestMain
// leak report or build errors.
func packageOutput(jsonOut string) string {
	var b strings.Builder
	dec := json.NewDecoder(strings.NewReader(jsonOut))
	for {
		var ev struct{ Action, Test, Output string }
		if dec.Decode(&ev) != nil {
			break
		}
		if ev.Action == "output" && ev.Test == "" {
			b.WriteString(ev.Output)
		}
	}
	return b.String()
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" && l != "FAIL" && !strings.HasPrefix(l, "FAIL\t") {
			return l
		}
	}
	return firstLine(s)
}

// testResults maps test names to their final action and output.
func testResults(jsonOut string) map[string]metrics.TestCase {
	out := map[string]metrics.TestCase{}
	output := map[string]*strings.Builder{}
	dec := json.NewDecoder(strings.NewReader(jsonOut))
	for {
		var ev struct{ Action, Test, Output string }
		if dec.Decode(&ev) != nil {
			break
		}
		if ev.Test == "" {
			continue
		}
		switch ev.Action {
		case "output":
			if output[ev.Test] == nil {
				output[ev.Test] = &strings.Builder{}
			}
			_, _ = output[ev.Test].WriteString(ev.Output)
		case "pass", "fail", "skip":
			tc := metrics.TestCase{Name: ev.Test, Package: ev.Action}
			if b := output[ev.Test]; b != nil {
				tc.Output = b.String()
			}
			out[ev.Test] = tc
		}
	}
	return out
}

func (x *executor) acceptanceFailed() bool {
	for name, tc := range testResults(x.testJSON) {
		if isAcceptance(name) && tc.Package == "fail" {
			return true
		}
	}
	return false
}

// failureMessage extracts the assertion lines from a failed test's output.
func failureMessage(out string) string {
	var lines []string
	for _, l := range strings.Split(out, "\n") {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "=== ") || strings.HasPrefix(l, "--- ") {
			continue
		}
		lines = append(lines, l)
	}
	return strings.Join(lines, "\n")
}

// stageAcceptance judges the spec's examples from the test run's output.
func (x *executor) stageAcceptance(_ context.Context, st *StageState) error {
	if len(x.model.Examples) == 0 {
		return skip("the spec has no examples")
	}
	results := testResults(x.testJSON)
	acc := &metrics.Acceptance{}
	var visible strings.Builder
	hiddenFailed := 0
	for _, ex := range x.model.Examples {
		e := metrics.Example{Method: ex.Method, Name: ex.Name, Holdout: ex.Holdout}
		if ex.Holdout {
			acc.Holdout++
		}
		tc, ran := results[gen.ExampleTestName(ex.Method, ex.Name)]
		switch {
		case slices.Contains(x.pending, ex.Method):
			e.Status = "pending"
			acc.Pending++
		case ran && tc.Package == "pass":
			e.Status = "passed"
			acc.Passed++
		default:
			e.Status = "failed"
			acc.Failed++
			e.Message = failureMessage(tc.Output)
			if !ran {
				e.Message = "did not run"
			}
			if ex.Holdout {
				hiddenFailed++
			} else {
				fmt.Fprintf(&visible, "--- FAIL example %q of %s:\n%s\n\n", ex.Name, ex.Method, e.Message)
			}
		}
		x.log(st, fmt.Sprintf("%-7s %s / %s%s", e.Status, ex.Method, ex.Name, map[bool]string{true: "  (holdout)"}[ex.Holdout]))
		if e.Message != "" {
			x.log(st, "        "+strings.ReplaceAll(e.Message, "\n", "\n        "))
		}
		acc.Examples = append(acc.Examples, e)
	}
	x.rep.Acceptance = acc
	if acc.Failed > 0 {
		// Holdout details stay out of the repair prompt, or they would stop being hidden.
		if hiddenFailed > 0 && visible.Len() > 0 {
			fmt.Fprintf(&visible, "%s also fail; their details are withheld.\n", fmtCount(hiddenFailed, "hidden example", "hidden examples"))
		}
		x.fail("acceptance", visible.String())
		return fmt.Errorf("%d of %d examples fail (%d hidden)", acc.Failed, len(acc.Examples), hiddenFailed)
	}
	pending := ""
	if acc.Pending > 0 {
		pending = fmt.Sprintf("; %d pending until %s implemented", acc.Pending, strings.Join(x.pending, ", "))
	}
	x.summary(st, "%d of %d examples pass, %d of them holdout%s", acc.Passed, len(acc.Examples), acc.Holdout, pending)
	return nil
}

func (x *executor) isGenerated(rel string) bool {
	return strings.HasPrefix(rel, "gen/") || (x.man != nil && x.man.Origin(rel) == gen.OriginBuf)
}

func (x *executor) stageVuln(ctx context.Context, st *StageState) error {
	if !x.has("govulncheck") {
		return skip("govulncheck is not installed")
	}
	res, err := x.command(ctx, st, cmdOpts{quiet: true, network: true}, "govulncheck", "-format", "json", "./...")
	if err != nil {
		return warn("govulncheck: %v", err)
	}
	summaries := map[string]string{}
	fixed := map[string]string{}
	reached := map[string]metrics.Finding{}
	dec := json.NewDecoder(strings.NewReader(res.stdout))
	for {
		var msg struct {
			OSV *struct {
				ID      string `json:"id"`
				Summary string `json:"summary"`
			} `json:"osv"`
			Finding *struct {
				OSV          string `json:"osv"`
				FixedVersion string `json:"fixed_version"`
				Trace        []struct {
					Module   string `json:"module"`
					Package  string `json:"package"`
					Function string `json:"function"`
				} `json:"trace"`
			} `json:"finding"`
		}
		if dec.Decode(&msg) != nil {
			break
		}
		if msg.OSV != nil {
			summaries[msg.OSV.ID] = msg.OSV.Summary
		}
		if f := msg.Finding; f != nil && len(f.Trace) > 0 && f.Trace[0].Function != "" {
			reached[f.OSV] = metrics.Finding{Tool: "govulncheck", Code: f.OSV, File: f.Trace[0].Module, Message: f.Trace[0].Package + "." + f.Trace[0].Function}
			fixed[f.OSV] = f.FixedVersion
		}
	}
	var findings []metrics.Finding
	for id, f := range reached {
		if s := summaries[id]; s != "" {
			f.Message = s + " (via " + f.Message + ")"
		}
		if v := fixed[id]; v != "" {
			f.Message += "; fixed in " + v
		}
		findings = append(findings, f)
		x.log(st, id+": "+f.Message)
	}
	slices.SortFunc(findings, func(a, b metrics.Finding) int { return strings.Compare(a.Code, b.Code) })
	x.rep.Findings.Vulns = findings
	if len(findings) > 0 {
		std := 0
		for _, f := range findings {
			if f.File == "stdlib" {
				std++
			}
		}
		where := ""
		switch {
		case std == len(findings):
			where = "; all in the Go standard library: upgrade the Go toolchain to the latest patch release"
		case std > 0:
			where = fmt.Sprintf("; %d in the Go standard library", std)
		}
		return warn("%s reachable from the code%s", fmtCount(len(findings), "known vulnerability", "known vulnerabilities"), where)
	}
	if res.code != 0 && len(summaries) == 0 && res.stdout == "" {
		return warn("govulncheck could not run: %s", firstLine(res.stderr))
	}
	x.summary(st, "no known vulnerabilities reached")
	return nil
}

func (x *executor) stageMeasure(_ context.Context, st *StageState, gates bool) error {
	if x.spec == nil || x.man == nil {
		return skip("nothing was generated")
	}
	root := x.r.ServiceDir()
	size, funcs, err := metrics.AnalyzeSource(root, x.man.Origin)
	if err != nil {
		return err
	}
	rep := x.rep
	rep.GeneratedAt = time.Now().UTC()
	rep.Size, rep.Functions = size, funcs
	if _, err := os.Stat(x.coverPath()); err == nil {
		if c, err := metrics.ParseCoverProfile(x.mergedCoverPath(), x.spec.Module, x.isGenerated, rep.Functions); err == nil {
			rep.Coverage = c
		}
	}
	rep.Complexity = metrics.Summarize(rep.Functions)

	// API surface, counted from the spec.
	api := metrics.API{Services: 1, Custom: len(x.spec.Methods)}
	crudFields := map[string]int{"create": 2, "get": 2, "list": 4, "update": 3, "delete": 1}
	for _, e := range x.spec.Entities {
		api.CRUD += len(e.Ops)
		api.Messages++
		api.Fields += len(e.Fields) + 3
		for _, op := range e.Ops {
			api.Fields += crudFields[op]
		}
	}
	for _, m := range x.spec.Methods {
		api.Fields += len(m.Request) + len(m.Response)
	}
	api.RPCs = api.CRUD + api.Custom
	api.Messages += 2 * api.RPCs
	rep.API = api
	rep.Security = security.Audit(root, x.spec, rep, func(id string) bool {
		s := x.r.stage(id)
		return s != nil && (s.Status == Passed || s.Status == Warning || s.Status == Failed)
	})

	comp := metrics.Completeness{Total: api.RPCs, Implemented: api.RPCs, Stubbed: x.stubs()}
	comp.Implemented -= len(comp.Stubbed)
	rep.Completeness = comp
	rep.Score = metrics.ComputeScore(rep, gates)

	c := rep.Complexity
	x.log(st, fmt.Sprintf("functions: %d production, %d test", c.Funcs, c.TestFuncs))
	x.log(st, fmt.Sprintf("cyclomatic: mean %.2f, p90 %.0f, max %d; cognitive: mean %.2f, max %d", c.MeanCyclomatic, c.P90Cyclomatic, c.MaxCyclomatic, c.MeanCognitive, c.MaxCognitive))
	x.log(st, fmt.Sprintf("maintainability index: mean %.1f, min %.1f", c.MeanMI, c.MinMI))
	if rep.Coverage != nil {
		x.log(st, fmt.Sprintf("coverage: %.1f%% of service statements, %.1f%% overall", rep.Coverage.ServicePercent(), rep.Coverage.Percent))
	}
	for _, p := range rep.Score.Parts {
		pts := fmt.Sprintf("%5.1f / %2.0f", p.Points, p.Weight)
		if !p.Measured {
			pts = "   –  / " + fmt.Sprintf("%2.0f", p.Weight)
		}
		x.log(st, fmt.Sprintf("score %-16s %s  %s", p.Name, pts, p.Detail))
	}
	capped := ""
	if rep.Score.Capped {
		capped = ", capped because a gate failed"
	}
	x.summary(st, "quality score %.0f (%s)%s", rep.Score.Total, rep.Score.Grade, capped)
	return nil
}

func fmtCount(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", one)
	}
	return fmt.Sprintf("%d %s", n, many)
}

// usd formats a cost for a summary, or nothing when it is unknown.
func usd(v float64) string {
	if v <= 0 {
		return ""
	}
	return fmt.Sprintf(", $%.4f", v)
}

// stageReview has a second model review the LLM-written code against the
// method docs. It reports; it never stops the line.
func (x *executor) stageReview(ctx context.Context, st *StageState) error {
	if !x.llmWrote || x.m.llm == nil || !x.r.Options.LLM {
		return skip("no LLM-written code to review")
	}
	model := x.reviewer()
	if model == "" {
		return skip("no reviewer model configured (FLIESSBAND_LLM_REVIEWER)")
	}
	in, err := x.llmContext()
	if err != nil {
		return err
	}
	x.log(st, "asking "+model+" to review custom.go and custom_test.go")
	found, res, err := x.m.llm.Review(ctx, model, in)
	x.record(st, "review", model, res)
	rev := &metrics.Review{Model: model}
	x.rep.Factory.Review = rev
	if err != nil {
		rev.Note = err.Error()
		return warn("review failed: %v", err)
	}
	bugs := 0
	for _, f := range found {
		rev.Findings = append(rev.Findings, metrics.ReviewFinding(f))
		x.log(st, fmt.Sprintf("%s %s:%d %s", f.Severity, f.File, f.Line, f.Message))
		if f.Severity == "bug" {
			bugs++
		}
	}
	if model == x.llmModel() {
		rev.Note = "the reviewer is the model that wrote the code; a different model catches more"
	}
	if bugs > 0 {
		return warn("%s reports %s and %d other findings", model, fmtCount(bugs, "likely bug", "likely bugs"), len(found)-bugs)
	}
	x.summary(st, "%s found no bugs (%d other findings)%s", model, len(found), usd(res.Usage.CostUSD))
	return nil
}
