// Package modes implements the test modes: soak, ramp and live (run from the
// controller), plus compare (two saved runs side by side).
package modes

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/roganis/ez2pyro/internal/agent"
	"github.com/roganis/ez2pyro/internal/analysis"
	"github.com/roganis/ez2pyro/internal/clock"
	"github.com/roganis/ez2pyro/internal/model"
	"github.com/roganis/ez2pyro/internal/proto"
	"github.com/roganis/ez2pyro/internal/report"
	"github.com/roganis/ez2pyro/internal/router"
	"github.com/roganis/ez2pyro/internal/telemetry"
)

// Mode names.
const (
	Soak = "soak"
	Ramp = "ramp"
	Live = "live"
)

// Options configures `linkdoctor run`.
type Options struct {
	Controller agent.ControllerOptions
	Mode       string
	Traffic    proto.Config // bitrate, fps, jitter, spread, packet size, profile
	Duration   time.Duration
	Budget     time.Duration // 0 = one frame interval

	RampStart, RampStop, RampStep float64
	StepDuration                  time.Duration

	OutDir     string // parent folder for runs/<date>_<time>
	JSON       bool   // print the summary as JSON on stdout
	LiveReport bool   // live mode: also write a report at the end
	Router     router.Router
	Out        io.Writer // progress output (stderr when JSON is set)
}

// Defaults fills unset options with the documented defaults.
func (o *Options) Defaults() {
	t := &o.Traffic
	if t.BitrateMbps == 0 {
		t.BitrateMbps = 150
	}
	if t.FPS == 0 {
		t.FPS = 60
	}
	if t.PktSize == 0 {
		t.PktSize = 1200
	}
	if t.Profile == "" {
		t.Profile = proto.ProfilePyrowave
	}
	if o.Mode == "" {
		o.Mode = Soak
	}
	if o.Duration == 0 && o.Mode == Soak {
		o.Duration = 30 * time.Minute
	}
	if o.RampStart == 0 {
		o.RampStart = 50
	}
	if o.RampStop == 0 {
		o.RampStop = 500
	}
	if o.RampStep == 0 {
		o.RampStep = 25
	}
	if o.StepDuration == 0 {
		o.StepDuration = 10 * time.Second
	}
	if o.Budget == 0 {
		o.Budget = time.Duration(float64(time.Second) / float64(t.FPS))
	}
	if o.OutDir == "" {
		o.OutDir = "runs"
	}
	if o.Out == nil {
		o.Out = os.Stdout
	}
}

// session collects everything a run produces.
type session struct {
	opt   *Options
	ctl   *agent.Controller
	t0    int64
	start time.Time
	run   *model.Run
	prior int64
}

func (s *session) out(format string, a ...any) { fmt.Fprintf(s.opt.Out, format, a...) }

