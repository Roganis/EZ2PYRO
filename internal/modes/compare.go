package modes

import (
	"fmt"
	"io"
	"path/filepath"

	"github.com/roganis/ez2pyro/internal/analysis"
	"github.com/roganis/ez2pyro/internal/model"
	"github.com/roganis/ez2pyro/internal/report"
)

// Analyze reloads a run folder, re-runs the analysis (optionally with a new
// late budget) and rewrites its report and derived files.
func Analyze(dir string, budgetMs float64) (*model.Run, error) {
	run, err := report.Load(dir)
	if err != nil {
		return nil, err
	}
	if budgetMs > 0 {
		run.Options.BudgetMs = budgetMs
	}
	analysis.Analyze(run)
	return run, report.Write(dir, run)
}

// Compare loads two run folders and writes a side-by-side report to out
// (default: <b>/compare.html).
func Compare(a, b, out string, w io.Writer) (string, error) {
	ra, err := report.Load(a)
	if err != nil {
		return "", fmt.Errorf("%s: %w", a, err)
	}
	rb, err := report.Load(b)
	if err != nil {
		return "", fmt.Errorf("%s: %w", b, err)
	}
	analysis.Analyze(ra)
	analysis.Analyze(rb)
	if out == "" {
		out = filepath.Join(b, "compare.html")
	}
	if err := report.WriteCompare(out, filepath.Base(filepath.Clean(a)), ra, filepath.Base(filepath.Clean(b)), rb); err != nil {
		return "", err
	}
	if w != nil {
		PrintCompare(w, filepath.Base(a), ra, filepath.Base(b), rb)
	}
	return out, nil
}

// PrintCompare prints the key numbers of two runs side by side.
func PrintCompare(w io.Writer, na string, a *model.Run, nb string, b *model.Run) {
	fmt.Fprintf(w, "%-24s %14s %14s\n", "", "A: "+na, "B: "+nb)
	for _, r := range report.CompareRows(a, b) {
		fmt.Fprintf(w, "%-24s %14s %14s   %s\n", r.Label, r.A, r.B, r.Delta)
	}
	top := func(r *model.Run) string {
		if len(r.Verdicts) == 0 {
			return "—"
		}
		return r.Verdicts[0].Cause
	}
	fmt.Fprintf(w, "%-24s %s → %s\n", "top cause", top(a), top(b))
}
