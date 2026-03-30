// spank detects slaps/hits on the laptop and plays djembe drum sounds.
// Bass on hard hits, snare on lighter hits. Both voices can overlap freely.
// It reads the Apple Silicon accelerometer directly via IOKit HID —
// no separate sensor daemon required. Needs sudo.
package main

import (
	"bufio"
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/rand"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/charmbracelet/fang"
	"github.com/gopxl/beep/v2"
	"github.com/gopxl/beep/v2/effects"
	"github.com/gopxl/beep/v2/mp3"
	"github.com/gopxl/beep/v2/speaker"
	"github.com/spf13/cobra"
	"github.com/taigrr/apple-silicon-accelerometer/detector"
	"github.com/taigrr/apple-silicon-accelerometer/sensor"
	"github.com/taigrr/apple-silicon-accelerometer/shm"
)

var version = "dev"

//go:embed audio/djembe/bass/*.mp3
var djembeBassAudio embed.FS

//go:embed audio/djembe/snare/*.mp3
var djembeSnareAudio embed.FS

var (
	fastMode      bool
	minAmplitude  float64
	cooldownMs    int
	bassThreshold float64
	bassDir       string
	snareDir      string
	stdioMode     bool
	volumeScaling bool
	paused        bool
	pausedMu      sync.RWMutex
	speedRatio    float64
)

// sensorReady is closed once shared memory is created and the sensor
// worker is about to enter the CFRunLoop.
var sensorReady = make(chan struct{})

// sensorErr receives any error from the sensor worker.
var sensorErr = make(chan error, 1)

const (
	// defaultMinAmplitude is the default detection threshold.
	defaultMinAmplitude = 0.03

	// defaultCooldownMs is the default per-voice cooldown between audio responses.
	defaultCooldownMs = 300

	// defaultSpeedRatio is the default playback speed (1.0 = normal).
	defaultSpeedRatio = 1.0

	// defaultBassThreshold is the amplitude above which a hit is routed to bass.
	// Hits below this go to snare. 0.10 targets moderate taps for bass,
	// leaving very light taps (0.03–0.10) as snare.
	defaultBassThreshold = 0.10

	// defaultSensorPollInterval is how often we check for new accelerometer data.
	defaultSensorPollInterval = 10 * time.Millisecond

	// defaultMaxSampleBatch caps the number of accelerometer samples processed
	// per tick to avoid falling behind.
	defaultMaxSampleBatch = 200

	// sensorStartupDelay gives the sensor time to start producing data.
	sensorStartupDelay = 100 * time.Millisecond
)

type runtimeTuning struct {
	minAmplitude float64
	cooldown     time.Duration
	pollInterval time.Duration
	maxBatch     int
}

func defaultTuning() runtimeTuning {
	return runtimeTuning{
		minAmplitude: defaultMinAmplitude,
		cooldown:     time.Duration(defaultCooldownMs) * time.Millisecond,
		pollInterval: defaultSensorPollInterval,
		maxBatch:     defaultMaxSampleBatch,
	}
}

func applyFastOverlay(base runtimeTuning) runtimeTuning {
	base.pollInterval = 4 * time.Millisecond
	base.cooldown = 150 * time.Millisecond
	if base.minAmplitude > 0.18 {
		base.minAmplitude = 0.18
	}
	if base.maxBatch < 320 {
		base.maxBatch = 320
	}
	return base
}

// soundPack holds a set of audio files for one drum voice (bass or snare).
type soundPack struct {
	fs     embed.FS
	dir    string
	files  []string
	custom bool
}

func (sp *soundPack) loadFiles() error {
	if sp.custom {
		entries, err := os.ReadDir(sp.dir)
		if err != nil {
			return err
		}
		sp.files = make([]string, 0, len(entries))
		for _, entry := range entries {
			if !entry.IsDir() && strings.HasSuffix(strings.ToLower(entry.Name()), ".mp3") {
				sp.files = append(sp.files, sp.dir+"/"+entry.Name())
			}
		}
	} else {
		entries, err := sp.fs.ReadDir(sp.dir)
		if err != nil {
			return err
		}
		sp.files = make([]string, 0, len(entries))
		for _, entry := range entries {
			if !entry.IsDir() {
				sp.files = append(sp.files, sp.dir+"/"+entry.Name())
			}
		}
	}
	sort.Strings(sp.files)
	if len(sp.files) == 0 {
		return fmt.Errorf("no audio files found in %s", sp.dir)
	}
	return nil
}

// dualPack holds bass and snare voices and routes hits by amplitude threshold.
type dualPack struct {
	bass  *soundPack
	snare *soundPack
}