// Execute runs a soak, ramp or live test and writes the run folder. It
// returns the run and its folder ("" if nothing was written).
func Execute(ctx context.Context, opt Options) (*model.Run, string, error) {
	opt.Defaults()
	if err := opt.Traffic.Validate(); err != nil {
		return nil, "", err
	}
	ctl, err := agent.Dial(ctx, opt.Controller)
	if err != nil {
		return nil, "", err
	}
	defer ctl.Close()

	s := &session{opt: &opt, ctl: ctl, start: time.Now()}
	s.t0 = clock.Now()
	ctl.Recv.SetT0(s.t0)
	s.run = &model.Run{
		Tool: "linkdoctor", Version: agent.Version, Started: s.start.Format(time.RFC3339),
		Options: model.Options{
			Mode: opt.Mode, BitrateMbps: opt.Traffic.BitrateMbps, FPS: opt.Traffic.FPS,
			FrameJitter: opt.Traffic.FrameJitter, BurstSpread: opt.Traffic.BurstSpread,
			PktSize: opt.Traffic.PktSize, Profile: opt.Traffic.Profile,
			DurationS: opt.Duration.Seconds(), BudgetMs: float64(opt.Budget) / 1e6,
		},
	}
	if opt.Mode == Ramp {
		s.run.Options.RampStart, s.run.Options.RampStop, s.run.Options.RampStep = opt.RampStart, opt.RampStop, opt.RampStep
		s.run.Options.StepS = opt.StepDuration.Seconds()
	}
	s.environment()
	for _, w := range s.run.Environment.Warnings {
		s.out("warning: %s\n", w)
	}
	peerName := opt.Controller.Peer
	if h := ctl.PeerHello; h != nil {
		peerName = fmt.Sprintf("%s (%s, %s)", opt.Controller.Peer, h.Hostname, h.OS)
	}
	s.out("connected to %s — mode %s\n", peerName, opt.Mode)

	if opt.Router != nil {
		stop := s.startRouter(ctx)
		defer stop()
	}

	switch opt.Mode {
	case Soak:
		err = s.soak(ctx)
	case Ramp:
		err = s.ramp(ctx)
	case Live:
		err = s.live(ctx)
	default:
		err = fmt.Errorf("unknown mode %q (soak, ramp, live)", opt.Mode)
	}
	if err != nil && len(s.run.Segments) == 0 {
		return nil, "", err
	}
	if err != nil {
		s.out("warning: test ended early: %v\n", err)
	}
	s.finish()
	if opt.Mode == Live && !opt.LiveReport {
		return s.run, "", nil
	}
	dir, werr := report.NewRunDir(opt.OutDir, s.start)
	if werr != nil {
		return s.run, "", werr
	}
	if werr = report.Write(dir, s.run); werr != nil {
		return s.run, dir, werr
	}
	if raw, dropped := ctl.Recv.Raw(); len(raw) > 0 {
		if werr = report.WritePackets(dir, raw, s.t0); werr != nil {
			return s.run, dir, werr
		}
		if dropped > 0 {
			s.out("warning: --raw kept the first %d packets, dropped %d\n", len(raw), dropped)
		}
	}
	return s.run, dir, nil
}

func (s *session) environment() {
	ctl, env := s.ctl, &s.run.Environment
	env.Peer = s.opt.Controller.Peer
	env.RcvBufBytes = ctl.Recv.RcvBuf()
	if off, ok := ctl.ClockOffset(); ok {
		env.ClockOffsetMs = float64(off) / 1e6
	}
	env.Controller.Hello = ctl.LocalHello
	env.Controller.Iface, env.Controller.IfaceKind = agent.UsedIface(ctl.LocalHello)
	env.Server.Hello = ctl.PeerHello
	env.Server.Iface, env.Server.IfaceKind = agent.UsedIface(ctl.PeerHello)

	// Linux reports twice the usable buffer; warn below ~2 MB usable.
	if env.RcvBufBytes > 0 && env.RcvBufBytes < 4<<20 {
		env.Warnings = append(env.Warnings, fmt.Sprintf(
			"socket receive buffer is only %d KB; at high bitrates the Deck may drop packets itself. Raise it with `sudo sysctl -w net.core.rmem_max=16777216`",
			env.RcvBufBytes/1024))
	}
	if _, ok := ctl.ClockOffset(); !ok {
		env.Warnings = append(env.Warnings, fmt.Sprintf("no probe replies from the host on UDP %d; RTT and host telemetry timing are unavailable", s.opt.Controller.ProbePort))
	}
	env.Warnings = append(env.Warnings, competingTraffic()...)
}

func (s *session) startRouter(ctx context.Context) func() {
	rctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := s.opt.Router.Connect(rctx); err != nil {
			s.out("warning: router module: %v\n", err)
			return
		}
		defer s.opt.Router.Close()
		_ = telemetry.Every(rctx, 5*time.Second, func() {
			smp, err := s.opt.Router.Sample(rctx)
			if err != nil {
				s.ctl.Note(telemetry.Sample{Kind: telemetry.KindInfo, Detail: "router: " + err.Error()})
				return
			}
			now := clock.Now()
			for _, x := range smp {
				x.T = now
				x.Side = telemetry.SideController
				s.ctl.Note(x)
			}
		})
	}()
	return func() { cancel(); <-done }
}

