package bench

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/tillknuesting/fliessband/internal/metrics"
	"github.com/tillknuesting/fliessband/internal/pipeline"
)

func f(v float64) *float64 { return &v }

func row(spec, model string, repeat int, status string, score float64, holdOK, hold int) Row {
	return Row{Mutation: f(0.5), Spec: spec, Model: model, Repeat: repeat, Status: status, Score: f(score), HoldoutPassed: holdOK, HoldoutTotal: hold, CostUSD: 0.01, DurationMs: 1000}
}

func TestFromRun(t *testing.T) {
	r := &pipeline.Run{ID: "x", Status: pipeline.Failed, Stages: []*pipeline.StageState{
		{StageDef: pipeline.StageDef{ID: "build", Name: "Build"}, Status: pipeline.Passed, DurationMs: 400},
		{StageDef: pipeline.StageDef{ID: "smoke", Name: "Smoke"}, Status: pipeline.Failed, DurationMs: 600},
	}, Report: &metrics.Report{
		Score:    metrics.Score{Total: 38.5, Grade: "F"},
		Mutation: &metrics.Mutation{Score: f(0.71)},
		Acceptance: &metrics.Acceptance{Examples: []metrics.Example{
			{Status: "passed"}, {Status: "failed"}, {Status: "pending"},
			{Holdout: true, Status: "passed"}, {Holdout: true, Status: "pending"},
		}},
		Security: &metrics.Security{Passed: 20, Warned: 2, Failed: 1},
		Factory:  metrics.Factory{CostUSD: 0.02},
	}}
	got := FromRun("tasks", TemplateModel, 2, r)
	if got.Status != "failed" || got.FailedAt != "Smoke" || got.DurationMs != 1000 || got.RunID != "x" {
		t.Errorf("status/failed_at/duration: %+v", got)
	}
	if got.Score == nil || *got.Score != 38.5 || got.Grade != "F" || got.Mutation == nil || *got.Mutation != 0.71 {
		t.Errorf("score/mutation: %+v", got)
	}
	if got.VisiblePassed != 1 || got.VisibleTotal != 3 || got.HoldoutPassed != 1 || got.HoldoutTotal != 2 {
		t.Errorf("examples: %+v", got)
	}
	if got.Security == nil || *got.Security != [3]int{20, 2, 1} || got.CostUSD != 0.02 || got.Repeat != 2 {
		t.Errorf("security/cost: %+v", got)
	}
}

func TestAggregate(t *testing.T) {
	failed := row("tasks", "m", 2, "failed", 0, 0, 1)
	failed.Score = nil // a run without a score counts as 0
	res := New([]Row{
		row("tasks", "m", 1, "passed", 90, 1, 1),
		failed,
		row("inventory", "m", 1, "passed", 80, 2, 2),
		row("inventory", TemplateModel, 1, "passed", 70, 0, 0),
	}, 2)
	if len(res.BySpecModel) != 3 || len(res.BySpec) != 2 || len(res.ByModel) != 2 {
		t.Fatalf("groups: %d %d %d", len(res.BySpecModel), len(res.BySpec), len(res.ByModel))
	}
	if res.Runs[0].Spec != "inventory" || res.BySpecModel[0].Key() != "inventory/m" {
		t.Errorf("not sorted: %s, %s", res.Runs[0].Spec, res.BySpecModel[0].Key())
	}
	tasks := res.BySpecModel[2]
	if tasks.Key() != "tasks/m" || tasks.Runs != 2 || tasks.Passed != 1 || tasks.PassRate != 0.5 ||
		tasks.MeanScore != 45 || tasks.MinScore != 0 || tasks.HoldoutPassRate == nil || *tasks.HoldoutPassRate != 0.5 ||
		tasks.MeanCostUSD != 0.01 || tasks.MeanDurationMs != 1000 {
		t.Errorf("tasks/m: %+v", tasks)
	}
	if tmpl := res.BySpecModel[1]; tmpl.HoldoutPassRate != nil {
		t.Errorf("no holdouts ran, want no rate: %+v", tmpl)
	}
	m := res.ByModel[0]
	if m.Key() != "m" || m.Runs != 3 || m.HoldoutPassed != 3 || m.HoldoutTotal != 4 {
		t.Errorf("by model: %+v", m)
	}
}

func TestCompare(t *testing.T) {
	base := New([]Row{
		row("a", "m", 1, "passed", 80, 1, 1),
		row("b", "m", 1, "passed", 80, 0, 1),
		row("c", "m", 1, "passed", 80, 0, 0),
		row("gone", "m", 1, "passed", 80, 0, 0),
	}, 1)
	cur := New([]Row{
		row("a", "m", 1, "passed", 77, 1, 1), // -3: within the allowance
		row("b", "m", 1, "passed", 85, 1, 1), // better
		row("c", "m", 1, "failed", 76, 0, 0), // pass rate and score drop
		row("new", "m", 1, "passed", 50, 0, 0),
	}, 1)
	c := Compare(base, cur, 3)
	if c.OK() || len(c.Regressions) != 2 {
		t.Fatalf("regressions: %+v", c.Regressions)
	}
	for _, r := range c.Regressions {
		if r.Key != "c/m" {
			t.Errorf("unexpected regression %s", r)
		}
	}
	if len(c.Improvements) != 2 || c.Improvements[0].Key != "b/m" {
		t.Errorf("improvements: %+v", c.Improvements)
	}
	if len(c.Missing) != 1 || c.Missing[0] != "gone/m" || len(c.New) != 1 || c.New[0] != "new/m" {
		t.Errorf("missing %v, new %v", c.Missing, c.New)
	}
	rep := c.Report()
	for _, want := range []string{"2 regression(s)", "c/m: pass rate 1 -> 0", "c/m: mean score 80 -> 76 (-4.00)", "gone/m", "new/m"} {
		if !strings.Contains(rep, want) {
			t.Errorf("report lacks %q:\n%s", want, rep)
		}
	}
	if !Compare(base, base, 0).OK() {
		t.Error("a result regresses against itself")
	}
}

func TestSaveLoadMarkdown(t *testing.T) {
	res := New([]Row{row("a", TemplateModel, 1, "passed", 80, 1, 2), {Spec: "b", Model: TemplateModel, Repeat: 1, Status: "error", Error: "bad | spec"}}, 1)
	path := filepath.Join(t.TempDir(), "r.json")
	if err := res.Save(path); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !Compare(res, got, 0).OK() || len(got.Runs) != 2 {
		t.Errorf("round trip changed the results: %+v", got)
	}
	md := got.Markdown()
	for _, want := range []string{"| a | template | 1 | passed |", "1/2", "| 50.0% |", `error: bad \| spec`, "## By model", "| template | 2 | 50% | 40.0 | 0.0 | 50% (1/2) |"} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown lacks %q:\n%s", want, md)
		}
	}
	got.Format = 99
	_ = got.Save(path)
	if _, err := Load(path); err == nil {
		t.Error("loaded a results file of another format")
	}
}
