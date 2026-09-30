// Package receiver receives the synthetic frame bursts, aggregates packets
// into per-frame records and turns them into late-frame measurements.
package receiver

import (
	"errors"
	"math"
	"net"
	"net/netip"
	"sort"
	"sync"
	"time"

	"github.com/roganis/ez2pyro/internal/clock"
	"github.com/roganis/ez2pyro/internal/model"
	"github.com/roganis/ez2pyro/internal/proto"
)

const (
	// GapThreshold is the no-packet period that counts as a stall on its own.
	GapThreshold = int64(50 * time.Millisecond)
	// BaselineWindow is the rolling window of the minimum-delay baseline.
	BaselineWindow = int64(10 * time.Second)
	// BinWidth is the width of arrival-throughput bins.
	BinWidth = int64(100 * time.Millisecond)
	// DesiredRcvBuf is the socket receive buffer we ask for.
	DesiredRcvBuf = 16 << 20
	// MaxRawPackets caps --raw memory use.
	MaxRawPackets = 20_000_000
)

// RawPacket is one received packet, kept only with --raw.
type RawPacket struct {
	Seq     uint64
	FrameID uint32
	PktIdx  uint16
	SendTS  int64 // sender clock
	RecvTS  int64 // controller clock
	Size    int
}

type frameAcc struct {
	sendStart int64 // sender clock; 0 until seen
	lastSend  int64 // highest send_ts seen
	firstRecv int64
	lastRecv  int64
	minD      int64
	pktCount  uint16
	recv      uint16
	seen      []uint64 // bitmap of received pkt indexes, freed once the frame is old
}

// segment holds the in-flight state of one traffic segment.
type segment struct {
	cfg       proto.Config
	frames    []frameAcc
	maxID     int64
	packets   uint64
	bytes     uint64
	dups      uint64
	firstRecv int64
	lastRecv  int64
	gaps      []model.Gap // controller clock ns, stored as float for reuse
	anchorID  int64
	anchorTS  int64
	liveNext  int
}

// Receiver owns the data socket.
type Receiver struct {
	conn    *net.UDPConn
	rcvbuf  int
	keepRaw bool

	mu      sync.Mutex
	t0      int64
	seg     *segment
	bins    []uint64 // arrival bytes per BinWidth since t0
	raw     []RawPacket
	rawDrop uint64
	foreign uint64
	bad     uint64

	// Online baseline for live mode: min d per 1 s bucket.
	bucketMin [11]int64
	bucketID  [11]int64

	closed chan struct{}
}

// Listen opens the receive socket on addr (e.g. ":0").
func Listen(addr string, keepRaw bool) (*Receiver, error) {
	ua, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return nil, err
	}
	c, err := net.ListenUDP("udp", ua)
	if err != nil {
		return nil, err
	}
	r := &Receiver{conn: c, keepRaw: keepRaw, closed: make(chan struct{})}
	r.rcvbuf = setRcvBuf(c, DesiredRcvBuf)
	r.t0 = clock.Now()
	go r.loop()
	return r, nil
}

// LocalPort returns the bound UDP port.
func (r *Receiver) LocalPort() int { return r.conn.LocalAddr().(*net.UDPAddr).Port }

// RcvBuf returns the effective socket receive buffer in bytes (as reported
// by the OS; Linux reports double the usable size).
func (r *Receiver) RcvBuf() int { return r.rcvbuf }

// Conn exposes the socket so the controller can send hole-punch packets from
// the same port.
func (r *Receiver) Conn() *net.UDPConn { return r.conn }

// SetT0 sets the run start (controller clock) used for relative times.
func (r *Receiver) SetT0(t0 int64) {
	r.mu.Lock()
	r.t0 = t0
	r.bins = r.bins[:0]
	r.mu.Unlock()
}

// Begin starts accepting packets for a new segment.
func (r *Receiver) Begin(cfg proto.Config) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seg = &segment{cfg: cfg, maxID: -1, anchorID: -1}
}

// Close shuts the socket.
func (r *Receiver) Close() error {
	err := r.conn.Close()
	<-r.closed
	return err
}

func (r *Receiver) loop() {
	defer close(r.closed)
	buf := make([]byte, 65536)
	var h proto.Header
	for {
		n, _, err := r.conn.ReadFromUDPAddrPort(buf)
		now := clock.Now()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		if h.Decode(buf[:n]) != nil || h.Flags&proto.FlagPunch != 0 {
			r.mu.Lock()
			r.bad++
			r.mu.Unlock()
			continue
		}
		r.handle(&h, n, now)
	}
}

