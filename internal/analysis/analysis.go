package analysis

import (
	"fmt"
	"math"
	"sort"

	"github.com/roganis/ez2pyro/internal/model"
	"github.com/roganis/ez2pyro/internal/telemetry"
)

// Clean-step thresholds (ramp) and the recommended share of the highest
// clean bitrate.
const (
	CleanLatePct     = 1.0
	CleanLossPct     = 0.5
	RecommendedShare = 0.7
)

// Analyze fills in the derived parts of a run: segment stats, per-second
// summaries, the timeline, stalls, periodicity, ramp result, verdicts and
// the summary. It can be rerun on saved data.
func Analyze(run *model.Run) {
	budget := run.Options.BudgetMs
	if budget <= 0 && run.Options.FPS > 0 {
		budget = 1000 / float64(run.Options.FPS)
	}
	Relate(run.Frames, budget)
	sort.SliceStable(run.Frames, func(i, j int) bool {
		a, b := run.Frames[i], run.Frames[j]
		if a.Segment != b.Segment {
			return a.Segment < b.Segment
		}
		return a.ID < b.ID
	})
	sort.SliceStable(run.Telemetry, func(i, j int) bool { return run.Telemetry[i].T < run.Telemetry[j].T })

	SegmentStats(run)
	fillEnvironment(run)
	run.Seconds = Seconds(run)
	run.Timeline = Timeline(run.Frames, 100)
	run.Stalls = DetectStalls(run.Frames, run.Gaps, func(seg int) float64 { return intervalMs(run, seg) })
	run.Periodicity = Periodicity(run.Stalls, run.Seconds)
	if run.Options.Mode == "ramp" {
		run.Ramp = Ramp(run.Segments)
	}
	summarize(run)
	tele := NewTele(run.Telemetry)
	run.Verdicts = Diagnose(&Ctx{Run: run, Tele: tele})
}

func intervalMs(run *model.Run, seg int) float64 {
	if seg >= 0 && seg < len(run.Segments) && run.Segments[seg].Config.FPS > 0 {
		return 1000 / float64(run.Segments[seg].Config.FPS)
	}
	if run.Options.FPS > 0 {
		return 1000 / float64(run.Options.FPS)
	}
	return 1000.0 / 60
}

func pktSize(run *model.Run, seg int) int {
	if seg >= 0 && seg < len(run.Segments) && run.Segments[seg].Config.PktSize > 0 {
		return run.Segments[seg].Config.PktSize
	}
	return run.Options.PktSize
}

// SegmentStats computes per-segment late/loss figures and cleanliness.
func SegmentStats(run *model.Run) {
	type acc struct {
		frames, late, pkts, lost, recv int
		delays                         []float64
	}
	accs := make([]acc, len(run.Segments))
	for i := range run.Frames {
		f := &run.Frames[i]
		if f.Segment < 0 || f.Segment >= len(accs) {
			continue
		}
		a := &accs[f.Segment]
		a.frames++
		if f.Late {
			a.late++
		}
		a.pkts += f.Pkts
		a.lost += f.Lost()
		a.recv += f.Recv
		if f.Complete {
			a.delays = append(a.delays, f.DelayMs)
		}
	}
	for i := range run.Segments {
		s, a := &run.Segments[i], accs[i]
		s.Frames, s.LateFrames = a.frames, a.late
		if a.frames > 0 {
			s.LatePct = 100 * float64(a.late) / float64(a.frames)
			dur := float64(a.frames) * intervalMs(run, i) / 1000
			s.RecvMbps = float64(a.recv*pktSize(run, i)) * 8 / dur / 1e6
		}
		if a.pkts > 0 {
			s.LossPct = 100 * float64(a.lost) / float64(a.pkts)
		}
		s.P99DelayMs = percentile(a.delays, 0.99)
		s.Clean = a.frames > 0 && s.LatePct <= CleanLatePct && s.LossPct <= CleanLossPct
	}
}

// Ramp derives the highest clean bitrate and the recommendation.
func Ramp(segs []model.Segment) *model.RampResult {
	r := &model.RampResult{}
	fails := 0
	for _, s := range segs {
		r.Steps = append(r.Steps, model.RampStep{
			BitrateMbps: s.Config.BitrateMbps, RecvMbps: s.RecvMbps, LatePct: s.LatePct,
			LossPct: s.LossPct, P99DelayMs: s.P99DelayMs, Clean: s.Clean,
		})
		if !s.Clean {
			fails++
		} else if s.Config.BitrateMbps > r.HighestCleanMbps {
			r.HighestCleanMbps = s.Config.BitrateMbps
		}
	}
	r.StoppedEarly = fails >= 2
	r.RecommendedMbps = math.Floor(r.HighestCleanMbps*RecommendedShare/5) * 5
	return r
}

