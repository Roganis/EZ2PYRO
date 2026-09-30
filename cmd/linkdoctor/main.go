// Command linkdoctor measures a LAN link with Pyrowave-shaped traffic, counts
// frames that would arrive too late to display and explains each stall with
// Wi-Fi telemetry. Run `linkdoctor serve` on the host and
// `linkdoctor run --peer <host-ip>` on the Deck.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strings"
	"time"

	"github.com/roganis/ez2pyro/internal/agent"
	"github.com/roganis/ez2pyro/internal/gui"
	"github.com/roganis/ez2pyro/internal/modes"
	"github.com/roganis/ez2pyro/internal/proto"
	"github.com/roganis/ez2pyro/internal/router/freebox"
	"github.com/roganis/ez2pyro/internal/telemetry"
)

const usage = `linkdoctor — find out why your Remote Play / Pyrowave stream stutters

Usage:
  linkdoctor                                 opens the GUI in your browser (same as "gui")
  linkdoctor gui     [flags]                 simple point-and-click interface
  linkdoctor serve   [flags]                 on the host (Windows or Linux PC)
  linkdoctor run     --peer <host-ip> [flags] on the Deck; starts a test
  linkdoctor analyze <run folder> [--budget ms]
  linkdoctor compare <run folder A> <run folder B> [--out file.html]
  linkdoctor version

Examples (on the Deck):
  linkdoctor run --peer 192.168.1.20 --mode ramp
  linkdoctor run --peer 192.168.1.20 --mode soak --bitrate 200 --duration 30m
  linkdoctor run --peer 192.168.1.20 --mode live
  linkdoctor compare runs/2026-09-30_2100 runs/2026-09-30_2145

Run "linkdoctor <command> -h" for the flags of a command.
`

func main() {
	log.SetFlags(log.Ltime)
	var err error
	if len(os.Args) < 2 {
		// Double-clicked (or run without arguments): open the GUI.
		if err = cmdGUI(nil); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		return
	}
	switch os.Args[1] {
	case "gui":
		err = cmdGUI(os.Args[2:])
	case "serve":
		err = cmdServe(os.Args[2:])
	case "run":
		err = cmdRun(os.Args[2:])
	case "analyze":
		err = cmdAnalyze(os.Args[2:])
	case "compare":
		err = cmdCompare(os.Args[2:])
	case "version", "--version", "-v":
		fmt.Println("linkdoctor", agent.Version)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(0)
		}
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

type ports struct{ control, data, probe int }

func (p *ports) register(fs *flag.FlagSet) {
	fs.IntVar(&p.control, "port", proto.DefaultControlPort, "control port (TCP)")
	fs.IntVar(&p.data, "data-port", proto.DefaultDataPort, "data port (UDP)")
	fs.IntVar(&p.probe, "probe-port", proto.DefaultProbePort, "probe port (UDP)")
}

func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	var p ports
	p.register(fs)
	bind := fs.String("bind", "", "address to listen on (default: all)")
	iface := fs.String("iface", "", "interface carrying the test traffic (default: auto)")
	wifi := fs.String("wifi-iface", "", "Wi-Fi interface to watch if the host is on Wi-Fi (default: auto)")
	noTel := fs.Bool("no-telemetry", false, "do not collect host telemetry")
	if err := fs.Parse(args); err != nil {
		return err
	}
	srv, err := agent.Listen(agent.ServerOptions{
		Bind: *bind, ControlPort: p.control, DataPort: p.data, ProbePort: p.probe,
		Iface: *iface, WifiIface: *wifi, NoTelemetry: *noTel,
	})
	if err != nil {
		return fmt.Errorf("cannot listen: %w", err)
	}
	log.Printf("linkdoctor %s serving on TCP %d, UDP %d/%d", agent.Version, p.control, p.data, p.probe)
	for _, a := range agent.LocalAddrs() {
		log.Printf("  on the Deck run: linkdoctor run --peer %s", a)
	}
	ctx, cancel := modes.SignalContext()
	defer cancel()
	return srv.Serve(ctx)
}