func (r *Receiver) handle(h *proto.Header, n int, now int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.seg
	if s == nil || h.SessionID != s.cfg.SessionID {
		r.foreign++
		return
	}
	// Arrival throughput bins.
	if b := (now - r.t0) / BinWidth; b >= 0 {
		for int64(len(r.bins)) <= b {
			r.bins = append(r.bins, 0)
		}
		r.bins[b] += uint64(n)
	}
	id := int64(h.FrameID)
	if id > 10_000_000 { // corrupt / absurd
		r.bad++
		return
	}
	for int64(len(s.frames)) <= id {
		s.frames = append(s.frames, frameAcc{})
	}
	f := &s.frames[id]
	if f.seen == nil {
		if f.recv > 0 { // already freed: late duplicate or very late packet
			s.dups++
			return
		}
		f.pktCount = h.PktCount
		f.seen = make([]uint64, (int(h.PktCount)+63)/64)
		f.firstRecv = now
		f.minD = math.MaxInt64
	}
	if int(h.PktIdx) >= int(f.pktCount) {
		r.bad++
		return
	}
	w, bit := h.PktIdx/64, uint64(1)<<(h.PktIdx%64)
	if f.seen[w]&bit != 0 {
		s.dups++
		return
	}
	f.seen[w] |= bit
	f.recv++
	f.sendStart = h.FrameSendStart
	if h.SendTS > f.lastSend {
		f.lastSend = h.SendTS
	}
	f.lastRecv = now
	d := now - h.SendTS
	if d < f.minD {
		f.minD = d
	}
	r.noteBaseline(now, d)

	if s.firstRecv == 0 {
		s.firstRecv = now
	} else if now-s.lastRecv > GapThreshold {
		s.gaps = append(s.gaps, model.Gap{Start: float64(s.lastRecv), End: float64(now)})
	}
	s.lastRecv = now
	s.packets++
	s.bytes += uint64(n)
	if id > s.maxID {
		s.maxID = id
		s.anchorID, s.anchorTS = id, h.FrameSendStart
		// Free bitmaps of frames that are long done.
		if old := id - int64(4*s.cfg.FPS); old >= 0 && s.frames[old].seen != nil && s.frames[old].recv == s.frames[old].pktCount {
			s.frames[old].seen = nil
		}
	}
	if r.keepRaw {
		if len(r.raw) < MaxRawPackets {
			r.raw = append(r.raw, RawPacket{h.Seq, h.FrameID, h.PktIdx, h.SendTS, now, n})
		} else {
			r.rawDrop++
		}
	}
}

func (r *Receiver) noteBaseline(now, d int64) {
	b := now / int64(time.Second)
	i := b % int64(len(r.bucketMin))
	if r.bucketID[i] != b {
		r.bucketID[i], r.bucketMin[i] = b, d
	} else if d < r.bucketMin[i] {
		r.bucketMin[i] = d
	}
}

// onlineBaseline returns the minimum d over the last ~10 s.
func (r *Receiver) onlineBaseline(now int64) (int64, bool) {
	b := now / int64(time.Second)
	min, ok := int64(math.MaxInt64), false
	for i := range r.bucketMin {
		if r.bucketID[i] > b-int64(len(r.bucketMin)) && r.bucketID[i] <= b && r.bucketMin[i] < min {
			min, ok = r.bucketMin[i], true
		}
	}
	return min, ok
}

// LiveStats summarises frames that became due since the previous call, using
// an online baseline. It is used by live mode for its per-second line.
type LiveStats struct {
	Frames     int
	Late       int
	Lost       int
	Pkts       int
	P99DelayMs float64
	Mbps       float64
	SinceLast  time.Duration // time since the last packet
}

