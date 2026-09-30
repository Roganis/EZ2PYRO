package report

import (
	"bytes"
	"fmt"
	"html/template"
	"math"
	"os"

	"github.com/roganis/ez2pyro/internal/model"
)

// CompareRow is one key number of a two-run comparison.
type CompareRow struct {
	Label  string
	A, B   string
	Delta  string
	Better int // 1 = B better, -1 = B worse, 0 = same / n.a.
}

// CompareRows builds the comparison table. lowerIsBetter decides the sign.
func CompareRows(a, b *model.Run) []CompareRow {
	row := func(label, unit string, prec int, va, vb float64, lowerIsBetter bool) CompareRow {
		f := func(v float64) string { return fmt.Sprintf("%.*f%s", prec, v, unit) }
		r := CompareRow{Label: label, A: f(va), B: f(vb)}
		d := vb - va
		if math.Abs(d) < math.Pow(10, -float64(prec))/2 {
			r.Delta = "same"
			return r
		}
		r.Delta = fmt.Sprintf("%+.*f%s", prec, d, unit)
		if (d < 0) == lowerIsBetter {
			r.Better = 1
		} else {
			r.Better = -1
		}
		return r
	}
	sa, sb := a.Summary, b.Summary
	rows := []CompareRow{
		row("Late frames", "%", 2, sa.LatePct, sb.LatePct, true),
		row("Stalls per minute", "", 2, sa.StallsPerMin, sb.StallsPerMin, true),
		row("Stalls", "", 0, float64(sa.Stalls), float64(sb.Stalls), true),
		row("Longest stall", " ms", 0, sa.LongestStallMs, sb.LongestStallMs, true),
		row("Packet loss", "%", 3, sa.LossPct, sb.LossPct, true),
		row("Frame delay p99", " ms", 1, sa.P99DelayMs, sb.P99DelayMs, true),
		row("Throughput", " Mbit/s", 0, sa.MeanMbps, sb.MeanMbps, false),
		row("RTT (median)", " ms", 1, sa.MedianRTTms, sb.MedianRTTms, true),
		row("Test length", " s", 0, a.DurationS, b.DurationS, false),
	}
	rows[len(rows)-1].Better = 0
	if sa.RecommendedMbps > 0 || sb.RecommendedMbps > 0 {
		rows = append(rows, row("Recommended bitrate", " Mbit/s", 0, sa.RecommendedMbps, sb.RecommendedMbps, false))
	}
	return rows
}

type compareView struct {
	NameA, NameB string
	A, B         *model.Run
	Rows         []CompareRow
	UPlotJS      template.JS
	UPlotCSS     template.CSS
	StyleCSS     template.CSS
	Data         template.JS
}

// WriteCompare renders the comparison report to path.
func WriteCompare(path, na string, a *model.Run, nb string, b *model.Run) error {
	t, err := template.New("compare").Funcs(funcs).Parse(asset("compare.html.tmpl"))
	if err != nil {
		return err
	}
	series := func(r *model.Run) (late, p99 []any) {
		for _, s := range r.Seconds {
			late = append(late, s.Late)
			p99 = append(p99, nullable(s.P99DelayMs, s.Frames > 0))
		}
		return
	}
	n := len(a.Seconds)
	if len(b.Seconds) > n {
		n = len(b.Seconds)
	}
	x := make([]any, n)
	for i := range x {
		x[i] = i
	}
	la, pa := series(a)
	lb, pb := series(b)
	pad := func(v []any) []any {
		for len(v) < n {
			v = append(v, nil)
		}
		return v
	}
	data := map[string]any{
		"late":  [][]any{x, pad(la), pad(lb)},
		"p99":   [][]any{x, pad(pa), pad(pb)},
		"names": []string{na, nb},
	}
	v := compareView{
		NameA: na, NameB: nb, A: a, B: b, Rows: CompareRows(a, b),
		UPlotJS: template.JS(asset("uPlot.iife.min.js")), UPlotCSS: template.CSS(asset("uPlot.min.css")),
		StyleCSS: template.CSS(asset("style.css")), Data: jsonJS(data),
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, v); err != nil {
		return err
	}
	return os.WriteFile(path, buf.Bytes(), 0o644)
}
