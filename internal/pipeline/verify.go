package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tillknuesting/fliessband/internal/gen"
	"github.com/tillknuesting/fliessband/internal/metrics"
	"github.com/tillknuesting/fliessband/internal/mutate"
	"github.com/tillknuesting/fliessband/internal/policy"
	"github.com/tillknuesting/fliessband/internal/supply"
)

const (
	defaultMaxMutants = 150
	mutationBudget    = 4 * time.Minute
	postgresImage     = "postgres:17-alpine"
)

// mutationFiles lists the production files to mutate: the service, store
// and server code the factory or the LLM wrote. The PostgreSQL store is
// left out because unit tests do not run against a database.
func (x *executor) mutationFiles() []string {
	var out []string
	for _, f := range x.man.Files {
		p := f.Path
		if (f.Origin != gen.OriginTemplate && f.Origin != gen.OriginLLM) || !strings.HasSuffix(p, ".go") ||
			strings.HasSuffix(p, "_test.go") || !strings.HasPrefix(p, "internal/") ||
			strings.HasPrefix(p, "internal/telemetry/") || p == "internal/store/postgres.go" {
			continue
		}
		out = append(out, p)
	}
	return out
}

func (x *executor) mutationRunner(covered func(string, int) bool, timeout time.Duration, st *StageState) *mutate.Runner {
	r := &mutate.Runner{
		Root:    x.r.ServiceDir(),
		Env:     x.m.childEnv(false),
		Args:    x.skipArgs(),
		Workers: max(2, runtime.NumCPU()/2),
		Timeout: timeout,
		Budget:  mutationBudget,
		Covered: covered,
		Prepare: isolate,
		Progress: func(done, total int, m mutate.Mutant) {
			if done%10 == 0 || done == total {
				x.log(st, fmt.Sprintf("%d/%d mutants run", done, total))
			}
		},
	}
	x.sandboxMutations(r)
	return r
}

// stageMutation measures how well the tests detect injected faults. When
// the LLM wrote code, surviving mutants in it are sent back to the model
// with a request for tests that kill them (mutation-guided test hardening).
func (x *executor) stageMutation(ctx context.Context, st *StageState) error {
	start := time.Now()
	var all []mutate.Mutant
	for _, rel := range x.mutationFiles() {
		src, err := os.ReadFile(filepath.Join(x.r.ServiceDir(), filepath.FromSlash(rel)))
		if err != nil {
			return err
		}
		ms, err := mutate.Find(rel, src)
		if err != nil {
			return err
		}
		all = append(all, ms...)
	}
	limit := x.r.Options.MaxMutants
	if limit == 0 {
		limit = defaultMaxMutants
	}
	ms, sampled := mutate.Sample(all, limit)
	for i := range ms {
		ms[i].ID = i + 1
	}
	if len(ms) == 0 {
		return skip("no code to mutate")
	}
	covered, err := mutate.Coverage(x.coverPath(), x.spec.Module)
	if err != nil {
		return fmt.Errorf("read coverage: %w", err)
	}
	pkgs := map[string]bool{}
	for _, m := range ms {
		pkgs["./"+filepath.ToSlash(filepath.Dir(m.File))] = true
	}
	var pkgList []string
	for p := range pkgs {
		pkgList = append(pkgList, p)
	}
	slices.Sort(pkgList)
	runner := x.mutationRunner(covered, 0, st)
	base, err := runner.Baseline(ctx, pkgList)
	if err != nil {
		return warn("%v", err)
	}
	runner.Timeout = min(max(3*base, 10*time.Second), time.Minute)
	x.log(st, fmt.Sprintf("%d mutants in %d files (%d found%s); tests take %.1fs without mutants; %d workers, %s per mutant",
		len(ms), len(x.mutationFiles()), len(all), map[bool]string{true: ", sampled"}[sampled], base.Seconds(), runner.Workers, runner.Timeout))
	cache := x.newMutantCache(ctx, ms, runner.Args, runner.Timeout)
	if cache != nil {
		runner.Cache = cache
	}
	if ms, err = runner.Run(ctx, ms); err != nil {
		return err
	}
	if cache != nil {
		cache.flush()
		if cache.hits > 0 {
			x.log(st, fmt.Sprintf("%d of %d mutants answered from the cache: their package and its dependencies are unchanged since an earlier run", cache.hits, len(ms)))
		}
	}
	runner.Cache = nil // hardening changes the tests, so its reruns are never cached
	x.harden(ctx, st, ms, runner)
	rep := x.mutationReport(ms, sampled, time.Since(start))
	x.rep.Mutation = rep
	for _, s := range rep.Survivors {
		x.log(st, fmt.Sprintf("survived #%d %s:%d %s: %s  →  %s", s.ID, s.File, s.Line, s.Operator, s.Before, s.After))
	}
	if rep.Score == nil {
		return warn("no mutant could be judged")
	}
	msg := fmt.Sprintf("mutation score %.0f%%: %d of %d mutants detected, %d survived, %d not covered",
		*rep.Score*100, rep.Killed+rep.TimedOut, rep.Total-rep.Invalid-rep.Skipped-rep.Equivalent, rep.Survived, rep.NotCovered)
	if h := x.rep.Factory.Hardening; h != nil {
		msg += fmt.Sprintf("; LLM tests killed %d of %d survivors", h.Killed, h.Survivors)
	}
	if *rep.Score < metrics.MutationTarget {
		return warn("%s", msg)
	}
	x.summary(st, "%s", msg)
	return nil
}

