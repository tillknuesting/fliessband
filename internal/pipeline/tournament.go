package pipeline

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tillknuesting/fliessband/internal/gen"
	"github.com/tillknuesting/fliessband/internal/llm"
	"github.com/tillknuesting/fliessband/internal/metrics"
	"github.com/tillknuesting/fliessband/internal/policy"
)

// The implement tournament: several models each write the custom RPCs and
// their tests. Every implementation then runs against its own tests, the
// visible examples and the tests every other model wrote (cross-testing,
// after CodeT's dual execution agreement). The implementation that the most
// independent evidence agrees with wins. Holdout examples run too, but their
// results never take part in the choice, so they stay an unbiased judge of
// the winner.

const (
	implFile = "internal/service/custom.go"
	testFile = "internal/service/custom_test.go"
)

type candidate struct {
	model string
	files map[string]string
	usage llm.Usage
	sha   string
	err   error
	ms    float64
	tests []string // top-level tests declared in its test file
	m     metrics.Candidate
	// crossRuns holds, per test author, which of that author's tests
	// passed on this candidate's code; mergeTests reuses it.
	crossRuns map[string]map[string]bool
}

// Straggler limits: after the first answer, the tournament waits until
// stragglerFactor times that long (at least stragglerMin) for the rest.
var (
	stragglerMin    = 60 * time.Second
	stragglerFactor = 3
)

func straggler(first time.Duration) time.Duration {
	return max(stragglerMin, time.Duration(stragglerFactor)*first)
}

