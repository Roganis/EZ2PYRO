//go:build windows

package telemetry

import (
	"context"
	"os/exec"
	"syscall"
	"time"
	"unsafe"
)

var (
	kernel32           = syscall.NewLazyDLL("kernel32.dll")
	procGetSystemTimes = kernel32.NewProc("GetSystemTimes")
)

// hidden runs a console command without flashing a window.
func hidden(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	return cmd.Output()
}

// cpuWindows reports total CPU load via GetSystemTimes at 1 Hz.
type cpuWindows struct{}

func (cpuWindows) Name() string { return "cpu" }

func (cpuWindows) Run(ctx context.Context, emit func(Sample)) error {
	read := func() (CPUTimes, bool) {
		var idle, kernel, user syscall.Filetime
		r, _, _ := procGetSystemTimes.Call(uintptr(unsafe.Pointer(&idle)), uintptr(unsafe.Pointer(&kernel)), uintptr(unsafe.Pointer(&user)))
		if r == 0 {
			return CPUTimes{}, false
		}
		ft := func(f syscall.Filetime) uint64 { return uint64(f.HighDateTime)<<32 | uint64(f.LowDateTime) }
		// Kernel time includes idle time.
		return CPUTimes{Idle: ft(idle), Total: ft(kernel) + ft(user)}, true
	}
	var prev CPUTimes
	return Every(ctx, time.Second, func() {
		cur, ok := read()
		if !ok {
			return
		}
		if prev.Total > 0 {
			emit(Sample{Kind: KindCPU, CPUPct: F64(Load(prev, cur))})
		}
		prev = cur
	})
}

// nicWindows reports wired link speed and duplex via Get-NetAdapter every 5 s.
type nicWindows struct{ iface string }

func (n nicWindows) Name() string { return "nic" }

func (n nicWindows) Run(ctx context.Context, emit func(Sample)) error {
	const ps = `Get-NetAdapter | Where-Object Status -eq 'Up' | ForEach-Object { $_.Name + '|' + $_.ReceiveLinkSpeed + '|' + $_.FullDuplex }`
	return Every(ctx, 5*time.Second, func() {
		out, err := hidden(ctx, "powershell", "-NoProfile", "-NonInteractive", "-Command", ps)
		if err != nil {
			return
		}
		if sp, dup, ok := ParseNetAdapter(string(out), n.iface); ok {
			emit(Sample{Kind: KindNIC, Iface: n.iface, LinkSpeedMbps: Int(sp), Duplex: dup})
		}
	})
}

// wlanWindows samples `netsh wlan show interfaces` at 1 Hz, only when the
// host itself is on Wi-Fi.
type wlanWindows struct{ iface string }

func (w wlanWindows) Name() string { return "netsh-wlan" }

func (w wlanWindows) Run(ctx context.Context, emit func(Sample)) error {
	lastCh := 0
	return Every(ctx, time.Second, func() {
		out, err := hidden(ctx, "netsh", "wlan", "show", "interfaces")
		if err != nil {
			return
		}
		s, ok := ParseNetshWlan(string(out))
		if !ok {
			return
		}
		if s.Channel != nil && *s.Channel != lastCh {
			if lastCh != 0 {
				emit(Sample{Kind: KindEvent, Iface: s.Iface, Event: EventFreqChange, Channel: s.Channel})
			}
			lastCh = *s.Channel
		}
		emit(s)
	})
}

// Available returns the collectors this platform supports.
func Available(o Options) []Collector {
	cols := []Collector{cpuWindows{}}
	switch IfaceKind(o.Iface) {
	case "wifi":
		cols = append(cols, wlanWindows{iface: o.Iface})
	default:
		if o.Iface != "" {
			cols = append(cols, nicWindows{iface: o.Iface})
		}
	}
	return cols
}
