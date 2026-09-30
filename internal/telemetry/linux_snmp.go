//go:build linux

package telemetry

import (
	"context"
	"os"
	"strconv"
	"strings"
	"time"
)

// snmp reads UDP receive-buffer errors from /proc/net/snmp at 1 Hz, so that
// drops on the Deck's own socket are not mistaken for Wi-Fi loss.
type snmp struct{}

func (snmp) Name() string { return "udp-snmp" }

func (snmp) Run(ctx context.Context, emit func(Sample)) error {
	return Every(ctx, time.Second, func() {
		b, err := os.ReadFile("/proc/net/snmp")
		if err != nil {
			return
		}
		if rb, ie, ok := ParseSNMP(string(b)); ok {
			emit(Sample{Kind: KindSNMP, RcvbufErrors: U64(rb), InErrors: U64(ie)})
		}
	})
}

// cpuLinux reports per-core load from /proc/stat at 1 Hz.
type cpuLinux struct{}

func (cpuLinux) Name() string { return "cpu" }

func (cpuLinux) Run(ctx context.Context, emit func(Sample)) error {
	var prev CPUTimes
	var prevCores []CPUTimes
	return Every(ctx, time.Second, func() {
		b, err := os.ReadFile("/proc/stat")
		if err != nil {
			return
		}
		all, cores := ParseProcStat(string(b))
		if prev.Total > 0 {
			s := Sample{Kind: KindCPU, CPUPct: F64(Load(prev, all))}
			for i := range cores {
				if i < len(prevCores) {
					s.CPUCores = append(s.CPUCores, Load(prevCores[i], cores[i]))
				}
			}
			emit(s)
		}
		prev, prevCores = all, cores
	})
}

// nicLinux reports wired link speed and duplex from sysfs every 5 s (the
// same values ethtool prints, without needing root).
type nicLinux struct{ iface string }

func (n nicLinux) Name() string { return "nic" }

func (n nicLinux) Run(ctx context.Context, emit func(Sample)) error {
	return Every(ctx, 5*time.Second, func() {
		sp, err := os.ReadFile("/sys/class/net/" + n.iface + "/speed")
		if err != nil {
			return
		}
		v, err := strconv.Atoi(strings.TrimSpace(string(sp)))
		if err != nil || v <= 0 {
			return
		}
		d, _ := os.ReadFile("/sys/class/net/" + n.iface + "/duplex")
		emit(Sample{Kind: KindNIC, Iface: n.iface, LinkSpeedMbps: Int(v), Duplex: strings.TrimSpace(string(d))})
	})
}

// Available returns the collectors this platform supports.
func Available(o Options) []Collector {
	cols := []Collector{cpuLinux{}}
	if o.Side == SideController {
		cols = append(cols, snmp{})
	}
	wif := o.WifiIface
	if wif == "" && IfaceKind(o.Iface) == "wifi" {
		wif = o.Iface
	}
	if wif == "" && o.Side == SideController {
		wif = FirstWifiIface()
	}
	if wif != "" {
		cols = append(cols, &wifiLink{iface: wif}, &powerSave{iface: wif}, &wifiEvents{iface: wif})
	}
	if o.Iface != "" && IfaceKind(o.Iface) == "ethernet" {
		cols = append(cols, nicLinux{iface: o.Iface})
	}
	return cols
}
