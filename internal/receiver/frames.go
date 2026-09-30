package receiver

import (
	"math"
	"time"

	"github.com/roganis/ez2pyro/internal/model"
)

// buildFrames turns the per-frame accumulators into frame records.
//
// Delay without synced clocks: for each packet d = recv_ts − send_ts mixes
// the unknown clock offset with the one-way delay. The baseline — a rolling
// 10-second minimum of d — removes the offset and slow drift, leaving the
// queuing delay. During the first 10 s the window looks forward instead, so
// early frames are not judged against a baseline that has not settled yet.
func buildFrames(s *segment, n int, t0, startCtrl, prior int64, budget time.Duration) ([]model.Frame, int64) {
	interval := int64(1e9 / float64(s.cfg.FPS))
	get := func(i int) *frameAcc {
		if i < len(s.frames) {
			return &s.frames[i]
		}
		return &frameAcc{}
	}

	// Send start of every frame; interpolate for frames that never arrived.
	send := make([]int64, n)
	known := make([]bool, n)
	lastKnown := -1
	for i := 0; i < n; i++ {
		if f := get(i); f.recv > 0 {
			send[i], known[i] = f.sendStart, true
			lastKnown = i
		} else if lastKnown >= 0 {
			send[i] = send[lastKnown] + int64(i-lastKnown)*interval
		}
	}
	// Leading frames with no earlier anchor: extrapolate backwards.
	firstKnown := -1
	for i := 0; i < n; i++ {
		if known[i] {
			firstKnown = i
			break
		}
	}
	for i := 0; i < n && (firstKnown < 0 || i < firstKnown); i++ {
		if firstKnown >= 0 {
			send[i] = send[firstKnown] - int64(firstKnown-i)*interval
		}
	}

	// Baseline per frame.
	base := make([]int64, n)
	if firstKnown < 0 {
		// Nothing arrived: fall back to the sender's own schedule.
		b := prior
		for i := range base {
			base[i] = b
		}
		out := make([]model.Frame, n)
		for i := range out {
			t := startCtrl + int64(i)*interval
			if startCtrl == 0 {
				t = t0
			}
			out[i] = model.Frame{
				Segment: s.cfg.Segment, ID: uint32(i), T: float64(t-t0) / 1e6,
				BitrateMbps: s.cfg.BitrateMbps, Pkts: expectedPkts(s.cfg),
				FirstMs: -1, LastMs: -1, DelayMs: -1, Late: true,
			}
		}
		return out, prior
	}
	W := BaselineWindow
	firstSend := send[firstKnown]
	initMin := int64(math.MaxInt64)
	if prior != 0 {
		initMin = prior
	}
	for i := firstKnown; i < n && send[i] <= firstSend+W; i++ {
		if known[i] && get(i).minD < initMin {
			initMin = get(i).minD
		}
	}
	// Monotonic deque of known frame indexes with increasing minD.
	var dq []int
	head := 0
	for i := 0; i < n; i++ {
		if known[i] {
			d := get(i).minD
			for len(dq) > head && get(dq[len(dq)-1]).minD >= d {
				dq = dq[:len(dq)-1]
			}
			dq = append(dq, i)
		}
		for head < len(dq) && send[dq[head]] < send[i]-W {
			head++
		}
		if send[i]-firstSend < W || head >= len(dq) {
			base[i] = initMin
		} else {
			base[i] = get(dq[head]).minD
		}
		if head > 4096 { // compact
			dq = append(dq[:0], dq[head:]...)
			head = 0
		}
	}

	budgetMs := float64(budget) / 1e6
	out := make([]model.Frame, n)
	for i := 0; i < n; i++ {
		f := get(i)
		b := base[i]
		fr := model.Frame{
			Segment: s.cfg.Segment, ID: uint32(i),
			T:           float64(send[i]+b-t0) / 1e6,
			BitrateMbps: s.cfg.BitrateMbps,
			Pkts:        int(f.pktCount),
			Recv:        int(f.recv),
			FirstMs:     -1, LastMs: -1, DelayMs: -1,
		}
		if f.recv == 0 {
			fr.Pkts = expectedPkts(s.cfg)
		} else {
			fr.FirstMs = float64(f.firstRecv-b-send[i]) / 1e6
			fr.LastMs = float64(f.lastRecv-b-send[i]) / 1e6
		}
		fr.Complete = f.recv > 0 && f.recv == f.pktCount
		if fr.Complete {
			fr.DelayMs = float64(f.lastRecv-f.lastSend-b) / 1e6
		}
		fr.Late = !fr.Complete || fr.DelayMs > budgetMs
		out[i] = fr
	}
	return out, base[n-1]
}