func (x *executor) tournament(ctx context.Context, st *StageState, models []string, in llm.Context) error {
	n := len(x.spec.Methods)
	// The module cache must hold every dependency before candidates
	// compile; candidates themselves never reach the network.
	if res, err := x.command(ctx, st, cmdOpts{network: true}, "go", "mod", "tidy"); err != nil || res.code != 0 {
		if err == nil {
			err = fmt.Errorf("go mod tidy failed: %s", firstLine(res.stderr))
		}
		return err
	}
	x.log(st, fmt.Sprintf("tournament: %d models implement %s in parallel: %s", len(models), fmtCount(n, "RPC", "RPCs"), strings.Join(models, ", ")))
	cands := make([]*candidate, len(models))
	var wg sync.WaitGroup
	var mu sync.Mutex
	// The line does not wait for a straggler: once the first model has
	// answered, the others get until the deadline (see straggler) and are
	// cut off after it.
	callCtx, cutOff := context.WithCancel(ctx)
	defer cutOff()
	begin := time.Now()
	answered := make(chan time.Duration, len(models))
	var cut atomic.Bool
	var deadline time.Duration
	go func() {
		select {
		case first := <-answered:
			deadline = straggler(first)
			t := time.NewTimer(deadline - time.Since(begin))
			defer t.Stop()
			select {
			case <-t.C:
				cut.Store(true)
				cutOff()
			case <-callCtx.Done():
			}
		case <-callCtx.Done():
		}
	}()
	pending := sync.WaitGroup{}
	pending.Add(len(models))
	go func() { pending.Wait(); cutOff() }() // every call returned: stop the timer
	for i, model := range models {
		cands[i] = &candidate{model: model}
		wg.Go(func() {
			defer pending.Done()
			c := cands[i]
			start := time.Now()
			res, err := x.m.llm.Implement(callCtx, model, in)
			c.ms = float64(time.Since(start).Milliseconds())
			if err != nil && cut.Load() && ctx.Err() == nil {
				err = fmt.Errorf("no answer within %.0f s, when the others had answered; cut off so the line does not wait", deadline.Seconds())
			}
			c.files, c.usage, c.sha, c.err = res.Files, res.Usage, res.PromptSHA, err
			mu.Lock()
			x.record(st, "implement", model, res)
			mu.Unlock()
			if err != nil {
				x.log(st, fmt.Sprintf("%s: %v", model, err))
				return
			}
			answered <- time.Since(begin)
			x.log(st, fmt.Sprintf("%s: answered in %.1f s, %d tokens in, %d out", model, c.ms/1000, res.Usage.PromptTokens, res.Usage.CompletionTokens))
		})
	}
	wg.Wait()
	if ctx.Err() != nil {
		return errCanceled
	}

	dir := filepath.Join(x.r.Dir(), "_tournament") // "_": the go command ignores it
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	// Holdout examples run with every candidate but are never counted here.
	var visible []string
	for _, ex := range x.model.Examples {
		if !ex.Holdout {
			visible = append(visible, gen.ExampleTestName(ex.Method, ex.Name))
		}
	}

	// Policy and own tests, one candidate at a time per CPU-heavy go test.
	for i, c := range cands {
		c.m = metrics.Candidate{Model: c.model, TokensIn: c.usage.PromptTokens, TokensOut: c.usage.CompletionTokens, CostUSD: c.usage.CostUSD, Ms: c.ms}
		switch {
		case c.err != nil:
			c.m.Status, c.m.Note = "no answer", c.err.Error()
			continue
		case c.files[implFile] == "" || c.files[testFile] == "":
			c.m.Status, c.m.Note = "incomplete", "did not return both custom.go and custom_test.go"
			continue
		}
		srcs := map[string][]byte{implFile: []byte(c.files[implFile]), testFile: []byte(c.files[testFile])}
		if vs := policy.Check(policy.Config{Module: x.spec.Module, PBPackage: x.model.GoPkgPath}, srcs); len(vs) > 0 {
			c.m.Status, c.m.Note = "policy", vs[0].String()
			continue
		}
		c.tests = testNames(c.files[testFile])
		results, out, err := x.candidateTest(ctx, st, dir, fmt.Sprintf("c%d", i), c.files[implFile], c.files[testFile], "")
		if err != nil {
			return err
		}
		if results == nil {
			c.m.Status, c.m.Note = "compile", compileError(out)
			continue
		}
		c.m.Status = "ran"
		c.m.Own = count(results, c.tests)
		c.m.Visible = count(results, visible)
	}

	// Cross-testing: each runnable implementation against every other
	// candidate's tests.
	runnable := slices.DeleteFunc(slices.Clone(cands), func(c *candidate) bool { return c.m.Status != "ran" })
	for i, impl := range runnable {
		impl.m.Cross = map[string]metrics.PassCount{}
		for j, tests := range runnable {
			if i == j || len(tests.tests) == 0 {
				continue
			}
			pattern := "^(" + strings.Join(tests.tests, "|") + ")$"
			results, _, err := x.candidateTest(ctx, st, dir, fmt.Sprintf("x%d-%d", i, j), impl.files[implFile], tests.files[testFile], pattern)
			if err != nil {
				return err
			}
			if results == nil {
				// The tests lean on helpers of their own implementation;
				// they say nothing about this one.
				x.log(st, fmt.Sprintf("%s's tests do not compile against %s's code; not counted", tests.model, impl.model))
				continue
			}
			if impl.crossRuns == nil {
				impl.crossRuns = map[string]map[string]bool{}
			}
			impl.crossRuns[tests.model] = results
			pc := count(results, tests.tests)
			for _, name := range tests.tests {
				if !results[name] {
					impl.m.FailedTests = append(impl.m.FailedTests, tests.model+": "+name)
				}
			}
			impl.m.Cross[tests.model] = pc
			impl.m.Foreign.Passed += pc.Passed
			impl.m.Foreign.Total += pc.Total
		}
	}

	for _, c := range cands {
		c.m.Score = selectionScore(c.m)
	}
	ranked := slices.Clone(cands)
	slices.SortStableFunc(ranked, func(a, b *candidate) int {
		if c := cmp.Compare(b.m.Score, a.m.Score); c != 0 {
			return c
		}
		return cmp.Compare(a.m.CostUSD+float64(a.m.TokensOut)/1e9, b.m.CostUSD+float64(b.m.TokensOut)/1e9)
	})
	win := ranked[0]
	t := &metrics.Tournament{Winner: win.model}
	for _, c := range cands {
		c.m.Chosen = c == win
		t.Candidates = append(t.Candidates, c.m)
		x.log(st, fmt.Sprintf("%-40s %-10s own %s, examples %s, others' tests %s, selection %.2f%s", c.model, c.m.Status,
			c.m.Own, c.m.Visible, c.m.Foreign, c.m.Score, map[bool]string{true: "  <- chosen"}[c.m.Chosen]))
	}
	x.rep.Factory.Tournament = t
	if len(win.files) == 0 {
		return warn("no model returned usable files, stubs kept")
	}
	x.winner = win.model
	x.rep.Factory.LLMModel = win.model
	if err := x.mergeTests(ctx, st, dir, win, cands, t); err != nil { // see merge.go
		return err
	}
	if err := x.writeOwned(st, win.files); err != nil {
		return err
	}
	merged := ""
	if t.MergedTests > 0 {
		merged = fmt.Sprintf("; %s merged from losing models", fmtCount(t.MergedTests, "test", "tests"))
	}
	x.summary(st, "%s of %d models chosen: %s; examples %s, others' tests %s%s", win.model, len(models), win.m.Status, win.m.Visible, win.m.Foreign, merged)
	return nil
}

