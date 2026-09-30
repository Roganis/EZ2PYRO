package analysis

import (
	"flag"
	"path/filepath"
	"strings"
	"testing"

	"github.com/roganis/ez2pyro/internal/model"
)

// TestScenarios runs every synthetic scenario in testdata/scenarios and
// checks that the analysis reaches the expected top verdict.
func TestScenarios(t *testing.T) {
	files, err := filepath.Glob("../../testdata/scenarios/*.json")
	if err != nil || len(files) == 0 {
		t.Fatalf("no scenarios: %v", err)
	}
	covered := map[string]bool{}
	for _, f := range files {
		sc, err := LoadScenario(f)
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		t.Run(strings.TrimSuffix(filepath.Base(f), ".json"), func(t *testing.T) {
			run := Synth(sc)
			Analyze(run)
			if len(run.Verdicts) == 0 {
				t.Fatal("no verdicts")
			}
			top := run.Verdicts[0]
			covered[top.Rule] = true
			if top.Rule != sc.Expect.Top {
				for _, v := range run.Verdicts {
					t.Logf("verdict %s %d/%d: %s", v.Rule, v.Explained, v.Total, v.Evidence)
				}
				for _, s := range run.Stalls {
					t.Logf("stall %+v", s)
				}
				t.Fatalf("top verdict %q, want %q", top.Rule, sc.Expect.Top)
			}
			if sc.Expect.Stalls > 0 || sc.Expect.Top == "clean" {
				if len(run.Stalls) != sc.Expect.Stalls {
					t.Errorf("stalls %d, want %d", len(run.Stalls), sc.Expect.Stalls)
				}
			}
			if sc.Expect.MinShare > 0 && top.Share < sc.Expect.MinShare {
				t.Errorf("share %.2f < %.2f", top.Share, sc.Expect.MinShare)
			}
			if sc.Expect.Periodic && (run.Periodicity == nil || !run.Periodicity.Periodic) {
				t.Errorf("expected periodic stalls, got %+v", run.Periodicity)
			}
			if sc.Expect.Recommended > 0 && run.Summary.RecommendedMbps != sc.Expect.Recommended {
				t.Errorf("recommended %.0f, want %.0f", run.Summary.RecommendedMbps, sc.Expect.Recommended)
			}
			t.Logf("%s → %s: %s", sc.Name, top.Cause, top.Evidence)
		})
	}
	if f := flag.Lookup("test.run"); f != nil && f.Value.String() != "" {
		return // filtered run: coverage can't be judged
	}
	for _, r := range Rules {
		if !covered[r.ID] {
			t.Errorf("rule %q has no scenario reaching it as the top verdict", r.ID)
		}
	}
}

func frames(late ...bool) []model.Frame {
	out := make([]model.Frame, len(late))
	for i, l := range late {
		out[i] = model.Frame{ID: uint32(i), T: float64(i) * 1000 / 60, Late: l, Complete: !l, Pkts: 10, Recv: 10, DelayMs: 1}
		if l {
			out[i].Recv = 5
			out[i].DelayMs = -1
		}
	}
	return out
}

func TestDetectStalls(t *testing.T) {
	iv := func(int) float64 { return 1000.0 / 60 }
	// Two late frames are not a stall; three are.
	f := frames(false, true, true, false, true, true, true, false, false)
	st := DetectStalls(f, nil, iv)
	if len(st) != 1 || st[0].LateFrames != 3 || st[0].PacketsLost != 15 {
		t.Fatalf("stalls %+v", st)
	}
	// A gap alone is a stall; overlapping periods merge.
	st = DetectStalls(f, []model.Gap{{Start: 60, End: 120}, {Start: 1000, End: 1100}}, iv)
	if len(st) != 2 || !st[0].Gap || st[1].Start != 1000 {
		t.Fatalf("stalls %+v", st)
	}
}

func TestPeriodicity(t *testing.T) {
	var st []model.Stall
	for _, s := range []float64{10, 128, 247, 363, 482, 600} {
		st = append(st, model.Stall{Start: s * 1000})
	}
	p := Periodicity(st, nil)
	if p == nil || !p.Periodic || p.PeriodS < 115 || p.PeriodS > 120 {
		t.Fatalf("%+v", p)
	}
	st[3].Start = 300_000
	if p := Periodicity(st, nil); p.Periodic {
		t.Fatalf("irregular stalls flagged periodic: %+v", p)
	}
}

func TestAutocorr(t *testing.T) {
	sec := make([]model.Second, 600)
	for i := range sec {
		if i%45 == 7 {
			sec[i].Late = 5
		}
	}
	lag, r := autocorr(sec)
	if lag != 45 || r < 0.3 {
		t.Fatalf("lag %d r %.2f", lag, r)
	}
}

func TestRampRecommendation(t *testing.T) {
	segs := []model.Segment{}
	for i, clean := range []bool{true, true, true, false, true, false} {
		segs = append(segs, model.Segment{Clean: clean})
		segs[i].Config.BitrateMbps = 50 + 25*float64(i)
	}
	r := Ramp(segs)
	if r.HighestCleanMbps != 150 || r.RecommendedMbps != 105 || !r.StoppedEarly {
		t.Fatalf("%+v", r)
	}
}