func (x *executor) mutationReport(ms []mutate.Mutant, sampled bool, d time.Duration) *metrics.Mutation {
	rep := &metrics.Mutation{Total: len(ms), Sampled: sampled, DurationMs: float64(d.Milliseconds())}
	ops := map[string]*metrics.OperatorStat{}
	files := map[string]*metrics.FileMutation{}
	for _, m := range ms {
		if ops[m.Operator] == nil {
			ops[m.Operator] = &metrics.OperatorStat{Operator: m.Operator}
		}
		if files[m.File] == nil {
			files[m.File] = &metrics.FileMutation{File: m.File, Origin: x.man.Origin(m.File)}
		}
		switch m.Status {
		case mutate.Killed:
			rep.Killed++
		case mutate.TimedOut:
			rep.TimedOut++
		case mutate.Survived:
			rep.Survived++
		case mutate.NotCovered:
			rep.NotCovered++
		case mutate.Invalid:
			rep.Invalid++
		case mutate.Equivalent:
			rep.Equivalent++
		case mutate.Skipped:
			rep.Skipped++
		}
		if m.Status == mutate.Invalid || m.Status == mutate.Skipped || m.Status == mutate.Equivalent {
			continue
		}
		ops[m.Operator].Total++
		files[m.File].Total++
		if m.Detected() {
			ops[m.Operator].Killed++
			files[m.File].Killed++
		}
		if m.Status == mutate.Survived {
			rep.Survivors = append(rep.Survivors, metrics.Mutant{ID: m.ID, File: m.File, Line: m.Line, Operator: m.Operator,
				Original: m.Original, Mutated: m.Mutated, Before: m.Before, After: m.After, Origin: x.man.Origin(m.File), Status: string(m.Status)})
		}
	}
	detected := float64(rep.Killed + rep.TimedOut)
	if judged := detected + float64(rep.Survived+rep.NotCovered); judged > 0 {
		s := detected / judged
		rep.Score = &s
	}
	if covered := detected + float64(rep.Survived); covered > 0 {
		s := detected / covered
		rep.Strength = &s
	}
	for _, o := range ops {
		rep.ByOperator = append(rep.ByOperator, *o)
	}
	slices.SortFunc(rep.ByOperator, func(a, b metrics.OperatorStat) int { return b.Total - a.Total })
	for _, f := range files {
		if f.Total > 0 {
			s := float64(f.Killed) / float64(f.Total)
			f.Score = &s
		}
		rep.ByFile = append(rep.ByFile, *f)
	}
	slices.SortFunc(rep.ByFile, func(a, b metrics.FileMutation) int { return strings.Compare(a.File, b.File) })
	return rep
}

