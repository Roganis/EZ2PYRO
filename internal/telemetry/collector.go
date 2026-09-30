package telemetry

import (
	"context"
	"net"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/roganis/ez2pyro/internal/clock"
)

// Collector produces telemetry samples until ctx is cancelled.
type Collector interface {
	Name() string
	Run(ctx context.Context, emit func(Sample)) error
}

// Options selects what to collect.
type Options struct {
	Side      string // SideController or SideServer
	Iface     string // interface carrying the test traffic ("" = auto)
	WifiIface string // Wi-Fi interface to watch ("" = auto)
}

// Hub runs collectors and buffers their samples.
type Hub struct {
	mu     sync.Mutex
	buf    []Sample
	names  []string
	cancel context.CancelFunc
	wg     sync.WaitGroup
	side   string
}

// NewHub creates a hub for the given side.
func NewHub(side string) *Hub { return &Hub{side: side} }

// Emit adds a sample, stamping side and time if missing.
func (h *Hub) Emit(s Sample) {
	if s.T == 0 {
		s.T = clock.Now()
	}
	if s.Side == "" {
		s.Side = h.side
	}
	h.mu.Lock()
	h.buf = append(h.buf, s)
	h.mu.Unlock()
}

// Start runs the collectors in the background.
func (h *Hub) Start(cols []Collector) {
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	for _, c := range cols {
		h.names = append(h.names, c.Name())
		h.wg.Add(1)
		go func(c Collector) {
			defer h.wg.Done()
			if err := c.Run(ctx, h.Emit); err != nil && ctx.Err() == nil {
				h.Emit(Sample{Kind: KindInfo, Detail: c.Name() + ": " + err.Error()})
			}
		}(c)
	}
}

// Names lists the running collectors.
func (h *Hub) Names() []string { return h.names }

// Drain returns and clears the buffered samples.
func (h *Hub) Drain() []Sample {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := h.buf
	h.buf = nil
	return out
}

// Stop stops all collectors and waits for them.
func (h *Hub) Stop() {
	if h.cancel != nil {
		h.cancel()
	}
	h.wg.Wait()
}

// Every calls fn immediately and then at the given interval until ctx ends.
func Every(ctx context.Context, d time.Duration, fn func()) error {
	t := time.NewTicker(d)
	defer t.Stop()
	for {
		fn()
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// IfaceForAddr returns the name of the interface that owns ip.
func IfaceForAddr(ip net.IP) string {
	ifs, err := net.Interfaces()
	if err != nil {
		return ""
	}
	for _, ifc := range ifs {
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok && n.IP.Equal(ip) {
				return ifc.Name
			}
		}
	}
	return ""
}

// IfaceKind guesses whether an interface is Wi-Fi, Ethernet or other.
func IfaceKind(name string) string {
	if runtime.GOOS == "linux" {
		if _, err := os.Stat("/sys/class/net/" + name + "/wireless"); err == nil {
			return "wifi"
		}
		if _, err := os.Stat("/sys/class/net/" + name + "/phy80211"); err == nil {
			return "wifi"
		}
		if _, err := os.Stat("/sys/class/net/" + name + "/device"); err == nil {
			return "ethernet"
		}
		return "other"
	}
	l := strings.ToLower(name)
	switch {
	case strings.Contains(l, "wi-fi"), strings.Contains(l, "wifi"), strings.Contains(l, "wlan"), strings.Contains(l, "wireless"):
		return "wifi"
	case strings.Contains(l, "ethernet"), strings.HasPrefix(l, "eth"), strings.HasPrefix(l, "en"):
		return "ethernet"
	}
	return "other"
}

// FirstWifiIface returns the first up interface that looks like Wi-Fi.
func FirstWifiIface() string {
	ifs, _ := net.Interfaces()
	for _, ifc := range ifs {
		if ifc.Flags&net.FlagUp != 0 && IfaceKind(ifc.Name) == "wifi" {
			return ifc.Name
		}
	}
	return ""
}