// Live evaluates frames due before now−grace.
func (r *Receiver) Live(budget time.Duration, window time.Duration) LiveStats {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := clock.Now()
	var ls LiveStats
	s := r.seg
	if s == nil || s.anchorID < 0 {
		return ls
	}
	base, ok := r.onlineBaseline(now)
	if !ok {
		return ls
	}
	interval := int64(1e9 / float64(s.cfg.FPS))
	grace := int64(budget) + int64(50*time.Millisecond)
	var delays []float64
	for ; s.liveNext <= int(s.maxID); s.liveNext++ {
		i := s.liveNext
		f := &s.frames[i]
		send := f.sendStart
		if f.recv == 0 {
			send = s.anchorTS + (int64(i)-s.anchorID)*interval
		}
		if send+base+int64(s.cfg.BurstSpread*float64(interval))+grace > now {
			break
		}
		ls.Frames++
		pk := int(f.pktCount)
		if f.recv == 0 {
			pk = expectedPkts(s.cfg)
		}
		ls.Pkts += pk
		ls.Lost += pk - int(f.recv)
		if f.recv == 0 || f.recv < f.pktCount {
			ls.Late++
			continue
		}
		dl := float64(f.lastRecv-f.lastSend-base) / 1e6
		delays = append(delays, dl)
		if dl > float64(budget)/1e6 {
			ls.Late++
		}
	}
	if len(delays) > 0 {
		sort.Float64s(delays)
		ls.P99DelayMs = delays[int(math.Ceil(0.99*float64(len(delays))))-1]
	}
	// Throughput over the last window from the arrival bins.
	nb := int(int64(window) / BinWidth)
	cur := int((now - r.t0) / BinWidth)
	var bytes uint64
	for b := cur - nb; b < cur; b++ {
		if b >= 0 && b < len(r.bins) {
			bytes += r.bins[b]
		}
	}
	ls.Mbps = float64(bytes) * 8 / window.Seconds() / 1e6
	if s.lastRecv > 0 {
		ls.SinceLast = time.Duration(now - s.lastRecv)
	}
	return ls
}

func expectedPkts(cfg proto.Config) int {
	n := int(math.Ceil(cfg.BitrateMbps * 1e6 / float64(cfg.FPS) / 8 / float64(cfg.PktSize)))
	if n < 1 {
		n = 1
	}
	return n
}

// Result is the outcome of one finished segment.
type Result struct {
	Frames   []model.Frame
	Gaps     []model.Gap
	Packets  uint64
	Bytes    uint64
	Dups     uint64
	Baseline int64 // last baseline (controller − sender clock), for chaining segments
}

// SenderInfo is what the controller knows about the sender side of a segment.
type SenderInfo struct {
	FramesSent uint32 // from RESULTS; frames lost entirely at the tail still count
	StartCtrl  int64  // sender start mapped onto the controller clock (0 = unknown)
	EndCtrl    int64  // sender end mapped onto the controller clock (0 = unknown)
}

// Finish closes the current segment and builds its frame records.
//
// A trailing silence longer than the gap threshold before the sender's end
// becomes a gap. prior is the baseline carried from the previous segment
// (0 = none). budget is the late threshold.
func (r *Receiver) Finish(si SenderInfo, prior int64, budget time.Duration) Result {
	r.mu.Lock()
	s := r.seg
	r.seg = nil
	t0 := r.t0
	r.mu.Unlock()
	if s == nil {
		return Result{}
	}
	if s.lastRecv > 0 && si.EndCtrl > 0 && si.EndCtrl-s.lastRecv > GapThreshold {
		s.gaps = append(s.gaps, model.Gap{Start: float64(s.lastRecv), End: float64(si.EndCtrl)})
	}
	n := int(si.FramesSent)
	if n < len(s.frames) {
		n = len(s.frames)
	}
	res := Result{Packets: s.packets, Bytes: s.bytes, Dups: s.dups}
	res.Frames, res.Baseline = buildFrames(s, n, t0, si.StartCtrl, prior, budget)
	for _, g := range s.gaps {
		res.Gaps = append(res.Gaps, model.Gap{Start: (g.Start - float64(t0)) / 1e6, End: (g.End - float64(t0)) / 1e6})
	}
	return res
}

// Raw returns the raw packets kept with --raw and how many were dropped.
func (r *Receiver) Raw() ([]RawPacket, uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.raw, r.rawDrop
}

// Bins returns arrival bytes per 100 ms since t0.
func (r *Receiver) Bins() []uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]uint64(nil), r.bins...)
}

// T0 returns the run start on the controller clock.
func (r *Receiver) T0() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.t0
}

// SendPunch sends a hole-punch packet to the sender's data port so that
// stateful firewalls and the sender learn our address.
func (r *Receiver) SendPunch(dst netip.AddrPort, session uint32) error {
	var h proto.Header
	h.Flags = proto.FlagPunch
	h.SessionID = session
	h.SendTS = clock.Now()
	b := make([]byte, proto.HeaderSize)
	h.Encode(b)
	_, err := r.conn.WriteToUDPAddrPort(b, dst)
	return err
}
