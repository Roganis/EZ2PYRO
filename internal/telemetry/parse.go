package telemetry

import (
	"bufio"
	"regexp"
	"strconv"
	"strings"
)

// This file holds the pure text parsers (iw fallback, /proc, netsh) so they
// can be unit-tested on every platform.

// ParseSNMP extracts Udp RcvbufErrors and InErrors from /proc/net/snmp.
func ParseSNMP(text string) (rcvbuf, inErr uint64, ok bool) {
	var hdr []string
	sc := bufio.NewScanner(strings.NewReader(text))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 0 || f[0] != "Udp:" {
			continue
		}
		if hdr == nil {
			hdr = f
			continue
		}
		for i := 1; i < len(f) && i < len(hdr); i++ {
			v, _ := strconv.ParseUint(f[i], 10, 64)
			switch hdr[i] {
			case "RcvbufErrors":
				rcvbuf, ok = v, true
			case "InErrors":
				inErr = v
			}
		}
		return
	}
	return
}

// CPUTimes is one line of /proc/stat.
type CPUTimes struct{ Idle, Total uint64 }

// ParseProcStat returns the aggregate and per-core CPU times.
func ParseProcStat(text string) (all CPUTimes, cores []CPUTimes) {
	sc := bufio.NewScanner(strings.NewReader(text))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 5 || !strings.HasPrefix(f[0], "cpu") {
			continue
		}
		var t CPUTimes
		for i, s := range f[1:] {
			v, _ := strconv.ParseUint(s, 10, 64)
			t.Total += v
			if i == 3 || i == 4 { // idle, iowait
				t.Idle += v
			}
		}
		if f[0] == "cpu" {
			all = t
		} else {
			cores = append(cores, t)
		}
	}
	return
}

// Load returns the busy percentage between two readings.
func Load(prev, cur CPUTimes) float64 {
	dt := float64(cur.Total - prev.Total)
	if dt <= 0 {
		return 0
	}
	return 100 * (1 - float64(cur.Idle-prev.Idle)/dt)
}

var (
	reIwFreq   = regexp.MustCompile(`freq:\s*([\d.]+)`)
	reIwSignal = regexp.MustCompile(`signal:\s*(-?\d+)\s*dBm`)
	reIwTx     = regexp.MustCompile(`tx bitrate:\s*([\d.]+)\s*MBit/s(.*)`)
	reIwRx     = regexp.MustCompile(`rx bitrate:\s*([\d.]+)\s*MBit/s(.*)`)
	reIwMCS    = regexp.MustCompile(`(?:VHT-|HE-|EHT-)?MCS\s*(\d+)`)
	reIwWidth  = regexp.MustCompile(`(\d+)MHz`)
	reIwSSID   = regexp.MustCompile(`(?m)^\s*SSID:\s*(.*)$`)
	reIwBSSID  = regexp.MustCompile(`Connected to ([0-9a-f:]{17})`)
	reIwRetr   = regexp.MustCompile(`tx retries:\s*(\d+)`)
	reIwFailed = regexp.MustCompile(`tx failed:\s*(\d+)`)
	reIwBeacon = regexp.MustCompile(`beacon loss:\s*(\d+)`)
)

// ParseIwLink parses `iw dev <if> link` (optionally concatenated with
// `iw dev <if> station dump`) into a KindLink sample.
func ParseIwLink(text string) (Sample, bool) {
	s := Sample{Kind: KindLink}
	if strings.Contains(text, "Not connected") {
		s.Detail = "not connected"
		return s, true
	}
	if m := reIwBSSID.FindStringSubmatch(text); m != nil {
		s.BSSID = m[1]
	}
	if m := reIwSSID.FindStringSubmatch(text); m != nil {
		s.SSID = strings.TrimSpace(m[1])
	}
	if m := reIwFreq.FindStringSubmatch(text); m != nil {
		f, _ := strconv.ParseFloat(m[1], 64)
		fi := int(f)
		s.FreqMHz = &fi
		if b, ch := BandChannel(fi); b != "" {
			s.Band, s.Channel = b, Int(ch)
		}
	}
	if m := reIwSignal.FindStringSubmatch(text); m != nil {
		v, _ := strconv.Atoi(m[1])
		s.SignalDBm = &v
	}
	rate := func(re *regexp.Regexp) (*float64, *int, *int) {
		m := re.FindStringSubmatch(text)
		if m == nil {
			return nil, nil, nil
		}
		v, _ := strconv.ParseFloat(m[1], 64)
		var mcs, width *int
		if mm := reIwMCS.FindStringSubmatch(m[2]); mm != nil {
			x, _ := strconv.Atoi(mm[1])
			mcs = &x
		}
		if mm := reIwWidth.FindStringSubmatch(m[2]); mm != nil {
			x, _ := strconv.Atoi(mm[1])
			width = &x
		}
		return &v, mcs, width
	}
	var w1, w2 *int
	s.TxBitrateMbps, s.TxMCS, w1 = rate(reIwTx)
	s.RxBitrateMbps, s.RxMCS, w2 = rate(reIwRx)
	if w1 != nil {
		s.WidthMHz = w1
	} else {
		s.WidthMHz = w2
	}
	u := func(re *regexp.Regexp) *uint64 {
		if m := re.FindStringSubmatch(text); m != nil {
			v, _ := strconv.ParseUint(m[1], 10, 64)
			return &v
		}
		return nil
	}
	s.TxRetries, s.TxFailed, s.BeaconLoss = u(reIwRetr), u(reIwFailed), u(reIwBeacon)
	return s, s.FreqMHz != nil || s.SignalDBm != nil
}

