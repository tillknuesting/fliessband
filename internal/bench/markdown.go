package bench

import (
	"fmt"
	"strconv"
	"strings"
)

// Markdown renders the results as markdown tables: every run, then the
// aggregates per spec and model, per spec and per model.
func (r *Results) Markdown() string {
	var b strings.Builder
	b.WriteString("## Runs\n\n")
	b.WriteString("| Spec | Model | # | Status | Failed at | Score | Grade | Holdout | Visible | Mutation | Coverage | Security p/w/f | Cost $ | Time s |\n")
	b.WriteString("|---|---|--:|---|---|--:|---|--:|--:|--:|--:|--:|--:|--:|\n")
	for _, row := range r.Runs {
		status := row.Status
		if row.Error != "" {
			status = "error: " + row.Error
		}
		sec := "–"
		if row.Security != nil {
			sec = fmt.Sprintf("%d/%d/%d", row.Security[0], row.Security[1], row.Security[2])
		}
		fmt.Fprintf(&b, "| %s | %s | %d | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %.1f |\n",
			row.Spec, row.Model, row.Repeat, cell(status), dash(row.FailedAt), ptr(row.Score, "%.1f"), dash(row.Grade),
			frac(row.HoldoutPassed, row.HoldoutTotal), frac(row.VisiblePassed, row.VisibleTotal),
			pct(row.Mutation), ptr(row.Coverage, "%.1f%%"), sec, fmtCost(row.CostUSD), row.DurationMs/1000)
	}
	section := func(title string, aggs []Agg) {
		fmt.Fprintf(&b, "\n## %s\n\n", title)
		b.WriteString("| Group | Runs | Pass rate | Mean score | Min score | Holdout pass rate | Mean cost $ | Mean time s |\n")
		b.WriteString("|---|--:|--:|--:|--:|--:|--:|--:|\n")
		for _, a := range aggs {
			hold := "–"
			if a.HoldoutPassRate != nil {
				hold = fmt.Sprintf("%.0f%% (%d/%d)", 100**a.HoldoutPassRate, a.HoldoutPassed, a.HoldoutTotal)
			}
			fmt.Fprintf(&b, "| %s | %d | %.0f%% | %.1f | %.1f | %s | %s | %.1f |\n",
				a.Key(), a.Runs, 100*a.PassRate, a.MeanScore, a.MinScore, hold, fmtCost(a.MeanCostUSD), a.MeanDurationMs/1000)
		}
	}
	section("By spec and model", r.BySpecModel)
	if len(r.BySpec) > 1 && len(r.ByModel) > 1 {
		section("By spec", r.BySpec)
	}
	section("By model", r.ByModel)
	return b.String()
}

func cell(s string) string { return strings.ReplaceAll(s, "|", `\|`) }

func dash(s string) string {
	if s == "" {
		return "–"
	}
	return cell(s)
}

func ptr(v *float64, format string) string {
	if v == nil {
		return "–"
	}
	return fmt.Sprintf(format, *v)
}

// pct renders a 0-1 fraction as a percentage.
func pct(v *float64) string {
	if v == nil {
		return "–"
	}
	return fmt.Sprintf("%.1f%%", 100**v)
}

func frac(n, total int) string {
	if total == 0 {
		return "–"
	}
	return fmt.Sprintf("%d/%d", n, total)
}

func fmtCost(v float64) string {
	if v == 0 {
		return "0"
	}
	return strconv.FormatFloat(v, 'f', 4, 64)
}

func fmtNum(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }
