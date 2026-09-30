// Package clock provides a process-local monotonic nanosecond clock, precise
// sleeping (coarse sleep followed by a short spin-wait) and clock-offset
// estimation from probe pings.
package clock

import (
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

var base = time.Now()

// Now returns monotonic nanoseconds since process start. Values from two
// machines are not comparable without an offset estimate.
func Now() int64 { return int64(time.Since(base)) }

// BaseSpinMargin is the minimum time before a deadline at which SleepUntil
// stops sleeping and starts spinning. OS timers on Windows are only accurate
// to about 1 ms, so a wider margin is used there.
var BaseSpinMargin = func() time.Duration {
	if runtime.GOOS == "windows" {
		return 1500 * time.Microsecond
	}
	return 200 * time.Microsecond
}()

// overshoot tracks the ~95th percentile of how late time.Sleep wakes up, so
// the spin margin adapts to the machine (VMs and busy hosts oversleep more).
var overshoot atomic.Int64

const maxSpinMargin = 3 * time.Millisecond

// SpinMargin returns the current spin-wait margin.
func SpinMargin() time.Duration {
	m := time.Duration(overshoot.Load()) + 100*time.Microsecond
	if m < BaseSpinMargin {
		m = BaseSpinMargin
	}
	if m > maxSpinMargin {
		m = maxSpinMargin
	}
	return m
}

func noteOvershoot(o int64) {
	// Stochastic p95 estimator: step up by 19 units when above, down by 1 below.
	const unit = int64(2 * time.Microsecond)
	q := overshoot.Load()
	if o > q {
		q += 19 * unit
	} else if q >= unit {
		q -= unit
	}
	overshoot.Store(q)
}

// SleepUntil blocks until Now() >= target. It sleeps while the deadline is
// more than SpinMargin away, then spin-waits the rest.
func SleepUntil(target int64) {
	for {
		d := time.Duration(target - Now())
		if d <= 0 {
			return
		}
		if m := SpinMargin(); d > m {
			want := Now() + int64(d-m)
			time.Sleep(d - m)
			noteOvershoot(Now() - want)
			continue
		}
		for Now() < target {
			// Spin. Gosched keeps other goroutines responsive on small GOMAXPROCS.
			if runtime.GOMAXPROCS(0) < 2 {
				runtime.Gosched()
			}
		}
		return
	}
}

// ProbeSample is one completed probe exchange (NTP-style timestamps).
// T1/T4 are on the local clock, T2/T3 on the peer clock.
type ProbeSample struct {
	T1, T2, T3, T4 int64
}

// RTT returns the network round trip, excluding peer processing time.
func (p ProbeSample) RTT() int64 { return (p.T4 - p.T1) - (p.T3 - p.T2) }

// Offset returns peer clock minus local clock, assuming symmetric paths.
func (p ProbeSample) Offset() int64 { return ((p.T2 - p.T1) + (p.T3 - p.T4)) / 2 }

// OffsetEstimator keeps the recent probe samples and estimates the peer clock
// offset from the lowest-RTT ones, which suffer least from queuing asymmetry.
type OffsetEstimator struct {
	mu      sync.Mutex
	samples []ProbeSample
	max     int
}

// NewOffsetEstimator keeps up to window samples (e.g. 300 = 30 s at 10 Hz).
func NewOffsetEstimator(window int) *OffsetEstimator {
	if window < 1 {
		window = 1
	}
	return &OffsetEstimator{max: window}
}

// Add records a probe sample.
func (e *OffsetEstimator) Add(p ProbeSample) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.samples = append(e.samples, p)
	if len(e.samples) > e.max {
		e.samples = e.samples[len(e.samples)-e.max:]
	}
}

// Offset returns the estimated peer−local clock offset and whether any
// samples exist. It averages the offsets of the fastest 10% of probes.
func (e *OffsetEstimator) Offset() (int64, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.samples) == 0 {
		return 0, false
	}
	s := append([]ProbeSample(nil), e.samples...)
	sort.Slice(s, func(i, j int) bool { return s[i].RTT() < s[j].RTT() })
	n := len(s) / 10
	if n < 1 {
		n = 1
	}
	var sum int64
	for _, p := range s[:n] {
		sum += p.Offset()
	}
	return sum / int64(n), true
}

// ToLocal converts a peer timestamp to the local clock.
func (e *OffsetEstimator) ToLocal(peerT int64) int64 {
	off, _ := e.Offset()
	return peerT - off
}
