package agent

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/roganis/ez2pyro/internal/analysis"
	"github.com/roganis/ez2pyro/internal/clock"
	"github.com/roganis/ez2pyro/internal/model"
	"github.com/roganis/ez2pyro/internal/proto"
)

// impairProxy sits between the controller and the server's data port, like
// `tc qdisc netem` would: it forwards punches upstream and data downstream,
// delaying or dropping data packets during configured windows.
type impairProxy struct {
	conn    *net.UDPConn // controller-facing and server-facing (one socket)
	server  netip.AddrPort
	mu      sync.Mutex
	client  netip.AddrPort
	start   time.Time
	windows []impairment
}

type impairment struct {
	from, to time.Duration // since the first data packet
	delay    time.Duration
	drop     bool
}

func newProxy(t *testing.T, server netip.AddrPort, w []impairment) *impairProxy {
	c, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	_ = c.SetReadBuffer(16 << 20)
	p := &impairProxy{conn: c, server: server, windows: w}
	go p.loop()
	return p
}

func (p *impairProxy) port() int { return p.conn.LocalAddr().(*net.UDPAddr).Port }

func (p *impairProxy) loop() {
	buf := make([]byte, 2048)
	var h proto.Header
	for {
		n, from, err := p.conn.ReadFromUDPAddrPort(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		if from == p.server {
			p.mu.Lock()
			dst := p.client
			if p.start.IsZero() {
				p.start = time.Now()
			}
			el := time.Since(p.start)
			p.mu.Unlock()
			var imp *impairment
			for i := range p.windows {
				if el >= p.windows[i].from && el < p.windows[i].to {
					imp = &p.windows[i]
				}
			}
			switch {
			case imp == nil:
				_, _ = p.conn.WriteToUDPAddrPort(buf[:n], dst)
			case imp.drop:
			default:
				b := append([]byte(nil), buf[:n]...)
				time.AfterFunc(imp.delay, func() { _, _ = p.conn.WriteToUDPAddrPort(b, dst) })
			}
			continue
		}
		if h.Decode(buf[:n]) == nil && h.Flags&proto.FlagPunch != 0 {
			p.mu.Lock()
			p.client = from
			p.mu.Unlock()
			_, _ = p.conn.WriteToUDPAddrPort(buf[:n], p.server)
		}
	}
}

func freePort(t *testing.T) int {
	l, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.LocalAddr().(*net.UDPAddr).Port
}

func freeTCPPort(t *testing.T) int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// runImpaired runs one soak segment through the proxy and analyses it.
func runImpaired(t *testing.T, dur time.Duration, w []impairment) *model.Run {
	t.Helper()
	srv, err := Listen(ServerOptions{Bind: "127.0.0.1", ControlPort: freeTCPPort(t), DataPort: freePort(t), ProbePort: freePort(t),
		NoTelemetry: true, Logf: func(string, ...any) {}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go srv.Serve(ctx)
	ctrlPort := srv.Addr().(*net.TCPAddr).Port
	dataPort := srv.data.LocalAddr().(*net.UDPAddr).Port
	probePort := srv.probe.LocalAddr().(*net.UDPAddr).Port
	px := newProxy(t, netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(dataPort)), w)
	defer px.conn.Close()

	ctl, err := Dial(ctx, ControllerOptions{Peer: "127.0.0.1", ControlPort: ctrlPort, DataPort: px.port(), ProbePort: probePort, NoTelemetry: true})
	if err != nil {
		t.Fatal(err)
	}
	defer ctl.Close()
	t0 := clock.Now()
	ctl.Recv.SetT0(t0)
	cfg := proto.Config{SessionID: NewSessionID(), BitrateMbps: 80, FPS: 60, FrameJitter: 0.2, BurstSpread: 0.3,
		PktSize: 1200, Profile: proto.ProfilePyrowave, DurationMs: dur.Milliseconds(), Seed: 3}
	budget := time.Second / 60
	res, err := ctl.RunSegment(ctx, cfg, nil, 0, budget)
	if err != nil {
		t.Fatal(err)
	}
	run := &model.Run{
		Options:  model.Options{Mode: "soak", FPS: 60, PktSize: 1200, BudgetMs: 1000.0 / 60, BitrateMbps: 80},
		Segments: []model.Segment{{Config: cfg, Sender: res.Sender}},
		Frames:   res.Receiver.Frames, Gaps: res.Receiver.Gaps,
	}
	for _, s := range ctl.Telemetry() {
		s.T -= t0
		run.Telemetry = append(run.Telemetry, s)
	}
	analysis.Analyze(run)
	t.Logf("frames %d late %.2f%% loss %.3f%% overruns %d stalls %d", run.Summary.Frames, run.Summary.LatePct,
		run.Summary.LossPct, res.Sender.Overruns, len(run.Stalls))
	for _, s := range run.Stalls {
		t.Logf("  stall at %.0f ms, %.0f ms long, worst %.1f ms, lost %d", s.Start, s.DurationMs, s.WorstDelay, s.PacketsLost)
	}
	return run
}

// networkStalls drops stalls the analysis attributes to the sender falling
// behind: on a loaded test machine the in-process sender can pause, and the
// tool is right to blame the host for that rather than the network.
func networkStalls(run *model.Run) []model.Stall {
	var out []model.Stall
	for _, s := range run.Stalls {
		host := false
		for _, c := range s.Causes {
			host = host || c == "host"
		}
		if !host {
			out = append(out, s)
		}
	}
	return out
}

// TestInjectedStalls is the milestone-3 check without netem: delay and loss
// bursts injected by a proxy must each show up as one stall, and nothing else.
func TestInjectedStalls(t *testing.T) {
	if testing.Short() {
		t.Skip("network integration test")
	}
	run := runImpaired(t, 14*time.Second, []impairment{
		{from: 4 * time.Second, to: 4*time.Second + 300*time.Millisecond, delay: 80 * time.Millisecond},
		{from: 9 * time.Second, to: 9*time.Second + 250*time.Millisecond, drop: true},
	})
	// Both injected bursts must be found at the right time; any other stall
	// must be one the analysis attributes to the (loaded) test host.
	near := func(ms, want float64) bool { return ms > want-600 && ms < want+600 }
	var delay, loss *model.Stall
	for i := range run.Stalls {
		s := &run.Stalls[i]
		switch {
		case near(s.Start, 4000):
			delay = s
		case near(s.Start, 9000):
			loss = s
		}
	}
	if delay == nil || delay.WorstDelay < 60 {
		t.Errorf("delay burst not detected correctly: %+v", delay)
	}
	if loss == nil || loss.PacketsLost == 0 || !loss.Gap {
		t.Errorf("loss burst not detected correctly: %+v", loss)
	}
	for _, s := range networkStalls(run) {
		if !near(s.Start, 4000) && !near(s.Start, 9000) {
			t.Errorf("false stall at %.0f ms: %+v", s.Start, s)
		}
	}
}

func TestCleanRunHasNoStalls(t *testing.T) {
	if testing.Short() {
		t.Skip("network integration test")
	}
	run := runImpaired(t, 8*time.Second, nil)
	if st := networkStalls(run); len(st) != 0 || run.Summary.LossPct > 0 {
		t.Fatalf("clean run: %d network stalls, %.3f%% loss", len(st), run.Summary.LossPct)
	}
	if len(run.Stalls) == 0 && run.Verdicts[0].Rule != "clean" {
		t.Fatalf("verdict %s", run.Verdicts[0].Rule)
	}
}
