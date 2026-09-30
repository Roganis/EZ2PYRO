package clock

import (
	"sort"
	"testing"
	"time"
)

func TestSleepUntilPrecision(t *testing.T) {
	var late []time.Duration
	for i := 0; i < 50; i++ {
		target := Now() + int64(3*time.Millisecond)
		SleepUntil(target)
		l := time.Duration(Now() - target)
		if l < 0 {
			t.Fatalf("woke early by %v", -l)
		}
		late = append(late, l)
	}
	sort.Slice(late, func(i, j int) bool { return late[i] < late[j] })
	p50 := late[len(late)/2]
	t.Logf("lateness p50 %v p90 %v max %v", p50, late[len(late)*9/10], late[len(late)-1])
	// On an idle machine the spin-wait lands within a few µs. This is only a
	// sanity bound: shared CI machines running other tests preempt the spin.
	// Real pacing is checked on the host (sender overrun counter, jitter stats).
	if p50 > time.Millisecond {
		t.Fatalf("median lateness %v", p50)
	}
}

func TestOffsetEstimator(t *testing.T) {
	e := NewOffsetEstimator(100)
	const offset = int64(5_000_000_000) // peer clock is 5 s ahead
	for i := 0; i < 100; i++ {
		t1 := int64(i) * 100_000_000
		owdUp := int64(1_000_000)   // 1 ms
		owdDown := int64(1_000_000) // 1 ms
		if i%3 == 0 {               // queued, asymmetric samples
			owdUp += 20_000_000
		}
		t2 := t1 + owdUp + offset
		t3 := t2 + 50_000
		t4 := t3 - offset + owdDown
		e.Add(ProbeSample{t1, t2, t3, t4})
	}
	got, ok := e.Offset()
	if !ok {
		t.Fatal("no estimate")
	}
	if d := got - offset; d > 100_000 || d < -100_000 {
		t.Fatalf("offset error %d ns", d)
	}
	if e.ToLocal(offset+42) != 42 {
		t.Fatalf("ToLocal wrong: %d", e.ToLocal(offset+42))
	}
}
