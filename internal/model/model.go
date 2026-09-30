// Package model holds the record types shared by the receiver, the analysis
// engine and the report writer. All times are milliseconds relative to the
// run start on the controller's clock unless noted otherwise.
package model

import (
	"github.com/roganis/ez2pyro/internal/proto"
	"github.com/roganis/ez2pyro/internal/telemetry"
)

// Frame is the per-frame record stored for every frame of a run.
type Frame struct {
	Segment     int     `json:"seg"`
	ID          uint32  `json:"frame_id"`
	T           float64 `json:"t_ms"`         // expected arrival of the first packet (no queuing)
	BitrateMbps float64 `json:"bitrate_mbps"` // segment target
	Pkts        int     `json:"pkts"`         // packets in frame (estimated if none arrived)
	Recv        int     `json:"recv"`
	FirstMs     float64 `json:"first_ms"` // first arrival relative to frame send start, base delay removed; -1 if none
	LastMs      float64 `json:"last_ms"`  // last arrival relative to frame send start; -1 if none
	DelayMs     float64 `json:"delay_ms"` // completion (queuing) delay; -1 if incomplete
	Complete    bool    `json:"complete"`
	Late        bool    `json:"late"`
}

// Lost returns the number of packets not received.
func (f *Frame) Lost() int {
	if f.Recv >= f.Pkts {
		return 0
	}
	return f.Pkts - f.Recv
}

// Gap is a period with no packets arriving while traffic was expected.
type Gap struct {
	Start float64 `json:"start_ms"`
	End   float64 `json:"end_ms"`
}

// Segment describes one continuous traffic segment (soak = 1, ramp = 1 per step).
type Segment struct {
	Index      int               `json:"index"`
	Config     proto.Config      `json:"config"`
	Start      float64           `json:"start_ms"`
	End        float64           `json:"end_ms"`
	Sender     proto.SenderStats `json:"sender"`
	Frames     int               `json:"frames"`
	LateFrames int               `json:"late_frames"`
	LatePct    float64           `json:"late_pct"`
	LossPct    float64           `json:"loss_pct"`
	RecvMbps   float64           `json:"recv_mbps"`
	P99DelayMs float64           `json:"p99_delay_ms"`
	Clean      bool              `json:"clean"`
	Dups       uint64            `json:"dup_packets"`
}

// Second is the per-second summary.
type Second struct {
	T          int     `json:"t_s"`
	Mbps       float64 `json:"mbps"`
	LossPct    float64 `json:"loss_pct"`
	P50DelayMs float64 `json:"p50_delay_ms"`
	P99DelayMs float64 `json:"p99_delay_ms"`
	Frames     int     `json:"frames"`
	Late       int     `json:"late"`
	RTTms      float64 `json:"rtt_ms"` // -1 if unknown
}

// Stall is a detected stall event.
type Stall struct {
	Index       int      `json:"index"`
	Start       float64  `json:"start_ms"`
	End         float64  `json:"end_ms"`
	DurationMs  float64  `json:"duration_ms"`
	WorstDelay  float64  `json:"worst_delay_ms"` // -1 when frames were lost outright
	LateFrames  int      `json:"late_frames"`
	PacketsLost int      `json:"packets_lost"`
	Gap         bool     `json:"gap"` // includes a >50 ms no-packet period
	BitrateMbps float64  `json:"bitrate_mbps"`
	Nearby      []string `json:"nearby"` // telemetry events in the correlation window
	Causes      []string `json:"causes"` // rule IDs that explain this stall
}

// Endpoint describes one machine taking part in the run.
type Endpoint struct {
	Hello     *proto.Hello `json:"hello"`
	Iface     string       `json:"iface"`
	IfaceKind string       `json:"iface_kind"`
	SSID      string       `json:"ssid,omitempty"`
	Band      string       `json:"band,omitempty"`
	Channel   int          `json:"channel,omitempty"`
	WidthMHz  int          `json:"width_mhz,omitempty"`
}

// Environment is the "who and where" of a run.
type Environment struct {
	Controller    Endpoint `json:"controller"`
	Server        Endpoint `json:"server"`
	Peer          string   `json:"peer"`
	RcvBufBytes   int      `json:"rcvbuf_bytes"`
	ClockOffsetMs float64  `json:"clock_offset_ms"`
	Warnings      []string `json:"warnings,omitempty"`
}

