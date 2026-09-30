// Package analysis turns frame records and telemetry into stalls, a
// periodicity estimate and a ranked list of likely causes.
package analysis

import (
	"math"
	"sort"

	"github.com/roganis/ez2pyro/internal/model"
)

// MinLateRun is the number of consecutive late frames that makes a stall.
const MinLateRun = 3

// Relate recomputes Late for every frame with a new budget.
func Relate(frames []model.Frame, budgetMs float64) {
	for i := range frames {
		f := &frames[i]
		f.Late = !f.Complete || f.DelayMs > budgetMs
	}
}

type interval struct {
	start, end float64
	gap        bool
}

// DetectStalls finds stalls: MinLateRun or more consecutive late frames, or
// a no-packet gap longer than 50 ms. Overlapping or touching periods merge.
// intervalMs returns the frame interval of a segment.
func DetectStalls(frames []model.Frame, gaps []model.Gap, intervalMs func(seg int) float64) []model.Stall {
	var iv []interval
	run := 0
	for i := 0; i <= len(frames); i++ {
		if i < len(frames) && frames[i].Late && (run == 0 || frames[i].Segment == frames[i-1].Segment) {
			run++
			continue
		}
		if run >= MinLateRun {
			first, last := frames[i-run], frames[i-1]
			end := last.T + intervalMs(last.Segment)
			if last.LastMs > 0 && last.T+last.LastMs > end {
				end = last.T + last.LastMs
			}
			iv = append(iv, interval{start: first.T, end: end})
		}
		run = 0
		if i < len(frames) && frames[i].Late { // late frame starting a new segment
			run = 1
		}
	}
	for _, g := range gaps {
		iv = append(iv, interval{start: g.Start, end: g.End, gap: true})
	}
	if len(iv) == 0 {
		return nil
	}
	sort.Slice(iv, func(i, j int) bool { return iv[i].start < iv[j].start })
	merged := []interval{iv[0]}
	for _, x := range iv[1:] {
		m := &merged[len(merged)-1]
		if x.start <= m.end+1 {
			if x.end > m.end {
				m.end = x.end
			}
			m.gap = m.gap || x.gap
			continue
		}
		merged = append(merged, x)
	}

	stalls := make([]model.Stall, 0, len(merged))
	j := 0
	for idx, m := range merged {
		st := model.Stall{Index: idx + 1, Start: m.start, End: m.end, DurationMs: m.end - m.start, Gap: m.gap, WorstDelay: -1}
		for j < len(frames) && frames[j].T < m.start-intervalMs(frames[j].Segment) {
			j++
		}
		for k := j; k < len(frames) && frames[k].T <= m.end; k++ {
			f := &frames[k]
			if f.T < m.start-0.5 {
				continue
			}
			if st.BitrateMbps == 0 {
				st.BitrateMbps = f.BitrateMbps
			}
			if f.Late {
				st.LateFrames++
			}
			st.PacketsLost += f.Lost()
			if f.DelayMs > st.WorstDelay {
				st.WorstDelay = f.DelayMs
			}
		}
		stalls = append(stalls, st)
	}
	return stalls
}

// Periodicity checks whether stalls repeat at a regular interval, and
// autocorrelates the per-second late-frame series to catch fainter patterns.
func Periodicity(stalls []model.Stall, seconds []model.Second) *model.Periodicity {
	p := &model.Periodicity{}
	found := false
	if len(stalls) >= 4 {
		var iv []float64
		for i := 1; i < len(stalls); i++ {
			iv = append(iv, (stalls[i].Start-stalls[i-1].Start)/1000)
		}
		mean, std := meanStd(iv)
		p.Intervals = len(iv)
		p.PeriodS, p.StdS = mean, std
		if mean > 0 {
			p.CV = std / mean
			p.Periodic = p.CV < 0.15
		}
		found = true
	}
	if lag, r := autocorr(seconds); lag > 0 {
		p.AutoLagS, p.AutoCorr = lag, r
		found = true
	}
	if !found {
		return nil
	}
	return p
}

// autocorr returns the lag (s) with the strongest autocorrelation of the
// per-second late-frame count, if it is convincing (r ≥ 0.3, ≥ 3 repeats).
func autocorr(seconds []model.Second) (int, float64) {
	n := len(seconds)
	if n < 30 {
		return 0, 0
	}
	x := make([]float64, n)
	var mean float64
	for i, s := range seconds {
		x[i] = float64(s.Late)
		mean += x[i]
	}
	mean /= float64(n)
	var den float64
	for i := range x {
		x[i] -= mean
		den += x[i] * x[i]
	}
	if den == 0 {
		return 0, 0
	}
	bestLag, best := 0, 0.0
	for k := 5; k <= n/3; k++ {
		var num float64
		for i := 0; i+k < n; i++ {
			num += x[i] * x[i+k]
		}
		r := num / den
		if r > best+1e-9 {
			bestLag, best = k, r
		}
	}
	if best < 0.3 {
		return 0, 0
	}
	// Prefer the fundamental: if half the lag is almost as strong, use it.
	for bestLag >= 10 {
		h := bestLag / 2
		var num float64
		for i := 0; i+h < n; i++ {
			num += x[i] * x[i+h]
		}
		if num/den < 0.8*best {
			break
		}
		bestLag, best = h, num/den
	}
	return bestLag, math.Round(best*100) / 100
}

func meanStd(v []float64) (float64, float64) {
	if len(v) == 0 {
		return 0, 0
	}
	var m float64
	for _, x := range v {
		m += x
	}
	m /= float64(len(v))
	var s float64
	for _, x := range v {
		s += (x - m) * (x - m)
	}
	return m, math.Sqrt(s / float64(len(v)))
}

func percentile(v []float64, q float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	i := int(math.Ceil(q*float64(len(s)))) - 1
	if i < 0 {
		i = 0
	}
	return s[i]
}

func median(v []float64) float64 { return percentile(v, 0.5) }