// harden asks the LLM for tests that kill the survivors in its own code,
// keeps them only if they pass on the unmutated code and follow the policy,
// and then re-runs those mutants.
func (x *executor) harden(ctx context.Context, st *StageState, ms []mutate.Mutant, runner *mutate.Runner) {
	if !x.llmWrote || x.m.llm == nil || !x.r.Options.LLM {
		return
	}
	const prodFile, testFile = "internal/service/custom.go", "internal/service/custom_test.go"
	var idx []int
	var desc strings.Builder
	for i, m := range ms {
		if m.Status == mutate.Survived && m.File == prodFile {
			idx = append(idx, i)
			fmt.Fprintf(&desc, "#%d line %d (%s): `%s`  →  `%s`\n", m.ID, m.Line, m.Operator, m.Before, m.After)
		}
	}
	if len(idx) == 0 {
		return
	}
	h := &metrics.Hardening{Survivors: len(idx)}
	x.rep.Factory.Hardening = h
	model := x.hardenModel()
	x.log(st, fmt.Sprintf("asking %s for tests that kill %s in the code", model, fmtCount(len(idx), "surviving mutant", "surviving mutants")))
	in, err := x.llmContext()
	if err != nil {
		h.Note = err.Error()
		return
	}
	res, err := x.m.llm.Harden(ctx, model, in, testFile, desc.String())
	x.record(st, "harden", model, res)
	for _, id := range res.Equivalent {
		for _, i := range idx {
			if ms[i].ID == id {
				ms[i].Status = mutate.Equivalent
				h.Equivalent++
			}
		}
	}
	if err != nil {
		h.Note = "the model returned no tests: " + err.Error()
		x.log(st, h.Note)
		return
	}
	path := filepath.Join(x.r.ServiceDir(), filepath.FromSlash(testFile))
	old, _ := os.ReadFile(path)
	restore := func(note string) {
		_ = os.WriteFile(path, old, 0o644) //nolint:gosec // generated source is meant to be shared
		h.Note = note
		x.log(st, note)
	}
	if err := os.WriteFile(path, []byte(res.Files[testFile]), 0o644); err != nil { //nolint:gosec // generated source is meant to be shared
		h.Note = err.Error()
		return
	}
	if vs := policy.Check(policy.Config{Module: x.spec.Module, PBPackage: x.model.GoPkgPath}, map[string][]byte{testFile: []byte(res.Files[testFile])}); len(vs) > 0 {
		restore(fmt.Sprintf("the new tests break the policy (%s); kept the old tests", vs[0]))
		return
	}
	tests, out, err := x.runTests(ctx, st)
	if err != nil || tests.Failed > 0 || (out.code != 0 && !x.acceptanceFailed()) {
		restore("the new tests fail on the unmutated code; kept the old tests")
		_, _, _ = x.runTests(ctx, st)
		return
	}
	x.rep.Tests = tests
	x.man.Set(testFile, gen.OriginLLM, "", []byte(res.Files[testFile]))
	_ = x.man.Save(x.r.ServiceDir())
	h.Kept = true
	var again []mutate.Mutant
	for _, i := range idx {
		if ms[i].Status == mutate.Survived {
			again = append(again, ms[i])
		}
	}
	runner.Covered = nil
	again, _ = runner.Run(ctx, again)
	for _, m := range again {
		for _, i := range idx {
			if ms[i].ID == m.ID {
				ms[i].Status = m.Status
				if m.Detected() {
					h.Killed++
				}
			}
		}
	}
	x.log(st, fmt.Sprintf("hardening: the new tests kill %d of %d survivors (%d judged equivalent)", h.Killed, h.Survivors, h.Equivalent))
}

var (
	reFuzzFunc  = regexp.MustCompile(`(?m)^func (Fuzz\w+)\(f \*testing\.F\)`)
	reFuzzStats = regexp.MustCompile(`execs: (\d+) .*new interesting: (\d+)`)
	reFuzzInput = regexp.MustCompile(`Failing input written to (\S+)`)
)

