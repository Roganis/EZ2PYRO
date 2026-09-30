// Package report writes a run folder — run.json, frames.csv, seconds.csv,
// stalls.csv, telemetry.jsonl and a self-contained report.html — and reads
// it back for `linkdoctor analyze` and `compare`.
package report

import (
	"bufio"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/roganis/ez2pyro/internal/model"
	"github.com/roganis/ez2pyro/internal/receiver"
	"github.com/roganis/ez2pyro/internal/telemetry"
)

// File names inside a run folder.
const (
	RunJSON    = "run.json"
	FramesCSV  = "frames.csv"
	SecondsCSV = "seconds.csv"
	StallsCSV  = "stalls.csv"
	TeleJSONL  = "telemetry.jsonl"
	PacketsCSV = "packets.csv"
	ReportHTML = "report.html"
)

// NewRunDir creates runs/<date>_<time>/ under base.
func NewRunDir(base string, t time.Time) (string, error) {
	name := t.Format("2006-01-02_1504")
	dir := filepath.Join(base, name)
	for i := 2; ; i++ {
		if _, err := os.Stat(dir); os.IsNotExist(err) {
			break
		}
		dir = filepath.Join(base, fmt.Sprintf("%s_%d", name, i))
	}
	return dir, os.MkdirAll(dir, 0o755)
}

func f64(v float64, prec int) string { return strconv.FormatFloat(v, 'f', prec, 64) }

// WriteData writes every data file of a run (not the HTML).
func WriteData(dir string, run *model.Run) error {
	if err := writeJSON(filepath.Join(dir, RunJSON), run); err != nil {
		return err
	}
	if err := writeCSV(filepath.Join(dir, FramesCSV),
		[]string{"seg", "frame_id", "t_ms", "bitrate_mbps", "pkts", "recv", "lost", "complete", "late", "first_ms", "last_ms", "delay_ms"},
		len(run.Frames), func(i int) []string {
			f := &run.Frames[i]
			return []string{strconv.Itoa(f.Segment), strconv.FormatUint(uint64(f.ID), 10), f64(f.T, 3), f64(f.BitrateMbps, 1),
				strconv.Itoa(f.Pkts), strconv.Itoa(f.Recv), strconv.Itoa(f.Lost()), b01(f.Complete), b01(f.Late),
				f64(f.FirstMs, 3), f64(f.LastMs, 3), f64(f.DelayMs, 3)}
		}); err != nil {
		return err
	}
	if err := writeCSV(filepath.Join(dir, SecondsCSV),
		[]string{"t_s", "mbps", "loss_pct", "p50_delay_ms", "p99_delay_ms", "frames", "late", "rtt_ms"},
		len(run.Seconds), func(i int) []string {
			s := &run.Seconds[i]
			return []string{strconv.Itoa(s.T), f64(s.Mbps, 2), f64(s.LossPct, 3), f64(s.P50DelayMs, 2), f64(s.P99DelayMs, 2),
				strconv.Itoa(s.Frames), strconv.Itoa(s.Late), f64(s.RTTms, 2)}
		}); err != nil {
		return err
	}
	if err := writeCSV(filepath.Join(dir, StallsCSV),
		[]string{"index", "start_ms", "end_ms", "duration_ms", "worst_delay_ms", "late_frames", "packets_lost", "gap", "bitrate_mbps", "causes", "nearby"},
		len(run.Stalls), func(i int) []string {
			s := &run.Stalls[i]
			return []string{strconv.Itoa(s.Index), f64(s.Start, 1), f64(s.End, 1), f64(s.DurationMs, 1), f64(s.WorstDelay, 2),
				strconv.Itoa(s.LateFrames), strconv.Itoa(s.PacketsLost), b01(s.Gap), f64(s.BitrateMbps, 1),
				strings.Join(s.Causes, ";"), strings.Join(s.Nearby, ";")}
		}); err != nil {
		return err
	}
	return writeTelemetry(filepath.Join(dir, TeleJSONL), run.Telemetry)
}