// segment runs one traffic segment and records it.
func (s *session) segment(ctx context.Context, bitrate float64, dur time.Duration, stop <-chan struct{}) (*model.Segment, error) {
	cfg := s.opt.Traffic
	cfg.SessionID = agent.NewSessionID()
	cfg.Segment = len(s.run.Segments)
	cfg.BitrateMbps = bitrate
	cfg.DurationMs = dur.Milliseconds()
	cfg.Seed = time.Now().UnixNano()
	res, err := s.ctl.RunSegment(ctx, cfg, stop, s.prior, s.opt.Budget)
	if err != nil {
		return nil, err
	}
	s.prior = res.Receiver.Baseline
	seg := model.Segment{
		Index: cfg.Segment, Config: cfg, Sender: res.Sender, Dups: res.Receiver.Dups,
		Start: float64(res.StartCtrl-s.t0) / 1e6, End: float64(res.EndCtrl-s.t0) / 1e6,
	}
	s.run.Segments = append(s.run.Segments, seg)
	s.run.Frames = append(s.run.Frames, res.Receiver.Frames...)
	s.run.Gaps = append(s.run.Gaps, res.Receiver.Gaps...)
	tmp := model.Run{Segments: []model.Segment{seg}, Frames: res.Receiver.Frames, Options: s.run.Options}
	for i := range tmp.Frames {
		tmp.Frames[i].Segment = 0
	}
	analysis.SegmentStats(&tmp)
	for i := range tmp.Frames {
		tmp.Frames[i].Segment = seg.Index
	}
	out := tmp.Segments[0]
	out.Index = seg.Index
	s.run.Segments[len(s.run.Segments)-1] = out
	return &s.run.Segments[len(s.run.Segments)-1], nil
}

// progress prints a line every interval while a segment runs.
func (s *session) progress(ctx context.Context, every time.Duration, line func(elapsed time.Duration, st liveStats)) func() {
	pctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		begin := time.Now()
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-pctx.Done():
				return
			case <-t.C:
			}
			ls := s.ctl.Recv.Live(s.opt.Budget, every)
			line(time.Since(begin), liveStats{ls, s.ctl.LastRTT()})
		}
	}()
	return func() { cancel(); <-done }
}

func (s *session) finish() {
	tel := s.ctl.Telemetry()
	for i := range tel {
		tel[i].T -= s.t0
	}
	s.run.Telemetry = tel
	s.run.DurationS = time.Since(s.start).Seconds()
	analysis.Analyze(s.run)
}

// PrintSummary writes a human summary of a finished run.
func PrintSummary(w io.Writer, run *model.Run, dir string) {
	sm := run.Summary
	fmt.Fprintf(w, "\n%d frames · %.2f%% late · %.3f%% loss · p99 delay %.1f ms · %d stalls (longest %.0f ms) · %.0f Mbit/s delivered\n",
		sm.Frames, sm.LatePct, sm.LossPct, sm.P99DelayMs, sm.Stalls, sm.LongestStallMs, sm.MeanMbps)
	if p := run.Periodicity; p != nil && p.Periodic {
		fmt.Fprintf(w, "stalls repeat every %.0f ± %.0f s\n", p.PeriodS, p.StdS)
	}
	if r := run.Ramp; r != nil {
		fmt.Fprintf(w, "highest clean bitrate %.0f Mbit/s → set Pyrowave to about %.0f Mbit/s\n", r.HighestCleanMbps, r.RecommendedMbps)
	}
	for i, v := range run.Verdicts {
		if i == 0 {
			fmt.Fprintf(w, "\nverdict: %s\n  evidence: %s\n  fix: %s\n", v.Cause, v.Evidence, v.Fix)
			continue
		}
		fmt.Fprintf(w, "also: %s — %s\n", v.Cause, v.Evidence)
	}
	if sm.SenderOverruns > 0 {
		fmt.Fprintf(w, "note: the host sent %d packets late (sender overruns)\n", sm.SenderOverruns)
	}
	if dir != "" {
		fmt.Fprintf(w, "\nreport: %s/%s\n", dir, report.ReportHTML)
	}
}

// PrintJSON writes the machine-readable summary.
func PrintJSON(w io.Writer, run *model.Run, dir string) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(struct {
		Dir         string             `json:"dir,omitempty"`
		Mode        string             `json:"mode"`
		Summary     model.Summary      `json:"summary"`
		Periodicity *model.Periodicity `json:"periodicity,omitempty"`
		Ramp        *model.RampResult  `json:"ramp,omitempty"`
		Verdicts    []model.Verdict    `json:"verdicts"`
	}{dir, run.Options.Mode, run.Summary, run.Periodicity, run.Ramp, run.Verdicts})
}
