package report

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"math"
	"os"
	"path/filepath"
	"strings"

	"github.com/roganis/ez2pyro/internal/model"
	"github.com/roganis/ez2pyro/internal/telemetry"
)

//go:embed assets/*
var assets embed.FS

func asset(name string) string {
	b, err := assets.ReadFile("assets/" + name)
	if err != nil {
		panic(err)
	}
	return string(b)
}

var funcs = template.FuncMap{
	"f0":  func(v float64) string { return fmt.Sprintf("%.0f", v) },
	"f1":  func(v float64) string { return fmt.Sprintf("%.1f", v) },
	"f2":  func(v float64) string { return fmt.Sprintf("%.2f", v) },
	"pct": func(v float64) string { return fmt.Sprintf("%.0f%%", v*100) },
	"clock": func(ms float64) string {
		s := int(ms / 1000)
		return fmt.Sprintf("%d:%02d.%d", s/60, s%60, int(math.Mod(ms, 1000)/100))
	},
	"secs": func(ms float64) string {
		if ms >= 10000 {
			return fmt.Sprintf("%.0f s", ms/1000)
		}
		if ms >= 1000 {
			return fmt.Sprintf("%.1f s", ms/1000)
		}
		return fmt.Sprintf("%.0f ms", ms)
	},
	"join":   strings.Join,
	"add":    func(a, b int) int { return a + b },
	"kb":     func(b int) string { return fmt.Sprintf("%d KB", b/1024) },
	"slice2": func(a, b *model.Run) []*model.Run { return []*model.Run{a, b} },
}

// chartData is the JSON embedded in the report for the charts.
type chartData struct {
	BudgetMs float64      `json:"budget"`
	Delay    [][]any      `json:"delay"` // [t_s[], p99[], lost[]]
	Link     [][]any      `json:"link"`  // [t_s[], signal[], rx[], tx[], band[]]
	Events   []chartEvent `json:"events"`
	Stalls   [][2]float64 `json:"stalls"`   // [start_s, end_s]
	Seconds  [][]any      `json:"seconds"`  // [t_s[], mbps[], late[]]
	Segments [][3]float64 `json:"segments"` // [start_s, end_s, mbps]
}

type chartEvent struct {
	T     float64 `json:"t"`
	Cat   int     `json:"cat"`
	Label string  `json:"label"`
}

// Event categories for the events lane (index = row).
var eventCats = []struct {
	name   string
	events []string
}{
	{"Scan", []string{telemetry.EventScanStart, telemetry.EventScanDone, telemetry.EventScanAborted, telemetry.EventSchedScan}},
	{"Roam / connect", []string{telemetry.EventRoam, telemetry.EventConnect}},
	{"Disconnect", []string{telemetry.EventDisconnect, telemetry.EventDeauth}},
	{"Channel change", []string{telemetry.EventChSwitch, telemetry.EventFreqChange, telemetry.EventRouterChan}},
}

func nullable(v float64, ok bool) any {
	if !ok || math.IsNaN(v) || math.IsInf(v, 0) {
		return nil
	}
	return math.Round(v*100) / 100
}

