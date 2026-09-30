# linkdoctor: Pyrowave Link Doctor

`linkdoctor` is a single command-line binary that runs on both ends of a LAN link. It sends traffic shaped like a Pyrowave stream (one burst of packets per video frame) from your PC to your Steam Deck. It counts the frames that would arrive too late to display and explains each stall using Wi-Fi telemetry. Every run ends in one self-contained `report.html` that works offline.

It answers three questions:

1. What bitrate can this link carry cleanly, and what manual Pyrowave bitrate should I set? (`--mode ramp`)
2. When do stalls happen, and do they repeat at a regular interval? (`--mode soak`)
3. What is the most likely cause of each stall, and what is the evidence?

**New here? Read the [user guide](docs/user-guide.md)**: setup on Windows and the Deck, each test mode, how to read the report, and what to do about each cause. The design is in [docs/architecture.md](docs/architecture.md).

## Quick start

Build (Go 1.25+, no cgo):

```
make dist      # dist/linkdoctor-linux-amd64, dist/linkdoctor-windows-amd64.exe
```

**On the host** (Windows 10/11 or Linux PC, ideally wired to the Freebox):

```
linkdoctor serve
```

On Windows, the first `serve` triggers a firewall prompt. Allow it on private networks. The tool uses TCP 47100 and UDP 47101/47102, which stay clear of Steam's own 27031–27036 range.

**On the Deck** (Desktop Mode → Konsole): copy `linkdoctor-linux-amd64` to your home folder and `chmod +x` it. Then run:

```
./linkdoctor-linux-amd64 run --peer 192.168.1.20 --mode ramp
./linkdoctor-linux-amd64 run --peer 192.168.1.20 --mode soak --bitrate 200 --duration 30m
./linkdoctor-linux-amd64 run --peer 192.168.1.20 --mode live
```

Each run writes `runs/<date>_<time>/` with `report.html` and its raw data (`run.json`, `frames.csv`, `seconds.csv`, `stalls.csv`, `telemetry.jsonl`). Ctrl+C stops a soak or ramp early and still writes the report.

```
linkdoctor analyze runs/2026-09-30_2100 [--budget 20]        # re-run the analysis on saved data
linkdoctor compare runs/2026-09-30_2100 runs/2026-09-30_2145 # before/after, e.g. power management off
```

No Deck handy? `linkdoctor run --loopback --duration 60s --bitrate 300` runs both ends in one process over localhost.

## Modes

| Mode | What it does | Output |
| --- | --- | --- |
| `soak` | Fixed bitrate for a long time (default 150 Mbit/s, 30 min) | Stall timeline, periodicity, causes |
| `ramp` | 50 → 500 Mbit/s in 25 Mbit/s steps of 10 s; stops after 2 steps with > 1 % late frames or > 0.5 % loss | Highest clean bitrate; recommended bitrate = 70 % of it |
| `live` | Runs until Ctrl+C and prints one line per second, flagging stalls | Terminal; `--report` also writes a report |
| `compare` | Two run folders side by side | `compare.html` |

Traffic flags: `--bitrate`, `--fps`, `--frame-jitter`, `--burst-spread`, `--pkt-size` (up to 1472 to test MTU behaviour), `--profile pyrowave|even`, `--budget`. Use `--json` for scripting and `--raw` to keep every packet on short runs.

## What counts as a stall

A frame is **late** if it is incomplete, or if its last packet arrives more than one frame interval after it should have (16.7 ms at 60 fps). A **stall** is 3 or more consecutive late frames, or a period of more than 50 ms with no packets.

The two machines don't share a clock. Queuing delay is `recv_ts − send_ts` minus a rolling 10-second minimum, which removes the clock offset and drift.

## Diagnosis rules

Each stall is matched against the telemetry from 2 s before it starts to 1 s after it ends. Verdicts are ranked by the share of stalls they explain. The rules are a data table in [`internal/analysis/rules.go`](internal/analysis/rules.go):

| Pattern | Likely cause |
| --- | --- |
| Stalls line up with scan events, often periodic | Background Wi-Fi scanning |
| Short stalls while power save is on | Wi-Fi power saving |
| Band switches to 2.4 GHz | Band steering |
| Frequency change / long disconnect | DFS radar channel change |
| Signal −6 dB or PHY rate −40 % | Distance, obstruction, interference |
| Retry spike at steady signal, or other devices busy (Freebox) | Congestion |
| Stalls only above a bitrate | Not enough link capacity |
| Sender overruns / host CPU > 90 % | The host can't keep up |
| UDP receive-buffer errors | Deck socket buffer too small |

## Telemetry

- **Deck / Linux:** nl80211 link and station stats at 2 Hz (signal, RX/TX bitrate, MCS, width, channel, retries, beacon loss). Also the power-save state, nl80211 scan/roam/disconnect/channel-switch events, `/proc/net/snmp` receive-buffer errors and CPU. It reads netlink directly and falls back to `iw`. None of this needs root.
- **Windows host:** CPU, `Get-NetAdapter` link speed, and `netsh wlan show interfaces` if the host is on Wi-Fi.
- **Freebox** (`--freebox`, optional, read-only): the box's channel and the traffic of other Wi-Fi stations. On first use, approve "linkdoctor" on the box's front display. The token is stored in your user config folder.

If the Deck drops packets itself at high bitrates, the report says so. Raise the buffer with `sudo sysctl -w net.core.rmem_max=16777216`.

## Development

```
make test        # unit tests + loopback integration tests
make test-short  # skip the traffic tests
```

`testdata/scenarios/*.json` holds synthetic runs, one or more per diagnosis rule. `internal/analysis` must reach the expected verdict for each of them. To add a new pattern, add a rule row and a scenario.

Layout: `cmd/linkdoctor` (CLI), `internal/proto` (wire formats), `internal/clock` (pacing and clock offset), `internal/sender`, `internal/receiver`, `internal/agent` (serve / controller), `internal/telemetry`, `internal/router/freebox`, `internal/modes`, `internal/analysis`, `internal/report`.