// WritePackets writes raw packets (--raw), times relative to t0 on the controller clock.
func WritePackets(dir string, pk []receiver.RawPacket, t0 int64) error {
	return writeCSV(filepath.Join(dir, PacketsCSV), []string{"seq", "frame_id", "pkt_idx", "send_ns", "recv_ns_rel", "size"},
		len(pk), func(i int) []string {
			p := &pk[i]
			return []string{strconv.FormatUint(p.Seq, 10), strconv.FormatUint(uint64(p.FrameID), 10), strconv.Itoa(int(p.PktIdx)),
				strconv.FormatInt(p.SendTS, 10), strconv.FormatInt(p.RecvTS-t0, 10), strconv.Itoa(p.Size)}
		})
}

func b01(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

func writeCSV(path string, header []string, n int, row func(int) []string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	bw := bufio.NewWriterSize(f, 1<<20)
	w := csv.NewWriter(bw)
	_ = w.Write(header)
	for i := 0; i < n; i++ {
		_ = w.Write(row(i))
	}
	w.Flush()
	if err := w.Error(); err != nil {
		f.Close()
		return err
	}
	if err := bw.Flush(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func writeTelemetry(path string, s []telemetry.Sample) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	bw := bufio.NewWriterSize(f, 1<<20)
	enc := json.NewEncoder(bw)
	for i := range s {
		if err := enc.Encode(&s[i]); err != nil {
			f.Close()
			return err
		}
	}
	if err := bw.Flush(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// Load reads a run folder back (run.json, frames.csv, telemetry.jsonl).
func Load(dir string) (*model.Run, error) {
	b, err := os.ReadFile(filepath.Join(dir, RunJSON))
	if err != nil {
		return nil, err
	}
	var run model.Run
	if err := json.Unmarshal(b, &run); err != nil {
		return nil, fmt.Errorf("%s: %w", RunJSON, err)
	}
	if run.Frames, err = loadFrames(filepath.Join(dir, FramesCSV)); err != nil {
		return nil, err
	}
	if run.Telemetry, err = loadTelemetry(filepath.Join(dir, TeleJSONL)); err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	return &run, nil
}

func loadFrames(path string) ([]model.Frame, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r := csv.NewReader(bufio.NewReaderSize(f, 1<<20))
	r.ReuseRecord = true
	hdr, err := r.Read()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", FramesCSV, err)
	}
	col := map[string]int{}
	for i, h := range hdr {
		col[h] = i
	}
	for _, need := range []string{"seg", "frame_id", "t_ms", "pkts", "recv", "complete", "delay_ms"} {
		if _, ok := col[need]; !ok {
			return nil, fmt.Errorf("%s: missing column %q", FramesCSV, need)
		}
	}
	get := func(rec []string, name string) string {
		if i, ok := col[name]; ok && i < len(rec) {
			return rec[i]
		}
		return ""
	}
	pf := func(s string) float64 { v, _ := strconv.ParseFloat(s, 64); return v }
	pi := func(s string) int { v, _ := strconv.Atoi(s); return v }
	var out []model.Frame
	for {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", FramesCSV, err)
		}
		out = append(out, model.Frame{
			Segment: pi(get(rec, "seg")), ID: uint32(pi(get(rec, "frame_id"))), T: pf(get(rec, "t_ms")),
			BitrateMbps: pf(get(rec, "bitrate_mbps")), Pkts: pi(get(rec, "pkts")), Recv: pi(get(rec, "recv")),
			Complete: get(rec, "complete") == "1", Late: get(rec, "late") == "1",
			FirstMs: pf(get(rec, "first_ms")), LastMs: pf(get(rec, "last_ms")), DelayMs: pf(get(rec, "delay_ms")),
		})
	}
	return out, nil
}

func loadTelemetry(path string) ([]telemetry.Sample, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	dec := json.NewDecoder(bufio.NewReaderSize(f, 1<<20))
	var out []telemetry.Sample
	for {
		var s telemetry.Sample
		if err := dec.Decode(&s); err == io.EOF {
			break
		} else if err != nil {
			return out, fmt.Errorf("%s: %w", TeleJSONL, err)
		}
		out = append(out, s)
	}
	return out, nil
}
