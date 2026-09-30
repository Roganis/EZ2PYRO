package sender

import (
	"context"
	"math"
	"net/netip"
	"sync"
	"testing"

	"github.com/roganis/ez2pyro/internal/proto"
	"github.com/roganis/ez2pyro/internal/telemetry"
)

func cfg(mbps float64) proto.Config {
	return proto.Config{
		SessionID: 1, BitrateMbps: mbps, FPS: 60, FrameJitter: 0.2, BurstSpread: 0.3,
		PktSize: 1200, Profile: proto.ProfilePyrowave, Seed: 7,
	}
}

func TestSchedulerFrameSize(t *testing.T) {
	s := NewScheduler(cfg(300), 0)
	var bytes int
	const n = 6000
	for i := 0; i < n; i++ {
		p := s.Next()
		if p.Start != int64(float64(i)*1e9/60) {
			t.Fatalf("frame %d start %d", i, p.Start)
		}
		if p.Gap*int64(p.Packets) > int64(0.3*1e9/60)+1 {
			t.Fatalf("burst longer than spread: gap %d × %d", p.Gap, p.Packets)
		}
		bytes += p.Bytes
	}
	// Average of ±20% jitter is ~0, plus up to one packet of rounding.
	mbps := float64(bytes) * 8 / (n / 60.0) / 1e6
	if math.Abs(mbps-300)/300 > 0.02 {
		t.Fatalf("average bitrate %.1f", mbps)
	}
}

func TestSchedulerEvenProfile(t *testing.T) {
	c := cfg(120)
	c.Profile = proto.ProfileEven
	s := NewScheduler(c, 0)
	p := s.Next()
	want := int64(1e9 / 60 / float64(p.Packets))
	if p.Gap < want-1 || p.Gap > want+1 {
		t.Fatalf("even gap %d want %d", p.Gap, want)
	}
}

type countWriter struct {
	mu      sync.Mutex
	packets int
	last    proto.Header
}

func (w *countWriter) WriteToUDPAddrPort(b []byte, _ netip.AddrPort) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.packets++
	_ = w.last.Decode(b)
	return len(b), nil
}

func TestRunPacing(t *testing.T) {
	c := cfg(100)
	c.DurationMs = 2000
	w := &countWriter{}
	var ticks []telemetry.Sample
	st := Run(context.Background(), w, netip.MustParseAddrPort("127.0.0.1:1"), c, func(s telemetry.Sample) {
		ticks = append(ticks, s)
	})
	if st.FramesSent != 120 {
		t.Fatalf("frames %d", st.FramesSent)
	}
	if uint64(w.packets) != st.PacketsSent {
		t.Fatalf("packets %d vs %d", w.packets, st.PacketsSent)
	}
	if math.Abs(st.AchievedMbps-100)/100 > 0.03 {
		t.Fatalf("achieved %.1f Mbit/s", st.AchievedMbps)
	}
	if w.last.Flags&proto.FlagLastPacket == 0 || w.last.FrameID != 119 {
		t.Fatalf("last header %+v", w.last)
	}
	if len(ticks) < 2 {
		t.Fatalf("ticks %d", len(ticks))
	}
	t.Logf("overruns=%d jitter p50=%.0fµs p99=%.0fµs max=%.0fµs",
		st.Overruns, st.FrameJitterP50, st.FrameJitterP99, st.FrameJitterMax)
}
