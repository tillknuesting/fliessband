package pipeline

import (
	"strings"
	"testing"

	"github.com/tillknuesting/fliessband/internal/metrics"
)

func TestFeedbackHidesHoldouts(t *testing.T) {
	cov := 50.0
	rep := &metrics.Report{
		Score: metrics.Score{Total: 71, Grade: "C", Parts: []metrics.ScorePart{
			{Name: "Mutation score", Weight: 20, Points: 12, Detail: "60% of faults caught", Measured: true},
			{Name: "Compatibility", Weight: 10, Points: 10, Measured: true},
		}},
		Acceptance: &metrics.Acceptance{Examples: []metrics.Example{
			{Method: "AdjustStock", Name: "refuses to go below zero", Status: "failed", Message: "got OK"},
			{Method: "AdjustStock", Name: "secret holdout case", Holdout: true, Status: "failed", Message: "got 5"},
		}},
		Mutation: &metrics.Mutation{Survivors: []metrics.Mutant{
			{File: "internal/service/custom.go", Line: 12, Before: "if d < 0 {", After: "if d <= 0 {"},
			{File: "internal/service/product.go", Line: 3, Before: "a", After: "b"},
		}},
		Functions: []metrics.Func{{Name: "Server.AdjustStock", File: "internal/service/custom.go", Cyclomatic: 14, Coverage: &cov}},
	}
	got := feedback(rep)
	for _, want := range []string{"scored 71", "Mutation score: 12.0 of 20", "refuses to go below zero", "1 hidden example fail", "custom.go:12", "cyclomatic complexity 14", "only 50% covered"} {
		if !strings.Contains(got, want) {
			t.Errorf("feedback lacks %q:\n%s", want, got)
		}
	}
	for _, leak := range []string{"secret holdout case", "got 5", "product.go", "Compatibility"} {
		if strings.Contains(got, leak) {
			t.Errorf("feedback contains %q:\n%s", leak, got)
		}
	}
}