// selectVoice returns the pack, a randomly chosen file, and the voice name
// ("bass" or "snare") based on amplitude vs bassThreshold.
func (dp *dualPack) selectVoice(amplitude float64) (*soundPack, string, string) {
	if amplitude >= bassThreshold {
		file := dp.bass.files[rand.Intn(len(dp.bass.files))]
		return dp.bass, file, "bass"
	}
	file := dp.snare.files[rand.Intn(len(dp.snare.files))]
	return dp.snare, file, "snare"
}

func main() {
	cmd := &cobra.Command{
		Use:   "spank",
		Short: "Turns your MacBook into a djembe drum",
		Long: `spank reads the Apple Silicon accelerometer directly via IOKit HID
and plays djembe drum sounds when a hit is detected.

Hard hits (amplitude >= bass-threshold) trigger bass sounds.
Lighter hits trigger snare sounds. Both voices can play simultaneously.

Requires sudo (for IOKit HID access to the accelerometer).

Replace audio/djembe/bass/ and audio/djembe/snare/ with your own MP3s,
or point --bass-dir / --snare-dir at directories of custom MP3 files.`,
		Version: version,
		RunE: func(cmd *cobra.Command, args []string) error {
			tuning := defaultTuning()
			if fastMode {
				tuning = applyFastOverlay(tuning)
			}
			// Explicit flags override fast preset defaults
			if cmd.Flags().Changed("min-amplitude") {
				tuning.minAmplitude = minAmplitude
			}
			if cmd.Flags().Changed("cooldown") {
				tuning.cooldown = time.Duration(cooldownMs) * time.Millisecond
			}
			return run(cmd.Context(), tuning)
		},
		SilenceUsage: true,
	}

	cmd.Flags().BoolVar(&fastMode, "fast", false, "Enable faster detection tuning (shorter cooldown, higher sensitivity)")
	cmd.Flags().Float64Var(&minAmplitude, "min-amplitude", defaultMinAmplitude, "Minimum amplitude threshold (0.0-1.0, lower = more sensitive)")
	cmd.Flags().IntVar(&cooldownMs, "cooldown", defaultCooldownMs, "Per-voice cooldown between responses in milliseconds")
	cmd.Flags().Float64Var(&bassThreshold, "bass-threshold", defaultBassThreshold, "Amplitude at or above which a hit triggers bass (below triggers snare)")
	cmd.Flags().StringVar(&bassDir, "bass-dir", "", "Directory of custom bass MP3 files (overrides embedded)")
	cmd.Flags().StringVar(&snareDir, "snare-dir", "", "Directory of custom snare MP3 files (overrides embedded)")
	cmd.Flags().BoolVar(&stdioMode, "stdio", false, "Enable stdio mode: JSON output and stdin commands (for GUI integration)")
	cmd.Flags().BoolVar(&volumeScaling, "volume-scaling", false, "Scale playback volume by slap amplitude (harder hits = louder)")
	cmd.Flags().Float64Var(&speedRatio, "speed", defaultSpeedRatio, "Playback speed multiplier (0.5 = half speed, 2.0 = double speed)")

	if err := fang.Execute(context.Background(), cmd); err != nil {
		os.Exit(1)
	}
}

func run(ctx context.Context, tuning runtimeTuning) error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("spank requires root privileges for accelerometer access, run with: sudo spank")
	}

	if tuning.minAmplitude < 0 || tuning.minAmplitude > 1 {
		return fmt.Errorf("--min-amplitude must be between 0.0 and 1.0")
	}
	if tuning.cooldown <= 0 {
		return fmt.Errorf("--cooldown must be greater than 0")
	}
	if bassThreshold <= 0 || bassThreshold > 1 {
		return fmt.Errorf("--bass-threshold must be between 0.0 and 1.0")
	}

	var bassPack, snarePack *soundPack
	if bassDir != "" {
		bassPack = &soundPack{dir: bassDir, custom: true}
	} else {
		bassPack = &soundPack{fs: djembeBassAudio, dir: "audio/djembe/bass"}
	}
	if snareDir != "" {
		snarePack = &soundPack{dir: snareDir, custom: true}
	} else {
		snarePack = &soundPack{fs: djembeSnareAudio, dir: "audio/djembe/snare"}
	}

	if err := bassPack.loadFiles(); err != nil {
		return fmt.Errorf("loading bass audio: %w", err)
	}
	if err := snarePack.loadFiles(); err != nil {
		return fmt.Errorf("loading snare audio: %w", err)
	}

	dp := &dualPack{bass: bassPack, snare: snarePack}

	ctx, cancel := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Create shared memory for accelerometer data.
	accelRing, err := shm.CreateRing(shm.NameAccel)
	if err != nil {
		return fmt.Errorf("creating accel shm: %w", err)
	}
	defer accelRing.Close()
	defer accelRing.Unlink()

	// Start the sensor worker in a background goroutine.
	go func() {
		close(sensorReady)
		if err := sensor.Run(sensor.Config{
			AccelRing: accelRing,
			Restarts:  0,
		}); err != nil {
			sensorErr <- err
		}
	}()

	// Wait for sensor to be ready.
	select {
	case <-sensorReady:
	case err := <-sensorErr:
		return fmt.Errorf("sensor worker failed: %w", err)
	case <-ctx.Done():
		return nil
	}

	time.Sleep(sensorStartupDelay)

	return listenForSlaps(ctx, dp, accelRing, tuning)
}

