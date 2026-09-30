package report

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/roganis/ez2pyro/internal/analysis"
)

func synthRun(t *testing.T, name string) *analysis.Scenario {
	sc, err := analysis.LoadScenario(filepath.Join("../../testdata/scenarios", name))
	if err != nil {
		t.Fatal(err)
	}
	return sc
}

func TestWriteLoadRoundTrip(t *testing.T) {
	run := analysis.Synth(synthRun(t, "scan_periodic.json"))
	run.Started = "2026-09-30T21:00:00Z"
	analysis.Analyze(run)
	dir := t.TempDir()
	if err := Write(dir, run); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{RunJSON, FramesCSV, SecondsCSV, StallsCSV, TeleJSONL, ReportHTML} {
		if st, err := os.Stat(filepath.Join(dir, f)); err != nil || st.Size() == 0 {
			t.Fatalf("%s missing or empty: %v", f, err)
		}
	}
	back, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(back.Frames) != len(run.Frames) || len(back.Telemetry) != len(run.Telemetry) {
		t.Fatalf("frames %d/%d telemetry %d/%d", len(back.Frames), len(run.Frames), len(back.Telemetry), len(run.Telemetry))
	}
	analysis.Analyze(back)
	if back.Verdicts[0].Rule != run.Verdicts[0].Rule || len(back.Stalls) != len(run.Stalls) {
		t.Fatalf("re-analysis differs: %s/%d vs %s/%d", back.Verdicts[0].Rule, len(back.Stalls), run.Verdicts[0].Rule, len(run.Stalls))
	}
}

func TestHTMLSelfContained(t *testing.T) {
	for _, name := range []string{"capacity_ramp.json", "clean.json", "unexplained.json"} {
		run := analysis.Synth(synthRun(t, name))
		analysis.Analyze(run)
		b, err := RenderHTML(run)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if bytes.Contains(b, []byte("src=\"http")) || bytes.Contains(b, []byte("href=\"http")) {
			t.Fatalf("%s: report references external resources", name)
		}
		if !bytes.Contains(b, []byte("uPlot")) || !bytes.Contains(b, []byte("LD_DATA")) {
			t.Fatalf("%s: chart library or data not inlined", name)
		}
	}
}
