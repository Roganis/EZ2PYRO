package modes

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/roganis/ez2pyro/internal/model"
	"github.com/roganis/ez2pyro/internal/receiver"
)

// Progress is one live measurement, sent once per second to
// Options.OnProgress.
type Progress struct {
	ElapsedS    float64 `json:"elapsed_s"`
	BitrateMbps float64 `json:"bitrate_mbps"` // target of the current segment
	Mbps        float64 `json:"mbps"`         // received
	LossPct     float64 `json:"loss_pct"`
	P99DelayMs  float64 `json:"p99_delay_ms"`
	Late        int     `json:"late"`
	Frames      int     `json:"frames"`
	RTTms       float64 `json:"rtt_ms"`
	Stall       bool    `json:"stall"`
}

type liveStats struct {
	receiver.LiveStats
	RTT float64
}

func (l liveStats) lossPct() float64 {
	if l.Pkts == 0 {
		return 0
	}
	return 100 * float64(l.Lost) / float64(l.Pkts)
}

func (l liveStats) stall() bool { return l.Late >= 3 || l.SinceLast > receiverGap }

func (l liveStats) progress(el time.Duration, bitrate float64) Progress {
	return Progress{
		ElapsedS: el.Seconds(), BitrateMbps: bitrate, Mbps: l.Mbps, LossPct: l.lossPct(),
		P99DelayMs: l.P99DelayMs, Late: l.Late, Frames: l.Frames, RTTms: l.RTT, Stall: l.stall(),
	}
}

// merge adds one per-second sample to an aggregate (Mbps is summed; the
// caller divides by the sample count).
func (l *liveStats) merge(o liveStats) {
	l.Frames += o.Frames
	l.Late += o.Late
	l.Lost += o.Lost
	l.Pkts += o.Pkts
	l.Mbps += o.Mbps
	l.P99DelayMs = max(l.P99DelayMs, o.P99DelayMs)
	l.SinceLast = max(l.SinceLast, o.SinceLast)
	l.RTT = o.RTT
}

func (l liveStats) line(elapsed time.Duration) string {
	flag := ""
	if l.stall() {
		flag = "  STALL"
	}
	return fmt.Sprintf("%7s  %6.1f Mbit/s  loss %5.2f%%  p99 %6.1f ms  late %3d/%-3d  rtt %5.1f ms%s",
		fmtElapsed(elapsed), l.Mbps, l.lossPct(), l.P99DelayMs, l.Late, l.Frames, l.RTT, flag)
}

const receiverGap = time.Duration(receiver.GapThreshold)

func fmtElapsed(d time.Duration) string {
	d = d.Round(time.Second)
	m := int(d.Minutes())
	return fmt.Sprintf("%d:%02d", m, int(d.Seconds())-60*m)
}

func (s *session) soak(ctx context.Context) error {
	s.out("soak: %.0f Mbit/s at %d fps for %v (Ctrl+C stops early and still writes the report)\n",
		s.opt.Traffic.BitrateMbps, s.opt.Traffic.FPS, s.opt.Duration)
	defer s.progress(ctx, 10*time.Second)()
	_, err := s.segment(ctx, s.opt.Traffic.BitrateMbps, s.opt.Duration, nil)
	return err
}

func (s *session) ramp(ctx context.Context) error {
	s.out("ramp: %.0f → %.0f Mbit/s by %.0f, %v per step; stops after 2 steps with >1%% late frames or >0.5%% loss\n",
		s.opt.RampStart, s.opt.RampStop, s.opt.RampStep, s.opt.StepDuration)
	defer s.progress(ctx, 0)()
	fails := 0
	for b := s.opt.RampStart; b <= s.opt.RampStop+1e-9; b += s.opt.RampStep {
		if ctx.Err() != nil {
			break
		}
		seg, err := s.segment(ctx, b, s.opt.StepDuration, nil)
		if err != nil {
			return err
		}
		mark := "clean"
		if !seg.Clean {
			mark = "STUTTERS"
			fails++
		}
		s.out("  %4.0f Mbit/s → delivered %5.1f  late %5.2f%%  loss %5.2f%%  p99 %5.1f ms  %s\n",
			b, seg.RecvMbps, seg.LatePct, seg.LossPct, seg.P99DelayMs, mark)
		if s.opt.OnStep != nil {
			s.opt.OnStep(model.RampStep{BitrateMbps: b, RecvMbps: seg.RecvMbps, LatePct: seg.LatePct,
				LossPct: seg.LossPct, P99DelayMs: seg.P99DelayMs, Clean: seg.Clean})
		}
		if fails >= 2 {
			break
		}
		time.Sleep(500 * time.Millisecond) // let queues drain between steps
	}
	return nil
}

func (s *session) live(ctx context.Context) error {
	s.out("live: %.0f Mbit/s at %d fps until Ctrl+C\n", s.opt.Traffic.BitrateMbps, s.opt.Traffic.FPS)
	s.out("%7s  %13s  %10s  %10s  %9s  %s\n", "time", "throughput", "loss", "p99 delay", "late", "rtt")
	defer s.progress(ctx, time.Second)()
	_, err := s.segment(ctx, s.opt.Traffic.BitrateMbps, 0, nil)
	return err
}

// SignalContext returns a context cancelled on the first Ctrl+C; a second
// Ctrl+C exits immediately.
func SignalContext() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan os.Signal, 2)
	signal.Notify(ch, os.Interrupt)
	go func() {
		<-ch
		fmt.Fprintln(os.Stderr, "\nstopping… (Ctrl+C again to quit immediately)")
		cancel()
		<-ch
		os.Exit(130)
	}()
	return ctx, cancel
}

// competingTraffic warns about things that would skew results.
func competingTraffic() []string {
	var out []string
	for _, p := range runningProcesses() {
		switch {
		case strings.HasPrefix(p, "streaming_clien"):
			out = append(out, "Steam Remote Play streaming is running; stop it for clean results")
		case p == "steamwebhelper" || p == "steam":
			// Steam itself is fine; downloads cannot be detected reliably.
		}
	}
	return out
}