// Options are the user-facing test parameters recorded with a run.
type Options struct {
	Mode        string  `json:"mode"`
	BitrateMbps float64 `json:"bitrate_mbps"`
	FPS         int     `json:"fps"`
	FrameJitter float64 `json:"frame_jitter"`
	BurstSpread float64 `json:"burst_spread"`
	PktSize     int     `json:"pkt_size"`
	Profile     string  `json:"profile"`
	DurationS   float64 `json:"duration_s"`
	BudgetMs    float64 `json:"budget_ms"`
	RampStart   float64 `json:"ramp_start_mbps,omitempty"`
	RampStop    float64 `json:"ramp_stop_mbps,omitempty"`
	RampStep    float64 `json:"ramp_step_mbps,omitempty"`
	StepS       float64 `json:"ramp_step_s,omitempty"`
}

// Run is everything recorded about one test run.
type Run struct {
	Tool        string       `json:"tool"`
	Version     string       `json:"version"`
	Started     string       `json:"started"` // RFC 3339 wall-clock time
	DurationS   float64      `json:"duration_s"`
	Options     Options      `json:"options"`
	Environment Environment  `json:"environment"`
	Segments    []Segment    `json:"segments"`
	Gaps        []Gap        `json:"gaps"`
	Summary     Summary      `json:"summary"`
	Periodicity *Periodicity `json:"periodicity,omitempty"`
	Ramp        *RampResult  `json:"ramp,omitempty"`
	Verdicts    []Verdict    `json:"verdicts"`

	// Stored in separate files.
	Frames    []Frame            `json:"-"`
	Seconds   []Second           `json:"-"`
	Stalls    []Stall            `json:"-"`
	Telemetry []telemetry.Sample `json:"-"`
	Timeline  []TimelineBin      `json:"-"`
}

// Summary holds the key numbers.
type Summary struct {
	Frames          int     `json:"frames"`
	LateFrames      int     `json:"late_frames"`
	LatePct         float64 `json:"late_pct"`
	LossPct         float64 `json:"loss_pct"`
	PacketsLost     int     `json:"packets_lost"`
	Stalls          int     `json:"stalls"`
	LongestStallMs  float64 `json:"longest_stall_ms"`
	StallsPerMin    float64 `json:"stalls_per_min"`
	P50DelayMs      float64 `json:"p50_delay_ms"`
	P99DelayMs      float64 `json:"p99_delay_ms"`
	MeanMbps        float64 `json:"mean_mbps"`
	MedianRTTms     float64 `json:"median_rtt_ms"`
	RcvbufErrors    uint64  `json:"rcvbuf_errors"`
	SenderOverruns  uint64  `json:"sender_overruns"`
	RecommendedMbps float64 `json:"recommended_mbps,omitempty"`
}

// Periodicity describes a repeating stall pattern.
type Periodicity struct {
	Periodic  bool    `json:"periodic"`
	PeriodS   float64 `json:"period_s"`
	StdS      float64 `json:"std_s"`
	CV        float64 `json:"cv"`
	AutoLagS  int     `json:"autocorr_lag_s,omitempty"`
	AutoCorr  float64 `json:"autocorr,omitempty"`
	Intervals int     `json:"intervals"`
}

// RampStep is one step of a ramp test.
type RampStep struct {
	BitrateMbps float64 `json:"bitrate_mbps"`
	RecvMbps    float64 `json:"recv_mbps"`
	LatePct     float64 `json:"late_pct"`
	LossPct     float64 `json:"loss_pct"`
	P99DelayMs  float64 `json:"p99_delay_ms"`
	Clean       bool    `json:"clean"`
}

// RampResult is the outcome of a ramp test.
type RampResult struct {
	Steps            []RampStep `json:"steps"`
	HighestCleanMbps float64    `json:"highest_clean_mbps"`
	RecommendedMbps  float64    `json:"recommended_mbps"`
	StoppedEarly     bool       `json:"stopped_early"`
}

// Verdict is one ranked likely cause.
type Verdict struct {
	Rule      string  `json:"rule"`
	Cause     string  `json:"cause"`
	Fix       string  `json:"fix"`
	Explained int     `json:"explained"` // stalls explained
	Total     int     `json:"total"`
	Share     float64 `json:"share"`
	Evidence  string  `json:"evidence"`
	RunLevel  bool    `json:"run_level,omitempty"` // finding not tied to individual stalls
}

// TimelineBin is one 100 ms bin of the timeline chart.
type TimelineBin struct {
	T          float64 `json:"t"`   // bin start, ms
	P99DelayMs float64 `json:"p99"` // -1 if no complete frames
	Lost       int     `json:"lost"`
	Late       int     `json:"late"`
}
