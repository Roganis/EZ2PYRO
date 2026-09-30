// Package sender generates Pyrowave-shaped UDP traffic: one burst of packets
// per video frame, paced with a high-resolution clock, with overrun counting
// so that a slow host is never blamed on the network.
package sender

import (
	"context"
	"math"
	"math/rand"
	"net/netip"
	"sort"
	"time"

	"github.com/roganis/ez2pyro/internal/clock"
	"github.com/roganis/ez2pyro/internal/proto"
	"github.com/roganis/ez2pyro/internal/telemetry"
)

// OverrunThreshold is how late a packet may leave before it counts as an
// overrun (the sender itself fell behind).
const OverrunThreshold = int64(time.Millisecond)

// Writer is the subset of *net.UDPConn used by the sender.
type Writer interface {
	WriteToUDPAddrPort(b []byte, addr netip.AddrPort) (int, error)
}

// Plan describes the packet schedule of one frame.
type Plan struct {
	FrameID uint32
	Start   int64 // scheduled start, sender clock
	Packets int
	Gap     int64 // ns between packets within the burst
	Bytes   int
}

// Scheduler produces frame plans for a config. It is deterministic for a
// given seed so that tests and re-runs are reproducible.
type Scheduler struct {
	cfg        proto.Config
	rng        *rand.Rand
	interval   float64
	frameBytes float64
	spread     float64
	start      int64
	next       uint32
}

// NewScheduler creates a scheduler whose first frame starts at start.
func NewScheduler(cfg proto.Config, start int64) *Scheduler {
	s := &Scheduler{
		cfg:        cfg,
		rng:        rand.New(rand.NewSource(cfg.Seed)),
		interval:   1e9 / float64(cfg.FPS),
		frameBytes: cfg.BitrateMbps * 1e6 / float64(cfg.FPS) / 8,
		spread:     cfg.BurstSpread,
		start:      start,
	}
	if cfg.Profile == proto.ProfileEven {
		s.spread = 1
	}
	return s
}

// Interval returns the frame interval in ns.
func (s *Scheduler) Interval() int64 { return int64(s.interval) }

// Next returns the plan for the next frame.
func (s *Scheduler) Next() Plan {
	id := s.next
	s.next++
	size := s.frameBytes
	if s.cfg.FrameJitter > 0 && s.cfg.Profile != proto.ProfileEven {
		size *= 1 + s.cfg.FrameJitter*(2*s.rng.Float64()-1)
	}
	n := int(math.Ceil(size / float64(s.cfg.PktSize)))
	if n < 1 {
		n = 1
	}
	if n > math.MaxUint16 {
		n = math.MaxUint16
	}
	gap := int64(0)
	if n > 1 {
		gap = int64(s.spread * s.interval / float64(n))
	}
	return Plan{
		FrameID: id,
		Start:   s.start + int64(float64(id)*s.interval),
		Packets: n,
		Gap:     gap,
		Bytes:   n * s.cfg.PktSize,
	}
}

// Run sends frames to dst until the configured duration elapses or ctx is
// cancelled. tick, if non-nil, receives one KindSender sample per second.
func Run(ctx context.Context, w Writer, dst netip.AddrPort, cfg proto.Config, tick func(telemetry.Sample)) proto.SenderStats {
	start := clock.Now() + int64(5*time.Millisecond)
	sch := NewScheduler(cfg, start)
	end := int64(math.MaxInt64)
	if cfg.DurationMs > 0 {
		end = start + cfg.DurationMs*int64(time.Millisecond)
	}

	buf := make([]byte, cfg.PktSize)
	for i := proto.HeaderSize; i < len(buf); i++ {
		buf[i] = byte(i) // non-zero padding; avoids compression on odd links
	}
	st := proto.SenderStats{TargetMbps: cfg.BitrateMbps, StartNs: start}
	var jitters []float64

	// Per-second accumulators for the telemetry tick.
	secStart := start
	var secBytes, secOverruns uint64
	var secMaxLate int64
	var secJitter []float64
	flush := func(now int64) {
		if tick == nil {
			return
		}
		el := float64(now-secStart) / 1e9
		if el <= 0 {
			return
		}
		s := telemetry.Sample{
			T: now, Side: telemetry.SideServer, Kind: telemetry.KindSender,
			Overruns:  telemetry.U64(secOverruns),
			SendMbps:  telemetry.F64(float64(secBytes) * 8 / el / 1e6),
			MaxLateUs: telemetry.F64(float64(secMaxLate) / 1e3),
		}
		if len(secJitter) > 0 {
			s.FrameJitterUs = telemetry.F64(percentile(secJitter, 0.99))
		}
		tick(s)
		secStart, secBytes, secOverruns, secMaxLate, secJitter = now, 0, 0, 0, secJitter[:0]
	}

	var h proto.Header
	h.SessionID = cfg.SessionID
	done := ctx.Done()
	var frames uint32
loop:
	for {
		p := sch.Next()
		if p.Start >= end {
			break
		}
		select {
		case <-done:
			break loop
		default:
		}
		h.FrameID = p.FrameID
		h.PktCount = uint16(p.Packets)
		for j := 0; j < p.Packets; j++ {
			due := p.Start + int64(j)*p.Gap
			clock.SleepUntil(due)
			now := clock.Now()
			if now-due > secMaxLate {
				secMaxLate = now - due
			}
			if now-due > OverrunThreshold {
				st.Overruns++
				secOverruns++
			}
			if j == 0 {
				h.FrameSendStart = now
				jit := float64(now-p.Start) / 1e3
				jitters = append(jitters, jit)
				secJitter = append(secJitter, jit)
			}
			h.Flags = 0
			if j == p.Packets-1 {
				h.Flags = proto.FlagLastPacket
			}
			h.PktIdx = uint16(j)
			h.Seq = st.PacketsSent
			h.SendTS = now
			h.Encode(buf)
			if _, err := w.WriteToUDPAddrPort(buf, dst); err != nil {
				st.SendErrors++
			}
			st.PacketsSent++
			st.BytesSent += uint64(len(buf))
			secBytes += uint64(len(buf))
		}
		frames++
		if now := clock.Now(); now-secStart >= int64(time.Second) {
			flush(now)
		}
	}
	st.EndNs = clock.Now()
	flush(st.EndNs)
	st.FramesSent = frames
	if frames > 0 {
		dur := float64(frames) * sch.interval / 1e9
		st.AchievedMbps = float64(st.BytesSent) * 8 / dur / 1e6
	}
	if len(jitters) > 0 {
		st.FrameJitterP50 = percentile(jitters, 0.5)
		st.FrameJitterP99 = percentile(jitters, 0.99)
		st.FrameJitterMax = percentile(jitters, 1)
	}
	return st
}

func percentile(v []float64, q float64) float64 {
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	i := int(math.Ceil(q*float64(len(s)))) - 1
	if i < 0 {
		i = 0
	}
	return s[i]
}
