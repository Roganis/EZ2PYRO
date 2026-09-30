package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/roganis/ez2pyro/internal/clock"
	"github.com/roganis/ez2pyro/internal/proto"
	"github.com/roganis/ez2pyro/internal/sender"
	"github.com/roganis/ez2pyro/internal/telemetry"
)

// ServerOptions configures `linkdoctor serve`.
type ServerOptions struct {
	Bind        string // address to bind ("" = all)
	ControlPort int
	DataPort    int
	ProbePort   int
	Iface       string // override the auto-detected interface
	WifiIface   string
	NoTelemetry bool
	Logf        func(format string, args ...any)
}

func (o *ServerOptions) defaults() {
	if o.ControlPort == 0 {
		o.ControlPort = proto.DefaultControlPort
	}
	if o.DataPort == 0 {
		o.DataPort = proto.DefaultDataPort
	}
	if o.ProbePort == 0 {
		o.ProbePort = proto.DefaultProbePort
	}
	if o.Logf == nil {
		o.Logf = log.Printf
	}
}

// Server waits for a controller and sends test traffic on request.
type Server struct {
	opt   ServerOptions
	ln    net.Listener
	data  *net.UDPConn
	probe *net.UDPConn

	mu     sync.Mutex
	punch  map[uint32]netip.AddrPort // session → receiver address
	busy   bool
	closed bool
}

// Listen opens the control, data and probe sockets.
func Listen(opt ServerOptions) (*Server, error) {
	opt.defaults()
	s := &Server{opt: opt, punch: map[uint32]netip.AddrPort{}}
	var err error
	if s.ln, err = net.Listen("tcp", net.JoinHostPort(opt.Bind, fmt.Sprint(opt.ControlPort))); err != nil {
		return nil, err
	}
	if s.data, err = listenUDP(opt.Bind, opt.DataPort); err != nil {
		s.ln.Close()
		return nil, err
	}
	_ = s.data.SetWriteBuffer(8 << 20)
	if s.probe, err = listenUDP(opt.Bind, opt.ProbePort); err != nil {
		s.ln.Close()
		s.data.Close()
		return nil, err
	}
	go s.dataLoop()
	go s.probeLoop()
	return s, nil
}

func listenUDP(host string, port int) (*net.UDPConn, error) {
	ua, err := net.ResolveUDPAddr("udp", net.JoinHostPort(host, fmt.Sprint(port)))
	if err != nil {
		return nil, err
	}
	return net.ListenUDP("udp", ua)
}

// Addr returns the control listener address.
func (s *Server) Addr() net.Addr { return s.ln.Addr() }

// Close stops the server.
func (s *Server) Close() error {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	s.ln.Close()
	s.data.Close()
	return s.probe.Close()
}

// Serve accepts controllers one at a time until ctx ends.
func (s *Server) Serve(ctx context.Context) error {
	go func() { <-ctx.Done(); s.Close() }()
	for {
		c, err := s.ln.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		s.mu.Lock()
		busy := s.busy
		if !busy {
			s.busy = true
		}
		s.mu.Unlock()
		if busy {
			_ = proto.NewCodec(c).Send(&proto.Msg{Type: proto.TypeError, Error: "server busy with another controller"})
			c.Close()
			continue
		}
		go func() {
			defer func() {
				s.mu.Lock()
				s.busy = false
				s.mu.Unlock()
			}()
			s.opt.Logf("controller connected from %s", c.RemoteAddr())
			err := s.handle(ctx, c)
			if err != nil && !errors.Is(err, io.EOF) {
				s.opt.Logf("session ended: %v", err)
			} else {
				s.opt.Logf("controller disconnected")
			}
		}()
	}
}

