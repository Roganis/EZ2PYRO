package analysis

import (
	"sort"

	"github.com/roganis/ez2pyro/internal/telemetry"
)

// Tele indexes telemetry samples (times in ms relative to run start).
type Tele struct {
	all []tsample
}

type tsample struct {
	t float64
	s *telemetry.Sample
}

// NewTele indexes samples whose T is in ns relative to run start.
func NewTele(samples []telemetry.Sample) *Tele {
	t := &Tele{}
	for i := range samples {
		t.all = append(t.all, tsample{float64(samples[i].T) / 1e6, &samples[i]})
	}
	sort.SliceStable(t.all, func(i, j int) bool { return t.all[i].t < t.all[j].t })
	return t
}

// Range returns samples with from ≤ t ≤ to that satisfy keep.
func (t *Tele) Range(from, to float64, keep func(*telemetry.Sample) bool) []tsample {
	i := sort.Search(len(t.all), func(i int) bool { return t.all[i].t >= from })
	var out []tsample
	for ; i < len(t.all) && t.all[i].t <= to; i++ {
		if keep == nil || keep(t.all[i].s) {
			out = append(out, t.all[i])
		}
	}
	return out
}

// Last returns the latest sample at or before t (within maxAge ms) that
// satisfies keep.
func (t *Tele) Last(at, maxAge float64, keep func(*telemetry.Sample) bool) *telemetry.Sample {
	i := sort.Search(len(t.all), func(i int) bool { return t.all[i].t > at })
	for i--; i >= 0 && t.all[i].t >= at-maxAge; i-- {
		if keep(t.all[i].s) {
			return t.all[i].s
		}
	}
	return nil
}

// Values extracts a numeric field from samples in [from, to].
func (t *Tele) Values(from, to float64, keep func(*telemetry.Sample) bool, get func(*telemetry.Sample) (float64, bool)) []float64 {
	var out []float64
	for _, x := range t.Range(from, to, keep) {
		if v, ok := get(x.s); ok {
			out = append(out, v)
		}
	}
	return out
}

// Selectors.

func kind(k, side string) func(*telemetry.Sample) bool {
	return func(s *telemetry.Sample) bool { return s.Kind == k && (side == "" || s.Side == side) }
}

func events(names ...string) func(*telemetry.Sample) bool {
	return func(s *telemetry.Sample) bool {
		if s.Kind != telemetry.KindEvent {
			return false
		}
		for _, n := range names {
			if s.Event == n {
				return true
			}
		}
		return false
	}
}

func signal(s *telemetry.Sample) (float64, bool) {
	if s.SignalDBm == nil || *s.SignalDBm == 0 {
		return 0, false
	}
	return float64(*s.SignalDBm), true
}

// phyRate returns the PHY rate relevant to host→Deck traffic: the Deck's RX
// bitrate when known, else its TX bitrate.
func phyRate(s *telemetry.Sample) (float64, bool) {
	if s.RxBitrateMbps != nil && *s.RxBitrateMbps > 0 {
		return *s.RxBitrateMbps, true
	}
	if s.TxBitrateMbps != nil && *s.TxBitrateMbps > 0 {
		return *s.TxBitrateMbps, true
	}
	return 0, false
}

// counterRates converts a cumulative counter into per-second rates between
// consecutive samples; out[i] is the rate ending at times[i].
func counterRates(xs []tsample, get func(*telemetry.Sample) *uint64) (times, rates []float64) {
	var pt float64
	var pv uint64
	have := false
	for _, x := range xs {
		p := get(x.s)
		if p == nil {
			continue
		}
		if have && x.t > pt && *p >= pv {
			times = append(times, x.t)
			rates = append(rates, float64(*p-pv)/((x.t-pt)/1000))
		}
		pt, pv, have = x.t, *p, true
	}
	return
}
