package proto

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"sync"

	"github.com/roganis/ez2pyro/internal/telemetry"
)

// Control message types (JSON lines over TCP).
const (
	TypeHello     = "HELLO"
	TypeConfig    = "CONFIG"
	TypeStart     = "START"
	TypeStop      = "STOP"
	TypeHeartbeat = "HEARTBEAT"
	TypeTelemetry = "TELEMETRY"
	TypeResults   = "RESULTS"
	TypeError     = "ERROR"
)

// Default ports. They stay clear of Steam's own 27031–27036 range.
const (
	DefaultControlPort = 47100
	DefaultDataPort    = 47101
	DefaultProbePort   = 47102
)

// Msg is one control-channel message. Exactly one payload field is set,
// matching Type.
type Msg struct {
	Type      string          `json:"type"`
	Hello     *Hello          `json:"hello,omitempty"`
	Config    *Config         `json:"config,omitempty"`
	Start     *Start          `json:"start,omitempty"`
	Stop      *Stop           `json:"stop,omitempty"`
	Heartbeat *Heartbeat      `json:"heartbeat,omitempty"`
	Telemetry *TelemetryBatch `json:"telemetry,omitempty"`
	Results   *Results        `json:"results,omitempty"`
	Error     string          `json:"error,omitempty"`
}

// Hello is exchanged by both sides right after connecting.
type Hello struct {
	ProtoVersion int         `json:"proto_version"`
	ToolVersion  string      `json:"tool_version"`
	Role         string      `json:"role"` // "controller" or "server"
	OS           string      `json:"os"`
	Arch         string      `json:"arch"`
	Hostname     string      `json:"hostname"`
	Ifaces       []IfaceInfo `json:"ifaces"`
	Telemetry    []string    `json:"telemetry"` // collectors this side can run
	Clock        int64       `json:"clock_ns"`  // sender's monotonic clock at send time
}

// IfaceInfo describes a network interface.
type IfaceInfo struct {
	Name  string   `json:"name"`
	Kind  string   `json:"kind"` // "wifi", "ethernet", "other"
	Addrs []string `json:"addrs,omitempty"`
	Used  bool     `json:"used,omitempty"` // carries the test traffic
}

// Profiles.
const (
	ProfilePyrowave = "pyrowave"
	ProfileEven     = "even"
)

// Config carries the full traffic parameters for one test segment.
type Config struct {
	SessionID   uint32  `json:"session_id"`
	Segment     int     `json:"segment"`
	BitrateMbps float64 `json:"bitrate_mbps"`
	FPS         int     `json:"fps"`
	FrameJitter float64 `json:"frame_jitter"`
	BurstSpread float64 `json:"burst_spread"`
	PktSize     int     `json:"pkt_size"`
	Profile     string  `json:"profile"`
	DurationMs  int64   `json:"duration_ms"` // 0 = until STOP
	Seed        int64   `json:"seed"`
}

// Validate checks parameter ranges.
func (c *Config) Validate() error {
	switch {
	case c.BitrateMbps < 1 || c.BitrateMbps > 1000:
		return fmt.Errorf("bitrate %.0f Mbit/s out of range 1–1000", c.BitrateMbps)
	case c.FPS < 30 || c.FPS > 144:
		return fmt.Errorf("fps %d out of range 30–144", c.FPS)
	case c.FrameJitter < 0 || c.FrameJitter > 1:
		return fmt.Errorf("frame jitter %.2f out of range 0–1", c.FrameJitter)
	case c.BurstSpread < 0 || c.BurstSpread > 1:
		return fmt.Errorf("burst spread %.2f out of range 0–1", c.BurstSpread)
	case c.PktSize < MinPacketSize || c.PktSize > MaxPacketSize:
		return fmt.Errorf("packet size %d out of range %d–%d", c.PktSize, MinPacketSize, MaxPacketSize)
	case c.Profile != ProfilePyrowave && c.Profile != ProfileEven:
		return fmt.Errorf("unknown profile %q", c.Profile)
	}
	return nil
}

// Start tells the server to begin sending the configured session.
type Start struct {
	SessionID uint32 `json:"session_id"`
}

// Stop tells the server to stop sending.
type Stop struct {
	SessionID uint32 `json:"session_id"`
}

// Heartbeat keeps the control connection alive in both directions.
type Heartbeat struct {
	Clock int64 `json:"clock_ns"`
}

// TelemetryBatch carries telemetry samples from the server to the controller.
// Sample times are on the server's monotonic clock.
type TelemetryBatch struct {
	Samples []telemetry.Sample `json:"samples"`
}

// Results reports sender-side statistics at the end of a segment.
type Results struct {
	SessionID uint32      `json:"session_id"`
	Sender    SenderStats `json:"sender"`
}

// SenderStats summarises sender pacing for one segment.
type SenderStats struct {
	FramesSent     uint32  `json:"frames_sent"`
	PacketsSent    uint64  `json:"packets_sent"`
	BytesSent      uint64  `json:"bytes_sent"`
	SendErrors     uint64  `json:"send_errors"`
	Overruns       uint64  `json:"overruns"` // packets sent >1 ms late because the sender fell behind
	StartNs        int64   `json:"start_ns"` // sender clock
	EndNs          int64   `json:"end_ns"`
	AchievedMbps   float64 `json:"achieved_mbps"`
	TargetMbps     float64 `json:"target_mbps"`
	FrameJitterP50 float64 `json:"frame_start_jitter_p50_us"`
	FrameJitterP99 float64 `json:"frame_start_jitter_p99_us"`
	FrameJitterMax float64 `json:"frame_start_jitter_max_us"`
}

// Codec reads and writes JSON-lines messages. Writes are safe for concurrent
// use; reads must happen from a single goroutine.
type Codec struct {
	mu  sync.Mutex
	w   io.Writer
	enc *json.Encoder
	sc  *bufio.Scanner
}

// NewCodec wraps a connection.
func NewCodec(rw io.ReadWriter) *Codec {
	sc := bufio.NewScanner(rw)
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
	return &Codec{w: rw, enc: json.NewEncoder(rw), sc: sc}
}

// Send writes one message.
func (c *Codec) Send(m *Msg) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.enc.Encode(m) // Encode appends '\n'
}

// Recv reads one message.
func (c *Codec) Recv() (*Msg, error) {
	if !c.sc.Scan() {
		if err := c.sc.Err(); err != nil {
			return nil, err
		}
		return nil, io.EOF
	}
	var m Msg
	if err := json.Unmarshal(c.sc.Bytes(), &m); err != nil {
		return nil, fmt.Errorf("control: bad message: %w", err)
	}
	return &m, nil
}