func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	var p ports
	p.register(fs)
	peer := fs.String("peer", "", "`ip` address of the host running \"linkdoctor serve\" (required)")
	mode := fs.String("mode", "soak", "test mode: soak, ramp or live")
	bitrate := fs.Float64("bitrate", 150, "average bitrate in Mbit/s (10–1000)")
	fps := fs.Int("fps", 60, "frame rate (30–144)")
	jitter := fs.Float64("frame-jitter", 0.2, "random ± variation of frame size (0–1)")
	spread := fs.Float64("burst-spread", 0.3, "share of the frame interval each burst is spread over (0 = back-to-back)")
	pkt := fs.Int("pkt-size", 1200, "UDP payload size in bytes (200–1472)")
	profile := fs.String("profile", proto.ProfilePyrowave, "traffic profile: pyrowave or even")
	duration := fs.Duration("duration", 30*time.Minute, "soak duration (e.g. 30m, 90s)")
	budget := fs.Float64("budget", 0, "late-frame budget in ms (default: one frame interval)")
	rampStart := fs.Float64("ramp-start", 50, "ramp: first step in Mbit/s")
	rampStop := fs.Float64("ramp-stop", 500, "ramp: last step in Mbit/s")
	rampStep := fs.Float64("ramp-step", 25, "ramp: step size in Mbit/s")
	stepDur := fs.Duration("step-duration", 10*time.Second, "ramp: duration of each step")
	out := fs.String("out", "runs", "folder for run results")
	raw := fs.Bool("raw", false, "also keep every raw packet (packets.csv); short runs only")
	jsonOut := fs.Bool("json", false, "print the summary as JSON on stdout")
	report := fs.Bool("report", false, "live mode: also write a report when stopped")
	iface := fs.String("iface", "", "interface carrying the test traffic (default: auto)")
	wifi := fs.String("wifi-iface", "", "Wi-Fi interface to watch (default: auto)")
	noTel := fs.Bool("no-telemetry", false, "do not collect telemetry on this side")
	fbx := fs.Bool("freebox", false, "also sample the Freebox (read-only; asks for approval on the box the first time)")
	fbxURL := fs.String("freebox-url", freebox.DefaultURL, "Freebox address")
	loopback := fs.Bool("loopback", false, "self-test: start a server in this process and test over localhost")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if *peer == "" && !*loopback {
		fs.Usage()
		return errors.New("--peer is required (the IP address printed by `linkdoctor serve`)")
	}
	switch *mode {
	case modes.Soak, modes.Ramp, modes.Live:
	default:
		return fmt.Errorf("unknown --mode %q (soak, ramp, live; compare is its own command)", *mode)
	}
	if *bitrate < 10 || *bitrate > 1000 {
		return fmt.Errorf("--bitrate %.0f out of range 10–1000", *bitrate)
	}

	var progress io.Writer = os.Stdout
	if *jsonOut {
		progress = os.Stderr
	}
	ctx, cancel := modes.SignalContext()
	defer cancel()

	if *loopback {
		*peer = "127.0.0.1"
		srv, err := agent.Listen(agent.ServerOptions{Bind: "127.0.0.1", ControlPort: p.control, DataPort: p.data, ProbePort: p.probe,
			NoTelemetry: *noTel, Logf: func(string, ...any) {}})
		if err != nil {
			return fmt.Errorf("loopback server: %w", err)
		}
		sctx, stop := context.WithCancel(context.Background())
		defer stop()
		go srv.Serve(sctx)
	}

	opt := modes.Options{
		Controller: agent.ControllerOptions{
			Peer: *peer, ControlPort: p.control, DataPort: p.data, ProbePort: p.probe,
			Iface: *iface, WifiIface: *wifi, Raw: *raw, NoTelemetry: *noTel,
		},
		Mode: *mode,
		Traffic: proto.Config{
			BitrateMbps: *bitrate, FPS: *fps, FrameJitter: *jitter, BurstSpread: *spread,
			PktSize: *pkt, Profile: *profile,
		},
		Duration:  *duration,
		Budget:    time.Duration(*budget * float64(time.Millisecond)),
		RampStart: *rampStart, RampStop: *rampStop, RampStep: *rampStep, StepDuration: *stepDur,
		OutDir: *out, JSON: *jsonOut, LiveReport: *report, Out: progress,
	}
	if *mode != modes.Soak {
		opt.Duration = 0
	}
	if *fbx {
		opt.Router = freebox.New(freebox.Options{URL: *fbxURL, SelfMACs: wifiMACs(*wifi), AppVersion: agent.Version, Out: progress})
	}
	run, dir, err := modes.Execute(ctx, opt)
	if err != nil && run == nil {
		return err
	}
	if *jsonOut {
		if jerr := modes.PrintJSON(os.Stdout, run, dir); jerr != nil {
			return jerr
		}
	} else {
		modes.PrintSummary(os.Stdout, run, dir)
	}
	return err
}

