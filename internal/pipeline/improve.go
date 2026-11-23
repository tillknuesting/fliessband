package pipeline

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/tillknuesting/fliessband/internal/gen"
	"github.com/tillknuesting/fliessband/internal/metrics"
	"github.com/tillknuesting/fliessband/internal/spec"
)

// baseVersion is what a new run inherits from the run it improves on.
type baseVersion struct {
	id    string
	files map[string]string
	rep   *metrics.Report
	model string
}

// loadBase reads the LLM-owned files and the report of the base run. It
// returns nil when there is nothing usable to start from: no such run, a
// report missing, stubs only, or custom methods that differ from this spec.
func (x *executor) loadBase(st *StageState) *baseVersion {
	id := x.r.Options.Base
	b := x.m.Get(id)
	if b == nil {
		x.log(st, "base run "+id+" not found; building from scratch")
		return nil
	}
	b.mu.Lock()
	src, rep := b.Spec, b.Report
	b.mu.Unlock()
	if rep == nil {
		x.log(st, "base run "+id+" has no report; building from scratch")
		return nil
	}
	bs, _, err := spec.Parse([]byte(src))
	if err != nil || !sameMethods(bs, x.spec) {
		x.log(st, "the custom methods changed since "+id+"; building from scratch")
		return nil
	}
	v := &baseVersion{id: id, files: map[string]string{}, rep: rep, model: rep.Factory.LLMModel}
	for _, p := range gen.LLMOwnedFiles {
		data, err := os.ReadFile(filepath.Join(b.ServiceDir(), filepath.FromSlash(p)))
		if err != nil {
			x.log(st, "base run "+id+" has no "+p+"; building from scratch")
			return nil
		}
		v.files[p] = string(data)
	}
	if strings.Contains(v.files["internal/service/custom.go"], "fliessband:implement") {
		x.log(st, "base run "+id+" has only stubs; building from scratch")
		return nil
	}
	return v
}

func sameMethods(a, b *spec.Spec) bool {
	names := func(s *spec.Spec) []string {
		var out []string
		for _, m := range s.Methods {
			out = append(out, m.Name)
		}
		slices.Sort(out)
		return out
	}
	return slices.Equal(names(a), names(b))
}

// improve starts from the base run's implementation. With an LLM, the model
// gets that code and what the base run measured as weak; without one, the
// implementation is carried forward unchanged so the rest of the line
// re-measures it.
func (x *executor) improve(ctx context.Context, st *StageState, base *baseVersion) error {
	if err := x.writeOwned(st, base.files); err != nil {
		return err
	}
	x.log(st, fmt.Sprintf("starting from the implementation of %s (score %.0f, %s)", base.id, base.rep.Score.Total, base.rep.Score.Grade))
	if !x.r.Options.LLM || x.m.llm == nil {
		x.summary(st, "carried forward the implementation of %s unchanged", base.id)
		return nil
	}
	in, err := x.llmContext()
	if err != nil {
		return err
	}
	x.winner = x.models()[0]
	if base.model != "" && slices.Contains(x.models(), base.model) {
		x.winner = base.model
	}
	x.rep.Factory.LLMModel = x.llmModel()
	brief := feedback(base.rep)
	x.log(st, fmt.Sprintf("asking %s to improve it on %s", x.llmModel(), fmtCount(strings.Count(brief, "\n- "), "point", "points")))
	res, err := x.m.llm.Improve(ctx, x.llmModel(), in, brief)
	x.record(st, "improve", x.llmModel(), res)
	if err != nil {
		return warn("LLM call failed, kept the implementation of %s: %v", base.id, err)
	}
	if err := x.writeOwned(st, res.Files); err != nil {
		return err
	}
	x.summary(st, "improved on %s by %s, %d tokens in, %d out%s", base.id, x.llmModel(), res.Usage.PromptTokens, res.Usage.CompletionTokens, usd(res.Usage.CostUSD))
	return nil
}

func owned(file string) bool { return slices.Contains(gen.LLMOwnedFiles, file) }

// feedback turns the base run's measurements into a brief for the model. It
// names visible examples only: holdouts are counted, never described.
func feedback(rep *metrics.Report) string {
	var b strings.Builder
	fmt.Fprintf(&b, "The previous version scored %.0f of 100 (%s). Where it lost points:\n", rep.Score.Total, rep.Score.Grade)
	for _, p := range rep.Score.Parts {
		if p.Measured && p.Weight-p.Points >= 0.5 {
			fmt.Fprintf(&b, "- %s: %.1f of %.0f. %s\n", p.Name, p.Points, p.Weight, p.Detail)
		}
	}
	if a := rep.Acceptance; a != nil {
		hidden := 0
		for _, e := range a.Examples {
			switch {
			case e.Status != "failed":
			case e.Holdout:
				hidden++
			default:
				fmt.Fprintf(&b, "- Example %q of %s fails: %s\n", e.Name, e.Method, e.Message)
			}
		}
		if hidden > 0 {
			fmt.Fprintf(&b, "- %s fail. Re-read the method docs and handle every documented case in general.\n", fmtCount(hidden, "hidden example", "hidden examples"))
		}
	}
	if m := rep.Mutation; m != nil {
		n := 0
		for _, s := range m.Survivors {
			if !owned(s.File) || n >= 15 {
				continue
			}
			n++
			fmt.Fprintf(&b, "- Mutant survives at %s:%d: %q became %q and no test noticed.\n", s.File, s.Line, strings.TrimSpace(s.Before), strings.TrimSpace(s.After))
		}
	}
	if r := rep.Factory.Review; r != nil {
		for _, f := range r.Findings {
			if f.Severity != "nit" {
				fmt.Fprintf(&b, "- Reviewer (%s) at %s:%d: %s\n", f.Severity, f.File, f.Line, f.Message)
			}
		}
	}
	for _, fn := range rep.Functions {
		if !owned(fn.File) || fn.Test {
			continue
		}
		if fn.Cyclomatic > 10 {
			fmt.Fprintf(&b, "- %s has cyclomatic complexity %d; split it into smaller functions.\n", fn.Name, fn.Cyclomatic)
		}
		if fn.Coverage != nil && *fn.Coverage < 80 {
			fmt.Fprintf(&b, "- %s is only %.0f%% covered by tests.\n", fn.Name, *fn.Coverage)
		}
	}
	for _, fs := range [][]metrics.Finding{rep.Findings.Vet, rep.Findings.Lint, rep.Findings.Modernize, rep.Findings.Policy} {
		for _, f := range fs {
			if owned(f.File) {
				fmt.Fprintf(&b, "- %s at %s:%d: %s\n", f.Tool, f.File, f.Line, f.Message)
			}
		}
	}
	return b.String()
}
