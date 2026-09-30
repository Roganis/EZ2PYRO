// Package telemetry collects Wi-Fi, NIC, CPU and socket statistics on both
// ends of the link. Every sample is timestamped on the local monotonic clock
// (see internal/clock); the controller later shifts peer samples onto its own
// clock.
package telemetry

// Sample kinds.
const (
	KindLink      = "link"      // nl80211 link info + station stats (or iw / netsh fallback)
	KindPowerSave = "powersave" // Wi-Fi power save state
	KindEvent     = "event"     // discrete Wi-Fi event (scan, roam, disconnect, ...)
	KindSNMP      = "snmp"      // UDP socket counters from /proc/net/snmp
	KindCPU       = "cpu"       // CPU load
	KindNIC       = "nic"       // wired link speed / duplex
	KindSender    = "sender"    // per-second sender pacing stats (host side)
	KindRTT       = "rtt"       // per-second probe RTT summary (controller side)
	KindRouter    = "router"    // router-side view (e.g. Freebox)
	KindInfo      = "info"      // free-form notes (collector failures, fallbacks)
)

// Event names (Sample.Event when Kind == KindEvent).
const (
	EventScanStart   = "scan_start"
	EventScanDone    = "scan_done"
	EventScanAborted = "scan_aborted"
	EventSchedScan   = "sched_scan"
	EventConnect     = "connect"
	EventRoam        = "roam"
	EventDisconnect  = "disconnect"
	EventDeauth      = "deauth"
	EventChSwitch    = "ch_switch"
	EventFreqChange  = "freq_change" // synthesised when sampled frequency changes
	EventRouterChan  = "router_channel_change"
)

// Sides.
const (
	SideController = "controller" // the Deck / receiver
	SideServer     = "server"     // the host / sender
)

// Sample is one telemetry record. All measurement fields are optional:
// different chips and platforms expose different subsets.
type Sample struct {
	T     int64  `json:"t_ns"` // monotonic ns; relative to run start once stored
	Side  string `json:"side"`
	Kind  string `json:"kind"`
	Iface string `json:"iface,omitempty"`
	Event string `json:"event,omitempty"`

	// Wi-Fi link.
	SignalDBm     *int     `json:"signal_dbm,omitempty"`
	TxBitrateMbps *float64 `json:"tx_mbps,omitempty"`
	RxBitrateMbps *float64 `json:"rx_mbps,omitempty"`
	TxMCS         *int     `json:"tx_mcs,omitempty"`
	RxMCS         *int     `json:"rx_mcs,omitempty"`
	WidthMHz      *int     `json:"width_mhz,omitempty"`
	FreqMHz       *int     `json:"freq_mhz,omitempty"`
	Band          string   `json:"band,omitempty"` // "2.4", "5", "6"
	Channel       *int     `json:"channel,omitempty"`
	SSID          string   `json:"ssid,omitempty"`
	BSSID         string   `json:"bssid,omitempty"`
	TxRetries     *uint64  `json:"tx_retries,omitempty"`
	TxFailed      *uint64  `json:"tx_failed,omitempty"`
	BeaconLoss    *uint64  `json:"beacon_loss,omitempty"`
	PowerSave     *bool    `json:"power_save,omitempty"`

	// Sockets.
	RcvbufErrors *uint64 `json:"rcvbuf_errors,omitempty"`
	InErrors     *uint64 `json:"in_errors,omitempty"`

	// CPU.
	CPUPct   *float64  `json:"cpu_pct,omitempty"`
	CPUCores []float64 `json:"cpu_cores,omitempty"`

	// Wired NIC.
	LinkSpeedMbps *int   `json:"link_speed_mbps,omitempty"`
	Duplex        string `json:"duplex,omitempty"`

	// Sender pacing (host).
	Overruns      *uint64  `json:"overruns,omitempty"`
	SendMbps      *float64 `json:"send_mbps,omitempty"`
	FrameJitterUs *float64 `json:"frame_jitter_us,omitempty"`
	MaxLateUs     *float64 `json:"max_late_us,omitempty"` // worst packet send lateness in the second

	// Probe RTT (controller).
	RTTms     *float64 `json:"rtt_ms,omitempty"`
	RTTMaxms  *float64 `json:"rtt_max_ms,omitempty"`
	ProbeLost *int     `json:"probe_lost,omitempty"`

	// Router: other stations' traffic.
	OtherMbps *float64 `json:"other_mbps,omitempty"`
	TopTalker string   `json:"top_talker,omitempty"`

	Detail string `json:"detail,omitempty"`
}

// Helpers to take the address of literals.
func Int(v int) *int         { return &v }
func F64(v float64) *float64 { return &v }
func U64(v uint64) *uint64   { return &v }
func Bool(v bool) *bool      { return &v }

// BandChannel converts a centre frequency in MHz to a band name and channel.
func BandChannel(freq int) (string, int) {
	switch {
	case freq == 2484:
		return "2.4", 14
	case freq >= 2412 && freq < 2484:
		return "2.4", (freq - 2407) / 5
	case freq >= 5955 && freq <= 7115:
		return "6", (freq - 5950) / 5
	case freq >= 5150 && freq <= 5925:
		return "5", (freq - 5000) / 5
	default:
		return "", 0
	}
}

// IsDFS reports whether a 5 GHz channel is a DFS (radar) channel in the EU/US
// (channels 52–144).
func IsDFS(channel int) bool { return channel >= 52 && channel <= 144 }
