package analysis

import (
	"fmt"
	"math"
	"strings"

	"github.com/roganis/ez2pyro/internal/model"
	"github.com/roganis/ez2pyro/internal/telemetry"
)

// Correlation window around each stall, in ms.
const (
	WindowBefore = 2000.0
	WindowAfter  = 1000.0
)

// Ctx is what rules see.
type Ctx struct {
	Run  *model.Run
	Tele *Tele
}

// Hit is one stall a rule explains, with a short note.
type Hit struct {
	Stall *model.Stall
	Note  string
	Value float64 // rule-specific magnitude (dB drop, Mbit/s, ...)
}

// Rule is one row of the diagnosis table. Rules are data: add a row to
// Rules to teach the analysis a new pattern.
type Rule struct {
	ID      string
	Pattern string // what we look for, in words (shown in docs/report)
	Cause   string
	Fix     string
	// Stall reports whether the rule explains one stall. from/to is the
	// correlation window (2 s before start to 1 s after end).
	Stall func(c *Ctx, st *model.Stall, from, to float64) (bool, string, float64)
	// Evidence phrases the verdict from the stalls this rule explains.
	Evidence func(c *Ctx, hits []Hit, total int) string
	// Run, if set, reports a run-level finding that holds even without stalls.
	Run func(c *Ctx) (bool, string)
}

var scanEvents = events(telemetry.EventScanStart, telemetry.EventScanDone, telemetry.EventScanAborted, telemetry.EventSchedScan)

