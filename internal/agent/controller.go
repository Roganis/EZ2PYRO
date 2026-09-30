package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/netip"
	"sort"
	"sync"
	"time"

	"github.com/roganis/ez2pyro/internal/clock"
	"github.com/roganis/ez2pyro/internal/proto"
	"github.com/roganis/ez2pyro/internal/receiver"
	"github.com/roganis/ez2pyro/internal/telemetry"
)

// ControllerOptions configures the controller side of `linkdoctor run`.
type ControllerOptions struct {
	Peer        string
	ControlPort int
	DataPort    int // server's data port; the receiver also tries to bind it locally
	ProbePort   int
	Iface       string
	WifiIface   string
	Raw         bool
	NoTelemetry bool
}

// Controller is a connected test session with a server.
type Controller struct {
	opt   ControllerOptions
	conn  net.Conn
	codec *proto.Codec

	PeerHello  *proto.Hello
	LocalHello *proto.Hello
	Recv       *receiver.Receiver

	est    *clock.OffsetEstimator
	hub    *telemetry.Hub
	probe  *net.UDPConn
	peerIP netip.Addr

	mu      sync.Mutex
	samples []telemetry.Sample // all telemetry on the controller clock (absolute ns)
	pending []telemetry.Sample // server samples waiting for a clock estimate
	results chan *proto.Results
	errs    chan error
	rtts    []float64 // RTTs (ms) of the current second
	probeTx int
	probeRx int
	lastRTT float64

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// Dial connects to a server, exchanges HELLO and starts probes, telemetry
// and the receiver.
func Dial(ctx context.Context, opt ControllerOptions) (*Controller, error) {
	if opt.ControlPort == 0 {
		opt.ControlPort = proto.DefaultControlPort
	}
	if opt.DataPort == 0 {
		opt.DataPort = proto.DefaultDataPort
	}
	if opt.ProbePort == 0 {
		opt.ProbePort = proto.DefaultProbePort
	}
	d := net.Dialer{Timeout: 5 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(opt.Peer, fmt.Sprint(opt.ControlPort)))
	if err != nil {
		return nil, fmt.Errorf("cannot reach linkdoctor serve on %s:%d: %w", opt.Peer, opt.ControlPort, err)
	}
	c := &Controller{
		opt: opt, conn: conn, codec: proto.NewCodec(conn),
		est:     clock.NewOffsetEstimator(300),
		results: make(chan *proto.Results, 4),
		errs:    make(chan error, 4),
	}
	c.ctx, c.cancel = context.WithCancel(context.Background())
	ra := conn.RemoteAddr().(*net.TCPAddr)
	c.peerIP, _ = netip.AddrFromSlice(ra.IP)
	c.peerIP = c.peerIP.Unmap()

	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	m, err := c.codec.Recv()
	_ = conn.SetReadDeadline(time.Time{})
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("no HELLO from server: %w", err)
	}
	if m.Type == proto.TypeError {
		conn.Close()
		return nil, errors.New("server: " + m.Error)
	}
	if m.Type != proto.TypeHello || m.Hello == nil {
		conn.Close()
		return nil, fmt.Errorf("expected HELLO, got %s", m.Type)
	}
	if m.Hello.ProtoVersion != int(proto.Version) {
		conn.Close()
		return nil, fmt.Errorf("protocol mismatch: server v%d, controller v%d — use the same linkdoctor version on both ends", m.Hello.ProtoVersion, proto.Version)
	}
	c.PeerHello = m.Hello

	// Receiver: prefer the well-known data port, fall back to any port.
	if c.Recv, err = receiver.Listen(fmt.Sprintf(":%d", opt.DataPort), opt.Raw); err != nil {
		if c.Recv, err = receiver.Listen(":0", opt.Raw); err != nil {
			conn.Close()
			return nil, err
		}
	}
	if c.probe, err = net.ListenUDP("udp", &net.UDPAddr{}); err != nil {
		c.Recv.Close()
		conn.Close()
		return nil, err
	}

	c.hub = telemetry.NewHub(telemetry.SideController)
	iface := opt.Iface
	if iface == "" {
		iface = telemetry.IfaceForAddr(localIP(conn))
	}
	if !opt.NoTelemetry {
		c.hub.Start(telemetry.Available(telemetry.Options{Side: telemetry.SideController, Iface: iface, WifiIface: opt.WifiIface}))
	}
	c.LocalHello = makeHello(telemetry.SideController, localIP(conn), c.hub.Names())
	if opt.Iface != "" {
		for i := range c.LocalHello.Ifaces {
			c.LocalHello.Ifaces[i].Used = c.LocalHello.Ifaces[i].Name == opt.Iface
		}
	}
	if err := c.codec.Send(&proto.Msg{Type: proto.TypeHello, Hello: c.LocalHello}); err != nil {
		c.Close()
		return nil, err
	}

	c.wg.Add(4)
	go c.readLoop()
	go c.heartbeatLoop()
	go c.probeSendLoop()
	go c.probeRecvLoop()
	// Give the probes a moment to produce a clock estimate.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := c.est.Offset(); ok {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	return c, nil
}