// selectionScore ranks a candidate by evidence it could not tune itself
// to: the visible examples it was given, and the tests other models wrote
// without seeing its code. Its own tests count least.
func selectionScore(c metrics.Candidate) float64 {
	switch c.Status {
	case "ran":
	case "compile":
		return -1
	case "policy":
		return -2
	default:
		return -3
	}
	frac := func(p metrics.PassCount, none float64) float64 {
		if p.Total == 0 {
			return none
		}
		return float64(p.Passed) / float64(p.Total)
	}
	return 0.5*frac(c.Visible, 1) + 0.35*frac(c.Foreign, frac(c.Own, 0)) + 0.15*frac(c.Own, 0)
}

// candidateTest runs the service package's tests with a candidate's files
// laid over the generated ones, without touching the service directory.
// It returns nil results when the package does not compile.
func (x *executor) candidateTest(ctx context.Context, st *StageState, dir, name, impl, tests, run string) (map[string]bool, string, error) {
	svc := x.r.ServiceDir()
	if real, err := filepath.EvalSymlinks(svc); err == nil {
		svc = real // the go command matches overlay paths after resolving links
	}
	implPath := filepath.Join(dir, name+"-custom.go")
	testPath := filepath.Join(dir, name+"-custom_test.go")
	if err := os.WriteFile(implPath, []byte(impl), 0o600); err != nil {
		return nil, "", err
	}
	if err := os.WriteFile(testPath, []byte(tests), 0o600); err != nil {
		return nil, "", err
	}
	ov, _ := json.Marshal(map[string]map[string]string{"Replace": {
		filepath.Join(svc, filepath.FromSlash(implFile)): implPath,
		filepath.Join(svc, filepath.FromSlash(testFile)): testPath,
	}})
	ovPath := filepath.Join(dir, name+"-overlay.json")
	if err := os.WriteFile(ovPath, ov, 0o600); err != nil {
		return nil, "", err
	}
	args := []string{"test", "-overlay=" + ovPath, "-count=1", "-json", "-timeout=120s"}
	if run != "" {
		args = append(args, "-run", run)
	}
	res, err := x.command(ctx, st, cmdOpts{quiet: true, timeout: 4 * time.Minute}, "go", append(args, "./internal/service/")...)
	if err != nil {
		return nil, "", err
	}
	results := map[string]bool{}
	for test, tc := range testResults(res.stdout) {
		results[test] = tc.Package == "pass"
	}
	if len(results) == 0 && res.code != 0 {
		return nil, packageOutput(res.stdout) + res.stderr, nil
	}
	return results, "", nil
}

func count(results map[string]bool, tests []string) metrics.PassCount {
	pc := metrics.PassCount{Total: len(tests)}
	for _, t := range tests {
		if results[t] {
			pc.Passed++
		}
	}
	return pc
}

// testNames lists the top-level Test functions a test file declares.
func testNames(src string) []string {
	f, err := parser.ParseFile(token.NewFileSet(), "custom_test.go", src, parser.SkipObjectResolution)
	if err != nil {
		return nil
	}
	var names []string
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if ok && fn.Recv == nil && strings.HasPrefix(fn.Name.Name, "Test") && fn.Name.Name != "TestMain" {
			names = append(names, fn.Name.Name)
		}
	}
	return names
}

// compileError picks the first compiler message out of go test output.
func compileError(out string) string {
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, ".go:") {
			return strings.TrimSpace(l)
		}
	}
	return firstLine(out)
}
