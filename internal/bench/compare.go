package bench

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// Change is one metric of one group that differs from the baseline.
type Change struct {
	Key      string  `json:"key"` // spec/model
	Metric   string  `json:"metric"`
	Baseline float64 `json:"baseline"`
	Current  float64 `json:"current"`
}

func (c Change) String() string {
	return fmt.Sprintf("%s: %s %s -> %s (%+.2f)", c.Key, c.Metric, fmtNum(c.Baseline), fmtNum(c.Current), c.Current-c.Baseline)
}

// Comparison is the outcome of comparing results with a baseline.
type Comparison struct {
	MaxRegression float64  `json:"max_regression"`
	Regressions   []Change `json:"regressions"`  // fail the benchmark
	Improvements  []Change `json:"improvements"` // informational
	Missing       []string `json:"missing"`      // in the baseline, not run now
	New           []string `json:"new"`          // run now, not in the baseline
}

// OK reports whether nothing regressed.
func (c *Comparison) OK() bool { return len(c.Regressions) == 0 }

// eps absorbs rounding in stored rates and scores.
const eps = 1e-9

// Compare checks each spec/model group of cur against the same group in
// base. A group regresses when its mean score drops by more than
// maxRegression points or its pass rate drops at all. Holdout pass rate
// changes are reported but only the score and pass rate fail a comparison,
// since the score already weighs acceptance.
func Compare(base, cur *Results, maxRegression float64) *Comparison {
	c := &Comparison{MaxRegression: maxRegression}
	old := map[string]Agg{}
	for _, a := range base.BySpecModel {
		old[a.Key()] = a
	}
	seen := map[string]bool{}
	for _, a := range cur.BySpecModel {
		k := a.Key()
		seen[k] = true
		b, ok := old[k]
		if !ok {
			c.New = append(c.New, k)
			continue
		}
		score := Change{Key: k, Metric: "mean score", Baseline: b.MeanScore, Current: a.MeanScore}
		switch d := a.MeanScore - b.MeanScore; {
		case d < -maxRegression-eps:
			c.Regressions = append(c.Regressions, score)
		case d > eps:
			c.Improvements = append(c.Improvements, score)
		}
		pass := Change{Key: k, Metric: "pass rate", Baseline: b.PassRate, Current: a.PassRate}
		switch d := a.PassRate - b.PassRate; {
		case d < -eps:
			c.Regressions = append(c.Regressions, pass)
		case d > eps:
			c.Improvements = append(c.Improvements, pass)
		}
		if a.HoldoutPassRate != nil && b.HoldoutPassRate != nil && *a.HoldoutPassRate > *b.HoldoutPassRate+eps {
			c.Improvements = append(c.Improvements, Change{Key: k, Metric: "holdout pass rate", Baseline: *b.HoldoutPassRate, Current: *a.HoldoutPassRate})
		}
	}
	for _, a := range base.BySpecModel {
		if !seen[a.Key()] {
			c.Missing = append(c.Missing, a.Key())
		}
	}
	return c
}

// Report renders the comparison as markdown.
func (c *Comparison) Report() string {
	var b strings.Builder
	b.WriteString("## Against the baseline\n\n")
	if c.OK() {
		fmt.Fprintf(&b, "No regressions (mean score may drop at most %s points; pass rate may not drop).\n", fmtNum(c.MaxRegression))
	} else {
		fmt.Fprintf(&b, "**%d regression(s)** (mean score may drop at most %s points; pass rate may not drop):\n\n", len(c.Regressions), fmtNum(c.MaxRegression))
		for _, r := range c.Regressions {
			b.WriteString("- " + r.String() + "\n")
		}
	}
	if len(c.Improvements) > 0 {
		b.WriteString("\nImprovements:\n\n")
		for _, r := range c.Improvements {
			b.WriteString("- " + r.String() + "\n")
		}
	}
	if len(c.Missing) > 0 {
		b.WriteString("\nIn the baseline but not run now: " + strings.Join(c.Missing, ", ") + "\n")
	}
	if len(c.New) > 0 {
		b.WriteString("\nNot in the baseline: " + strings.Join(c.New, ", ") + "\n")
	}
	return b.String()
}

// Load reads a results file.
func Load(path string) (*Results, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var r Results
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if r.Format != FormatVersion {
		return nil, fmt.Errorf("%s: results format %d, want %d", path, r.Format, FormatVersion)
	}
	return &r, nil
}

// Save writes a results file.
func (r *Results) Save(path string) error {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o600)
}
