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

## Best Practices

### Single-file code organization

Functions in `main.go` are ordered as: types → type methods → helper functions → `main()` → `run()` → event loop → utilities. When adding code, place it logically with related functions. Types must be defined before their methods are called.

### Global state and mutexes

- `pausedMu` is an `RWMutex` (readers: audio playback checks pause state; writers: stdin commands). Use `RLock()` for reads during high-frequency playback.
- `slapTracker.mu` protects escalation score updates during concurrent audio plays.
- `speakerMu` protects speaker initialization (once, single speaker for all audio).
- Never hold multiple mutexes at once (no lock ordering).

### Goroutine patterns

Audio playback runs in goroutines (`go playAudio(...)`). The sensor reads continuously in a background worker spawned by `sensor.Run()`. Context cancellation (`ctx.Done()`) gracefully shuts down the event loop. Use the `sensorErr` channel for error propagation from the sensor goroutine.

### Adding new audio modes

1. Add `//go:embed audio/newmode/*.mp3` variable at the top.
2. Add CLI flag in `main()` (e.g., `cmd.Flags().BoolVar(&newMode, "newmode", false, "...")`).
3. Add mutually exclusive check in `main()` after parsing (must not combine with `--sexy`, `--halo`, etc.).
4. Add case in `run()` switch statement that creates a `soundPack{fs: newmodeAudio, playMode: modeRandom/modeEscalation}`.

### Testing strategy

Only pure functions are tested (see `stdin_test.go`). Hardware-dependent paths (accelerometer read, audio playback) are not covered by CI tests. Before modifying those paths, test locally with `sudo ./spank`. The `processCommands()` function is fully testable without hardware — add tests here for new stdin commands.

### Amplitude and volume scaling

`amplitudeToVolume()` applies logarithmic scaling: `10 * log10(amplitude)`. This is called in `playAudio()` if `volumeScaling` is enabled. The amplitude value comes directly from the detector's categorization (0.0 to 1.0+ range).

### Private dependency

`github.com/taigrr/apple-silicon-accelerometer` is a private module. Local development needs:

```bash
export GOPRIVATE=github.com/taigrr/apple-silicon-accelerometer
```

CI uses a GitHub PAT configured in repository secrets.
