# linkdoctor: notes for Claude Code

Pyrowave Link Doctor. It is one Go binary (`cmd/linkdoctor`) that runs on both ends of a LAN link. The host runs `serve` and sends Pyrowave-shaped UDP bursts. The Deck runs `run --peer <ip>`: it is the controller and receiver, it gathers telemetry from both sides, and it owns the analysis and the report. The full design is in `docs/architecture.md`.

## Invariants

- Go, `CGO_ENABLED=0`, and static binaries for linux/amd64 and windows/amd64. The code must also build on darwin, where the telemetry is a stub.
- The Deck is always the controller, the host only runs `serve`, and traffic flows host → Deck.
- Channels: control is TCP 47100 with JSON lines (`HELLO CONFIG START STOP HEARTBEAT TELEMETRY RESULTS ERROR`). Data is UDP 47101 with a 40-byte little-endian header (`internal/proto/header.go`). Probe is UDP 47102 with 32-byte NTP-style pings. The receiver hole-punches the server's data port, and the server sends to the punch source.
- Clocks are never synced. Queuing delay = `recv − send − rolling 10 s min`. Host telemetry is shifted onto the controller clock with the probe offset estimate.
- Late frame = incomplete, or completion delay > budget (default one frame interval). Stall = ≥ 3 consecutive late frames, or > 50 ms without packets.
- The sender counts overruns (packets > 1 ms late) so a slow host is never blamed on the network. `clock.SleepUntil` sleeps, then spin-waits, with an adaptive margin.
- Every telemetry field is optional (`telemetry.Sample` uses pointer fields). Chips differ.
- The diagnosis rules are data (`internal/analysis/rules.go`). Every rule needs a scenario in `testdata/scenarios/` that makes it the top verdict (`TestScenarios` enforces this).
- The report is a single offline HTML file. uPlot and all data are inlined via `embed`. Colours are CSS tokens with light and dark themes.

## Commands

```
make vet test      # vet includes GOOS=windows
go test -short ./...   # skip loopback traffic tests
go run ./cmd/linkdoctor run --loopback --duration 20s --bitrate 300
```

## Status against the build plan

Milestones 1–9 are implemented: protocol and loopback, pacing, soak and stall detection, Linux Wi-Fi telemetry, analysis rules, the HTML report, ramp, live and compare, and Freebox. Hardware checks that still need the real devices:

- Pacing on a real Windows host: ±2 % bitrate and < 1 ms frame-start jitter.
- On the Deck, `telemetry.jsonl` should have link samples every 500 ms, and scan events should appear when the Wi-Fi menu is opened.
- Freebox endpoints are checked against the public API docs and a fake box in the tests, but not against a real box yet.
- A `calibrate` command, which would fit burst shape from a Wireshark capture, is not built yet.