// Rules is the diagnosis table, in tie-break order.
var Rules = []Rule{
	{
		ID:      "scan",
		Pattern: "Stalls line up with scan events, often periodic (30–120 s)",
		Cause:   "Background Wi-Fi scanning",
		Fix:     "Disable Wi-Fi power management on the Deck and stay on one network (forget other saved networks)",
		Stall: func(c *Ctx, st *model.Stall, from, to float64) (bool, string, float64) {
			ev := c.Tele.Range(from, to, scanEvents)
			if len(ev) == 0 {
				return false, "", 0
			}
			best := math.Inf(1)
			for _, e := range ev {
				best = math.Min(best, math.Abs(e.t-st.Start))
			}
			return true, fmt.Sprintf("scan %.1f s from stall start", best/1000), best
		},
		Evidence: func(c *Ctx, hits []Hit, total int) string {
			near := 0
			for _, h := range hits {
				if h.Value <= 1000 {
					near++
				}
			}
			s := fmt.Sprintf("%d of %d stalls started within 1 s of a Wi-Fi scan (%d had a scan between 2 s before and 1 s after)", near, total, len(hits))
			if p := c.Run.Periodicity; p != nil && p.Periodic {
				s += fmt.Sprintf("; stalls repeat every %.0f ± %.0f s", p.PeriodS, p.StdS)
			}
			return s
		},
	},
	{
		ID:      "powersave",
		Pattern: "Many short stalls while power save is on",
		Cause:   "Wi-Fi power saving",
		Fix:     "Disable Wi-Fi power management (SteamOS: Settings → Developer → Wi-Fi power management off; or `iw dev wlan0 set power_save off`)",
		Stall: func(c *Ctx, st *model.Stall, from, to float64) (bool, string, float64) {
			ps := c.Tele.Last(st.Start, 5000, func(s *telemetry.Sample) bool {
				return s.Kind == telemetry.KindPowerSave && s.Side == telemetry.SideController && s.PowerSave != nil
			})
			if ps == nil || !*ps.PowerSave || st.DurationMs >= 500 {
				return false, "", 0
			}
			return true, "power save on", 0
		},
		Evidence: func(c *Ctx, hits []Hit, total int) string {
			return fmt.Sprintf("%d of %d stalls were short (< 0.5 s) while Wi-Fi power saving was on", len(hits), total)
		},
	},
	{
		ID:      "band",
		Pattern: "Band switches to 2.4 GHz around stalls",
		Cause:   "Band steering",
		Fix:     "Give the 2.4 GHz and 5 GHz networks separate names and join only the 5 GHz one",
		Stall: func(c *Ctx, st *model.Stall, from, to float64) (bool, string, float64) {
			link := kind(telemetry.KindLink, telemetry.SideController)
			before := c.Tele.Last(from, 30000, func(s *telemetry.Sample) bool { return link(s) && s.Band != "" })
			if before == nil || before.Band == "2.4" {
				return false, "", 0
			}
			for _, x := range c.Tele.Range(from, to, link) {
				if x.s.Band == "2.4" {
					return true, fmt.Sprintf("%s GHz → 2.4 GHz", before.Band), 0
				}
			}
			return false, "", 0
		},
		Evidence: func(c *Ctx, hits []Hit, total int) string {
			return fmt.Sprintf("%d of %d stalls coincided with the Deck moving to 2.4 GHz", len(hits), total)
		},
	},
	{
		ID:      "dfs",
		Pattern: "Frequency changes, disconnect of ~30–60 s",
		Cause:   "DFS radar channel change",
		Fix:     "Pick a fixed non-DFS 5 GHz channel on the router (36–48)",
		Stall: func(c *Ctx, st *model.Stall, from, to float64) (bool, string, float64) {
			ch := c.Tele.Range(from, to, events(telemetry.EventChSwitch, telemetry.EventFreqChange, telemetry.EventRouterChan))
			if len(ch) > 0 {
				note := ch[0].s.Event
				if ch[0].s.Detail != "" {
					note += " " + ch[0].s.Detail
				}
				return true, note, st.DurationMs
			}
			disc := c.Tele.Range(from, to, events(telemetry.EventDisconnect, telemetry.EventDeauth))
			if len(disc) > 0 && st.DurationMs >= 20000 {
				return true, fmt.Sprintf("disconnected for %.0f s", st.DurationMs/1000), st.DurationMs
			}
			return false, "", 0
		},
		Evidence: func(c *Ctx, hits []Hit, total int) string {
			longest := 0.0
			for _, h := range hits {
				longest = math.Max(longest, h.Value)
			}
			s := fmt.Sprintf("%d of %d stalls coincided with a channel change or a long disconnect (longest outage %.1f s)", len(hits), total, longest/1000)
			if ch := c.Run.Environment.Controller.Channel; telemetry.IsDFS(ch) {
				s += fmt.Sprintf("; the Deck was on DFS channel %d", ch)
			}
			return s
		},
	},
	{
		ID:      "signal",
		Pattern: "Signal drops > 6 dB or TX bitrate drops > 40%",
		Cause:   "Distance, obstruction or interference",
		Fix:     "Move closer to the Freebox, fix the channel, or add a repeater / access point",
		Stall: func(c *Ctx, st *model.Stall, from, to float64) (bool, string, float64) {
			link := kind(telemetry.KindLink, telemetry.SideController)
			bs := c.Tele.Values(st.Start-12000, from, link, signal)
			ws := c.Tele.Values(st.Start-1000, to, link, signal)
			if len(bs) >= 2 && len(ws) > 0 {
				if drop := median(bs) - minOf(ws); drop > 6 {
					return true, fmt.Sprintf("signal −%.0f dB", drop), drop
				}
			}
			br := c.Tele.Values(st.Start-12000, from, link, phyRate)
			wr := c.Tele.Values(st.Start-1000, to, link, phyRate)
			if len(br) >= 2 && len(wr) > 0 {
				b := median(br)
				if drop := (b - minOf(wr)) / b; b > 0 && drop > 0.4 {
					return true, fmt.Sprintf("PHY rate −%.0f%%", drop*100), 0
				}
			}
			return false, "", 0
		},
		Evidence: func(c *Ctx, hits []Hit, total int) string {
			worst := 0.0
			for _, h := range hits {
				worst = math.Max(worst, h.Value)
			}
			s := fmt.Sprintf("%d of %d stalls came with a signal drop > 6 dB or a PHY-rate drop > 40%%", len(hits), total)
			if worst > 0 {
				s += fmt.Sprintf(" (worst signal drop %.0f dB)", worst)
			}
			return s
		},
	},
	{
		ID:      "congestion",
		Pattern: "Retry spike with steady signal (or other devices busy, from the router)",
		Cause:   "Congestion from other networks or devices",
		Fix:     "Change the Wi-Fi channel and pause other traffic (downloads, TV boxes) while streaming",
		Stall: func(c *Ctx, st *model.Stall, from, to float64) (bool, string, float64) {
			if rt := c.Tele.Range(from, to, kind(telemetry.KindRouter, "")); len(rt) > 0 {
				for _, x := range rt {
					if x.s.OtherMbps != nil && *x.s.OtherMbps >= 20 {
						who := x.s.TopTalker
						if who == "" {
							who = "other devices"
						}
						return true, fmt.Sprintf("%s pulled %.0f Mbit/s", who, *x.s.OtherMbps), *x.s.OtherMbps
					}
				}
			}
			link := kind(telemetry.KindLink, telemetry.SideController)
			xs := c.Tele.Range(st.Start-30000, to, link)
			ts, rates := counterRates(xs, func(s *telemetry.Sample) *uint64 { return s.TxRetries })
			var base, win []float64
			for i, t := range ts {
				if t < from {
					base = append(base, rates[i])
				} else {
					win = append(win, rates[i])
				}
			}
			if len(base) < 3 || len(win) == 0 {
				return false, "", 0
			}
			b, w := median(base), maxOf(win)
			if w < 20 || w < 3*(b+1) {
				return false, "", 0
			}
			bs := c.Tele.Values(st.Start-12000, from, link, signal)
			ws := c.Tele.Values(from, to, link, signal)
			if len(bs) > 0 && len(ws) > 0 && median(bs)-minOf(ws) >= 3 {
				return false, "", 0 // signal moved: that's the signal rule's job
			}
			return true, fmt.Sprintf("retries %.0f/s vs %.0f/s normally", w, b), w
		},
		Evidence: func(c *Ctx, hits []Hit, total int) string {
			var notes []string
			for _, h := range hits {
				if len(notes) < 2 && !contains(notes, h.Note) {
					notes = append(notes, h.Note)
				}
			}
			return fmt.Sprintf("%d of %d stalls came with a TX-retry spike at steady signal or heavy traffic from other devices (%s)", len(hits), total, strings.Join(notes, "; "))
		},
	},
	{
		ID:      "capacity",
		Pattern: "Stalls only above a certain bitrate",
		Cause:   "Not enough link capacity",
		Fix:     "Lower the Pyrowave bitrate",
		Stall: func(c *Ctx, st *model.Stall, from, to float64) (bool, string, float64) {
			if r := c.Run.Ramp; r != nil && len(r.Steps) > 1 {
				if r.HighestCleanMbps > 0 && st.BitrateMbps > r.HighestCleanMbps {
					return true, fmt.Sprintf("at %.0f Mbit/s", st.BitrateMbps), st.BitrateMbps
				}
				return false, "", 0
			}
			phy := c.Tele.Values(st.Start-10000, to, kind(telemetry.KindLink, telemetry.SideController), phyRate)
			if len(phy) == 0 {
				return false, "", 0
			}
			if p := median(phy); st.BitrateMbps > 0.65*p {
				return true, fmt.Sprintf("%.0f Mbit/s on a %.0f Mbit/s PHY", st.BitrateMbps, p), p
			}
			return false, "", 0
		},
		Evidence: func(c *Ctx, hits []Hit, total int) string {
			if r := c.Run.Ramp; r != nil && len(r.Steps) > 1 {
				lo := math.Inf(1)
				for _, h := range hits {
					lo = math.Min(lo, h.Value)
				}
				return fmt.Sprintf("%d of %d stalls happened at %.0f Mbit/s or more; the link was clean up to %.0f Mbit/s", len(hits), total, lo, r.HighestCleanMbps)
			}
			return fmt.Sprintf("%d of %d stalls happened while the test bitrate was above 65%% of the Wi-Fi PHY rate (%s); usable throughput is usually 60–65%% of PHY", len(hits), total, hits[0].Note)
		},
	},
	{
		ID:      "host",
		Pattern: "Sender fell behind (overruns) or host CPU > 90%",
		Cause:   "The host can't keep up (not a network problem)",
		Fix:     "Close heavy programs on the host, or plug it into power / use a performance power plan",
		Stall: func(c *Ctx, st *model.Stall, from, to float64) (bool, string, float64) {
			// Scattered 1 ms overruns are common on busy hosts and don't make
			// a stall: the sender must have fallen a whole frame budget behind,
			// close to the stall (sender samples are per second).
			var ov uint64
			var worst float64
			for _, x := range c.Tele.Range(st.Start-1000, st.End+1000, kind(telemetry.KindSender, telemetry.SideServer)) {
				if x.s.Overruns != nil {
					ov += *x.s.Overruns
				}
				if x.s.MaxLateUs != nil {
					worst = math.Max(worst, *x.s.MaxLateUs/1000)
				}
			}
			if ov > 0 && worst >= c.Run.Options.BudgetMs {
				return true, fmt.Sprintf("sender %.0f ms behind (%d overruns)", worst, ov), float64(ov)
			}
			cpu := c.Tele.Values(from, to, kind(telemetry.KindCPU, telemetry.SideServer), func(s *telemetry.Sample) (float64, bool) {
				if s.CPUPct == nil {
					return 0, false
				}
				return *s.CPUPct, true
			})
			if len(cpu) > 0 && maxOf(cpu) > 90 {
				return true, fmt.Sprintf("host CPU %.0f%%", maxOf(cpu)), 0
			}
			return false, "", 0
		},
		Evidence: func(c *Ctx, hits []Hit, total int) string {
			return fmt.Sprintf("%d of %d stalls coincided with the sender falling behind or host CPU above 90%%; %d packets left the host late in total",
				len(hits), total, c.Run.Summary.SenderOverruns)
		},
		Run: func(c *Ctx) (bool, string) {
			var pk uint64
			for _, s := range c.Run.Segments {
				pk += s.Sender.PacketsSent
			}
			ov := c.Run.Summary.SenderOverruns
			if pk == 0 || float64(ov)/float64(pk) < 0.001 {
				return false, ""
			}
			return true, fmt.Sprintf("%d of %d packets (%.2f%%) left the host more than 1 ms late because the sender fell behind",
				ov, pk, 100*float64(ov)/float64(pk))
		},
	},
	{
		ID:      "rcvbuf",
		Pattern: "Receive buffer errors",
		Cause:   "Deck socket buffer too small (not a Wi-Fi problem)",
		Fix:     "Raise the receive buffer: `sudo sysctl -w net.core.rmem_max=16777216` on the Deck",
		Stall: func(c *Ctx, st *model.Stall, from, to float64) (bool, string, float64) {
			xs := c.Tele.Range(from-1500, to, kind(telemetry.KindSNMP, telemetry.SideController))
			var first, last *uint64
			for _, x := range xs {
				if x.s.RcvbufErrors != nil {
					if first == nil {
						first = x.s.RcvbufErrors
					}
					last = x.s.RcvbufErrors
				}
			}
			if first != nil && *last > *first {
				return true, fmt.Sprintf("%d receive-buffer drops", *last-*first), float64(*last - *first)
			}
			return false, "", 0
		},
		Evidence: func(c *Ctx, hits []Hit, total int) string {
			return fmt.Sprintf("%d of %d stalls coincided with UDP receive-buffer drops on the Deck (%d in total)", len(hits), total, c.Run.Summary.RcvbufErrors)
		},
		Run: func(c *Ctx) (bool, string) {
			if n := c.Run.Summary.RcvbufErrors; n > 0 {
				return true, fmt.Sprintf("the Deck dropped %d packets because its socket buffer was full (socket buffer %d KB)", n, c.Run.Environment.RcvBufBytes/1024)
			}
			return false, ""
		},
	},
}

// RuleByID returns a rule from the table.
func RuleByID(id string) *Rule {
	for i := range Rules {
		if Rules[i].ID == id {
			return &Rules[i]
		}
	}
	return nil
}

func minOf(v []float64) float64 {
	m := math.Inf(1)
	for _, x := range v {
		m = math.Min(m, x)
	}
	return m
}

func maxOf(v []float64) float64 {
	m := math.Inf(-1)
	for _, x := range v {
		m = math.Max(m, x)
	}
	return m
}

func contains(v []string, s string) bool {
	for _, x := range v {
		if x == s {
			return true
		}
	}
	return false
}
