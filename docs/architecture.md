# Pyrowave Link Doctor — Architecture

Sep 30, 2026 · @Ben

## Purpose and scope

`linkdoctor` is one command-line binary that runs on both ends of a LAN link, sends traffic shaped like a Pyrowave stream, counts frames that would arrive too late to display, and explains each stall using Wi-Fi telemetry. Every run ends in a single self-contained HTML report.

It answers three questions:

1. What bitrate can this link carry cleanly, and what manual Pyrowave bitrate should I set?
2. When do stalls happen, and do they repeat at a regular interval?
3. What is the most likely cause of each stall, with evidence?

Non-goals for v1: it does not encode real video, hook into Steam, test internet links, or have a GUI.

Target platforms: the host is Windows 10/11 or Linux (x86-64). The client is the original Steam Deck (LCD) in Desktop Mode; other Linux machines should work but aren't a priority. Its Wi-Fi 5 (802.11ac, 2×2) radio caps the link well below gigabit, so expect ramp results well under 500 Mbit/s. macOS can come later.

## System overview

Two copies of the same binary talk over three logical channels, and all of that traffic physically crosses the Ethernet → Freebox → Wi-Fi path you want to test.

&#91;embedded content: linkdoctor architecture · 2 agents, 3 channels\]

The Deck is the controller: it starts tests, receives the traffic, gathers telemetry from both sides, and writes the report.

## Agent design

The same binary plays every role. `linkdoctor serve` waits on one machine; `linkdoctor run --peer <ip>` on the other machine starts a test and becomes the controller. The Deck is always the controller, and the host only ever runs serve. Test traffic flows host → Deck, matching a real Remote Play stream.

Three channels:

| Channel | Transport | Direction | Content |
| --- | --- | --- | --- |
| Control | TCP, port 47100 | both | JSON lines: `HELLO`, `CONFIG`, `START`, `STOP`, `HEARTBEAT`, `TELEMETRY`, `RESULTS` |
| Data | UDP, port 47101 | host → Deck | Synthetic frame bursts |
| Probe | UDP, port 47102 | echo | 10 Hz timestamped pings for RTT and clock-offset estimates |

Ports are configurable and stay clear of Steam's own range (27031–27036).

`HELLO` exchanges protocol version, OS, interface names and which telemetry each side can collect. `CONFIG` carries the full test parameters so both sides agree. The serving side streams its own telemetry to the controller over `TELEMETRY` messages; the controller owns all analysis and writes the report.

Data packet header (little-endian, 40 bytes, then padding up to the packet size):

| Field | Type | Notes |
| --- | --- | --- |
| magic | u16 | `0x5057` ("PW") |
| version | u8 | protocol version |
| flags | u8 | last-packet-of-frame, probe, etc. |
| session\_id | u32 | random per run |
| seq | u64 | global packet counter |
| frame\_id | u32 | frame counter |
| pkt\_idx | u16 | index within frame |
| pkt\_count | u16 | packets in this frame |
| frame\_send\_start\_ns | u64 | sender monotonic clock, first packet of frame |
| send\_ts\_ns | u64 | sender monotonic clock, this packet |

Default packet size is 1200 bytes, configurable up to 1472 to test MTU behaviour.

## Traffic model

The sender emits one burst of packets per video frame, which is how a game stream actually loads a Wi-Fi link. This is the main difference from iperf3, which spreads packets evenly.

Frame size = bitrate ÷ fps ÷ 8. At 300 Mbit/s and 60 fps that is about 625 KB per frame, or roughly 520 packets of 1200 bytes.

| Parameter | Default | Range | Purpose |
| --- | --- | --- | --- |
| `--bitrate` | 150 Mbit/s | 10–1000 | Average target |
| `--fps` | 60 | 30–144 | Frame rate |
| `--frame-jitter` | 0.2 | 0–1 | Random ± variation of frame size |
| `--burst-spread` | 0.3 | 0–1 | Share of the frame interval the burst is spread over (0 = back-to-back) |
| `--pkt-size` | 1200 B | 200–1472 | UDP payload size |
| `--profile` | `pyrowave` | `pyrowave`, `even` | `even` paces uniformly, for comparison with iperf-style tests |

