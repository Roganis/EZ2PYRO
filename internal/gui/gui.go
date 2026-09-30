// Package gui is a small local web interface for linkdoctor. It runs inside
// the same static binary: an HTTP server bound to 127.0.0.1 serves a
// single-page app and a JSON API that drives the existing host (serve) and
// test (run) code. No toolkit, no cgo — it opens in the system browser.
package gui

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/roganis/ez2pyro/internal/agent"
	"github.com/roganis/ez2pyro/internal/model"
	"github.com/roganis/ez2pyro/internal/modes"
	"github.com/roganis/ez2pyro/internal/proto"
	"github.com/roganis/ez2pyro/internal/report"
	"github.com/roganis/ez2pyro/internal/router/freebox"
)

//go:embed assets/*
var assets embed.FS

// Options configures the GUI.
type Options struct {
	Addr   string // listen address, default 127.0.0.1:0 (random port)
	OutDir string // run folders, default "runs"
	Logf   func(format string, args ...any)
}

// App is the GUI backend.
type App struct {
	opt    Options
	token  string
	outAbs string
	ln     net.Listener

	mu   sync.Mutex
	host hostState
	test testState
}

type hostState struct {
	Running bool     `json:"running"`
	Addrs   []string `json:"addrs"`
	Log     []string `json:"log"`
	Error   string   `json:"error,omitempty"`
	cancel  context.CancelFunc
	done    chan struct{}
}

// TestParams is what the page sends to start a test.
type TestParams struct {
	Peer        string  `json:"peer"`
	Mode        string  `json:"mode"`
	BitrateMbps float64 `json:"bitrate_mbps"`
	DurationMin float64 `json:"duration_min"`
	FPS         int     `json:"fps"`
	BudgetMs    float64 `json:"budget_ms"`
	PktSize     int     `json:"pkt_size"`
	Freebox     bool    `json:"freebox"`
}

type testResult struct {
	Dir         string             `json:"dir"`
	ReportURL   string             `json:"report_url"`
	Summary     model.Summary      `json:"summary"`
	Verdicts    []model.Verdict    `json:"verdicts"`
	Ramp        *model.RampResult  `json:"ramp,omitempty"`
	Periodicity *model.Periodicity `json:"periodicity,omitempty"`
	Warnings    []string           `json:"warnings,omitempty"`
}

type testState struct {
	State    string           `json:"state"` // idle, running, stopping, done, error
	Params   TestParams       `json:"params"`
	Started  int64            `json:"started_unix_ms"`
	Progress []modes.Progress `json:"progress"`
	Steps    []model.RampStep `json:"steps"`
	Log      []string         `json:"log"`
	Result   *testResult      `json:"result,omitempty"`
	Error    string           `json:"error,omitempty"`
	cancel   context.CancelFunc
}

const (
	maxLog      = 400
	maxProgress = 3600
)

// New creates the app and binds its listener.
func New(opt Options) (*App, error) {
	if opt.Addr == "" {
		opt.Addr = "127.0.0.1:0"
	}
	if opt.OutDir == "" {
		opt.OutDir = "runs"
	}
	if opt.Logf == nil {
		opt.Logf = func(string, ...any) {}
	}
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	a := &App{opt: opt, token: hex.EncodeToString(b)}
	a.test.State = "idle"
	var err error
	if a.outAbs, err = filepath.Abs(opt.OutDir); err != nil {
		return nil, err
	}
	if a.ln, err = net.Listen("tcp", opt.Addr); err != nil {
		return nil, err
	}
	return a, nil
}

// URL is the address to open in a browser (it carries the session token).
func (a *App) URL() string {
	return fmt.Sprintf("http://%s/?t=%s", a.ln.Addr(), a.token)
}

