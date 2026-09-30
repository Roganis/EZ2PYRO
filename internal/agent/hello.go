// Package agent implements the two roles of linkdoctor: the server (`serve`,
// the host that sends traffic) and the controller session (`run`, the Deck
// that receives traffic, gathers telemetry and owns the analysis).
package agent

import (
	"net"
	"os"
	"runtime"

	"github.com/roganis/ez2pyro/internal/clock"
	"github.com/roganis/ez2pyro/internal/proto"
	"github.com/roganis/ez2pyro/internal/telemetry"
)

// Version is the tool version, set at build time via -ldflags.
var Version = "dev"

func makeHello(role string, usedIP net.IP, collectors []string) *proto.Hello {
	h := &proto.Hello{
		ProtoVersion: int(proto.Version),
		ToolVersion:  Version,
		Role:         role,
		OS:           runtime.GOOS,
		Arch:         runtime.GOARCH,
		Telemetry:    collectors,
		Clock:        clock.Now(),
	}
	h.Hostname, _ = os.Hostname()
	used := ""
	if usedIP != nil {
		used = telemetry.IfaceForAddr(usedIP)
	}
	ifs, _ := net.Interfaces()
	for _, ifc := range ifs {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			if ifc.Name != used {
				continue
			}
		}
		info := proto.IfaceInfo{Name: ifc.Name, Kind: telemetry.IfaceKind(ifc.Name), Used: ifc.Name == used}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			info.Addrs = append(info.Addrs, a.String())
		}
		h.Ifaces = append(h.Ifaces, info)
	}
	return h
}

func localIP(c net.Conn) net.IP {
	if a, ok := c.LocalAddr().(*net.TCPAddr); ok {
		return a.IP
	}
	return nil
}

// UsedIface returns the name of the interface marked as carrying traffic.
func UsedIface(h *proto.Hello) (string, string) {
	if h == nil {
		return "", ""
	}
	for _, i := range h.Ifaces {
		if i.Used {
			return i.Name, i.Kind
		}
	}
	return "", ""
}
