//go:build linux

package telemetry

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"strings"
	"time"

	"github.com/mdlayher/genetlink"
	"github.com/mdlayher/netlink"
	"github.com/mdlayher/wifi"
	"golang.org/x/sys/unix"
)

// wifiLink samples nl80211 link info and station stats at 2 Hz. It falls
// back to parsing `iw` output when nl80211 is unavailable.
type wifiLink struct{ iface string }

func (w *wifiLink) Name() string { return "nl80211-link" }

func widthMHz(cw wifi.ChannelWidth) *int {
	switch cw {
	case wifi.ChannelWidth20NoHT, wifi.ChannelWidth20:
		return Int(20)
	case wifi.ChannelWidth40:
		return Int(40)
	case wifi.ChannelWidth80:
		return Int(80)
	case wifi.ChannelWidth80P80, wifi.ChannelWidth160:
		return Int(160)
	case wifi.ChannelWidth320:
		return Int(320)
	}
	return nil
}

func (w *wifiLink) Run(ctx context.Context, emit func(Sample)) error {
	c, err := wifi.New()
	if err != nil {
		emit(Sample{Kind: KindInfo, Detail: "nl80211 unavailable, using iw: " + err.Error()})
		return w.runIw(ctx, emit)
	}
	defer c.Close()
	lastFreq := 0
	var ssid, bssid string
	var lastBSS time.Time
	fails := 0
	return Every(ctx, 500*time.Millisecond, func() {
		ifs, err := c.Interfaces()
		if err != nil {
			fails++
			return
		}
		var ifi *wifi.Interface
		for _, x := range ifs {
			if x.Name == w.iface {
				ifi = x
			}
		}
		if ifi == nil {
			return
		}
		s := Sample{Kind: KindLink, Iface: w.iface}
		if ifi.Frequency > 0 {
			s.FreqMHz = Int(ifi.Frequency)
			if b, ch := BandChannel(ifi.Frequency); b != "" {
				s.Band, s.Channel = b, Int(ch)
			}
			s.WidthMHz = widthMHz(ifi.ChannelWidth)
		}
		if time.Since(lastBSS) > 5*time.Second {
			lastBSS = time.Now()
			if bss, err := c.BSS(ifi); err == nil {
				ssid, bssid = bss.SSID, bss.BSSID.String()
			}
		}
		s.SSID, s.BSSID = ssid, bssid
		sts, err := c.StationInfo(ifi)
		if err == nil && len(sts) > 0 {
			st := sts[0]
			s.SignalDBm = Int(st.Signal)
			s.TxBitrateMbps = F64(float64(st.TransmitBitrate) / 1e6)
			s.RxBitrateMbps = F64(float64(st.ReceiveBitrate) / 1e6)
			if m := st.TransmitRateInfo.Modulation; m != nil {
				s.TxMCS = Int(m.GetMCS())
			}
			if m := st.ReceiveRateInfo.Modulation; m != nil {
				s.RxMCS = Int(m.GetMCS())
			}
			if s.WidthMHz == nil {
				s.WidthMHz = widthMHz(st.TransmitRateInfo.ChannelWidth)
			}
			s.TxRetries = U64(uint64(st.TransmitRetries))
			s.TxFailed = U64(uint64(st.TransmitFailed))
			s.BeaconLoss = U64(uint64(st.BeaconLoss))
		} else if errors.Is(err, net.ErrClosed) {
			return
		} else if ifi.Frequency == 0 {
			s.Detail = "not connected"
		}
		if s.FreqMHz != nil && *s.FreqMHz != lastFreq {
			if lastFreq != 0 {
				emit(Sample{Kind: KindEvent, Iface: w.iface, Event: EventFreqChange,
					FreqMHz: s.FreqMHz, Detail: fmt.Sprintf("%d → %d MHz", lastFreq, *s.FreqMHz)})
			}
			lastFreq = *s.FreqMHz
		}
		emit(s)
	})
}

func (w *wifiLink) runIw(ctx context.Context, emit func(Sample)) error {
	if _, err := exec.LookPath("iw"); err != nil {
		return errors.New("neither nl80211 nor iw available")
	}
	lastFreq := 0
	return Every(ctx, 500*time.Millisecond, func() {
		link, err := exec.CommandContext(ctx, "iw", "dev", w.iface, "link").Output()
		if err != nil {
			return
		}
		st, _ := exec.CommandContext(ctx, "iw", "dev", w.iface, "station", "dump").Output()
		s, ok := ParseIwLink(string(link) + "\n" + string(st))
		if !ok {
			return
		}
		s.Iface = w.iface
		if s.FreqMHz != nil && *s.FreqMHz != lastFreq {
			if lastFreq != 0 {
				emit(Sample{Kind: KindEvent, Iface: w.iface, Event: EventFreqChange, FreqMHz: s.FreqMHz,
					Detail: fmt.Sprintf("%d → %d MHz", lastFreq, *s.FreqMHz)})
			}
			lastFreq = *s.FreqMHz
		}
		emit(s)
	})
}

// nl80211 opens a generic netlink connection to the nl80211 family.
func nl80211() (*genetlink.Conn, genetlink.Family, error) {
	c, err := genetlink.Dial(nil)
	if err != nil {
		return nil, genetlink.Family{}, err
	}
	f, err := c.GetFamily("nl80211")
	if err != nil {
		c.Close()
		return nil, genetlink.Family{}, err
	}
	return c, f, nil
}