func listenForSlaps(ctx context.Context, dp *dualPack, accelRing *shm.RingBuffer, tuning runtimeTuning) error {
	speakerInit := false
	det := detector.New()
	var lastAccelTotal uint64
	var lastEventTime time.Time
	var lastBassYell time.Time
	var lastSnareYell time.Time
	hitCount := 0

	if stdioMode {
		go readStdinCommands()
	}

	presetLabel := "default"
	if fastMode {
		presetLabel = "fast"
	}
	fmt.Printf("spank: djembe mode (%s tuning, bass>=%.2f)... (ctrl+c to quit)\n", presetLabel, bassThreshold)
	if stdioMode {
		fmt.Println(`{"status":"ready"}`)
	}

	ticker := time.NewTicker(tuning.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			fmt.Println("\nbye!")
			return nil
		case err := <-sensorErr:
			return fmt.Errorf("sensor worker failed: %w", err)
		case <-ticker.C:
		}

		// Check if paused
		pausedMu.RLock()
		isPaused := paused
		pausedMu.RUnlock()
		if isPaused {
			continue
		}

		now := time.Now()
		tNow := float64(now.UnixNano()) / 1e9

		samples, newTotal := accelRing.ReadNew(lastAccelTotal, shm.AccelScale)
		lastAccelTotal = newTotal
		if len(samples) > tuning.maxBatch {
			samples = samples[len(samples)-tuning.maxBatch:]
		}

		nSamples := len(samples)
		for idx, sample := range samples {
			tSample := tNow - float64(nSamples-idx-1)/float64(det.FS)
			det.Process(sample.X, sample.Y, sample.Z, tSample)
		}

		if len(det.Events) == 0 {
			continue
		}

		ev := det.Events[len(det.Events)-1]
		if ev.Time.Equal(lastEventTime) {
			continue
		}
		lastEventTime = ev.Time

		if ev.Amplitude < tuning.minAmplitude {
			continue
		}

		// Route to bass or snare, enforce per-voice cooldown.
		sp, file, voice := dp.selectVoice(ev.Amplitude)
		cooldown := tuning.cooldown
		switch voice {
		case "bass":
			if time.Since(lastBassYell) <= cooldown {
				continue
			}
			lastBassYell = now
		case "snare":
			if time.Since(lastSnareYell) <= cooldown {
				continue
			}
			lastSnareYell = now
		}

		hitCount++
		if stdioMode {
			event := map[string]interface{}{
				"timestamp": now.Format(time.RFC3339Nano),
				"hit":       hitCount,
				"voice":     voice,
				"amplitude": ev.Amplitude,
				"severity":  string(ev.Severity),
				"file":      file,
			}
			if data, err := json.Marshal(event); err == nil {
				fmt.Println(string(data))
			}
		} else {
			fmt.Printf("hit #%d [%s amp=%.5fg] -> %s\n", hitCount, voice, ev.Amplitude, file)
		}
		go playAudio(sp, file, ev.Amplitude, &speakerInit)
	}
}

var speakerMu sync.Mutex

// amplitudeToVolume maps a detected amplitude to a beep/effects.Volume
// level. Amplitude typically ranges from ~0.05 (light tap) to ~1.0+
// (hard slap). The mapping uses a logarithmic curve so that light taps
// are noticeably quieter and hard hits play near full volume.
//
// Returns a value in the range [-3.0, 0.0] for use with effects.Volume
// (base 2): -3.0 is ~1/8 volume, 0.0 is full volume.
func amplitudeToVolume(amplitude float64) float64 {
	const (
		minAmp = 0.05 // softest detectable
		maxAmp = 0.80 // treat anything above this as max
		minVol = -3.0 // quietest playback (1/8 volume with base 2)
		maxVol = 0.0  // full volume
	)

	if amplitude <= minAmp {
		return minVol
	}
	if amplitude >= maxAmp {
		return maxVol
	}

	t := (amplitude - minAmp) / (maxAmp - minAmp)
	t = math.Log(1+t*99) / math.Log(100)
	return minVol + t*(maxVol-minVol)
}

