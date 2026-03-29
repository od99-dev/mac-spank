# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

> See also: AGENTS.md — contains full project overview, key constants, gotchas, and audio pack instructions.

## Commands

```bash
# Build
go build -o spank .

# Run tests (no hardware required — stdin/command processing only)
go test ./...

# Run a single test
go test -run TestPauseCommand .

# Lint
go vet ./...

# Release (CI handles this on v* tags)
git tag v1.0.0 && git push origin v1.0.0
```

Tests run without `sudo` and without hardware — they only cover `processCommands()` (JSON stdin handling). The accelerometer and audio paths are not tested automatically.

## Architecture

All application code lives in a single file: **`main.go`**. There are no packages, sub-directories of Go code, or interfaces to navigate.

### Data flow

```
IOKit HID accelerometer
  → sensor.Run() [background goroutine]
  → shm.RingBuffer [shared memory ring buffer]
  → detector.New().Process() [STA/LTA + CUSUM + kurtosis]
  → listenForSlaps() event loop [10ms poll]
  → playAudio() [goroutine per playback]
```

### Mode selection

Modes are mutually exclusive CLI flags (`--sexy`, `--halo`, `--lizard`, `--custom`). Each constructs a `soundPack` with either `modeRandom` or `modeEscalation`. The `run()` function's switch statement is the single place to add new modes.

### Escalation scoring (`slapTracker`)

The `slapTracker` maps cumulative slap score → file index using `1 - exp(-x)` so intensity asymptotically approaches the maximum. Score decays with a 30-second half-life between slaps.

### Live control (stdio mode)

When `--stdio` is passed, `processCommands()` reads newline-delimited JSON from stdin in a goroutine and writes JSON responses to stdout. This is used for GUI integration. The `stdinCommand` struct defines the protocol. Tests in `stdin_test.go` cover this path entirely.

## Private dependency

`github.com/taigrr/apple-silicon-accelerometer` is a private module. Local development needs:

```bash
export GOPRIVATE=github.com/taigrr/apple-silicon-accelerometer
```

CI uses a GitHub PAT configured in repository secrets.