// ParseIwPowerSave parses `iw dev <if> get power_save`.
func ParseIwPowerSave(text string) (bool, bool) {
	switch {
	case strings.Contains(text, "Power save: on"):
		return true, true
	case strings.Contains(text, "Power save: off"):
		return false, true
	}
	return false, false
}

// ParseIwEvent maps one line of `iw event -t` to an event name and the
// interface it concerns.
func ParseIwEvent(line string) (event, iface string, ok bool) {
	// Example: "1727720000.123456: wlan0 (phy #0): scan started"
	i := strings.Index(line, ": ")
	if i < 0 {
		return
	}
	rest := line[i+2:]
	if j := strings.Index(rest, " "); j > 0 {
		iface = rest[:j]
	}
	l := strings.ToLower(rest)
	switch {
	case strings.Contains(l, "scan started"):
		event = EventScanStart
	case strings.Contains(l, "scan aborted"):
		event = EventScanAborted
	case strings.Contains(l, "scan finished"), strings.Contains(l, "new scan results"):
		event = EventScanDone
	case strings.Contains(l, "sched scan"):
		event = EventSchedScan
	case strings.Contains(l, "ch_switch"), strings.Contains(l, "channel switch"):
		event = EventChSwitch
	case strings.Contains(l, "disconnected"):
		event = EventDisconnect
	case strings.Contains(l, "deauth"), strings.Contains(l, "disassoc"):
		event = EventDeauth
	case strings.Contains(l, "roamed"):
		event = EventRoam
	case strings.Contains(l, "connected to"):
		event = EventConnect
	default:
		return "", "", false
	}
	return event, iface, true
}

// ParseNetshWlan parses `netsh wlan show interfaces` (English output).
func ParseNetshWlan(text string) (Sample, bool) {
	s := Sample{Kind: KindLink}
	kv := map[string]string{}
	sc := bufio.NewScanner(strings.NewReader(text))
	for sc.Scan() {
		line := sc.Text()
		i := strings.Index(line, " : ")
		if i < 0 {
			continue
		}
		k := strings.ToLower(strings.TrimSpace(line[:i]))
		if _, dup := kv[k]; !dup {
			kv[k] = strings.TrimSpace(line[i+3:])
		}
	}
	if st := kv["state"]; st != "" && !strings.EqualFold(st, "connected") {
		s.Detail = "not connected"
		return s, true
	}
	s.Iface = kv["name"]
	s.SSID = kv["ssid"]
	s.BSSID = kv["bssid"]
	if v, err := strconv.Atoi(strings.TrimSuffix(kv["signal"], "%")); err == nil {
		d := v/2 - 100 // Windows' quality % → approximate dBm
		s.SignalDBm = &d
	}
	if v, err := strconv.ParseFloat(kv["receive rate (mbps)"], 64); err == nil {
		s.RxBitrateMbps = &v
	}
	if v, err := strconv.ParseFloat(kv["transmit rate (mbps)"], 64); err == nil {
		s.TxBitrateMbps = &v
	}
	if v, err := strconv.Atoi(kv["channel"]); err == nil {
		s.Channel = &v
		switch {
		case v <= 14:
			s.Band = "2.4"
		default:
			s.Band = "5"
		}
	}
	if b := kv["band"]; b != "" {
		switch {
		case strings.HasPrefix(b, "2.4"):
			s.Band = "2.4"
		case strings.HasPrefix(b, "5"):
			s.Band = "5"
		case strings.HasPrefix(b, "6"):
			s.Band = "6"
		}
	}
	return s, s.SignalDBm != nil || s.Channel != nil
}

// ParseNetAdapter parses lines of "Name|LinkSpeedBps|FullDuplex" produced by
// the PowerShell one-liner used on Windows.
func ParseNetAdapter(text, iface string) (speedMbps int, duplex string, ok bool) {
	sc := bufio.NewScanner(strings.NewReader(text))
	for sc.Scan() {
		f := strings.Split(strings.TrimSpace(sc.Text()), "|")
		if len(f) < 3 || (iface != "" && f[0] != iface) {
			continue
		}
		bps, err := strconv.ParseUint(f[1], 10, 64)
		if err != nil {
			continue
		}
		duplex = "half"
		if strings.EqualFold(f[2], "true") {
			duplex = "full"
		}
		return int(bps / 1_000_000), duplex, true
	}
	return
}
