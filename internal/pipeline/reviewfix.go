package pipeline

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/tillknuesting/fliessband/internal/gen"
	"github.com/tillknuesting/fliessband/internal/llm"
	"github.com/tillknuesting/fliessband/internal/metrics"
	"github.com/tillknuesting/fliessband/internal/policy"
)

// The review fix closes the loop after the review station: the reviewer's
// "bug" findings in LLM-written files go back to the winning model as one
// more repair. The answer is checked against every gate LLM code can break
// (policy, build, vet, test, acceptance with the holdouts) and kept only if
// all of them pass and no test or example that passed before fails.
// Otherwise the previous files, test results and coverage are restored
// exactly. One attempt, never more: nothing about the failed check goes
// back to the model, so holdout results stay hidden.

// fixSnapshot is what a review fix may change, so a rejected fix leaves no trace.
type fixSnapshot struct {
	files    map[string][]byte // owned path -> previous content; nil when the file did not exist
	testJSON string
	pending  []string
	coverage []byte // nil when there was no profile
}

func (x *executor) takeFixSnapshot(paths []string) fixSnapshot {
	s := fixSnapshot{files: map[string][]byte{}, testJSON: x.testJSON, pending: slices.Clone(x.pending)}
	for _, p := range paths {
		b, err := os.ReadFile(filepath.Join(x.r.ServiceDir(), filepath.FromSlash(p)))
		if err == nil {
			s.files[p] = b
		} else {
			s.files[p] = nil
		}
	}
	if b, err := os.ReadFile(x.coverPath()); err == nil {
		s.coverage = b
	}
	return s
}

func (x *executor) restoreFixSnapshot(s fixSnapshot) error {
	var errs []error
	for p, b := range s.files {
		path := filepath.Join(x.r.ServiceDir(), filepath.FromSlash(p))
		if b == nil {
			if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
				errs = append(errs, err)
			}
			continue
		}
		if err := os.WriteFile(path, b, 0o644); err != nil { //nolint:gosec // generated source is meant to be shared
			errs = append(errs, err)
		}
	}
	x.testJSON, x.pending = s.testJSON, s.pending
	if s.coverage != nil {
		errs = append(errs, os.WriteFile(x.coverPath(), s.coverage, 0o600))
	} else {
		_ = os.Remove(x.coverPath())
	}
	return errors.Join(errs...)
}

// fixReview runs the guarded review fix and records its outcome in rev.
func (x *executor) fixReview(ctx context.Context, st *StageState, rev *metrics.Review, found []llm.ReviewFinding) {
	var bugs []llm.ReviewFinding
	for _, f := range found {
		if f.Severity == "bug" && slices.Contains(gen.LLMOwnedFiles, f.File) && x.man.Origin(f.File) == gen.OriginLLM {
			bugs = append(bugs, f)
		}
	}
	if len(bugs) == 0 || x.m.llm == nil || !x.llmWrote || ctx.Err() != nil {
		return
	}
	model := x.llmModel()
	rev.FixAttempted, rev.FixModel = true, model
	note := func(format string, a ...any) {
		rev.FixNote = fmt.Sprintf(format, a...)
		x.log(st, "review fix: "+rev.FixNote)
	}
	in, err := x.llmContext()
	if err != nil {
		note("could not read the files: %v", err)
		return
	}
	x.log(st, fmt.Sprintf("sending %s back to %s as one repair", fmtCount(len(bugs), "bug finding", "bug findings"), model))
	res, err := x.m.llm.FixReview(ctx, model, in, bugs)
	x.record(st, "review-fix", model, res)
	if err != nil {
		note("the model returned no files: %v", err)
		return
	}
	paths := make([]string, 0, len(res.Files))
	changed := false
	for p, content := range res.Files {
		paths = append(paths, p)
		changed = changed || in.Owned[p] != content
	}
	slices.Sort(paths)
	if !changed {
		note("the model changed nothing; it judged no finding a real bug")
		return
	}

	snap := x.takeFixSnapshot(paths)
	before := testResults(x.testJSON)
	tmpBin := x.binary() + ".reviewfix"
	defer func() { _ = os.Remove(tmpBin) }()
	for _, p := range paths {
		if err := os.WriteFile(filepath.Join(x.r.ServiceDir(), filepath.FromSlash(p)), []byte(res.Files[p]), 0o644); err != nil { //nolint:gosec // generated source is meant to be shared
			x.revertFix(st, snap, note, "could not write %s: %v", p, err)
			return
		}
	}
	tests, why, err := x.checkFix(ctx, st, res.Files, tmpBin, snap, before)
	if err != nil {
		if ctx.Err() != nil {
			x.revertFix(st, snap, note, "canceled while checking the fix; previous files restored")
		} else {
			x.revertFix(st, snap, note, "could not check the fix (%v); previous files restored", err)
		}
		return
	}
	if why != "" {
		x.revertFix(st, snap, note, "rejected: %s; previous files restored", why)
		return
	}

	// Kept: the manifest, binary, test results and lint findings follow the new code.
	for _, p := range paths {
		x.man.Set(p, gen.OriginLLM, "", []byte(res.Files[p]))
		x.log(st, fmt.Sprintf("review fix: wrote %s (%d lines)", p, strings.Count(res.Files[p], "\n")))
	}
	if err := x.man.Save(x.r.ServiceDir()); err != nil {
		x.revertFix(st, snap, note, "could not save the manifest: %v", err)
		return
	}
	if err := os.Rename(tmpBin, x.binary()); err == nil {
		if fi, err := os.Stat(x.binary()); err == nil {
			x.rep.Build.BinaryBytes = fi.Size()
		}
	}
	x.rep.Tests = tests
	if acc := x.rep.Acceptance; acc != nil {
		acc.Passed, acc.Failed, acc.Pending = 0, 0, 0
		for i := range acc.Examples {
			e := &acc.Examples[i]
			if slices.Contains(x.pending, e.Method) {
				e.Status, e.Message = "pending", ""
				acc.Pending++
			} else {
				e.Status, e.Message = "passed", ""
				acc.Passed++
			}
		}
	}
	if x.has("golangci-lint") || x.has("staticcheck") {
		_ = x.stageLint(ctx, st) // refresh the lint findings for the new code; lint is not a gate
	}
	rev.FixKept, rev.FixedFindings = true, len(bugs)
	note("kept: policy, build, vet, %d tests and every example (holdouts included) pass, and nothing that passed before fails", tests.Passed)
}

