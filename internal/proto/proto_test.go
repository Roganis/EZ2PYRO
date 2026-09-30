package proto

import (
	"bytes"
	"testing"
)

func TestHeaderRoundTrip(t *testing.T) {
	h := Header{
		Flags: FlagLastPacket, SessionID: 0xdeadbeef, Seq: 1<<40 + 7, FrameID: 123456,
		PktIdx: 519, PktCount: 520, FrameSendStart: 987654321012, SendTS: 987654329999,
	}
	b := make([]byte, 1200)
	h.Encode(b)
	if b[0] != 0x57 || b[1] != 0x50 {
		t.Fatalf("magic not little-endian: % x", b[:2])
	}
	var g Header
	if err := g.Decode(b); err != nil {
		t.Fatal(err)
	}
	if g != h {
		t.Fatalf("got %+v want %+v", g, h)
	}
	if err := g.Decode(b[:HeaderSize-1]); err != ErrShort {
		t.Fatalf("short: %v", err)
	}
	b[0] = 0
	if err := g.Decode(b); err != ErrBadMagic {
		t.Fatalf("magic: %v", err)
	}
}

func TestProbeRoundTrip(t *testing.T) {
	p := Probe{Reply: true, Seq: 42, T1: 1, T2: -2, T3: 3}
	b := make([]byte, ProbeSize)
	p.Encode(b)
	var q Probe
	if err := q.Decode(b); err != nil {
		t.Fatal(err)
	}
	if p != q {
		t.Fatalf("got %+v want %+v", q, p)
	}
}

func TestCodec(t *testing.T) {
	var buf bytes.Buffer
	c := NewCodec(&buf)
	cfg := &Config{SessionID: 9, BitrateMbps: 150, FPS: 60, PktSize: 1200, Profile: ProfilePyrowave}
	if err := c.Send(&Msg{Type: TypeConfig, Config: cfg}); err != nil {
		t.Fatal(err)
	}
	if err := c.Send(&Msg{Type: TypeStop, Stop: &Stop{SessionID: 9}}); err != nil {
		t.Fatal(err)
	}
	m, err := c.Recv()
	if err != nil || m.Type != TypeConfig || *m.Config != *cfg {
		t.Fatalf("recv config: %+v %v", m, err)
	}
	m, err = c.Recv()
	if err != nil || m.Type != TypeStop || m.Stop.SessionID != 9 {
		t.Fatalf("recv stop: %+v %v", m, err)
	}
}

func TestConfigValidate(t *testing.T) {
	ok := Config{BitrateMbps: 150, FPS: 60, FrameJitter: .2, BurstSpread: .3, PktSize: 1200, Profile: ProfilePyrowave}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	bad := ok
	bad.PktSize = 1500
	if bad.Validate() == nil {
		t.Fatal("expected error for pkt size 1500")
	}
}
