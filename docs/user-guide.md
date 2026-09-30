# linkdoctor user guide

This guide explains how to use `linkdoctor` to find out why a Steam Remote Play / Pyrowave stream stutters on your Steam Deck, and which bitrate your link can carry.

- [1. How it works in one minute](#1-how-it-works-in-one-minute)
- [2. What you need](#2-what-you-need)
- [3. Get the program](#3-get-the-program)
- [4. Set up the host (your PC)](#4-set-up-the-host-your-pc)
- [5. Set up the Deck](#5-set-up-the-deck)
- [6. Your first test: find a good bitrate (ramp)](#6-your-first-test-find-a-good-bitrate-ramp)
- [7. Hunt the stutters (soak)](#7-hunt-the-stutters-soak)
- [8. Watch the link live (live)](#8-watch-the-link-live-live)
- [9. Read the report](#9-read-the-report)
- [10. Causes and what to do about them](#10-causes-and-what-to-do-about-them)
- [11. Check a fix: before/after (compare)](#11-check-a-fix-beforeafter-compare)
- [12. Re-analyse a saved run (analyze)](#12-re-analyse-a-saved-run-analyze)
- [13. Optional: Freebox module](#13-optional-freebox-module)
- [14. Command reference](#14-command-reference)
- [15. Output files](#15-output-files)
- [16. Troubleshooting](#16-troubleshooting)
- [17. Tips for trustworthy results](#17-tips-for-trustworthy-results)

---

## 1. How it works in one minute

You run the same program on both machines:

| Machine | Command | Role |
| --- | --- | --- |
| Host (the PC that runs your games) | `linkdoctor serve` | Waits, then sends test traffic when asked |
| Steam Deck | `linkdoctor run --peer <host IP>` | Starts the test, receives the traffic, collects Wi-Fi data, writes the report |

The host sends traffic shaped like a real game stream: one burst of packets per video frame, 60 times a second. The Deck checks every frame. A frame that arrives incomplete, or later than one frame interval (16.7 ms at 60 fps), would be a visible stutter, so it counts as a **late frame**. **Three late frames in a row**, or **more than 50 ms with no packets**, is a **stall**.

While the test runs, the Deck records its Wi-Fi signal, bitrates, band, channel, background scans, power saving and more. At the end it matches each stall against that data and tells you the most likely cause, with the evidence and a suggested fix.

Every test ends with a single `report.html` file that you can open in any browser, even offline.

## 2. What you need

- **Host**: Windows 10/11 or Linux (x86-64). Ideally wired to the router with Ethernet, like it would be for streaming.
- **Client**: a Steam Deck in **Desktop Mode** (tested target: the original LCD Deck). Other Linux machines also work.
- Both machines on the **same local network**.
- Network access between them on **TCP 47100** and **UDP 47101–47102**. These ports stay clear of Steam's own ports (27031–27036), so the test does not interfere with Steam.

Nothing needs to be installed and no admin rights are required, except for the Windows firewall prompt the first time.

## 3. Get the program

There are two files, one per platform:

| File | Runs on |
| --- | --- |
| `linkdoctor-windows-amd64.exe` | Windows host |
| `linkdoctor-linux-amd64` | Steam Deck, or a Linux host |

**Build them yourself** (needs [Go](https://go.dev/dl/) 1.25 or newer):

```
git clone https://github.com/Roganis/EZ2PYRO.git
cd EZ2PYRO
make dist
```

The two binaries appear in `dist/`. Without `make`, run:

```
CGO_ENABLED=0 GOOS=linux   GOARCH=amd64 go build -o dist/linkdoctor-linux-amd64 ./cmd/linkdoctor
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -o dist/linkdoctor-windows-amd64.exe ./cmd/linkdoctor
```

**Or download them from CI**: every push builds both files. Open the repository's *Actions* tab, pick the latest green `ci` run and download the `linkdoctor` artifact.

Check that it works:

```
linkdoctor version
```

## 4. Set up the host (your PC)

### Windows

1. Put `linkdoctor-windows-amd64.exe` in a folder, for example `C:\linkdoctor`.
2. Open **PowerShell** in that folder: in File Explorer, Shift + right-click the folder background, then choose *Open PowerShell window here*.
3. Start the server:

   ```
   .\linkdoctor-windows-amd64.exe serve
   ```

4. The first time, Windows Defender Firewall asks whether to allow the program. Tick **Private networks** and click **Allow**.
5. The server prints the command to run on the Deck, including your PC's IP address:

   ```
   10:02:11 linkdoctor dev serving on TCP 47100, UDP 47101/47102
   10:02:11   on the Deck run: linkdoctor run --peer 192.168.1.20
   ```

   Note that IP address. If several are listed, use the one on your home network, usually `192.168.x.x`. You can also find it with `ipconfig` ("IPv4 Address").

Leave the window open while you test. Press **Ctrl+C** to stop the server.

### Linux host

```
chmod +x linkdoctor-linux-amd64
./linkdoctor-linux-amd64 serve
```

If a firewall is active (for example `ufw` or `firewalld`), allow TCP 47100 and UDP 47101–47102 from your LAN.

## 5. Set up the Deck

1. Switch to **Desktop Mode**: press the Steam button, then *Power* → *Switch to Desktop*.
2. Copy `linkdoctor-linux-amd64` to your home folder (`/home/deck`). You can use a USB stick or download it with the browser; then move it from `Downloads` to your home folder.
3. Open **Konsole**: application launcher → *System* → *Konsole*.
4. Make it executable, once:

   ```
   chmod +x ~/linkdoctor-linux-amd64
   ```

5. Optional: rename it so the commands in this guide work as written:

   ```
   mv ~/linkdoctor-linux-amd64 ~/linkdoctor
   ```

All commands below are typed in Konsole, from your home folder, as `./linkdoctor ...`.

Tips for the Deck:

- Plug the Deck into power. On battery, power saving can make results worse than they really are.
- Stay where you normally play. The test only tells you about the spot you test from.
- Results go into `~/runs/`. Open the `report.html` files with the Deck's browser (Dolphin → double-click).

## 6. Your first test: find a good bitrate (ramp)

The **ramp** test raises the bitrate step by step until the link starts to stutter. It tells you which bitrate to set in Pyrowave.

```
./linkdoctor run --peer 192.168.1.20 --mode ramp
```

What happens:

- It starts at 50 Mbit/s and adds 25 Mbit/s every 10 seconds, up to 500 Mbit/s.
- After each step it prints one line and marks the step **clean** or **STUTTERS**. A step is clean when at most 1 % of frames are late and at most 0.5 % of packets are lost.
- It stops after two failing steps, so a typical run takes 2–4 minutes.

Example output:

```
connected to 192.168.1.20 (gaming-pc, windows) — mode ramp
ramp: 50 → 500 Mbit/s by 25, 10s per step; stops after 2 steps with >1% late frames or >0.5% loss
    50 Mbit/s → delivered  50.1  late  0.00%  loss  0.00%  p99   1.2 ms  clean
    75 Mbit/s → delivered  75.0  late  0.00%  loss  0.00%  p99   1.4 ms  clean
   ...
   250 Mbit/s → delivered 249.2  late  0.33%  loss  0.01%  p99   9.8 ms  clean
   275 Mbit/s → delivered 262.0  late  6.20%  loss  0.80%  p99  31.0 ms  STUTTERS
   300 Mbit/s → delivered 270.4  late 14.1%   loss  2.10%  p99  48.5 ms  STUTTERS

highest clean bitrate 250 Mbit/s → set Pyrowave to about 175 Mbit/s
```

**What to do with it:** set Pyrowave's manual bitrate to the recommended value. It is 70 % of the highest clean step, which leaves headroom for the dips every Wi-Fi link has.

The original Deck's Wi-Fi 5 (2×2) radio usually tops out well below 500 Mbit/s, so a ramp that stops somewhere between 150 and 400 Mbit/s is normal.

Useful ramp options:

| Option | Default | Meaning |
| --- | --- | --- |
| `--ramp-start` | 50 | First step (Mbit/s) |
| `--ramp-stop` | 500 | Last step (Mbit/s) |
| `--ramp-step` | 25 | Step size (Mbit/s) |
| `--step-duration` | 10s | Length of each step |

For a finer result near the limit, try `--ramp-start 150 --ramp-stop 300 --ramp-step 10 --step-duration 20s`.

## 7. Hunt the stutters (soak)

If the stream drops "every few minutes", run a **soak** test: a fixed bitrate for a long time. This is the mode that catches periodic problems.

```
./linkdoctor run --peer 192.168.1.20 --mode soak --bitrate 150 --duration 30m
```

- Use the bitrate you actually stream at (or the ramp's recommendation).
- 30 minutes is the default and catches most periodic problems. Use `--duration 10m` for a quicker check or `--duration 1h` for rare drops.
- Every 10 seconds it prints a progress line. `STALL` at the end of a line means that interval had one.
- **Ctrl+C stops early and still writes the report** with what was measured so far. Press Ctrl+C twice to quit immediately without a report.

At the end you get a summary and the path of the report:

```
108000 frames · 0.31% late · 0.020% loss · p99 delay 4.2 ms · 15 stalls (longest 420 ms) · 150 Mbit/s delivered
stalls repeat every 118 ± 6 s

verdict: Background Wi-Fi scanning
  evidence: 13 of 15 stalls started within 1 s of a Wi-Fi scan (15 had a scan between 2 s before and 1 s after); stalls repeat every 118 ± 6 s
  fix: Disable Wi-Fi power management on the Deck and stay on one network (forget other saved networks)

report: runs/2026-09-30_2100/report.html
```

## 8. Watch the link live (live)

The **live** mode runs until you press Ctrl+C and prints one line per second. Use it to try things in real time: walking around, moving the Deck, starting a download on another device, or changing a router setting.

```
./linkdoctor run --peer 192.168.1.20 --mode live --bitrate 150
```

```
   time     throughput        loss   p99 delay       late  rtt
   0:01   150.3 Mbit/s  loss  0.00%  p99    1.2 ms  late   0/60   rtt   2.1 ms
   0:02   149.8 Mbit/s  loss  0.00%  p99    1.4 ms  late   0/60   rtt   2.0 ms
   0:03    96.2 Mbit/s  loss  3.10%  p99   42.0 ms  late  14/60   rtt  18.3 ms  STALL
```

| Column | Meaning |
| --- | --- |
| throughput | Data actually received during that second |
| loss | Share of packets that never arrived |
| p99 delay | Extra delay of the slowest frames (99th percentile), on top of the normal delay |
| late | Late frames out of the frames due that second |
| rtt | Round-trip time host ↔ Deck |
| `STALL` | 3+ late frames, or no packets for more than 50 ms |

Live mode does not write a report unless you add `--report`.

## 9. Read the report

Open `report.html` from the run folder in any browser. It works offline and is a single file, so you can attach it to a forum post as it is.

### Verdict box

The top box states the **most likely cause** in one sentence, the **evidence** ("13 of 15 stalls started within 1 s of a Wi-Fi scan") and a **suggested fix**. Other causes that explain some stalls are listed below it, most important first.

Special verdicts:

- **No stalls detected**: the link was clean at this bitrate.
- **Unexplained stalls**: stalls happened, but no telemetry event lined up with them. Run longer, try the [Freebox module](#13-optional-freebox-module) and look at other devices on the network.

### Key numbers

| Tile | Meaning |
| --- | --- |
| Late frames | Share of frames that would have stuttered |
| Stalls | Number of stalls, and how many per minute |
| Longest stall | Duration of the worst stall |
| Periodicity | If stalls repeat regularly, the period (e.g. "118 s ± 6 s"). "Faint pattern" means a weaker repeating signal was found. |
| Recommended Pyrowave bitrate | Ramp tests only |
| Packet loss | Share of packets that never arrived |
| Frame delay p99 | Extra delay of the slowest 1 % of frames |
| Throughput | Average data received, and the median round-trip time |

### Timeline

All lanes share the same time axis. **Drag across any chart to zoom all of them, and double-click to reset.** Hover to read exact values in the line above the charts. Red shaded bands are stalls.

| Lane | What to look for |
| --- | --- |
| Frame delay (p99 per 100 ms) | Spikes above the dashed line (the late budget) are stutters. Red ticks at the bottom mark incomplete frames. |
| Throughput per second | The delivered line dropping below the dashed target line |
| Deck Wi-Fi signal (dBm) | Dips at stall times. Around −50 dBm is excellent; below −70 dBm is weak. |
| Deck PHY rate | The radio's link speed. Sudden drops mean the link got worse. |
| Band | Jumps from 5 GHz to 2.4 GHz |
| Wi-Fi events | Dots for scans, roams/connects, disconnects and channel changes. Scans lined up with the red bands are the classic cause of periodic drops. |

### Stall table

One row per stall: time since the start, duration, worst delay ("lost" if frames never completed; "gap" if packets stopped completely), late frames, packets lost, likely cause, and telemetry events near it. Nearby events show their offset from the stall start, for example `scan_start (Deck) -0.2s`.

### Environment

This section shows the devices, operating systems, network interface, Wi-Fi network name, band, channel and width, socket buffer size, clock offset, test parameters and any warnings. Check it when comparing runs, to make sure you compared like with like.

## 10. Causes and what to do about them

| Verdict | What it means | What to try |
| --- | --- | --- |
| **Background Wi-Fi scanning** | The Deck periodically scans for other networks, pausing traffic for a fraction of a second. It is often periodic (every 30–120 s). | In Game Mode: *Settings → System* → enable *Developer Mode*, then *Settings → Developer* → turn off **Wi-Fi power management**. Forget saved networks you don't use. Then run a soak again and [compare](#11-check-a-fix-beforeafter-compare). |
| **Wi-Fi power saving** | Many short stalls while the radio's power saving is on. | Turn off Wi-Fi power management as above. In Desktop Mode you can test it temporarily with `sudo iw dev wlan0 set power_save off`; this resets at reboot. |
| **Band steering** | The router moves the Deck to 2.4 GHz around the stalls. | Give the 2.4 GHz and 5 GHz networks different names in the router settings and connect the Deck only to the 5 GHz one. |
| **DFS radar channel change** | The router changed channel (it must leave DFS channels when it detects radar), often with a 30–60 s disconnect. | Set a fixed non-DFS 5 GHz channel on the router: 36, 40, 44 or 48. |
| **Distance, obstruction or interference** | The signal dropped by more than 6 dB, or the link speed by more than 40 %, around the stalls. | Move closer to the router, remove obstacles, fix the channel, or add an access point / repeater near where you play. |
| **Congestion from other networks or devices** | Retries spiked while the signal stayed steady, or (with the Freebox module) another device used a lot of bandwidth. | Change the Wi-Fi channel, pause downloads, TV boxes and cloud backups while streaming. |
| **Not enough link capacity** | Stalls only happen above a certain bitrate. | Lower the Pyrowave bitrate. Run a ramp and use its recommendation. |
| **The host can't keep up** | The sending PC itself fell behind: it was late sending frames, or its CPU was above 90 %. This is not a network problem. | Close heavy programs, use a high-performance power plan, and plug a laptop into power. |
| **Deck socket buffer too small** | The Deck dropped packets itself because its receive buffer was full. This is not a Wi-Fi problem. | In Desktop Mode: `sudo sysctl -w net.core.rmem_max=16777216` (resets at reboot), then test again. |

A stall can have several causes. The report ranks causes by the share of stalls each one explains.

## 11. Check a fix: before/after (compare)

The best way to know whether a change helped is to run the same test before and after it, then compare the two runs.

1. Run a soak and note the folder name, for example `runs/2026-09-30_2100`.
2. Make **one** change (for example, turn off Wi-Fi power management).
3. Run the same soak again, with the same bitrate, duration and spot: `runs/2026-09-30_2145`.
4. Compare:

   ```
   ./linkdoctor compare runs/2026-09-30_2100 runs/2026-09-30_2145
   ```

The terminal shows each key number for run A and run B with the change. The command also writes `compare.html` into the second folder, with the table, both verdicts, and late frames and p99 delay over time for both runs on the same charts. Use `--out file.html` to save it elsewhere.

## 12. Re-analyse a saved run (analyze)

`analyze` re-runs the analysis on a saved run folder and rewrites its report. Use it after updating linkdoctor (newer versions may explain more), or to try a different late budget:

```
./linkdoctor analyze runs/2026-09-30_2100
./linkdoctor analyze runs/2026-09-30_2100 --budget 25
```

`--budget` is the delay in milliseconds after which a frame counts as late. The default is one frame interval (16.7 ms at 60 fps). If Pyrowave on your setup buffers more than one frame, a larger budget may match what you actually see.

## 13. Optional: Freebox module

If your router is a Freebox, linkdoctor can also read the box's side of the story: its Wi-Fi channel and how much traffic other Wi-Fi devices use during stalls. This adds evidence such as "the Freebox Player pulled 40 Mbit/s during 9 of 11 stalls" or a channel change on the box. The module is **read-only**: it never changes settings.

```
./linkdoctor run --peer 192.168.1.20 --mode soak --freebox
```

The first time:

1. The terminal says: *approve "linkdoctor" on the box's front display*.
2. On the Freebox Server's display, use the arrow button to select **Yes** (you have about 2 minutes).
3. The access token is saved in `~/.config/linkdoctor/freebox.json`, so you only approve once.

To revoke access, open the Freebox OS web interface (http://mafreebox.freebox.fr), find the list of authorised applications in the access-management settings, and remove "linkdoctor". Then delete the token file.

Use `--freebox-url` if your box is not reachable as `mafreebox.freebox.fr`.

> This module was built from the public Freebox OS API documentation and tested against a simulated box, not a real one yet. If it fails, the test still runs; you only lose the router data, and a warning is printed.

## 14. Command reference

### `linkdoctor serve` (host)

| Option | Default | Meaning |
| --- | --- | --- |
| `--port` | 47100 | Control port (TCP) |
| `--data-port` | 47101 | Data port (UDP) |
| `--probe-port` | 47102 | Probe port (UDP) |
| `--bind` | all | Listen only on this address |
| `--iface` | auto | Interface carrying the test traffic |
| `--wifi-iface` | auto | Wi-Fi interface to watch, if the host is on Wi-Fi |
| `--no-telemetry` | off | Don't collect host telemetry |

The server handles one Deck at a time. If a second Deck connects, it is told the server is busy. If the Deck disappears for 10 seconds, the server stops sending on its own.

### `linkdoctor run` (Deck)

| Option | Default | Meaning |
| --- | --- | --- |
| `--peer` | (required) | IP address of the host running `serve` |
| `--mode` | soak | `soak`, `ramp` or `live` |
| `--bitrate` | 150 | Average bitrate in Mbit/s (10–1000); soak and live |
| `--duration` | 30m | Soak length (`90s`, `10m`, `1h`, …) |
| `--fps` | 60 | Frame rate (30–144) |
| `--budget` | 1 frame | Late threshold in ms |
| `--frame-jitter` | 0.2 | Random ± variation of frame size (0–1) |
| `--burst-spread` | 0.3 | Share of the frame interval each burst is spread over (0 = all packets back to back) |
| `--pkt-size` | 1200 | UDP payload size (200–1472). Use 1472 to test for fragmentation/MTU problems. |
| `--profile` | pyrowave | `pyrowave` (bursts) or `even` (evenly paced like iperf, for comparison) |
| `--ramp-start`, `--ramp-stop`, `--ramp-step` | 50, 500, 25 | Ramp steps in Mbit/s |
| `--step-duration` | 10s | Length of each ramp step |
| `--out` | runs | Folder where run folders are created |
| `--report` | off | Live mode: also write a report when stopped |
| `--json` | off | Print the summary as JSON on stdout (progress goes to stderr) |
| `--raw` | off | Also save every packet to `packets.csv`. Use only for short runs: a minute at 300 Mbit/s is about 2 million lines. |
| `--freebox`, `--freebox-url` | off | [Freebox module](#13-optional-freebox-module) |
| `--iface`, `--wifi-iface` | auto | Override interface detection |
| `--no-telemetry` | off | Don't collect telemetry on the Deck |
| `--port`, `--data-port`, `--probe-port` | 47100–47102 | Must match the server |
| `--loopback` | off | Self-test on one machine (see [Troubleshooting](#16-troubleshooting)) |

### `linkdoctor analyze <run folder>`

Options: `--budget <ms>`, `--json`.

### `linkdoctor compare <run A> <run B>`

Options: `--out <file.html>`.

### `linkdoctor version`

Prints the version. Use the same version on the host and the Deck: different protocol versions refuse to connect.

## 15. Output files

Each `run` creates `runs/<date>_<time>/` (a suffix `_2`, `_3`… is added if the folder already exists):

| File | Content |
| --- | --- |
| `report.html` | The report (self-contained, works offline) |
| `run.json` | Test parameters, environment, per-step results, summary, periodicity, verdicts |
| `frames.csv` | One row per frame |
| `seconds.csv` | One row per second |
| `stalls.csv` | One row per stall |
| `telemetry.jsonl` | Every telemetry sample and event from both machines, one JSON object per line |
| `packets.csv` | Only with `--raw`: one row per packet |

`frames.csv` columns:

| Column | Meaning |
| --- | --- |
| `seg` | Segment (always 0 for soak; the step number for ramp) |
| `frame_id` | Frame number within the segment |
| `t_ms` | Time the frame should have started arriving, ms since the start of the run |
| `bitrate_mbps` | Target bitrate |
| `pkts`, `recv`, `lost` | Packets in the frame, received, lost |
| `complete`, `late` | 1/0 |
| `first_ms`, `last_ms` | First/last packet arrival relative to the frame's send start, with the normal network delay removed (−1 if nothing arrived) |
| `delay_ms` | Extra delay of the completed frame (−1 if incomplete) |

`seconds.csv` columns: `t_s`, `mbps`, `loss_pct`, `p50_delay_ms`, `p99_delay_ms`, `frames`, `late`, `rtt_ms` (−1 if unknown).

`stalls.csv` columns: `index`, `start_ms`, `end_ms`, `duration_ms`, `worst_delay_ms`, `late_frames`, `packets_lost`, `gap` (1 if packets stopped completely), `bitrate_mbps`, `causes`, `nearby` (semicolon-separated).

In `telemetry.jsonl`, `t_ns` is nanoseconds since the start of the run on the Deck's clock; host samples are shifted onto it. `side` is `controller` (Deck) or `server` (host). `kind` is one of `link`, `powersave`, `event`, `snmp`, `cpu`, `nic`, `sender`, `rtt`, `router`, `info`. Fields a device doesn't report are left out.

The files open in any spreadsheet or in Python/pandas. For scripts, `--json` prints the summary, verdicts and ramp result:

```
./linkdoctor run --peer 192.168.1.20 --mode ramp --json > ramp.json
```

## 16. Troubleshooting

**`cannot reach linkdoctor serve on 192.168.1.20:47100`**
- Is `serve` still running on the host?
- Is the IP right? Use the one `serve` prints.
- Windows: was the firewall prompt answered with *Allow* on **private** networks? If you clicked *Cancel*, open *Windows Defender Firewall → Allow an app through firewall* and tick linkdoctor for Private. Also check that Windows treats your home network as *Private*, not *Public*.
- Some routers isolate Wi-Fi clients from wired ones ("AP isolation" / "guest network"). The Deck must be on the main network.

**`no UDP packets from the controller reached port 47101; allow linkdoctor through the host firewall`**
TCP got through but UDP did not. Allow UDP 47101 and 47102 inbound on the host (the Windows prompt normally covers both).

**`warning: no probe replies from the host on UDP 47102`**
Same fix: UDP 47102 is blocked. The test still runs, but RTT and the timing of host telemetry are missing.

**`warning: socket receive buffer is only … KB`**
At high bitrates the Deck may drop packets itself. Run `sudo sysctl -w net.core.rmem_max=16777216` (needs a sudo password; set one with `passwd` in Konsole if you never did). The setting resets at reboot.

**`protocol mismatch: server vN, controller vM`**
The two machines run different versions. Copy the same build to both.

**`server busy with another controller`**
Another test is already running against this host. Wait for it to finish, or restart `serve`.

**`warning: Steam Remote Play streaming is running`**
A real stream competes with the test. Stop streaming and test again.

**The report says "No Wi-Fi link telemetry was recorded on the Deck"**
The Deck's Wi-Fi interface was not found. You are probably on a wired or USB adapter, or testing with `--loopback`. Pass `--wifi-iface wlan0` if the name differs (list interfaces with `ip link`).

**"The host can't keep up" on every run**
The PC is too busy while testing. Close games, launchers doing updates, browsers with video, and set Windows to a high-performance power mode. If it persists at low bitrates, report it: it may be a pacing problem on that PC.

**Test without a Deck (self-test)**
To check the program itself on one machine:

```
linkdoctor run --loopback --duration 20s --bitrate 300
```

This starts a server inside the same process and tests over `127.0.0.1`. The result says nothing about your Wi-Fi; it only checks that everything works.

**Stopping**
Ctrl+C once stops the test cleanly and writes the report. Ctrl+C twice quits at once.

## 17. Tips for trustworthy results

- **Test alone.** Pause downloads, streaming and backups on other devices, and close Steam's own streaming. The report can only blame what it sees.
- **Change one thing at a time**, and compare runs with the same bitrate, duration and location.
- **Soak long enough.** A problem that happens every 2 minutes needs at least 10 minutes to show a pattern; 30 minutes is better.
- **Plug the Deck in** and keep it where you play.
- **The traffic is an approximation.** linkdoctor imitates a Pyrowave stream (bursts per frame, ±20 % frame size), but it is not the real stream. If results and real play disagree, trust what you see in play and share the report.