// Seconds builds the per-second summary.
func Seconds(run *model.Run) []model.Second {
	if len(run.Frames) == 0 {
		return nil
	}
	maxT := 0.0
	for i := range run.Frames {
		maxT = math.Max(maxT, run.Frames[i].T)
	}
	n := int(maxT/1000) + 1
	out := make([]model.Second, n)
	delays := make([][]float64, n)
	pk := make([]int, n)
	lost := make([]int, n)
	for i := range out {
		out[i].T = i
		out[i].RTTms = -1
	}
	for i := range run.Frames {
		f := &run.Frames[i]
		s := int(f.T / 1000)
		if s < 0 {
			s = 0
		}
		out[s].Frames++
		if f.Late {
			out[s].Late++
		}
		pk[s] += f.Pkts
		lost[s] += f.Lost()
		out[s].Mbps += float64(f.Recv*pktSize(run, f.Segment)) * 8 / 1e6
		if f.Complete {
			delays[s] = append(delays[s], f.DelayMs)
		}
	}
	for i := range out {
		if pk[i] > 0 {
			out[i].LossPct = 100 * float64(lost[i]) / float64(pk[i])
		}
		out[i].P50DelayMs = percentile(delays[i], 0.5)
		out[i].P99DelayMs = percentile(delays[i], 0.99)
	}
	for _, s := range run.Telemetry {
		if s.Kind == telemetry.KindRTT && s.RTTms != nil {
			if i := int(float64(s.T) / 1e9); i >= 0 && i < n {
				out[i].RTTms = *s.RTTms
			}
		}
	}
	return out
}

// Timeline bins frames into binMs bins with the p99 completion delay.
func Timeline(frames []model.Frame, binMs float64) []model.TimelineBin {
	if len(frames) == 0 {
		return nil
	}
	maxT := 0.0
	for i := range frames {
		maxT = math.Max(maxT, frames[i].T)
	}
	n := int(maxT/binMs) + 1
	bins := make([]model.TimelineBin, n)
	vals := make([][]float64, n)
	for i := range bins {
		bins[i].T = float64(i) * binMs
		bins[i].P99DelayMs = -1
	}
	for i := range frames {
		f := &frames[i]
		b := int(f.T / binMs)
		if b < 0 {
			b = 0
		}
		if f.Late {
			bins[b].Late++
		}
		if !f.Complete {
			bins[b].Lost++
			continue
		}
		vals[b] = append(vals[b], f.DelayMs)
	}
	for i := range bins {
		if len(vals[i]) > 0 {
			bins[i].P99DelayMs = percentile(vals[i], 0.99)
		}
	}
	return bins
}

func summarize(run *model.Run) {
	s := &run.Summary
	*s = model.Summary{}
	var delays []float64
	var bits float64
	maxT := 0.0
	for i := range run.Frames {
		f := &run.Frames[i]
		s.Frames++
		if f.Late {
			s.LateFrames++
		}
		s.PacketsLost += f.Lost()
		if f.Complete {
			delays = append(delays, f.DelayMs)
		}
		bits += float64(f.Recv*pktSize(run, f.Segment)) * 8
		maxT = math.Max(maxT, f.T)
	}
	var pk int
	for i := range run.Frames {
		pk += run.Frames[i].Pkts
	}
	if s.Frames > 0 {
		s.LatePct = 100 * float64(s.LateFrames) / float64(s.Frames)
	}
	if pk > 0 {
		s.LossPct = 100 * float64(s.PacketsLost) / float64(pk)
	}
	s.P50DelayMs = percentile(delays, 0.5)
	s.P99DelayMs = percentile(delays, 0.99)
	var active float64
	for i := range run.Segments {
		active += float64(run.Segments[i].Frames) * intervalMs(run, i) / 1000
		s.SenderOverruns += run.Segments[i].Sender.Overruns
	}
	if active > 0 {
		s.MeanMbps = bits / active / 1e6
	}
	s.Stalls = len(run.Stalls)
	for _, st := range run.Stalls {
		s.LongestStallMs = math.Max(s.LongestStallMs, st.DurationMs)
	}
	if maxT > 0 {
		s.StallsPerMin = float64(s.Stalls) / (maxT / 60000)
	}
	var rtts []float64
	var first, last *uint64
	for i := range run.Telemetry {
		t := &run.Telemetry[i]
		if t.Kind == telemetry.KindRTT && t.RTTms != nil {
			rtts = append(rtts, *t.RTTms)
		}
		if t.Kind == telemetry.KindSNMP && t.Side == telemetry.SideController && t.RcvbufErrors != nil {
			if first == nil {
				first = t.RcvbufErrors
			}
			last = t.RcvbufErrors
		}
	}
	s.MedianRTTms = median(rtts)
	if first != nil && *last > *first {
		s.RcvbufErrors = *last - *first
	}
	if run.Ramp != nil {
		s.RecommendedMbps = run.Ramp.RecommendedMbps
	}
}