func playAudio(pack *soundPack, path string, amplitude float64, speakerInit *bool) {
	var streamer beep.StreamSeekCloser
	var format beep.Format

	if pack.custom {
		file, err := os.Open(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "spank: open %s: %v\n", path, err)
			return
		}
		defer file.Close()
		streamer, format, err = mp3.Decode(file)
		if err != nil {
			fmt.Fprintf(os.Stderr, "spank: decode %s: %v\n", path, err)
			return
		}
	} else {
		data, err := pack.fs.ReadFile(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "spank: read %s: %v\n", path, err)
			return
		}
		streamer, format, err = mp3.Decode(io.NopCloser(bytes.NewReader(data)))
		if err != nil {
			fmt.Fprintf(os.Stderr, "spank: decode %s: %v\n", path, err)
			return
		}
	}
	defer streamer.Close()

	speakerMu.Lock()
	if !*speakerInit {
		speaker.Init(format.SampleRate, format.SampleRate.N(time.Second/10))
		*speakerInit = true
	}
	speakerMu.Unlock()

	var source beep.Streamer = streamer
	if volumeScaling {
		source = &effects.Volume{
			Streamer: streamer,
			Base:     2,
			Volume:   amplitudeToVolume(amplitude),
			Silent:   false,
		}
	}

	if speedRatio != 1.0 && speedRatio > 0 {
		fakeRate := beep.SampleRate(int(float64(format.SampleRate) * speedRatio))
		source = beep.Resample(4, fakeRate, format.SampleRate, source)
	}

	done := make(chan bool)
	speaker.Play(beep.Seq(source, beep.Callback(func() {
		done <- true
	})))
	<-done
}

// stdinCommand represents a command received via stdin
type stdinCommand struct {
	Cmd           string  `json:"cmd"`
	Amplitude     float64 `json:"amplitude,omitempty"`
	Cooldown      int     `json:"cooldown,omitempty"`
	Speed         float64 `json:"speed,omitempty"`
	BassThreshold float64 `json:"bassThreshold,omitempty"`
}

// readStdinCommands reads JSON commands from stdin for live control
func readStdinCommands() {
	processCommands(os.Stdin, os.Stdout)
}

// processCommands reads JSON commands from r and writes responses to w.
// This is the testable core of the stdin command handler.
func processCommands(r io.Reader, w io.Writer) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		var cmd stdinCommand
		if err := json.Unmarshal([]byte(line), &cmd); err != nil {
			if stdioMode {
				fmt.Fprintf(w, `{"error":"invalid command: %s"}%s`, err.Error(), "\n")
			}
			continue
		}

		switch cmd.Cmd {
		case "pause":
			pausedMu.Lock()
			paused = true
			pausedMu.Unlock()
			if stdioMode {
				fmt.Fprintln(w, `{"status":"paused"}`)
			}
		case "resume":
			pausedMu.Lock()
			paused = false
			pausedMu.Unlock()
			if stdioMode {
				fmt.Fprintln(w, `{"status":"resumed"}`)
			}
		case "set":
			if cmd.Amplitude > 0 && cmd.Amplitude <= 1 {
				minAmplitude = cmd.Amplitude
			}
			if cmd.Cooldown > 0 {
				cooldownMs = cmd.Cooldown
			}
			if cmd.Speed > 0 {
				speedRatio = cmd.Speed
			}
			if cmd.BassThreshold > 0 && cmd.BassThreshold <= 1 {
				bassThreshold = cmd.BassThreshold
			}
			if stdioMode {
				fmt.Fprintf(w, `{"status":"settings_updated","amplitude":%.4f,"cooldown":%d,"speed":%.2f,"bass_threshold":%.4f}%s`,
					minAmplitude, cooldownMs, speedRatio, bassThreshold, "\n")
			}
		case "volume-scaling":
			volumeScaling = !volumeScaling
			if stdioMode {
				fmt.Fprintf(w, `{"status":"volume_scaling_toggled","volume_scaling":%t}%s`, volumeScaling, "\n")
			}
		case "status":
			pausedMu.RLock()
			isPaused := paused
			pausedMu.RUnlock()
			if stdioMode {
				fmt.Fprintf(w, `{"status":"ok","paused":%t,"amplitude":%.4f,"cooldown":%d,"volume_scaling":%t,"speed":%.2f,"bass_threshold":%.4f}%s`,
					isPaused, minAmplitude, cooldownMs, volumeScaling, speedRatio, bassThreshold, "\n")
			}
		default:
			if stdioMode {
				fmt.Fprintf(w, `{"error":"unknown command: %s"}%s`, cmd.Cmd, "\n")
			}
		}
	}
}