Pacing uses a high-resolution clock with a short spin-wait for the last \~200 µs before each send, because OS timers on Windows are only accurate to about 1 ms. The sender counts overruns (packets sent late because the sender itself fell behind) and reports them, so a slow host is never blamed on the network.

These defaults are guesses. A later `calibrate` command should read a Wireshark capture of a real Pyrowave session and fit burst size and spread from it.

## Measurements

The key metric is the late frame: a frame that is incomplete, or whose last packet arrives more than one frame interval after it should have. That is what the player sees as a stutter.

**Delay without synced clocks.** For each packet the receiver computes `d = recv_ts − send_ts`, using each machine's own monotonic clock. The unknown clock offset and slow drift are removed by subtracting a rolling 10-second minimum of `d`. What remains is queuing delay, the extra time a packet spent waiting on the link.

**Per frame** (stored for every frame):

- first and last packet arrival, relative to the frame's send start
- packets received and lost
- complete (yes/no) and late (yes/no)

**Late** = incomplete, or completion delay above the budget (default one frame interval, 16.7 ms at 60 fps; `--budget` to change).

**Stall** = 3 or more consecutive late frames, or no packets for more than 50 ms. Each stall is recorded as an event: start, end, duration, worst delay, packets lost.

**Per second**: delivered throughput, loss %, p50/p99 frame delay, late-frame count, RTT from probes.

**Receiver-side drops.** On Linux the receiver also reads `RcvbufErrors` from `/proc/net/snmp`. Packets dropped because the Deck's socket buffer was full are reported separately, so they aren't mistaken for Wi-Fi loss.

Storage: a 30-minute test at 300 Mbit/s is about 56 million packets, too many to keep raw. The receiver aggregates packets into frame records in memory (about 108,000 frames at 60 fps) and only keeps raw packets when `--raw` is set for short runs.

## Test modes

Soak is the mode that will catch your "every few minutes" drops; ramp tells you what bitrate to set.

| Mode | What it does | Default | Output |
| --- | --- | --- | --- |
| `soak` | Fixed bitrate for a long time | 150 Mbit/s, 30 min | Stall timeline, periodicity, causes |
| `ramp` | Steps 50 → 500 Mbit/s by 25, 10 s per step; stops after 2 steps with >1% late frames or >0.5% loss | — | Highest clean bitrate; recommended bitrate = 70% of it |
| `live` | Runs until Ctrl+C, prints one line per second and flags stalls as they happen | 150 Mbit/s | Terminal only, optional report |
| `compare` | Reads two result folders and shows the difference | — | Side-by-side report, e.g. before/after disabling power management |

Example commands, run on the Deck:

```
linkdoctor run --peer 192.168.1.20 --mode ramp
linkdoctor run --peer 192.168.1.20 --mode soak --bitrate 200 --duration 30m
linkdoctor compare runs/2026-09-30_2100 runs/2026-09-30_2145
```

## Wi-Fi telemetry

Telemetry is what turns "27 stalls" into "27 stalls caused by background scans". Every sample is timestamped on the same monotonic clock as the packet records on that machine; host samples are shifted onto the Deck's clock using the offset estimated from probe pings (millisecond accuracy is enough).

| Source | Platform | Rate | Fields |
| --- | --- | --- | --- |
| nl80211 link info | Linux / Deck | 2 Hz | Signal (dBm), TX/RX bitrate, MCS, channel width, frequency → band and channel |
| nl80211 station stats | Linux / Deck | 2 Hz | TX retries, TX failed, beacon loss |
| Power save state | Linux / Deck | 1 Hz | on/off |
| nl80211 events | Linux / Deck | event stream | Scan started/finished/aborted, roam, disconnect/connect, channel switch |
| `/proc/net/snmp` | Linux / Deck | 1 Hz | UDP receive buffer errors |
| CPU load | all | 1 Hz | Per-core load, to spot a busy sender or receiver |
| NIC link speed | Windows / Linux host | 0.2 Hz | Wired link speed and duplex (`Get-NetAdapter` / `ethtool`) |
| `netsh wlan show interfaces` | Windows host | 1 Hz | Only if the host is itself on Wi-Fi |

On Linux, read nl80211 directly through a netlink library rather than parsing `iw` output; keep `iw` parsing as a fallback. None of these reads should need root. Interface names are auto-detected, with `--iface` to override.

## Analysis engine

