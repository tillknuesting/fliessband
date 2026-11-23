// Package bench measures the factory itself: it turns finished runs into
// benchmark rows, aggregates them per spec and per model, renders a summary
// and compares a result against a baseline, so a change to prompts,
// templates or models can be shown to be better or worse.
//
// Everything here is pure; running the line is the caller's job.
package bench

import (
	"cmp"
	"math"
	"slices"
	"time"

	"github.com/tillknuesting/fliessband/internal/pipeline"
)

// FormatVersion is the version of the results file layout.
const FormatVersion = 1

// TemplateModel labels runs without an LLM: templates and stubs only.
const TemplateModel = "template"

// Results is one benchmark: every run and the aggregates over them.
type Results struct {
	Format      int       `json:"format"`
	CreatedAt   time.Time `json:"created_at"`
	Factory     string    `json:"factory_version"`
	GoVersion   string    `json:"go_version,omitempty"`
	Platform    string    `json:"platform,omitempty"`
	Repeat      int       `json:"repeat"`
	Runs        []Row     `json:"runs"`
	BySpecModel []Agg     `json:"by_spec_model"`
	BySpec      []Agg     `json:"by_spec"`
	ByModel     []Agg     `json:"by_model"`
}

// Row is one run of one spec with one model.
type Row struct {
	Spec          string   `json:"spec"`
	Model         string   `json:"model"`
	Repeat        int      `json:"repeat"`
	RunID         string   `json:"run_id,omitempty"`
	Status        string   `json:"status"`
	FailedAt      string   `json:"failed_at,omitempty"`
	Error         string   `json:"error,omitempty"` // the run could not be started
	Score         *float64 `json:"score,omitempty"`
	Grade         string   `json:"grade,omitempty"`
	HoldoutPassed int      `json:"holdout_passed"`
	HoldoutTotal  int      `json:"holdout_total"`
	VisiblePassed int      `json:"visible_passed"`
	VisibleTotal  int      `json:"visible_total"`
	Mutation      *float64 `json:"mutation_score,omitempty"` // fraction 0-1 of mutants detected
	Coverage      *float64 `json:"coverage,omitempty"`       // percent of service statements
	Security      *[3]int  `json:"security,omitempty"`       // checks passed, warned, failed
	CostUSD       float64  `json:"cost_usd"`
	DurationMs    float64  `json:"duration_ms"`
}

// Passed reports whether the run passed every gate.
func (r Row) Passed() bool { return r.Status == string(pipeline.Passed) }

// Agg aggregates the runs of one spec, one model, or one spec with one model.
// Empty Spec or Model means "all".
type Agg struct {
	Spec            string   `json:"spec,omitempty"`
	Model           string   `json:"model,omitempty"`
	Runs            int      `json:"runs"`
	Passed          int      `json:"passed"`
	PassRate        float64  `json:"pass_rate"`
	MeanScore       float64  `json:"mean_score"` // a run without a score counts as 0
	MinScore        float64  `json:"min_score"`
	HoldoutPassed   int      `json:"holdout_passed"`
	HoldoutTotal    int      `json:"holdout_total"`
	HoldoutPassRate *float64 `json:"holdout_pass_rate,omitempty"` // absent when no holdout ran
	MeanCostUSD     float64  `json:"mean_cost_usd"`
	MeanDurationMs  float64  `json:"mean_duration_ms"`
}

// Key names the group, e.g. "tasks/template".
func (a Agg) Key() string {
	switch {
	case a.Spec == "":
		return a.Model
	case a.Model == "":
		return a.Spec
	}
	return a.Spec + "/" + a.Model
}

// FromRun turns a finished run into a row.
func FromRun(spec, model string, repeat int, r *pipeline.Run) Row {
	s := r.Summary()
	row := Row{Spec: spec, Model: model, Repeat: repeat, RunID: s.ID, Status: string(s.Status), FailedAt: s.FailedAt,
		Score: s.Score, Grade: s.Grade, Mutation: s.Mutation, Coverage: s.Coverage, Security: s.Security,
		CostUSD: s.CostUSD, DurationMs: math.Round(s.DurationMs)}
	if row.Mutation != nil {
		m := round(*row.Mutation, 4)
		row.Mutation = &m
	}
	if r.Report != nil && r.Report.Acceptance != nil {
		for _, ex := range r.Report.Acceptance.Examples {
			ok := ex.Status == "passed"
			if ex.Holdout {
				row.HoldoutTotal++
				if ok {
					row.HoldoutPassed++
				}
				continue
			}
			row.VisibleTotal++
			if ok {
				row.VisiblePassed++
			}
		}
	}
	return row
}

// New builds results from rows, sorted and aggregated.
func New(rows []Row, repeat int) *Results {
	rows = slices.Clone(rows)
	slices.SortStableFunc(rows, func(a, b Row) int {
		return cmp.Or(cmp.Compare(a.Spec, b.Spec), cmp.Compare(a.Model, b.Model), cmp.Compare(a.Repeat, b.Repeat))
	})
	return &Results{
		Format:      FormatVersion,
		Repeat:      repeat,
		Runs:        rows,
		BySpecModel: Aggregate(rows, func(r Row) (string, string) { return r.Spec, r.Model }),
		BySpec:      Aggregate(rows, func(r Row) (string, string) { return r.Spec, "" }),
		ByModel:     Aggregate(rows, func(r Row) (string, string) { return "", r.Model }),
	}
}

// Aggregate groups rows by the (spec, model) pair key returns, in sorted order.
func Aggregate(rows []Row, key func(Row) (spec, model string)) []Agg {
	type k struct{ spec, model string }
	groups := map[k][]Row{}
	var order []k
	for _, r := range rows {
		s, m := key(r)
		g := k{s, m}
		if _, ok := groups[g]; !ok {
			order = append(order, g)
		}
		groups[g] = append(groups[g], r)
	}
	slices.SortFunc(order, func(a, b k) int { return cmp.Or(cmp.Compare(a.spec, b.spec), cmp.Compare(a.model, b.model)) })
	out := make([]Agg, 0, len(order))
	for _, g := range order {
		out = append(out, aggregate(g.spec, g.model, groups[g]))
	}
	return out
}

func aggregate(spec, model string, rows []Row) Agg {
	a := Agg{Spec: spec, Model: model, Runs: len(rows)}
	if len(rows) == 0 {
		return a
	}
	var score, cost, dur float64
	a.MinScore = math.Inf(1)
	for _, r := range rows {
		if r.Passed() {
			a.Passed++
		}
		s := 0.0
		if r.Score != nil {
			s = *r.Score
		}
		score += s
		a.MinScore = min(a.MinScore, s)
		a.HoldoutPassed += r.HoldoutPassed
		a.HoldoutTotal += r.HoldoutTotal
		cost += r.CostUSD
		dur += r.DurationMs
	}
	n := float64(len(rows))
	a.PassRate = round(float64(a.Passed)/n, 3)
	a.MeanScore = round(score/n, 2)
	a.MinScore = round(a.MinScore, 2)
	a.MeanCostUSD = round(cost/n, 6)
	a.MeanDurationMs = math.Round(dur / n)
	if a.HoldoutTotal > 0 {
		h := round(float64(a.HoldoutPassed)/float64(a.HoldoutTotal), 3)
		a.HoldoutPassRate = &h
	}
	return a
}

func round(v float64, places int) float64 {
	p := math.Pow(10, float64(places))
	return math.Round(v*p) / p
}