// stageFuzz runs each fuzz target for a short time to look for inputs that
// break the properties the generated fuzz tests state.
func (x *executor) stageFuzz(ctx context.Context, st *StageState) error {
	start := time.Now()
	dir := filepath.Join(x.r.ServiceDir(), "internal/service")
	matches, _ := filepath.Glob(filepath.Join(dir, "*_test.go"))
	var targets []string
	for _, p := range matches {
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		for _, m := range reFuzzFunc.FindAllSubmatch(b, -1) {
			targets = append(targets, string(m[1]))
		}
	}
	if len(targets) == 0 {
		return skip("no fuzz targets")
	}
	fuzztime := x.r.Options.FuzzTime
	if _, err := time.ParseDuration(fuzztime); err != nil {
		fuzztime = "3s"
	}
	fz := &metrics.Fuzz{}
	x.rep.Fuzz = fz
	var findings []metrics.Finding
	// Targets fuzz at the same time, a few at once: each is a separate go
	// test process with its own corpus directory, and one target alone
	// leaves most cores idle.
	type outcome struct {
		res cmdResult
		err error
	}
	outs := make([]outcome, len(targets))
	sem := make(chan struct{}, min(len(targets), max(1, runtime.NumCPU()/4)))
	var wg sync.WaitGroup
	for i, t := range targets {
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			res, err := x.command(ctx, st, cmdOpts{quiet: true, timeout: 3 * time.Minute}, "go", "test", "-run", "^$",
				"-fuzz", "^"+t+"$", "-fuzztime", fuzztime, "-parallel", "4", "./internal/service")
			outs[i] = outcome{res, err}
		})
	}
	wg.Wait()
	for i, t := range targets {
		res, err := outs[i].res, outs[i].err
		if err != nil {
			return err
		}
		out := res.stdout + res.stderr
		ft := metrics.FuzzTarget{Name: t}
		if all := reFuzzStats.FindAllStringSubmatch(out, -1); len(all) > 0 {
			last := all[len(all)-1]
			ft.Execs, _ = strconv.ParseInt(last[1], 10, 64)
			ft.Interesting, _ = strconv.Atoi(last[2])
		}
		if res.code != 0 {
			ft.Failed = true
			msg := lastLine(out)
			if m := reFuzzInput.FindStringSubmatch(out); m != nil {
				ft.Input = "internal/service/" + m[1]
			}
			findings = append(findings, metrics.Finding{Tool: "go fuzz", File: ft.Input, Code: t, Message: msg})
			x.log(st, fmt.Sprintf("✗ %s: %s", t, msg))
			fz.Failures++
		} else {
			x.log(st, fmt.Sprintf("%s: %d inputs, %d new interesting", t, ft.Execs, ft.Interesting))
		}
		fz.Execs += ft.Execs
		fz.Targets = append(fz.Targets, ft)
	}
	fz.DurationMs = float64(time.Since(start).Milliseconds())
	x.rep.Findings.Fuzz = findings
	if fz.Failures > 0 {
		return warn("%s found an input that breaks a property; it is saved as a regression test", fmtCount(fz.Failures, "fuzz target", "fuzz targets"))
	}
	x.summary(st, "%s, %d inputs in %s each, no failures", fmtCount(len(targets), "target", "targets"), fz.Execs, fuzztime)
	return nil
}

// stageIntegration runs the store contract tests against a real
// PostgreSQL in Docker and keeps it up for the smoke test.
func (x *executor) stageIntegration(ctx context.Context, st *StageState) error {
	switch {
	case !x.model.Postgres:
		return skip("the service has no PostgreSQL store")
	case !x.r.Options.Integration:
		return skip("turned off for this run")
	}
	d := x.m.docker()
	if !d.ok {
		return skip("PostgreSQL tests did not run: %s", d.reason)
	}
	x.log(st, fmt.Sprintf("starting %s in Docker %s", postgresImage, d.version))
	t0 := time.Now()
	db, err := x.m.startPostgres(ctx, x.r.ID, func(s string) { x.log(st, s) })
	if err != nil {
		return err
	}
	x.db = db
	in := &metrics.Integration{Database: postgresImage, StartMs: float64(time.Since(t0).Milliseconds())}
	x.rep.Integration = in
	x.log(st, fmt.Sprintf("PostgreSQL ready after %.1fs", in.StartMs/1000))
	// On the host even with a sandbox: the tests must reach the database, and
	// the store package they build does not link the LLM's code.
	res, err := x.command(ctx, st, cmdOpts{quiet: true, host: true, env: []string{"TEST_DATABASE_URL=" + db.url}},
		"go", "test", "-race", "-count=1", "-json", "-covermode=atomic", "-coverprofile="+x.integrationCoverPath(),
		"-run", "Postgres", "./internal/store/")
	if err != nil {
		return err
	}
	tests := metrics.ParseTestJSON(strings.NewReader(res.stdout), x.spec.Module)
	in.Tests, in.Failed = tests.Total-tests.Skipped, tests.Failed
	for _, f := range tests.Failures {
		x.log(st, "--- FAIL "+f.Name)
		x.log(st, "    "+strings.ReplaceAll(strings.TrimSpace(f.Output), "\n", "\n    "))
	}
	switch {
	case tests.Failed > 0:
		return fmt.Errorf("%d of %d PostgreSQL store tests failed", tests.Failed, in.Tests)
	case res.code != 0:
		return fmt.Errorf("the PostgreSQL tests did not run: %s", lastLine(packageOutput(res.stdout)+res.stderr))
	case in.Tests == 0:
		return fmt.Errorf("no PostgreSQL store tests ran")
	}
	x.summary(st, "%d store contract tests pass on %s; migrations are idempotent", in.Tests, postgresImage)
	return nil
}

