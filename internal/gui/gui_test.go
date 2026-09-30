package gui

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/roganis/ez2pyro/internal/analysis"
	"github.com/roganis/ez2pyro/internal/report"
)

func newApp(t *testing.T) (*App, string) {
	t.Helper()
	a, err := New(Options{OutDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go a.Serve(ctx)
	return a, "http://" + a.ln.Addr().String()
}

func do(t *testing.T, method, url, token, body string) (*http.Response, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	if token != "" {
		req.Header.Set("X-Token", token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var m map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&m)
	return resp, m
}

func TestGuards(t *testing.T) {
	a, base := newApp(t)
	if r, _ := do(t, "GET", base+"/", "", ""); r.StatusCode != 200 {
		t.Fatalf("page: %d", r.StatusCode)
	}
	if r, _ := do(t, "GET", base+"/api/state", "", ""); r.StatusCode != 403 {
		t.Fatalf("state without token: %d", r.StatusCode)
	}
	if r, m := do(t, "GET", base+"/api/state", a.token, ""); r.StatusCode != 200 || m["test"] == nil {
		t.Fatalf("state: %d %v", r.StatusCode, m)
	}
	req, _ := http.NewRequest("GET", base+"/", nil)
	req.Host = "attacker.example"
	if r, _ := http.DefaultClient.Do(req); r.StatusCode != 403 {
		t.Fatalf("foreign Host header accepted: %d", r.StatusCode)
	}
	if r, _ := do(t, "GET", base+"/runs/..%2F..%2Fetc/report.html", "", ""); r.StatusCode != 404 {
		t.Fatalf("path traversal: %d", r.StatusCode)
	}
}

func TestStartValidation(t *testing.T) {
	a, base := newApp(t)
	for body, want := range map[string]string{
		`{"peer":"","mode":"ramp"}`:                              "address",
		`{"peer":"192.168.1.20","mode":"burn"}`:                  "unknown test type",
		`{"peer":"192.168.1.20","mode":"soak","duration_min":0}`: "duration",
		`{"peer":"192.168.1.20","mode":"live","bitrate_mbps":5}`: "bitrate",
	} {
		r, m := do(t, "POST", base+"/api/test/start", a.token, body)
		if r.StatusCode != 400 || !strings.Contains(m["error"].(string), want) {
			t.Errorf("%s → %d %v, want error containing %q", body, r.StatusCode, m, want)
		}
	}
}

func TestRunsAndCompare(t *testing.T) {
	a, base := newApp(t)
	for _, name := range []string{"2026-09-30_2100", "2026-09-30_2145"} {
		sc, err := analysis.LoadScenario("../../testdata/scenarios/clean.json")
		if err != nil {
			t.Fatal(err)
		}
		run := analysis.Synth(sc)
		run.Started = strings.Replace(name, "_", "T", 1) + ":00Z"
		analysis.Analyze(run)
		dir := filepath.Join(a.outAbs, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := report.Write(dir, run); err != nil {
			t.Fatal(err)
		}
	}
	req, _ := http.NewRequest("GET", base+"/api/runs", nil)
	req.Header.Set("X-Token", a.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var runs []runInfo
	_ = json.NewDecoder(resp.Body).Decode(&runs)
	resp.Body.Close()
	if len(runs) != 2 || runs[0].Name != "2026-09-30_2145" || runs[0].Top == nil {
		t.Fatalf("runs %+v", runs)
	}
	r, m := do(t, "POST", base+"/api/compare", a.token, `{"a":"2026-09-30_2100","b":"2026-09-30_2145"}`)
	if r.StatusCode != 200 {
		t.Fatalf("compare: %d %v", r.StatusCode, m)
	}
	if r, _ := do(t, "GET", base+m["url"].(string), "", ""); r.StatusCode != 200 {
		t.Fatalf("compare page: %d", r.StatusCode)
	}
	if r, _ := do(t, "GET", base+"/runs/2026-09-30_2100/report.html", "", ""); r.StatusCode != 200 {
		t.Fatalf("report page: %d", r.StatusCode)
	}
}