// Close ends the session.
func (c *Controller) Close() error {
	c.cancel()
	c.conn.Close()
	c.probe.Close()
	c.hub.Stop()
	c.wg.Wait()
	return c.Recv.Close()
}

// ClockOffset returns the estimated server−controller clock offset.
func (c *Controller) ClockOffset() (time.Duration, bool) {
	o, ok := c.est.Offset()
	return time.Duration(o), ok
}

// ToLocal maps a server timestamp onto the controller clock.
func (c *Controller) ToLocal(t int64) int64 { return c.est.ToLocal(t) }

func (c *Controller) readLoop() {
	defer c.wg.Done()
	for {
		m, err := c.codec.Recv()
		if err != nil {
			if c.ctx.Err() == nil {
				if errors.Is(err, io.EOF) {
					err = errors.New("server closed the control connection")
				}
				c.errs <- err
			}
			return
		}
		switch m.Type {
		case proto.TypeTelemetry:
			if m.Telemetry != nil {
				c.addServerSamples(m.Telemetry.Samples)
			}
		case proto.TypeResults:
			if m.Results != nil {
				c.results <- m.Results
			}
		case proto.TypeError:
			c.errs <- errors.New("server: " + m.Error)
		}
	}
}

func (c *Controller) addServerSamples(s []telemetry.Sample) {
	c.mu.Lock()
	defer c.mu.Unlock()
	off, ok := c.est.Offset()
	if !ok {
		c.pending = append(c.pending, s...)
		return
	}
	for _, p := range append(c.pending, s...) {
		p.T -= off
		p.Side = telemetry.SideServer
		c.samples = append(c.samples, p)
	}
	c.pending = nil
}

func (c *Controller) heartbeatLoop() {
	defer c.wg.Done()
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-t.C:
		}
		_ = c.codec.Send(&proto.Msg{Type: proto.TypeHeartbeat, Heartbeat: &proto.Heartbeat{Clock: clock.Now()}})
		// Move local telemetry into the store.
		loc := c.hub.Drain()
		c.mu.Lock()
		c.samples = append(c.samples, loc...)
		c.mu.Unlock()
	}
}

// probeSendLoop sends 10 Hz probes and emits a per-second RTT summary.
func (c *Controller) probeSendLoop() {
	defer c.wg.Done()
	dst := netip.AddrPortFrom(c.peerIP, uint16(c.opt.ProbePort))
	t := time.NewTicker(100 * time.Millisecond)
	defer t.Stop()
	buf := make([]byte, proto.ProbeSize)
	var seq uint32
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-t.C:
		}
		seq++
		p := proto.Probe{Seq: seq, T1: clock.Now()}
		p.Encode(buf)
		_, _ = c.probe.WriteToUDPAddrPort(buf, dst)
		c.mu.Lock()
		c.probeTx++
		if seq%10 == 0 {
			s := telemetry.Sample{T: clock.Now(), Side: telemetry.SideController, Kind: telemetry.KindRTT,
				ProbeLost: telemetry.Int(c.probeTx - c.probeRx)}
			if len(c.rtts) > 0 {
				sort.Float64s(c.rtts)
				s.RTTms = telemetry.F64(c.rtts[len(c.rtts)/2])
				s.RTTMaxms = telemetry.F64(c.rtts[len(c.rtts)-1])
				c.lastRTT = *s.RTTms
			}
			if *s.ProbeLost < 0 {
				*s.ProbeLost = 0
			}
			c.samples = append(c.samples, s)
			c.rtts, c.probeTx, c.probeRx = c.rtts[:0], 0, 0
		}
		c.mu.Unlock()
	}
}