// stageSBOM writes the CycloneDX SBOM and the SLSA provenance statement.
func (x *executor) stageSBOM(_ context.Context, st *StageState) error {
	b, err := supply.ReadBuild(x.binary())
	if err != nil {
		return err
	}
	now := time.Now()
	sbom, err := supply.SBOM(b, Version, now)
	if err != nil {
		return err
	}
	sbomPath := filepath.Join(x.r.Dir(), "sbom.cdx.json")
	if err := os.WriteFile(sbomPath, sbom, 0o600); err != nil {
		return err
	}
	subjects := []supply.Subject{{Name: "bin/server", Digest: map[string]string{"sha256": b.SHA256}}}
	for _, f := range x.man.Files {
		subjects = append(subjects, supply.Subject{Name: f.Path, Digest: map[string]string{"sha256": f.SHA256}})
	}
	var deps []supply.Dependency
	for _, t := range x.m.cachedTools() {
		if t.Found {
			deps = append(deps, supply.Dependency{Name: t.Name, Annotations: map[string]string{"version": t.Version}})
		}
	}
	internal := map[string]any{
		"generator": map[string]any{"name": "fliessband", "version": Version, "templates": map[string]string{"sha256": gen.TemplatesDigest()}},
	}
	if f := x.rep.Factory; f.LLMCalls > 0 {
		internal["llm"] = map[string]any{"model": f.LLMModel, "prompts": f.Prompts}
	}
	opts, _ := json.Marshal(x.r.Options)
	var optsMap map[string]any
	_ = json.Unmarshal(opts, &optsMap)
	prov := supply.Provenance{
		RunID: x.r.ID, Started: x.started, Finished: now, Subjects: subjects,
		External:   map[string]any{"spec": map[string]any{"sha256": supply.SHA256(x.src)}, "options": optsMap},
		Internal:   internal,
		Deps:       deps,
		Byproducts: []supply.Subject{{Name: "sbom.cdx.json", Digest: map[string]string{"sha256": supply.SHA256(sbom)}}},
	}
	stmt, err := prov.Statement()
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(x.r.Dir(), "provenance.intoto.json"), stmt, 0o600); err != nil {
		return err
	}
	sup := &metrics.Supply{Components: len(b.Deps) + 1, GoVersion: b.GoVersion, BinarySHA256: b.SHA256, Settings: b.Settings,
		SBOMFile: "sbom.cdx.json", Provenance: "provenance.intoto.json"}
	for _, d := range b.Deps {
		sup.Modules = append(sup.Modules, metrics.Module{Path: d.Path, Version: d.Version, Sum: d.Sum})
	}
	x.rep.Supply = sup
	x.log(st, fmt.Sprintf("binary sha256 %s, built with %s", b.SHA256, b.GoVersion))
	x.summary(st, "SBOM lists %d components; provenance covers the binary and %d source files (unsigned)", sup.Components, len(x.man.Files))
	return nil
}

// hardenModel picks who writes tests against surviving mutants. Writing a
// test needs no authorship of the code, so after a tournament it is the
// quickest model whose implementation compiled and ran, not necessarily the
// winner: a slow winner would otherwise hold the line up a second time.
func (x *executor) hardenModel() string {
	t := x.rep.Factory.Tournament
	if t == nil {
		return x.llmModel()
	}
	best, ms := x.llmModel(), -1.0
	for _, c := range t.Candidates {
		if c.Status == "ran" && (ms < 0 || c.Ms < ms) {
			best, ms = c.Model, c.Ms
		}
	}
	return best
}
