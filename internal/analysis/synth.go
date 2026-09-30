package analysis

import (
	"encoding/json"
	"math/rand"
	"os"

	"github.com/roganis/ez2pyro/internal/model"
	"github.com/roganis/ez2pyro/internal/proto"
	"github.com/roganis/ez2pyro/internal/telemetry"
)

// Scenario declares a synthetic run for analysis tests (see testdata/scenarios).
type Scenario struct {
	Name        string  `json:"name"`
	Mode        string  `json:"mode"` // soak (default) or ramp
	DurationS   float64 `json:"duration_s"`
	BitrateMbps float64 `json:"bitrate_mbps"`
	Seed        int64   `json:"seed"`

	// Stalls at explicit times and/or on a period.
	Stalls   []SynthStall `json:"stalls"`
	Periodic *struct {
		FirstS  float64 `json:"first_s"`
		EveryS  float64 `json:"every_s"`
		JitterS float64 `json:"jitter_s"`
		DurMs   float64 `json:"dur_ms"`
		Kind    string  `json:"kind"`
	} `json:"periodic"`

	// Steady link state.
	Link struct {
		SignalDBm   int     `json:"signal_dbm"`
		RxMbps      float64 `json:"rx_mbps"`
		FreqMHz     int     `json:"freq_mhz"`
		PowerSave   bool    `json:"power_save"`
		RetriesPerS float64 `json:"retries_per_s"`
		NoTelemetry bool    `json:"no_telemetry"`
		Overruns    float64 `json:"overruns_per_s"` // scattered background overruns
	} `json:"link"`

	// Telemetry anomalies placed around every stall.
	AtEachStall []Effect `json:"at_each_stall"`

	// Ramp: per-step bitrates; steps listed in LateSteps get late frames.
	RampSteps []float64 `json:"ramp_steps"`
	LateSteps []float64 `json:"late_steps"`

	Expect struct {
		Top         string  `json:"top"`
		Periodic    bool    `json:"periodic"`
		Stalls      int     `json:"stalls"`
		MinShare    float64 `json:"min_share"`
		Recommended float64 `json:"recommended_mbps"`
	} `json:"expect"`
}

// SynthStall is one injected stall.
type SynthStall struct {
	AtS   float64 `json:"at_s"`
	DurMs float64 `json:"dur_ms"`
	Kind  string  `json:"kind"` // delay (default), loss, gap
}

// Effect is a telemetry anomaly relative to a stall.
type Effect struct {
	Kind     string  `json:"kind"` // event, signal, band, retries, overruns, cpu, rcvbuf, router, powersave
	Event    string  `json:"event,omitempty"`
	OffsetMs float64 `json:"offset_ms"`
	Side     string  `json:"side,omitempty"`
	Value    float64 `json:"value"`
	Freq     int     `json:"freq_mhz,omitempty"`
	Who      string  `json:"who,omitempty"`
}

// LoadScenario reads a scenario file.
func LoadScenario(path string) (*Scenario, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s Scenario
	return &s, json.Unmarshal(b, &s)
}