// fillEnvironment records SSID/band/channel from the first link samples.
func fillEnvironment(run *model.Run) {
	for _, side := range []string{telemetry.SideController, telemetry.SideServer} {
		ep := &run.Environment.Controller
		if side == telemetry.SideServer {
			ep = &run.Environment.Server
		}
		for i := range run.Telemetry {
			t := &run.Telemetry[i]
			if t.Kind != telemetry.KindLink || t.Side != side || t.FreqMHz == nil && t.Channel == nil {
				continue
			}
			if t.SSID != "" {
				ep.SSID = t.SSID
			}
			ep.Band = t.Band
			if t.Channel != nil {
				ep.Channel = *t.Channel
			}
			if t.WidthMHz != nil {
				ep.WidthMHz = *t.WidthMHz
			}
			break
		}
	}
}

// Diagnose applies the rules to every stall and ranks the verdicts by the
// share of stalls each explains.
func Diagnose(c *Ctx) []model.Verdict {
	run := c.Run
	total := len(run.Stalls)
	hits := make([][]Hit, len(Rules))
	for i := range run.Stalls {
		st := &run.Stalls[i]
		from, to := st.Start-WindowBefore, st.End+WindowAfter
		st.Nearby = nearby(c, st, from, to)
		st.Causes = nil
		for r := range Rules {
			if Rules[r].Stall == nil {
				continue
			}
			if ok, note, v := Rules[r].Stall(c, st, from, to); ok {
				hits[r] = append(hits[r], Hit{Stall: st, Note: note, Value: v})
				st.Causes = append(st.Causes, Rules[r].ID)
			}
		}
	}
	var out []model.Verdict
	for r := range Rules {
		rule := &Rules[r]
		if len(hits[r]) > 0 {
			out = append(out, model.Verdict{
				Rule: rule.ID, Cause: rule.Cause, Fix: rule.Fix,
				Explained: len(hits[r]), Total: total,
				Share:    float64(len(hits[r])) / float64(total),
				Evidence: rule.Evidence(c, hits[r], total),
			})
			continue
		}
		if rule.Run != nil {
			if ok, ev := rule.Run(c); ok {
				out = append(out, model.Verdict{Rule: rule.ID, Cause: rule.Cause, Fix: rule.Fix, Total: total, Evidence: ev, RunLevel: true})
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].RunLevel != out[j].RunLevel {
			return !out[i].RunLevel
		}
		return out[i].Explained > out[j].Explained
	})
	unexplained := 0
	for i := range run.Stalls {
		if len(run.Stalls[i].Causes) == 0 {
			unexplained++
		}
	}
	switch {
	case total == 0:
		// No stalls: say so first; run-level findings follow as notes.
		clean := model.Verdict{
			Rule: "clean", Cause: "No stalls detected",
			Fix: "Nothing to fix at this bitrate",
			Evidence: fmt.Sprintf("%d frames, %.2f%% late, %.3f%% packet loss, p99 frame delay %.1f ms",
				run.Summary.Frames, run.Summary.LatePct, run.Summary.LossPct, run.Summary.P99DelayMs),
		}
		out = append([]model.Verdict{clean}, out...)
	case unexplained > 0:
		v := model.Verdict{
			Rule: "unexplained", Cause: "Unexplained stalls",
			Fix:       "Run a longer soak, enable the Freebox module (--freebox) and check other devices on the network",
			Explained: unexplained, Total: total, Share: float64(unexplained) / float64(total),
			Evidence: fmt.Sprintf("%d of %d stalls had no matching telemetry event", unexplained, total),
		}
		if len(out) == 0 || out[0].RunLevel {
			out = append([]model.Verdict{v}, out...)
		} else {
			out = append(out, v)
		}
	}
	return out
}

func nearby(c *Ctx, st *model.Stall, from, to float64) []string {
	var out []string
	for _, x := range c.Tele.Range(from, to, func(s *telemetry.Sample) bool {
		return s.Kind == telemetry.KindEvent
	}) {
		side := "Deck"
		if x.s.Side == telemetry.SideServer {
			side = "host"
		}
		e := fmt.Sprintf("%s (%s) %+.1fs", x.s.Event, side, (x.t-st.Start)/1000)
		if !contains(out, e) {
			out = append(out, e)
		}
		if len(out) >= 8 {
			break
		}
	}
	return out
}