The analysis turns frame records and telemetry into a ranked list of likely causes, each with its evidence count. It runs after the test, on the controller, and can be rerun on saved data (`linkdoctor analyze <run folder>`).

1. **Detect stalls** from frame records (rules in Measurements).
2. **Check periodicity.** With 4 or more stalls, compute the intervals between them. If their coefficient of variation is below 0.15, the stalls are periodic with period P. Also autocorrelate the per-second late-frame series to catch fainter patterns.
3. **Correlate.** For each stall, collect telemetry events in the window from 2 s before its start to 1 s after its end.
4. **Apply rules** and rank verdicts by the share of stalls each one explains.

| Pattern in the data | Likely cause | Suggested fix |
| --- | --- | --- |
| Stalls line up with scan events, often periodic (30–120 s) | Background Wi-Fi scanning | Disable Wi-Fi power management on the Deck; stay on one network |
| Many short stalls while power save is on | Wi-Fi power saving | Disable Wi-Fi power management |
| Band switches to 2.4 GHz around stalls | Band steering | Separate 2.4 / 5 GHz network names, join only 5 GHz |
| Frequency changes, disconnect of \~30–60 s | DFS radar channel change | Pick a non-DFS 5 GHz channel |
| Signal drops > 6 dB or TX bitrate drops > 40% | Distance, obstruction, interference | Move closer, fixed channel, repeater |
| Retry spike with steady signal | Congestion from other networks or devices | Change channel, pause other traffic |
| Stalls only above a certain bitrate | Not enough link capacity | Lower Pyrowave bitrate |
| Sender overruns or host CPU > 90% | Host can't keep up | Not a network problem |
| Receive buffer errors | Deck socket buffer too small | Raise buffer size; not a Wi-Fi problem |

Keep rules as data (one Go table or a YAML file) so new patterns can be added without touching the detection code. Each verdict states its evidence, for example: "23 of 27 stalls started within 1 s of a Wi-Fi scan; stalls repeat every 118 ± 6 s."

## Report output

Each run writes a folder, `runs/<date>_<time>/`, containing one `report.html` that opens offline and the raw data behind it.

The report, top to bottom:

1. **Verdict box**: the top cause in one sentence, the suggested fix, and the evidence.
2. **Key numbers**: late-frame %, stall count, longest stall, periodicity (if any), recommended bitrate (ramp only).
3. **Timeline chart**: p99 frame delay per 100 ms on top, stalls as shaded bands, then aligned lanes for signal, TX bitrate and band, with scan/roam/disconnect events as markers. Zoomable.
4. **Stall table**: one row per stall with time, duration, worst delay, loss and the telemetry events nearby.
5. **Environment**: devices, OS, interface, band, channel, width, test parameters, tool version.

The chart library (e.g. uPlot, about 50 KB) and all data are inlined into the HTML, so the report works offline and can be posted to a forum as a single file.

Raw data files:

| File | Content |
| --- | --- |
| `run.json` | Config, environment, summary, verdicts |
| `frames.csv` | One row per frame |
| `seconds.csv` | One row per second |
| `stalls.csv` | One row per stall |
| `telemetry.jsonl` | All telemetry samples and events, both machines |

`--json` prints the summary to stdout for scripting.

## Optional Freebox module

The Freebox adds the router's side of the story: its channel, and what other devices are doing on the network during a stall. It is off by default and read-only in v1.

- **Access**: Freebox OS exposes a local HTTP API. On first use the tool requests an app token, and you approve it on the box, much like the first login to the web interface. The token is stored in the user config folder.
- **Reads**, every 5 s during a test: Wi-Fi configuration (band, channel, width), the list of Wi-Fi stations with their signal and rates, and LAN hosts with traffic counters where available.
- **Adds evidence** such as "the box changed channel at 21:14:32" or "the Freebox Player pulled 40 Mbit/s over Wi-Fi during 9 of 11 stalls".

Implement it behind a generic `router` interface (`Connect`, `Sample`, `Close`) so other routers can be added later. Check exact endpoint names and auth flow against Free's current developer documentation before building.

## Tech stack and repo layout

Go is the recommended language: it cross-compiles to a single static binary for Windows and Linux with no cgo, which matters on SteamOS, where the system partition is read-only and you'd rather not install packages. Rust is a fine alternative if you prefer it.