// dataLoop records hole-punch packets so we know where to send each session.
func (s *Server) dataLoop() {
	buf := make([]byte, 2048)
	var h proto.Header
	for {
		n, from, err := s.data.ReadFromUDPAddrPort(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		if h.Decode(buf[:n]) == nil && h.Flags&proto.FlagPunch != 0 {
			s.mu.Lock()
			s.punch[h.SessionID] = netip.AddrPortFrom(from.Addr().Unmap(), from.Port())
			s.mu.Unlock()
		}
	}
}

// probeLoop echoes probe pings with receive and send timestamps.
func (s *Server) probeLoop() {
	buf := make([]byte, 256)
	var p proto.Probe
	for {
		n, from, err := s.probe.ReadFromUDPAddrPort(buf)
		t2 := clock.Now()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		if p.Decode(buf[:n]) != nil || p.Reply {
			continue
		}
		p.Reply, p.T2, p.T3 = true, t2, clock.Now()
		p.Encode(buf)
		_, _ = s.probe.WriteToUDPAddrPort(buf[:proto.ProbeSize], from)
	}
}

func (s *Server) waitPunch(ctx context.Context, session uint32, timeout time.Duration) (netip.AddrPort, bool) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) && ctx.Err() == nil {
		s.mu.Lock()
		a, ok := s.punch[session]
		s.mu.Unlock()
		if ok {
			return a, true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return netip.AddrPort{}, false
}

func (s *Server) handle(parent context.Context, c net.Conn) error {
	defer c.Close()
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	codec := proto.NewCodec(c)

	hub := telemetry.NewHub(telemetry.SideServer)
	iface := s.opt.Iface
	if iface == "" {
		iface = telemetry.IfaceForAddr(localIP(c))
	}
	if !s.opt.NoTelemetry {
		hub.Start(telemetry.Available(telemetry.Options{Side: telemetry.SideServer, Iface: iface, WifiIface: s.opt.WifiIface}))
	}
	defer hub.Stop()
	hello := makeHello(telemetry.SideServer, localIP(c), hub.Names())
	if s.opt.Iface != "" {
		for i := range hello.Ifaces {
			hello.Ifaces[i].Used = hello.Ifaces[i].Name == s.opt.Iface
		}
	}
	if err := codec.Send(&proto.Msg{Type: proto.TypeHello, Hello: hello}); err != nil {
		return err
	}

	// Periodic telemetry and heartbeats.
	go func() {
		t := time.NewTicker(500 * time.Millisecond)
		defer t.Stop()
		n := 0
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			if smp := hub.Drain(); len(smp) > 0 {
				if codec.Send(&proto.Msg{Type: proto.TypeTelemetry, Telemetry: &proto.TelemetryBatch{Samples: smp}}) != nil {
					cancel()
					return
				}
			}
			if n++; n%2 == 0 {
				_ = codec.Send(&proto.Msg{Type: proto.TypeHeartbeat, Heartbeat: &proto.Heartbeat{Clock: clock.Now()}})
			}
		}
	}()

	// Watchdog: stop everything (including traffic) if the controller goes quiet.
	lastSeen := make(chan struct{}, 1)
	go func() {
		const quiet = 10 * time.Second
		t := time.NewTimer(quiet)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-lastSeen:
				if !t.Stop() {
					<-t.C
				}
				t.Reset(quiet)
			case <-t.C:
				s.opt.Logf("controller silent for %v, stopping", quiet)
				cancel()
				c.Close()
				return
			}
		}
	}()

	var (
		cfg      *proto.Config
		stopSend context.CancelFunc
		sendDone chan struct{}
	)
	defer func() {
		if stopSend != nil {
			stopSend()
			<-sendDone
		}
	}()
	for {
		m, err := codec.Recv()
		if err != nil {
			return err
		}
		select {
		case lastSeen <- struct{}{}:
		default:
		}
		switch m.Type {
		case proto.TypeHello:
			if m.Hello != nil {
				s.opt.Logf("controller %s (%s/%s, %s)", m.Hello.Hostname, m.Hello.OS, m.Hello.Arch, m.Hello.ToolVersion)
			}
		case proto.TypeConfig:
			if m.Config == nil {
				continue
			}
			if err := m.Config.Validate(); err != nil {
				_ = codec.Send(&proto.Msg{Type: proto.TypeError, Error: "invalid config: " + err.Error()})
				continue
			}
			cfg = m.Config
		case proto.TypeStart:
			if cfg == nil || m.Start == nil || m.Start.SessionID != cfg.SessionID {
				_ = codec.Send(&proto.Msg{Type: proto.TypeError, Error: "START without matching CONFIG"})
				continue
			}
			if stopSend != nil {
				stopSend()
				<-sendDone
			}
			dst, ok := s.waitPunch(ctx, cfg.SessionID, 3*time.Second)
			if !ok {
				_ = codec.Send(&proto.Msg{Type: proto.TypeError, Error: fmt.Sprintf(
					"no UDP packets from the controller reached port %d; allow linkdoctor through the host firewall (UDP %d and %d)",
					s.opt.DataPort, s.opt.DataPort, s.opt.ProbePort)})
				continue
			}
			sctx, cancelSend := context.WithCancel(ctx)
			stopSend = cancelSend
			sendDone = make(chan struct{})
			go func(cfg proto.Config, done chan struct{}) {
				defer close(done)
				s.opt.Logf("sending session %08x: %.0f Mbit/s @ %d fps to %s", cfg.SessionID, cfg.BitrateMbps, cfg.FPS, dst)
				st := sender.Run(sctx, s.data, dst, cfg, hub.Emit)
				_ = codec.Send(&proto.Msg{Type: proto.TypeResults, Results: &proto.Results{SessionID: cfg.SessionID, Sender: st}})
				s.mu.Lock()
				delete(s.punch, cfg.SessionID)
				s.mu.Unlock()
			}(*cfg, sendDone)
		case proto.TypeStop:
			if stopSend != nil {
				stopSend()
			}
		case proto.TypeHeartbeat:
		case proto.TypeError:
			s.opt.Logf("controller error: %s", m.Error)
		}
	}
}
