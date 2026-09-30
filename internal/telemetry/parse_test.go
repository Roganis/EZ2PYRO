package telemetry

import "testing"

const iwLink = `Connected to 3c:90:66:aa:bb:cc (on wlan0)
	SSID: Freebox-ABC123
	freq: 5500.0
	RX: 123456789 bytes (98765 packets)
	TX: 2345678 bytes (12345 packets)
	signal: -58 dBm
	rx bitrate: 866.7 MBit/s VHT-MCS 9 80MHz short GI VHT-NSS 2
	tx bitrate: 780.0 MBit/s VHT-MCS 8 80MHz short GI VHT-NSS 2
	bss flags: short-slot-time
	dtim period: 1
	beacon int: 100
Station 3c:90:66:aa:bb:cc (on wlan0)
	tx retries:	1234
	tx failed:	5
	beacon loss:	2
`

func TestParseIwLink(t *testing.T) {
	s, ok := ParseIwLink(iwLink)
	if !ok {
		t.Fatal("not parsed")
	}
	if *s.FreqMHz != 5500 || s.Band != "5" || *s.Channel != 100 {
		t.Fatalf("freq %d band %s ch %d", *s.FreqMHz, s.Band, *s.Channel)
	}
	if *s.SignalDBm != -58 || *s.RxBitrateMbps != 866.7 || *s.TxBitrateMbps != 780 {
		t.Fatalf("signal/rates %+v", s)
	}
	if *s.TxMCS != 8 || *s.RxMCS != 9 || *s.WidthMHz != 80 {
		t.Fatalf("mcs/width %d %d %d", *s.TxMCS, *s.RxMCS, *s.WidthMHz)
	}
	if *s.TxRetries != 1234 || *s.TxFailed != 5 || *s.BeaconLoss != 2 {
		t.Fatal("station stats")
	}
	if s.SSID != "Freebox-ABC123" || s.BSSID != "3c:90:66:aa:bb:cc" {
		t.Fatalf("ssid %q bssid %q", s.SSID, s.BSSID)
	}
	if s, ok := ParseIwLink("Not connected.\n"); !ok || s.Detail != "not connected" {
		t.Fatal("not connected")
	}
}

func TestParseIwEvent(t *testing.T) {
	cases := map[string]string{
		"1727720000.123456: wlan0 (phy #0): scan started":                        EventScanStart,
		"1727720003.223456: wlan0 (phy #0): scan finished: 2412 2437 5180, \"\"": EventScanDone,
		"1727720003.223456: wlan0 (phy #0): scan aborted":                        EventScanAborted,
		"1727720010.0: wlan0 (phy #0): disconnected (by AP) reason: 3":           EventDisconnect,
		"1727720011.0: wlan0 (phy #0): connected to 3c:90:66:aa:bb:cc":           EventConnect,
		"1727720012.0: wlan0 (phy #0): ch_switch_notify freq=5260 width=80 MHz":  EventChSwitch,
	}
	for line, want := range cases {
		ev, ifc, ok := ParseIwEvent(line)
		if !ok || ev != want || ifc != "wlan0" {
			t.Errorf("%q → %q %q %v, want %q", line, ev, ifc, ok, want)
		}
	}
}

func TestParseSNMPAndStat(t *testing.T) {
	snmp := "Ip: Forwarding DefaultTTL\nIp: 1 64\nUdp: InDatagrams NoPorts InErrors OutDatagrams RcvbufErrors SndbufErrors\nUdp: 100 2 7 50 5 0\n"
	rb, ie, ok := ParseSNMP(snmp)
	if !ok || rb != 5 || ie != 7 {
		t.Fatalf("snmp %d %d %v", rb, ie, ok)
	}
	a, c := ParseProcStat("cpu  100 0 100 700 100 0 0 0 0 0\ncpu0 50 0 50 350 50 0 0 0 0 0\ncpu1 50 0 50 350 50 0 0 0 0 0\nintr 1\n")
	b, _ := ParseProcStat("cpu  200 0 200 1300 200 0 0 0 0 0\n")
	if len(c) != 2 || a.Total != 1000 {
		t.Fatalf("stat %+v %d", a, len(c))
	}
	if l := Load(a, b); l < 22.1 || l > 22.3 { // 200 busy of 900
		t.Fatalf("load %.1f", l)
	}
}

func TestParseNetsh(t *testing.T) {
	out := `
There is 1 interface on the system:

    Name                   : Wi-Fi
    State                  : connected
    SSID                   : Freebox-ABC123
    BSSID                  : 3c:90:66:aa:bb:cc
    Radio type             : 802.11ac
    Band                   : 5 GHz
    Channel                : 36
    Receive rate (Mbps)    : 866.7
    Transmit rate (Mbps)   : 866.7
    Signal                 : 90%
`
	s, ok := ParseNetshWlan(out)
	if !ok || s.Band != "5" || *s.Channel != 36 || *s.SignalDBm != -55 || *s.RxBitrateMbps != 866.7 {
		t.Fatalf("%+v", s)
	}
	sp, dup, ok := ParseNetAdapter("Ethernet|1000000000|True\nWi-Fi|866700000|False\n", "Ethernet")
	if !ok || sp != 1000 || dup != "full" {
		t.Fatalf("adapter %d %s", sp, dup)
	}
}

func TestBandChannel(t *testing.T) {
	for f, want := range map[int][2]any{2412: {"2.4", 1}, 2484: {"2.4", 14}, 5180: {"5", 36}, 5825: {"5", 165}, 5955: {"6", 1}} {
		b, c := BandChannel(f)
		if b != want[0] || c != want[1] {
			t.Errorf("%d → %s %d", f, b, c)
		}
	}
	if !IsDFS(100) || IsDFS(36) {
		t.Fatal("dfs")
	}
}