// Synth builds a run from a scenario.
func Synth(sc *Scenario) *model.Run {
	rng := rand.New(rand.NewSource(sc.Seed + 1))
	fps := 60
	iv := 1000.0 / float64(fps)
	if sc.BitrateMbps == 0 {
		sc.BitrateMbps = 150
	}
	run := &model.Run{
		Tool: "linkdoctor", Version: "synthetic",
		Options: model.Options{Mode: sc.Mode, BitrateMbps: sc.BitrateMbps, FPS: fps, PktSize: 1200,
			BudgetMs: iv, Profile: proto.ProfilePyrowave, DurationS: sc.DurationS},
	}
	if run.Options.Mode == "" {
		run.Options.Mode = "soak"
	}

	// Segments.
	type seg struct {
		start, end, mbps float64
		late             bool
	}
	var segs []seg
	if len(sc.RampSteps) > 0 {
		t := 0.0
		for _, b := range sc.RampSteps {
			late := false
			for _, l := range sc.LateSteps {
				late = late || l == b
			}
			segs = append(segs, seg{t, t + 10000, b, late})
			t += 10500
		}
	} else {
		segs = []seg{{0, sc.DurationS * 1000, sc.BitrateMbps, false}}
	}

	// Stalls.
	stalls := append([]SynthStall(nil), sc.Stalls...)
	if p := sc.Periodic; p != nil {
		for t := p.FirstS; t < sc.DurationS-1; t += p.EveryS {
			stalls = append(stalls, SynthStall{AtS: t + p.JitterS*(2*rng.Float64()-1), DurMs: p.DurMs, Kind: p.Kind})
		}
	}
	in := func(t float64) *SynthStall {
		for i := range stalls {
			s := &stalls[i]
			if t >= s.AtS*1000 && t < s.AtS*1000+s.DurMs {
				return s
			}
		}
		return nil
	}

	for si, sg := range segs {
		cfg := proto.Config{Segment: si, BitrateMbps: sg.mbps, FPS: fps, PktSize: 1200, Profile: proto.ProfilePyrowave}
		pk := int(sg.mbps*1e6/float64(fps)/8/1200) + 1
		n := int((sg.end - sg.start) / iv)
		m := model.Segment{Index: si, Config: cfg, Start: sg.start, End: sg.end}
		m.Sender = proto.SenderStats{FramesSent: uint32(n), PacketsSent: uint64(n * pk)}
		run.Segments = append(run.Segments, m)
		for i := 0; i < n; i++ {
			t := sg.start + float64(i)*iv
			f := model.Frame{Segment: si, ID: uint32(i), T: t, BitrateMbps: sg.mbps, Pkts: pk, Recv: pk,
				FirstMs: 0.1, LastMs: 5, DelayMs: 0.3 + rng.Float64(), Complete: true}
			if sg.late && i%10 < 3 && i > 60 { // 30% bursts of 3 late frames
				f.DelayMs = 30
			}
			if s := in(t); s != nil {
				switch s.Kind {
				case "loss":
					f.Recv, f.Complete, f.DelayMs = pk/2, false, -1
				case "gap":
					f.Recv, f.Complete, f.DelayMs, f.FirstMs, f.LastMs = 0, false, -1, -1, -1
				default:
					f.DelayMs = 45
				}
			}
			run.Frames = append(run.Frames, f)
		}
	}
	for _, s := range stalls {
		if s.Kind == "gap" {
			run.Gaps = append(run.Gaps, model.Gap{Start: s.AtS * 1000, End: s.AtS*1000 + s.DurMs})
		}
	}

	// Telemetry.
	end := segs[len(segs)-1].end
	L := sc.Link
	if L.SignalDBm == 0 {
		L.SignalDBm = -55
	}
	if L.RxMbps == 0 {
		L.RxMbps = 780
	}
	if L.FreqMHz == 0 {
		L.FreqMHz = 5180
	}
	if L.RetriesPerS == 0 {
		L.RetriesPerS = 5
	}
	ns := func(ms float64) int64 { return int64(ms * 1e6) }
	near := func(t float64, kind string) *Effect {
		for i := range stalls {
			s := &stalls[i]
			for j := range sc.AtEachStall {
				e := &sc.AtEachStall[j]
				if e.Kind != kind {
					continue
				}
				from := s.AtS*1000 + e.OffsetMs - 500
				to := s.AtS*1000 + s.DurMs + 500
				if e.OffsetMs > 0 {
					to = s.AtS*1000 + e.OffsetMs + s.DurMs + 500
				}
				if t >= from && t <= to {
					return e
				}
			}
		}
		return nil
	}
	add := func(s telemetry.Sample) { run.Telemetry = append(run.Telemetry, s) }
	if !L.NoTelemetry {
		var retries uint64
		lastFreq := L.FreqMHz
		for t := 0.0; t < end; t += 500 {
			sig, rx, freq := L.SignalDBm, L.RxMbps, L.FreqMHz
			if e := near(t, "signal"); e != nil {
				sig = int(e.Value)
				rx = L.RxMbps / 3
			}
			if e := near(t, "band"); e != nil {
				freq = e.Freq
			}
			rate := L.RetriesPerS * (0.8 + 0.4*rng.Float64())
			if e := near(t, "retries"); e != nil {
				rate = e.Value
			}
			retries += uint64(rate / 2)
			if e := near(t, "freq"); e != nil {
				freq = e.Freq
			}
			if freq != lastFreq {
				add(telemetry.Sample{T: ns(t), Side: telemetry.SideController, Kind: telemetry.KindEvent, Event: telemetry.EventFreqChange, FreqMHz: telemetry.Int(freq)})
				lastFreq = freq
			}
			band, ch := telemetry.BandChannel(freq)
			add(telemetry.Sample{T: ns(t), Side: telemetry.SideController, Kind: telemetry.KindLink, Iface: "wlan0",
				SignalDBm: telemetry.Int(sig), RxBitrateMbps: telemetry.F64(rx), TxBitrateMbps: telemetry.F64(rx * 0.9),
				FreqMHz: telemetry.Int(freq), Band: band, Channel: telemetry.Int(ch), WidthMHz: telemetry.Int(80),
				TxRetries: telemetry.U64(retries), SSID: "Freebox-SYNTH"})
		}
		var rcv uint64
		for t := 0.0; t < end; t += 1000 {
			ps := L.PowerSave
			if e := near(t, "powersave"); e != nil {
				ps = e.Value != 0
			}
			add(telemetry.Sample{T: ns(t), Side: telemetry.SideController, Kind: telemetry.KindPowerSave, PowerSave: telemetry.Bool(ps)})
			if e := near(t, "rcvbuf"); e != nil {
				rcv += uint64(e.Value)
			}
			add(telemetry.Sample{T: ns(t), Side: telemetry.SideController, Kind: telemetry.KindSNMP, RcvbufErrors: telemetry.U64(rcv)})
			var ov uint64
			late := 300.0 // µs: normal scheduling noise
			if e := near(t, "overruns"); e != nil {
				ov, late = uint64(e.Value), 40_000
			} else if L.Overruns > 0 {
				ov, late = uint64(L.Overruns), 1500 // scattered small overruns
			}
			add(telemetry.Sample{T: ns(t), Side: telemetry.SideServer, Kind: telemetry.KindSender, Overruns: telemetry.U64(ov),
				MaxLateUs: telemetry.F64(late), SendMbps: telemetry.F64(sc.BitrateMbps)})
			cpu := 20.0
			if e := near(t, "cpu"); e != nil {
				cpu = e.Value
			}
			add(telemetry.Sample{T: ns(t), Side: telemetry.SideServer, Kind: telemetry.KindCPU, CPUPct: telemetry.F64(cpu)})
			add(telemetry.Sample{T: ns(t), Side: telemetry.SideController, Kind: telemetry.KindRTT, RTTms: telemetry.F64(2 + rng.Float64())})
		}
		if hasEffect(sc, "router") {
			for t := 0.0; t < end; t += 1000 {
				other := 2.0
				who := ""
				if e := near(t, "router"); e != nil {
					other, who = e.Value, e.Who
				}
				add(telemetry.Sample{T: ns(t), Side: telemetry.SideController, Kind: telemetry.KindRouter,
					OtherMbps: telemetry.F64(other), TopTalker: who})
			}
		}
	}
	// Point events.
	for _, s := range stalls {
		for _, e := range sc.AtEachStall {
			if e.Kind != "event" {
				continue
			}
			side := e.Side
			if side == "" {
				side = telemetry.SideController
			}
			add(telemetry.Sample{T: ns(s.AtS*1000 + e.OffsetMs), Side: side, Kind: telemetry.KindEvent, Event: e.Event})
		}
	}
	// Account for sender overruns in segment stats as the real sender would.
	for i := range run.Telemetry {
		if t := &run.Telemetry[i]; t.Kind == telemetry.KindSender && t.Overruns != nil && len(run.Segments) > 0 {
			run.Segments[0].Sender.Overruns += *t.Overruns
		}
	}
	return run
}

func hasEffect(sc *Scenario, kind string) bool {
	for _, e := range sc.AtEachStall {
		if e.Kind == kind {
			return true
		}
	}
	return false
}
