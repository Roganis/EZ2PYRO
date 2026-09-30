package modes

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/roganis/ez2pyro/internal/receiver"
)

type liveStats struct {
	receiver.LiveStats
	RTT float64
}

func (l liveStats) line(elapsed time.Duration) string {
	loss := 0.0
	if l.Pkts > 0 {
		loss = 100 * float64(l.Lost) / float64(l.Pkts)
	}
	flag := ""
	if l.Late >= 3 || l.SinceLast > receiverGap {
		flag = "  STALL"
	}
	return fmt.Sprintf("%7s  %6.1f Mbit/s  loss %5.2f%%  p99 %6.1f ms  late %3d/%-3d  rtt %5.1f ms%s",
		fmtElapsed(elapsed), l.Mbps, loss, l.P99DelayMs, l.Late, l.Frames, l.RTT, flag)
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
	stopProgress := s.progress(ctx, 10*time.Second, func(el time.Duration, st liveStats) {
		s.out("%s\n", st.line(el))
	})
	defer stopProgress()
	_, err := s.segment(ctx, s.opt.Traffic.BitrateMbps, s.opt.Duration, nil)
	return err
}

func (s *session) ramp(ctx context.Context) error {
	s.out("ramp: %.0f → %.0f Mbit/s by %.0f, %v per step; stops after 2 steps with >1%% late frames or >0.5%% loss\n",
		s.opt.RampStart, s.opt.RampStop, s.opt.RampStep, s.opt.StepDuration)
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
	stopProgress := s.progress(ctx, time.Second, func(el time.Duration, st liveStats) {
		s.out("%s\n", st.line(el))
	})
	defer stopProgress()
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