- **Language**: Go, latest stable, `CGO_ENABLED=0`
- **Netlink**: `github.com/mdlayher/wifi` (or `mdlayher/genetlink`) for nl80211 on Linux
- **Report**: `html/template` plus `embed` for the HTML, CSS and chart library
- **CLI**: standard `flag` package or `cobra`
- **Builds**: `linkdoctor-windows-amd64.exe`, `linkdoctor-linux-amd64` via a Makefile or `goreleaser`

```
linkdoctor/
  cmd/linkdoctor/main.go      CLI entry, subcommands
  internal/proto/             control messages, packet header encode/decode
  internal/clock/             monotonic time, offset estimation from probes
  internal/sender/            frame generator, pacing, overrun counting
  internal/receiver/          UDP receive, per-frame aggregation
  internal/telemetry/         linux_wifi.go, linux_snmp.go, windows_net.go, cpu.go
  internal/router/freebox/    optional Freebox API client
  internal/modes/             soak, ramp, live, compare
  internal/analysis/          stalls, periodicity, rules table
  internal/report/            template, embedded assets, CSV/JSON writers
  testdata/                   synthetic runs for analysis tests
  CLAUDE.md                   this document, condensed, for Claude Code
```

On the Deck, copy the binary to your home folder, `chmod +x` it, and run it from Konsole in Desktop Mode. On Windows, the first `serve` triggers a firewall prompt; allow it on private networks.

## Build plan

Aim for milestones 1–4 in the first Claude Code session: that is already enough to diagnose your drops. Each milestone ends with a check you can run.

1. **Protocol and loopback.** Control, data and probe channels; sender and receiver on one machine.
   - Done when: a 60 s run at 300 Mbit/s over localhost shows 0 loss and writes `frames.csv`.
2. **Pacing on Windows.** Spin-wait pacing and overrun counter.
   - Done when: achieved bitrate is within ±2% of target and frame start jitter is under 1 ms at 60 fps.
3. **Soak mode and stall detection.** Per-frame aggregation, late frames, stalls, per-second summary, CLI output.
   - Done when: stalls injected on Linux with `tc qdisc ... netem` (delay and loss bursts) are all detected, with no false stalls on a clean run.
4. **Linux Wi-Fi telemetry.** nl80211 sampling, event stream, power save state, `RcvbufErrors`.
   - Done when: on the Deck, `telemetry.jsonl` contains samples every 500 ms and scan events appear when you open the Wi-Fi menu.
5. **Analysis rules and periodicity.**
   - Done when: unit tests on synthetic data in `testdata/` produce the correct verdict for each rule in the table.
6. **HTML report.**
   - Done when: the report opens offline in a browser on the Deck and on Windows, and the timeline zooms.
7. **Ramp mode** and recommended bitrate.
8. **Live and compare modes.**
9. **Freebox module.**

Tip for the session: put a condensed version of this document in `CLAUDE.md` and ask Claude Code to write tests alongside each milestone before moving to the next.

## Risks and open questions

The biggest unknown is how closely the synthetic traffic matches real Pyrowave; the rest are engineering details with known workarounds.

| Risk | Impact | Mitigation |
| --- | --- | --- |
| Real Pyrowave packet pattern unknown (burst shape, FEC, retransmits) | Results may not match real stutter | `calibrate` from a Wireshark capture; compare a soak run with a real play session |
| Windows timer precision | Uneven bursts, false stalls | Spin-wait pacing; overrun counter; test on your actual host |
| Deck receive buffer too small at high bitrates | Local drops look like Wi-Fi loss | Request 16 MB `SO_RCVBUF`; report `RcvbufErrors`; document `sysctl net.core.rmem_max` (needs sudo) |
| nl80211 fields differ by Wi-Fi chip (LCD vs OLED Deck) | Missing telemetry | Treat every field as optional; fall back to `iw` parsing |
| The test competes with real traffic | Skewed results | Warn if Steam streaming or downloads are active; run tests alone |
| Freebox API changes | Module breaks | Keep it optional and isolated behind the router interface |

Decisions:

- [x] The Deck is always the controller; the host only runs `serve`.
- [x] Main target is the Steam Deck LCD (V1); OLED support comes later.
- [x] v1 is terminal-only on the Deck; no Game Mode UI for now.