// wifiMACs returns the MAC addresses of local Wi-Fi interfaces, so the
// Freebox module can tell the Deck apart from other stations.
func wifiMACs(prefer string) []string {
	var out []string
	ifs, _ := net.Interfaces()
	for _, ifc := range ifs {
		if len(ifc.HardwareAddr) == 0 {
			continue
		}
		if ifc.Name == prefer || telemetry.IfaceKind(ifc.Name) == "wifi" {
			out = append(out, strings.ToLower(ifc.HardwareAddr.String()))
		}
	}
	return out
}

func cmdGUI(args []string) error {
	fs := flag.NewFlagSet("gui", flag.ContinueOnError)
	addr := fs.String("addr", "127.0.0.1:0", "address for the local web interface (keep it on 127.0.0.1)")
	out := fs.String("out", "runs", "folder for run results")
	noBrowser := fs.Bool("no-browser", false, "don't open a browser; just print the address")
	if err := fs.Parse(args); err != nil {
		return err
	}
	app, err := gui.New(gui.Options{Addr: *addr, OutDir: *out})
	if err != nil {
		return err
	}
	url := app.URL()
	fmt.Printf("Link Doctor %s is running at:\n\n    %s\n\n", agent.Version, url)
	if *noBrowser {
		fmt.Println("Open that address in a browser on this machine.")
	} else if err := gui.OpenBrowser(url); err != nil {
		fmt.Println("Could not open a browser automatically; open the address above yourself.")
	} else {
		fmt.Println("Your browser should open now. If not, open the address above.")
	}
	fmt.Println("Keep this window open while you use Link Doctor. Close it (or press Ctrl+C) to quit.")
	ctx, cancel := modes.SignalContext()
	defer cancel()
	return app.Serve(ctx)
}

func cmdAnalyze(args []string) error {
	fs := flag.NewFlagSet("analyze", flag.ContinueOnError)
	budget := fs.Float64("budget", 0, "late-frame budget in ms (default: the run's own)")
	jsonOut := fs.Bool("json", false, "print the summary as JSON")
	if err := fs.Parse(reorder(args)); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: linkdoctor analyze <run folder> [--budget ms]")
	}
	dir := fs.Arg(0)
	run, err := modes.Analyze(dir, *budget)
	if err != nil {
		return err
	}
	if *jsonOut {
		return modes.PrintJSON(os.Stdout, run, dir)
	}
	modes.PrintSummary(os.Stdout, run, dir)
	return nil
}

func cmdCompare(args []string) error {
	fs := flag.NewFlagSet("compare", flag.ContinueOnError)
	out := fs.String("out", "", "output HTML file (default: <B>/compare.html)")
	if err := fs.Parse(reorder(args)); err != nil {
		return err
	}
	if fs.NArg() != 2 {
		return errors.New("usage: linkdoctor compare <run folder A> <run folder B>")
	}
	path, err := modes.Compare(fs.Arg(0), fs.Arg(1), *out, os.Stdout)
	if err != nil {
		return err
	}
	fmt.Println("\nreport:", path)
	return nil
}

// reorder moves flags before positional arguments so that
// `analyze runs/x --budget 20` works with the standard flag package.
func reorder(args []string) []string {
	var flags, pos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") {
			flags = append(flags, a)
			if !strings.Contains(a, "=") && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") && a != "--json" && a != "-json" {
				flags = append(flags, args[i+1])
				i++
			}
			continue
		}
		pos = append(pos, a)
	}
	return append(flags, pos...)
}