// Serve runs the HTTP server until ctx ends, then stops any host or test.
func (a *App) Serve(ctx context.Context) error {
	srv := &http.Server{Handler: a.routes(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		a.stopTest()
		a.stopHost()
		sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	if err := srv.Serve(a.ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func (a *App) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", a.page)
	mux.HandleFunc("GET /assets/{file}", a.asset)
	mux.HandleFunc("GET /api/state", a.api(a.state))
	mux.HandleFunc("POST /api/host/start", a.api(a.hostStart))
	mux.HandleFunc("POST /api/host/stop", a.api(a.hostStop))
	mux.HandleFunc("POST /api/test/start", a.api(a.testStart))
	mux.HandleFunc("POST /api/test/stop", a.api(a.testStop))
	mux.HandleFunc("POST /api/test/reset", a.api(a.testReset))
	mux.HandleFunc("GET /api/runs", a.api(a.runs))
	mux.HandleFunc("POST /api/compare", a.api(a.compare))
	mux.HandleFunc("GET /runs/{run}/{file}", a.runFile)
	return a.guard(mux)
}

// guard rejects requests whose Host is not this loopback server, so a web
// page elsewhere cannot reach the API through DNS rebinding.
func (a *App) guard(next http.Handler) http.Handler {
	_, port, _ := net.SplitHostPort(a.ln.Addr().String())
	allowed := map[string]bool{"127.0.0.1:" + port: true, "localhost:" + port: true, "[::1]:" + port: true}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !allowed[r.Host] {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

// api wraps JSON endpoints: they need the session token (sent by the page
// in a header, which cross-site pages cannot set without CORS approval).
func (a *App) api(fn func(r *http.Request) (any, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Token") != a.token {
			http.Error(w, "missing or wrong session token; reopen the link printed by linkdoctor", http.StatusForbidden)
			return
		}
		v, err := fn(r)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		_ = json.NewEncoder(w).Encode(v)
	}
}

func (a *App) page(w http.ResponseWriter, r *http.Request) {
	b, _ := assets.ReadFile("assets/index.html")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(b)
}

func (a *App) asset(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("file")
	b, err := assets.ReadFile("assets/" + name)
	if err != nil || strings.Contains(name, "/") {
		http.NotFound(w, r)
		return
	}
	switch filepath.Ext(name) {
	case ".js":
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	case ".css":
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
	}
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(b)
}

// runFile serves report.html / compare.html from a run folder.
func (a *App) runFile(w http.ResponseWriter, r *http.Request) {
	run, file := r.PathValue("run"), r.PathValue("file")
	if file != report.ReportHTML && file != "compare.html" || !validRunName(run) {
		http.NotFound(w, r)
		return
	}
	http.ServeFile(w, r, filepath.Join(a.outAbs, run, file))
}

func validRunName(s string) bool {
	return s != "" && s != "." && s != ".." && !strings.ContainsAny(s, `/\`)
}

type state struct {
	Version  string    `json:"version"`
	Hostname string    `json:"hostname"`
	OS       string    `json:"os"`
	OutDir   string    `json:"out_dir"`
	Host     hostState `json:"host"`
	Test     testState `json:"test"`
}

func (a *App) state(r *http.Request) (any, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	st := state{Version: agent.Version, OS: runtime.GOOS, OutDir: a.outAbs, Host: a.host, Test: a.test}
	st.Hostname, _ = os.Hostname()
	st.Host.Addrs = agent.LocalAddrs()
	// Copy slices so the JSON encoder never races with writers.
	st.Host.Log = append([]string(nil), a.host.Log...)
	st.Test.Log = append([]string(nil), a.test.Log...)
	st.Test.Progress = append([]modes.Progress(nil), a.test.Progress...)
	st.Test.Steps = append([]model.RampStep(nil), a.test.Steps...)
	return st, nil
}

// ---- host (serve) ----

func (a *App) hostLog(format string, args ...any) {
	line := time.Now().Format("15:04:05 ") + fmt.Sprintf(format, args...)
	a.mu.Lock()
	a.host.Log = appendCapped(a.host.Log, line)
	a.mu.Unlock()
}

func (a *App) hostStart(r *http.Request) (any, error) {
	a.mu.Lock()
	if a.host.Running {
		a.mu.Unlock()
		return map[string]bool{"ok": true}, nil
	}
	a.mu.Unlock()
	srv, err := agent.Listen(agent.ServerOptions{Logf: a.hostLog})
	if err != nil {
		msg := fmt.Sprintf("cannot listen on the linkdoctor ports (%v). Is another linkdoctor already hosting on this PC?", err)
		a.mu.Lock()
		a.host.Error = msg
		a.mu.Unlock()
		return nil, errors.New(msg)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	a.mu.Lock()
	a.host.Running, a.host.Error, a.host.cancel, a.host.done = true, "", cancel, done
	a.mu.Unlock()
	a.hostLog("hosting on TCP %d, UDP %d/%d — waiting for the Deck", proto.DefaultControlPort, proto.DefaultDataPort, proto.DefaultProbePort)
	go func() {
		defer close(done)
		if err := srv.Serve(ctx); err != nil {
			a.hostLog("error: %v", err)
		}
		a.mu.Lock()
		a.host.Running = false
		a.mu.Unlock()
		a.hostLog("stopped hosting")
	}()
	return map[string]bool{"ok": true}, nil
}

func (a *App) hostStop(r *http.Request) (any, error) {
	a.stopHost()
	return map[string]bool{"ok": true}, nil
}

func (a *App) stopHost() {
	a.mu.Lock()
	cancel, done := a.host.cancel, a.host.done
	a.host.cancel = nil
	a.mu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
}

// ---- test (run) ----

// lineWriter turns progress text into log lines.
type lineWriter struct {
	a   *App
	buf []byte
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for {
		i := strings.IndexByte(string(w.buf), '\n')
		if i < 0 {
			break
		}
		line := strings.TrimRight(string(w.buf[:i]), "\r")
		w.buf = w.buf[i+1:]
		if strings.TrimSpace(line) == "" {
			continue
		}
		w.a.mu.Lock()
		w.a.test.Log = appendCapped(w.a.test.Log, line)
		w.a.mu.Unlock()
	}
	return len(p), nil
}

func (p *TestParams) validate() error {
	p.Peer = strings.TrimSpace(p.Peer)
	if p.Peer == "" {
		return errors.New("enter the address of the host PC (shown in its Host tab)")
	}
	if net.ParseIP(p.Peer) == nil {
		if _, err := net.LookupHost(p.Peer); err != nil {
			return fmt.Errorf("%q is not an IP address or a known host name", p.Peer)
		}
	}
	switch p.Mode {
	case modes.Soak, modes.Ramp, modes.Live:
	default:
		return fmt.Errorf("unknown test type %q", p.Mode)
	}
	if p.BitrateMbps == 0 {
		p.BitrateMbps = 150
	}
	if p.BitrateMbps < 10 || p.BitrateMbps > 1000 {
		return errors.New("bitrate must be between 10 and 1000 Mbit/s")
	}
	if p.Mode == modes.Soak && (p.DurationMin <= 0 || p.DurationMin > 24*60) {
		return errors.New("duration must be between 1 minute and 24 hours")
	}
	if p.FPS == 0 {
		p.FPS = 60
	}
	if p.PktSize == 0 {
		p.PktSize = 1200
	}
	return nil
}

func (a *App) testStart(r *http.Request) (any, error) {
	var p TestParams
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&p); err != nil {
		return nil, fmt.Errorf("bad request: %w", err)
	}
	if err := p.validate(); err != nil {
		return nil, err
	}
	a.mu.Lock()
	if a.test.State == "running" || a.test.State == "stopping" {
		a.mu.Unlock()
		return nil, errors.New("a test is already running")
	}
	ctx, cancel := context.WithCancel(context.Background())
	a.test = testState{State: "running", Params: p, Started: time.Now().UnixMilli(), cancel: cancel}
	a.mu.Unlock()

	out := &lineWriter{a: a}
	opt := modes.Options{
		Controller: agent.ControllerOptions{Peer: p.Peer},
		Mode:       p.Mode,
		Traffic: proto.Config{BitrateMbps: p.BitrateMbps, FPS: p.FPS, FrameJitter: 0.2, BurstSpread: 0.3,
			PktSize: p.PktSize, Profile: proto.ProfilePyrowave},
		Duration:   time.Duration(p.DurationMin * float64(time.Minute)),
		Budget:     time.Duration(p.BudgetMs * float64(time.Millisecond)),
		OutDir:     a.outAbs,
		LiveReport: true,
		Out:        out,
		OnProgress: func(pr modes.Progress) {
			a.mu.Lock()
			a.test.Progress = append(a.test.Progress, pr)
			if len(a.test.Progress) > maxProgress {
				a.test.Progress = a.test.Progress[len(a.test.Progress)-maxProgress:]
			}
			a.mu.Unlock()
		},
		OnStep: func(s model.RampStep) {
			a.mu.Lock()
			a.test.Steps = append(a.test.Steps, s)
			a.mu.Unlock()
		},
	}
	if p.Mode != modes.Soak {
		opt.Duration = 0
	}
	if p.Freebox {
		opt.Router = freebox.New(freebox.Options{AppVersion: agent.Version, Out: out})
	}
	go func() {
		run, dir, err := modes.Execute(ctx, opt)
		a.mu.Lock()
		defer a.mu.Unlock()
		a.test.cancel = nil
		if run == nil {
			a.test.State, a.test.Error = "error", friendlyError(err)
			return
		}
		res := &testResult{Summary: run.Summary, Ramp: run.Ramp, Periodicity: run.Periodicity,
			Warnings: run.Environment.Warnings, Verdicts: run.Verdicts}
		if dir != "" {
			res.Dir = filepath.Base(dir)
			res.ReportURL = "/runs/" + res.Dir + "/" + report.ReportHTML
		}
		a.test.State, a.test.Result = "done", res
		if err != nil {
			a.test.Error = "The test ended early: " + err.Error()
		}
	}()
	return map[string]bool{"ok": true}, nil
}

func friendlyError(err error) string {
	if err == nil {
		return "the test stopped without results"
	}
	s := err.Error()
	if strings.Contains(s, "cannot reach linkdoctor serve") {
		s += ". Check that the PC shows “Hosting” in its Host tab, that the address is right, and that the Windows firewall allowed linkdoctor on private networks."
	}
	return s
}

func (a *App) testStop(r *http.Request) (any, error) {
	a.mu.Lock()
	if a.test.State == "running" && a.test.cancel != nil {
		a.test.State = "stopping"
		a.test.cancel()
	}
	a.mu.Unlock()
	return map[string]bool{"ok": true}, nil
}

func (a *App) testReset(r *http.Request) (any, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.test.State == "running" || a.test.State == "stopping" {
		return nil, errors.New("a test is still running")
	}
	a.test = testState{State: "idle"}
	return map[string]bool{"ok": true}, nil
}

func (a *App) stopTest() {
	a.mu.Lock()
	cancel := a.test.cancel
	a.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// ---- results ----

type runInfo struct {
	Name       string         `json:"name"`
	Started    string         `json:"started"`
	Mode       string         `json:"mode"`
	Bitrate    float64        `json:"bitrate_mbps"`
	DurationS  float64        `json:"duration_s"`
	Summary    model.Summary  `json:"summary"`
	Top        *model.Verdict `json:"top,omitempty"`
	HasCompare bool           `json:"has_compare"`
}

func (a *App) runs(r *http.Request) (any, error) {
	ents, err := os.ReadDir(a.outAbs)
	if err != nil {
		if os.IsNotExist(err) {
			return []runInfo{}, nil
		}
		return nil, err
	}
	out := []runInfo{}
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(a.outAbs, e.Name(), report.RunJSON))
		if err != nil {
			continue
		}
		var run model.Run
		if json.Unmarshal(b, &run) != nil {
			continue
		}
		ri := runInfo{Name: e.Name(), Started: run.Started, Mode: run.Options.Mode, Bitrate: run.Options.BitrateMbps,
			DurationS: run.DurationS, Summary: run.Summary}
		if len(run.Verdicts) > 0 {
			ri.Top = &run.Verdicts[0]
		}
		if _, err := os.Stat(filepath.Join(a.outAbs, e.Name(), "compare.html")); err == nil {
			ri.HasCompare = true
		}
		out = append(out, ri)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Started > out[j].Started })
	return out, nil
}

func (a *App) compare(r *http.Request) (any, error) {
	var req struct{ A, B string }
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<12)).Decode(&req); err != nil {
		return nil, err
	}
	if !validRunName(req.A) || !validRunName(req.B) || req.A == req.B {
		return nil, errors.New("pick two different runs")
	}
	if _, err := modes.Compare(filepath.Join(a.outAbs, req.A), filepath.Join(a.outAbs, req.B), "", nil); err != nil {
		return nil, err
	}
	return map[string]string{"url": "/runs/" + req.B + "/compare.html"}, nil
}

func appendCapped(s []string, line string) []string {
	s = append(s, line)
	if len(s) > maxLog {
		s = s[len(s)-maxLog:]
	}
	return s
}
