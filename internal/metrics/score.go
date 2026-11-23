package metrics

import (
	"fmt"
	"math"
)

// Score is a 0-100 quality score with its breakdown.
type Score struct {
	Total  float64     `json:"total"`
	Grade  string      `json:"grade"`
	Capped bool        `json:"capped"`
	Parts  []ScorePart `json:"parts"`
}

// ScorePart is one weighted component of the score. Parts that were not
// measured in a run are left out, and the total is scaled to 100 over the
// parts that were.
type ScorePart struct {
	Name     string  `json:"name"`
	Weight   float64 `json:"weight"`
	Points   float64 `json:"points"`
	Detail   string  `json:"detail"`
	Measured bool    `json:"measured"`
}

// GateCap is the highest score a run with a failed gate can get.
const GateCap = 40

// MutationTarget is the mutation score that earns full points. Industry
// experience (e.g. Google's mutation testing in code review) treats
// surviving mutants as review prompts rather than demanding 100%.
const MutationTarget = 0.80

// ComputeScore weighs test effectiveness, conformance to the spec,
// complexity, maintainability, findings and compatibility. If gatesPassed
// is false the total is capped at GateCap.
func ComputeScore(r *Report, gatesPassed bool) Score {
	var parts []ScorePart
	add := func(name string, weight, fraction float64, measured bool, detail string) {
		fraction = math.Max(0, math.Min(1, fraction))
		p := ScorePart{Name: name, Weight: weight, Detail: detail, Measured: measured}
		if measured {
			p.Points = round(weight*fraction, 1)
		}
		parts = append(parts, p)
	}

	if m := r.Mutation; m != nil && m.Score != nil {
		add("Mutation score", 20, *m.Score/MutationTarget, true,
			fmt.Sprintf("%.0f%% of mutants detected by the tests (full points at %.0f%%)", *m.Score*100, MutationTarget*100))
	} else {
		add("Mutation score", 20, 0, false, "not measured in this run")
	}

	cov := 0.0
	if r.Coverage != nil {
		cov = r.Coverage.ServicePercent()
	}
	add("Coverage", 10, cov/85, r.Coverage != nil, fmt.Sprintf("%.1f%% of service statements covered (full points at 85%%)", cov))

	if a := r.Acceptance; a != nil && len(a.Examples) > 0 {
		add("Acceptance", 15, float64(a.Passed)/float64(len(a.Examples)), true,
			fmt.Sprintf("%d of %d spec examples pass (%d pending, %d holdout)", a.Passed, len(a.Examples), a.Pending, a.Holdout))
	} else {
		add("Acceptance", 15, 0, false, "the spec has no examples")
	}

	done := 1.0
	if r.Completeness.Total > 0 {
		done = float64(r.Completeness.Implemented) / float64(r.Completeness.Total)
	}
	add("Completeness", 10, done, true, fmt.Sprintf("%d of %d RPCs implemented", r.Completeness.Implemented, r.Completeness.Total))

	simple := 1.0
	if r.Complexity.Funcs > 0 {
		var ok int
		for _, f := range r.Functions {
			if !f.Test && f.Cyclomatic <= 10 && f.Cognitive <= 15 {
				ok++
			}
		}
		simple = float64(ok) / float64(r.Complexity.Funcs)
	}
	add("Complexity", 15, simple, true, fmt.Sprintf("%.0f%% of functions have cyclomatic ≤ 10 and cognitive ≤ 15", simple*100))

	add("Maintainability", 5, (r.Complexity.MeanMI-20)/40, true,
		fmt.Sprintf("mean Maintainability Index %.1f (full points at 60, none at 20)", r.Complexity.MeanMI))

	f := r.Findings
	penalty := 3*float64(f.Code()) + 5*float64(len(f.Vulns)) + float64(len(f.Modernize))
	add("Findings", 15, (15-penalty)/15, true,
		fmt.Sprintf("%d code findings (−3 each), %d reachable vulnerabilities (−5 each), %d modernizations (−1 each)",
			f.Code(), len(f.Vulns), len(f.Modernize)))

	if c := r.Contract; c != nil && c.Baseline != "" {
		add("Compatibility", 10, 1-float64(len(c.Changes)), true,
			fmt.Sprintf("%d breaking changes against run %s", len(c.Changes), c.Baseline))
	} else {
		add("Compatibility", 10, 0, false, "no earlier passing run of this service to compare with")
	}

	s := Score{Parts: parts}
	var points, weight float64
	for _, p := range parts {
		if p.Measured {
			points += p.Points
			weight += p.Weight
		}
	}
	if weight > 0 {
		s.Total = round(points/weight*100, 1)
	}
	if !gatesPassed && s.Total > GateCap {
		s.Total = GateCap
		s.Capped = true
	}
	switch {
	case s.Total >= 90:
		s.Grade = "A"
	case s.Total >= 80:
		s.Grade = "B"
	case s.Total >= 65:
		s.Grade = "C"
	case s.Total >= 50:
		s.Grade = "D"
	default:
		s.Grade = "F"
	}
	return s
}