func buildChartData(run *model.Run) chartData {
	cd := chartData{BudgetMs: run.Options.BudgetMs, Events: []chartEvent{}, Stalls: [][2]float64{}, Segments: [][3]float64{}}
	t, p99, lost := []any{}, []any{}, []any{}
	for _, b := range run.Timeline {
		t = append(t, b.T/1000)
		p99 = append(p99, nullable(b.P99DelayMs, b.P99DelayMs >= 0))
		lost = append(lost, b.Lost)
	}
	cd.Delay = [][]any{t, p99, lost}

	lt, sig, rx, tx, band := []any{}, []any{}, []any{}, []any{}, []any{}
	for i := range run.Telemetry {
		s := &run.Telemetry[i]
		switch {
		case s.Kind == telemetry.KindLink && s.Side == telemetry.SideController:
			lt = append(lt, float64(s.T)/1e9)
			if s.SignalDBm != nil {
				sig = append(sig, *s.SignalDBm)
			} else {
				sig = append(sig, nil)
			}
			if s.RxBitrateMbps != nil {
				rx = append(rx, nullable(*s.RxBitrateMbps, true))
			} else {
				rx = append(rx, nil)
			}
			if s.TxBitrateMbps != nil {
				tx = append(tx, nullable(*s.TxBitrateMbps, true))
			} else {
				tx = append(tx, nil)
			}
			switch s.Band {
			case "2.4":
				band = append(band, 2.4)
			case "5":
				band = append(band, 5)
			case "6":
				band = append(band, 6)
			default:
				band = append(band, nil)
			}
		case s.Kind == telemetry.KindEvent:
			for ci, c := range eventCats {
				for _, e := range c.events {
					if e == s.Event {
						side := "Deck"
						if s.Side == telemetry.SideServer {
							side = "host"
						}
						lbl := s.Event + " (" + side + ")"
						if s.Detail != "" {
							lbl += " " + s.Detail
						}
						cd.Events = append(cd.Events, chartEvent{T: float64(s.T) / 1e9, Cat: ci, Label: lbl})
					}
				}
			}
		}
	}
	cd.Link = [][]any{lt, sig, rx, tx, band}
	for _, s := range run.Stalls {
		cd.Stalls = append(cd.Stalls, [2]float64{s.Start / 1000, s.End / 1000})
	}
	st, mbps, late := []any{}, []any{}, []any{}
	for _, s := range run.Seconds {
		st = append(st, s.T)
		mbps = append(mbps, math.Round(s.Mbps*10)/10)
		late = append(late, s.Late)
	}
	cd.Seconds = [][]any{st, mbps, late}
	for _, s := range run.Segments {
		cd.Segments = append(cd.Segments, [3]float64{s.Start / 1000, s.End / 1000, s.Config.BitrateMbps})
	}
	return cd
}

func jsonJS(v any) template.JS {
	b, err := json.Marshal(v) // escapes <, >, & so it is safe inside <script>
	if err != nil {
		panic(err)
	}
	return template.JS(b)
}

type reportView struct {
	Run        *model.Run
	Top        *model.Verdict
	Others     []model.Verdict
	Title      string
	UPlotJS    template.JS
	UPlotCSS   template.CSS
	StyleCSS   template.CSS
	ChartJS    template.JS
	Data       template.JS
	EventCats  template.JS
	HasLink    bool
	Stalls     []model.Stall
	MoreStalls int
}

// WriteHTML renders report.html into dir.
func WriteHTML(dir string, run *model.Run) error {
	b, err := RenderHTML(run)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, ReportHTML), b, 0o644)
}

// RenderHTML renders the single-file report.
func RenderHTML(run *model.Run) ([]byte, error) {
	t, err := template.New("report").Funcs(funcs).Parse(asset("report.html.tmpl"))
	if err != nil {
		return nil, err
	}
	v := reportView{
		Run:      run,
		Title:    fmt.Sprintf("linkdoctor %s — %s", run.Options.Mode, run.Started),
		UPlotJS:  template.JS(asset("uPlot.iife.min.js")),
		UPlotCSS: template.CSS(asset("uPlot.min.css")),
		StyleCSS: template.CSS(asset("style.css")),
		ChartJS:  template.JS(asset("charts.js")),
		Data:     jsonJS(buildChartData(run)),
	}
	var names []string
	for _, c := range eventCats {
		names = append(names, c.name)
	}
	v.EventCats = jsonJS(names)
	if len(run.Verdicts) > 0 {
		v.Top = &run.Verdicts[0]
		v.Others = run.Verdicts[1:]
	}
	for i := range run.Telemetry {
		if run.Telemetry[i].Kind == telemetry.KindLink {
			v.HasLink = true
			break
		}
	}
	// Keep the stall table readable: the first 500 stalls (all are in stalls.csv).
	v.Stalls = run.Stalls
	if len(v.Stalls) > 500 {
		v.MoreStalls = len(v.Stalls) - 500
		v.Stalls = v.Stalls[:500]
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Write writes the data files and the HTML report.
func Write(dir string, run *model.Run) error {
	if err := WriteData(dir, run); err != nil {
		return err
	}
	return WriteHTML(dir, run)
}