func (x *executor) revertFix(st *StageState, snap fixSnapshot, note func(string, ...any), format string, a ...any) {
	if err := x.restoreFixSnapshot(snap); err != nil {
		x.log(st, "review fix: restoring the previous files: "+err.Error())
	}
	note(format, a...)
}

// checkFix runs the gates LLM code can break against the fixed files. It
// returns why the fix must be rejected, or "" when every check passes; err
// is for failures of the factory itself, such as cancellation.
func (x *executor) checkFix(ctx context.Context, st *StageState, files map[string]string, tmpBin string, snap fixSnapshot, before map[string]metrics.TestCase) (*metrics.Tests, string, error) {
	srcs := x.ownedSources()
	for p, c := range files {
		srcs[p] = []byte(c)
	}
	if vs := policy.Check(policy.Config{Module: x.spec.Module, PBPackage: x.model.GoPkgPath}, srcs); len(vs) > 0 {
		return nil, "policy: " + vs[0].String(), nil
	}
	res, err := x.command(ctx, st, cmdOpts{quiet: true}, "go", "build", "./...")
	if err != nil {
		return nil, "", err
	}
	if res.code != 0 {
		return nil, "build: " + firstLine(res.stderr), nil
	}
	res, err = x.command(ctx, st, cmdOpts{quiet: true}, "go", "build", "-trimpath", "-buildvcs=false", "-o", tmpBin, "./cmd/server")
	if err != nil {
		return nil, "", err
	}
	if res.code != 0 {
		return nil, "build: " + firstLine(res.stderr), nil
	}
	res, err = x.command(ctx, st, cmdOpts{quiet: true}, "go", "vet", "./...")
	if err != nil {
		return nil, "", err
	}
	if res.code != 0 {
		return nil, "vet: " + firstLine(res.stderr), nil
	}
	tests, res, err := x.runTests(ctx, st)
	if err != nil {
		return nil, "", err
	}
	if len(x.pending) > len(snap.pending) {
		return nil, "it stubs a method again: " + strings.Join(x.pending, ", "), nil
	}
	if tests.Failed > 0 {
		var names []string
		for _, f := range tests.Failures {
			names = append(names, f.Name)
		}
		return nil, fmt.Sprintf("%d of %d tests fail (%s)", tests.Failed, tests.Total, strings.Join(names, ", ")), nil
	}
	if res.code != 0 && !x.acceptanceFailed() {
		return nil, "the test run failed outside a test: " + lastLine(packageOutput(res.stdout)+res.stderr), nil
	}
	after := testResults(x.testJSON)
	failed, hidden := 0, 0
	for _, ex := range x.model.Examples {
		if slices.Contains(x.pending, ex.Method) {
			continue
		}
		if tc, ok := after[gen.ExampleTestName(ex.Method, ex.Name)]; !ok || tc.Package != "pass" {
			failed++
			if ex.Holdout {
				hidden++
			}
		}
	}
	if failed > 0 {
		return nil, fmt.Sprintf("%s fail (%d hidden)", fmtCount(failed, "example", "examples"), hidden), nil
	}
	var lost []string
	for name, tc := range before {
		if tc.Package != "pass" || isAcceptance(name) {
			continue
		}
		if now, ok := after[name]; !ok || now.Package != "pass" {
			lost = append(lost, name)
		}
	}
	if len(lost) > 0 {
		slices.Sort(lost)
		return nil, fmt.Sprintf("%s that passed before no longer %s: %s", fmtCount(len(lost), "test", "tests"),
			map[bool]string{true: "passes", false: "pass"}[len(lost) == 1], strings.Join(lost, ", ")), nil
	}
	return tests, "", nil
}
