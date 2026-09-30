package receiver

import (
	"context"
	"net/netip"
	"testing"
	"time"

	"github.com/roganis/ez2pyro/internal/proto"
	"github.com/roganis/ez2pyro/internal/sender"
)

func testCfg() proto.Config {
	return proto.Config{SessionID: 5, BitrateMbps: 50, FPS: 60, BurstSpread: 0.3,
		PktSize: 1200, Profile: proto.ProfilePyrowave, Seed: 1}
}

// feed simulates a sender with clock offset off and per-frame extra delay.
func feed(r *Receiver, cfg proto.Config, frames int, off int64, extra func(id int) (delay int64, drop bool)) {
	sch := sender.NewScheduler(cfg, 1_000_000_000)
	var seq uint64
	for i := 0; i < frames; i++ {
		p := sch.Next()
		d, drop := extra(i)
		for j := 0; j < p.Packets; j++ {
			ts := p.Start + int64(j)*p.Gap
			if drop && j == p.Packets-1 {
				continue
			}
			h := proto.Header{SessionID: cfg.SessionID, Seq: seq, FrameID: p.FrameID,
				PktIdx: uint16(j), PktCount: uint16(p.Packets), FrameSendStart: p.Start, SendTS: ts}
			seq++
			r.handle(&h, cfg.PktSize, ts+off+2_000_000+d)
		}
	}
}

func newTestReceiver(cfg proto.Config) *Receiver {
	r := &Receiver{closed: make(chan struct{})}
	r.t0 = 1_000_000_000 + 7_000_000_000
	r.Begin(cfg)
	return r
}

func TestFramesDelayAndLoss(t *testing.T) {
	cfg := testCfg()
	r := newTestReceiver(cfg)
	const off = 7_000_000_000 // controller clock ahead of sender clock
	feed(r, cfg, 1200, off, func(id int) (int64, bool) {
		switch {
		case id >= 600 && id < 605:
			return 40_000_000, false // 40 ms queuing: late
		case id == 900:
			return 0, true // last packet lost: incomplete
		case id >= 300 && id < 310:
			return 5_000_000, false // 5 ms: within budget
		}
		return 0, false
	})
	res := r.Finish(SenderInfo{FramesSent: 1205}, 0, time.Second/60)
	if len(res.Frames) != 1205 {
		t.Fatalf("frames %d", len(res.Frames))
	}
	late := 0
	for i, f := range res.Frames {
		if f.Late {
			late++
		}
		want := (i >= 600 && i < 605) || i == 900 || i >= 1200
		if f.Late != want {
			t.Fatalf("frame %d late=%v delay=%.2f complete=%v", i, f.Late, f.DelayMs, f.Complete)
		}
	}
	if late != 11 {
		t.Fatalf("late %d", late)
	}
	if d := res.Frames[602].DelayMs; d < 39 || d > 41 {
		t.Fatalf("delay %.2f", d)
	}
	if d := res.Frames[305].DelayMs; d < 4.9 || d > 5.1 {
		t.Fatalf("delay %.2f", d)
	}
	if d := res.Frames[100].DelayMs; d > 0.01 || d < -0.01 {
		t.Fatalf("clean delay %.3f", d)
	}
	// Expected arrival time of frame 60 is ~1 s after run start (+2 ms base delay).
	if tt := res.Frames[60].T; tt < 1001 || tt > 1003 {
		t.Fatalf("T %.2f", tt)
	}
	// Tail frames: interpolated time, estimated packets.
	if f := res.Frames[1203]; f.Recv != 0 || f.Pkts == 0 || f.T < res.Frames[1199].T {
		t.Fatalf("tail frame %+v", f)
	}
}

func TestGapDetection(t *testing.T) {
	cfg := testCfg()
	// Frames 100–109 are lost entirely → ~167 ms of silence.
	r := newTestReceiver(cfg)
	sch := sender.NewScheduler(cfg, 1_000_000_000)
	for i := 0; i < 300; i++ {
		p := sch.Next()
		if i >= 100 && i < 110 {
			continue
		}
		for j := 0; j < p.Packets; j++ {
			ts := p.Start + int64(j)*p.Gap
			h := proto.Header{SessionID: cfg.SessionID, FrameID: p.FrameID, PktIdx: uint16(j),
				PktCount: uint16(p.Packets), FrameSendStart: p.Start, SendTS: ts}
			r.handle(&h, cfg.PktSize, ts+7_001_000_000)
		}
	}
	res := r.Finish(SenderInfo{FramesSent: 300}, 0, time.Second/60)
	if len(res.Gaps) != 1 {
		t.Fatalf("gaps %+v", res.Gaps)
	}
	g := res.Gaps[0]
	if dur := g.End - g.Start; dur < 150 || dur > 190 {
		t.Fatalf("gap duration %.1f ms", dur)
	}
	for i := 100; i < 110; i++ {
		if f := res.Frames[i]; !f.Late || f.Recv != 0 {
			t.Fatalf("frame %d %+v", i, f)
		}
	}
}

func TestLoopbackUDP(t *testing.T) {
	if testing.Short() {
		t.Skip("loopback traffic test")
	}
	r, err := Listen("127.0.0.1:0", false)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	cfg := testCfg()
	cfg.BitrateMbps = 100
	cfg.DurationMs = 2000
	r.Begin(cfg)
	w, err := Listen("127.0.0.1:0", false) // any UDP socket works as a writer
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	dst := netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(r.LocalPort()))
	st := sender.Run(context.Background(), w.Conn(), dst, cfg, nil)
	time.Sleep(100 * time.Millisecond)
	res := r.Finish(SenderInfo{FramesSent: st.FramesSent}, 0, time.Second/60)
	if uint64(len(res.Frames)) != uint64(st.FramesSent) {
		t.Fatalf("frames %d sent %d", len(res.Frames), st.FramesSent)
	}
	if res.Packets != st.PacketsSent {
		t.Logf("rcvbuf=%d", r.RcvBuf())
		t.Fatalf("received %d of %d packets", res.Packets, st.PacketsSent)
	}
}