// powerSave polls NL80211_CMD_GET_POWER_SAVE at 1 Hz (iw fallback).
type powerSave struct{ iface string }

func (p *powerSave) Name() string { return "powersave" }

func (p *powerSave) Run(ctx context.Context, emit func(Sample)) error {
	ifc, err := net.InterfaceByName(p.iface)
	if err != nil {
		return err
	}
	c, fam, err := nl80211()
	if err != nil {
		return p.runIw(ctx, emit)
	}
	defer c.Close()
	ae := netlink.NewAttributeEncoder()
	ae.Uint32(unix.NL80211_ATTR_IFINDEX, uint32(ifc.Index))
	data, err := ae.Encode()
	if err != nil {
		return err
	}
	return Every(ctx, time.Second, func() {
		msgs, err := c.Execute(genetlink.Message{
			Header: genetlink.Header{Command: unix.NL80211_CMD_GET_POWER_SAVE, Version: fam.Version},
			Data:   data,
		}, fam.ID, netlink.Request)
		if err != nil || len(msgs) == 0 {
			return
		}
		ad, err := netlink.NewAttributeDecoder(msgs[0].Data)
		if err != nil {
			return
		}
		for ad.Next() {
			if ad.Type() == unix.NL80211_ATTR_PS_STATE {
				emit(Sample{Kind: KindPowerSave, Iface: p.iface, PowerSave: Bool(ad.Uint32() == 1)})
			}
		}
	})
}

func (p *powerSave) runIw(ctx context.Context, emit func(Sample)) error {
	return Every(ctx, time.Second, func() {
		out, err := exec.CommandContext(ctx, "iw", "dev", p.iface, "get", "power_save").Output()
		if err != nil {
			return
		}
		if on, ok := ParseIwPowerSave(string(out)); ok {
			emit(Sample{Kind: KindPowerSave, Iface: p.iface, PowerSave: Bool(on)})
		}
	})
}

// wifiEvents listens to the nl80211 scan, mlme and config multicast groups
// (the same stream `iw event` shows).
type wifiEvents struct{ iface string }

func (e *wifiEvents) Name() string { return "nl80211-events" }

func (e *wifiEvents) Run(ctx context.Context, emit func(Sample)) error {
	ifc, err := net.InterfaceByName(e.iface)
	if err != nil {
		return err
	}
	c, fam, err := nl80211()
	if err != nil {
		return e.runIw(ctx, emit)
	}
	joined := 0
	for _, g := range fam.Groups {
		switch g.Name {
		case "scan", "mlme", "config":
			if c.JoinGroup(g.ID) == nil {
				joined++
			}
		}
	}
	if joined == 0 {
		c.Close()
		return e.runIw(ctx, emit)
	}
	go func() { <-ctx.Done(); c.Close() }()
	for {
		msgs, _, err := c.Receive()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		for _, m := range msgs {
			ev := nlEventName(m.Header.Command)
			if ev == "" {
				continue
			}
			s := Sample{Kind: KindEvent, Iface: e.iface, Event: ev}
			match := false
			if ad, err := netlink.NewAttributeDecoder(m.Data); err == nil {
				for ad.Next() {
					switch ad.Type() {
					case unix.NL80211_ATTR_IFINDEX:
						match = int(ad.Uint32()) == ifc.Index
					case unix.NL80211_ATTR_WIPHY_FREQ:
						s.FreqMHz = Int(int(ad.Uint32()))
					case unix.NL80211_ATTR_REASON_CODE:
						s.Detail = fmt.Sprintf("reason %d", ad.Uint16())
					}
				}
			}
			if match {
				emit(s)
			}
		}
	}
}

func nlEventName(cmd uint8) string {
	switch cmd {
	case unix.NL80211_CMD_TRIGGER_SCAN:
		return EventScanStart
	case unix.NL80211_CMD_NEW_SCAN_RESULTS:
		return EventScanDone
	case unix.NL80211_CMD_SCAN_ABORTED:
		return EventScanAborted
	case unix.NL80211_CMD_START_SCHED_SCAN, unix.NL80211_CMD_SCHED_SCAN_RESULTS:
		return EventSchedScan
	case unix.NL80211_CMD_CONNECT:
		return EventConnect
	case unix.NL80211_CMD_ROAM:
		return EventRoam
	case unix.NL80211_CMD_DISCONNECT:
		return EventDisconnect
	case unix.NL80211_CMD_DEAUTHENTICATE, unix.NL80211_CMD_DISASSOCIATE:
		return EventDeauth
	case unix.NL80211_CMD_CH_SWITCH_NOTIFY, unix.NL80211_CMD_CH_SWITCH_STARTED_NOTIFY:
		return EventChSwitch
	}
	return ""
}

func (e *wifiEvents) runIw(ctx context.Context, emit func(Sample)) error {
	cmd := exec.CommandContext(ctx, "iw", "event", "-t")
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	buf := make([]byte, 4096)
	var line strings.Builder
	for {
		n, err := out.Read(buf)
		for _, b := range buf[:n] {
			if b != '\n' {
				line.WriteByte(b)
				continue
			}
			if ev, ifname, ok := ParseIwEvent(line.String()); ok && (ifname == e.iface || ifname == "") {
				emit(Sample{Kind: KindEvent, Iface: e.iface, Event: ev, Detail: "iw"})
			}
			line.Reset()
		}
		if err != nil {
			_ = cmd.Wait()
			return nil
		}
	}
}