func (c *Controller) probeRecvLoop() {
	defer c.wg.Done()
	buf := make([]byte, 256)
	var p proto.Probe
	for {
		n, _, err := c.probe.ReadFromUDPAddrPort(buf)
		t4 := clock.Now()
		if err != nil {
			if c.ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		if p.Decode(buf[:n]) != nil || !p.Reply {
			continue
		}
		ps := clock.ProbeSample{T1: p.T1, T2: p.T2, T3: p.T3, T4: t4}
		c.est.Add(ps)
		c.mu.Lock()
		c.probeRx++
		c.rtts = append(c.rtts, float64(ps.RTT())/1e6)
		c.mu.Unlock()
	}
}

// Telemetry returns all telemetry collected so far (controller clock, absolute ns).
func (c *Controller) Telemetry() []telemetry.Sample {
	loc := c.hub.Drain()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.samples = append(c.samples, loc...)
	out := append([]telemetry.Sample(nil), c.samples...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].T < out[j].T })
	return out
}

// LastRTT returns the median RTT (ms) of the last full second of probes.
func (c *Controller) LastRTT() float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastRTT
}

// Note records a controller-side event in the telemetry stream.
func (c *Controller) Note(s telemetry.Sample) { c.hub.Emit(s) }

// SegmentResult is the outcome of one traffic segment.
type SegmentResult struct {
	Sender             proto.SenderStats
	Receiver           receiver.Result
	StartCtrl, EndCtrl int64
}

// NewSessionID returns a random non-zero session id.
func NewSessionID() uint32 {
	for {
		if id := rand.Uint32(); id != 0 {
			return id
		}
	}
}

// RunSegment runs one traffic segment with cfg. It returns when the
// configured duration has elapsed, or — for cfg.DurationMs == 0 or when stop
// fires / ctx is cancelled — after sending STOP.
func (c *Controller) RunSegment(ctx context.Context, cfg proto.Config, stop <-chan struct{}, prior int64, budget time.Duration) (*SegmentResult, error) {
	if cfg.SessionID == 0 {
		cfg.SessionID = NewSessionID()
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	// Drain stale results / errors.
	for len(c.results) > 0 {
		<-c.results
	}
	c.Recv.Begin(cfg)
	if err := c.codec.Send(&proto.Msg{Type: proto.TypeConfig, Config: &cfg}); err != nil {
		return nil, err
	}
	// Hole-punch before and during the segment so the server learns our address.
	dataDst := netip.AddrPortFrom(c.peerIP, uint16(c.opt.DataPort))
	punchCtx, stopPunch := context.WithCancel(ctx)
	defer stopPunch()
	go func() {
		t := time.NewTicker(200 * time.Millisecond)
		defer t.Stop()
		for {
			_ = c.Recv.SendPunch(dataDst, cfg.SessionID)
			select {
			case <-punchCtx.Done():
				return
			case <-t.C:
			}
		}
	}()
	if err := c.codec.Send(&proto.Msg{Type: proto.TypeStart, Start: &proto.Start{SessionID: cfg.SessionID}}); err != nil {
		return nil, err
	}

	var timeout <-chan time.Time
	if cfg.DurationMs > 0 {
		timeout = time.After(time.Duration(cfg.DurationMs)*time.Millisecond + 10*time.Second)
	}
	sendStop := func() {
		_ = c.codec.Send(&proto.Msg{Type: proto.TypeStop, Stop: &proto.Stop{SessionID: cfg.SessionID}})
	}
	var res *proto.Results
	stopped := false
	for res == nil {
		select {
		case r := <-c.results:
			if r.SessionID == cfg.SessionID {
				res = r
			}
		case err := <-c.errs:
			sendStop()
			return nil, err
		case <-ctx.Done():
			if !stopped {
				sendStop()
				stopped = true
				timeout = time.After(5 * time.Second)
			}
			ctx = context.Background()
		case <-stop:
			if !stopped {
				sendStop()
				stopped = true
				timeout = time.After(5 * time.Second)
			}
			stop = nil
		case <-timeout:
			sendStop()
			return nil, errors.New("timed out waiting for sender RESULTS")
		}
	}
	stopPunch()
	time.Sleep(300 * time.Millisecond) // let in-flight packets land
	sr := &SegmentResult{
		Sender:    res.Sender,
		StartCtrl: c.est.ToLocal(res.Sender.StartNs),
		EndCtrl:   c.est.ToLocal(res.Sender.EndNs),
	}
	sr.Receiver = c.Recv.Finish(receiver.SenderInfo{
		FramesSent: res.Sender.FramesSent, StartCtrl: sr.StartCtrl, EndCtrl: sr.EndCtrl,
	}, prior, budget)
	return sr, nil
}

// Errors exposes asynchronous session errors (e.g. the server went away).
func (c *Controller) Errors() <-chan error { return c.errs }
