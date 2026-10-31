package player

import (
	"errors"
	"io"
	"math"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestStreamArgsProtocolWhitelist(t *testing.T) {
	args := streamArgs("https://media.test/audio", 0, true, 1, 0)
	index := slices.Index(args, "-protocol_whitelist")
	if index < 0 || index+1 == len(args) {
		t.Fatalf("no protocol whitelist in %v", args)
	}
	if slices.Contains(strings.Split(args[index+1], ","), "file") {
		t.Fatalf("remote stream may read local files: %q", args[index+1])
	}
}

func TestReapObservesExitWhileReaderIsPaused(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test command uses sh")
	}
	p := &Player{}
	cmd := exec.Command("sh", "-c", "exit 7")
	pipe, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stdout = writer
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	_ = writer.Close()
	s := &session{cmd: cmd, pipe: pipe, done: make(chan struct{}), quit: make(chan struct{}), source: newCountingReader(pipe, nil)}
	go p.reap(s)
	select {
	case <-s.done:
	case <-time.After(time.Second):
		t.Fatal("process exit was not observed while the reader was idle")
	}
	close(s.quit)
}

func TestCleanExitWithUnreadOutputIsNotAFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test command uses sh")
	}
	p := &Player{}
	cmd := exec.Command("sh", "-c", "printf abcd")
	pipe, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stdout = writer
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	_ = writer.Close()
	s := &session{
		cmd: cmd, pipe: pipe, stderr: &boundedBuffer{limit: 16},
		done: make(chan struct{}), quit: make(chan struct{}), source: newCountingReader(pipe, nil),
	}
	go p.reap(s)
	<-s.done
	if !p.pendingSamples(s) {
		t.Fatal("clean exit with unread output must not read as a failed stream")
	}
	data, err := io.ReadAll(s.source)
	if err != nil || string(data) != "abcd" {
		t.Fatalf("read %q, %v: output lost after exit", data, err)
	}
	if p.pendingSamples(s) {
		t.Fatal("nothing is pending once the reader has the data")
	}
}

func TestPlaybackFilter(t *testing.T) {
	if got := playbackFilter(1, 0); got != "" {
		t.Fatalf("normal playback filter = %q, want none", got)
	}
	if got := playbackFilter(1.8, 0); got != "atempo=1.8" {
		t.Fatalf("speed filter = %q, want atempo=1.8", got)
	}
	const slow = "atempo=0.5,atempo=0.5,atempo=0.5,atempo=0.5,atempo=0.8"
	if got := playbackFilter(0.05, 0); got != slow {
		t.Fatalf("slow filter = %q, want %s", got, slow)
	}
	const up = "aresample=48000,asetrate=96000,aresample=48000,atempo=0.5"
	if got := playbackFilter(1, 12); got != up {
		t.Fatalf("pitch up filter = %q, want %s", got, up)
	}
	const down = "aresample=48000,asetrate=24000,aresample=48000,atempo=2"
	if got := playbackFilter(1, -12); got != down {
		t.Fatalf("pitch down filter = %q, want %s", got, down)
	}
}

func TestFiltersChangePitchAndTempo(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg is not installed")
	}
	pitched := renderFilter(t, ffmpeg, playbackFilter(1, 12), 0.5)
	if got := toneFrequency(pitched); math.Abs(got-880) > 20 {
		t.Fatalf("an octave up is %v Hz, want about 880", got)
	}
	slowed := renderFilter(t, ffmpeg, playbackFilter(0.05, 0), 0.2)
	seconds := float64(len(slowed)/4) / sampleRate
	if seconds < 3.4 || seconds > 4.6 {
		t.Fatalf("0.2s at 0.05× lasts %vs, want about 4s", seconds)
	}
	if got := toneFrequency(slowed); math.Abs(got-440) > 20 {
		t.Fatalf("slowed tone is %v Hz, want about 440", got)
	}
}

func renderFilter(t *testing.T, ffmpeg, filter string, seconds float64) []byte {
	t.Helper()
	args := []string{"-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=44100:duration=" + formatDecimal(seconds)}
	if filter != "" {
		args = append(args, "-af", filter)
	}
	args = append(args, "-f", "s16le", "-ac", "2", "-ar", "48000", "pipe:1")
	cmd := exec.Command(ffmpeg, args...)
	out, err := cmd.Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			t.Fatalf("ffmpeg %v: %v: %s", args, err, exit.Stderr)
		}
		t.Fatalf("ffmpeg %v: %v", args, err)
	}
	return out
}

// toneFrequency is the frequency of a stereo s16le buffer, from rising
// crossings of the left channel over the middle of it.
func toneFrequency(pcm []byte) float64 {
	const frame = 4
	frames := len(pcm) / frame
	if frames < 1000 {
		return 0
	}
	start, end := frames/10, frames-frames/10
	crossings := 0
	prev := pcmSample(pcm, start)
	for i := start + 1; i < end; i++ {
		sample := pcmSample(pcm, i)
		if prev < 0 && sample >= 0 {
			crossings++
		}
		prev = sample
	}
	return float64(crossings) / (float64(end-start) / sampleRate)
}

func pcmSample(pcm []byte, frame int) int16 {
	off := frame * 4
	return int16(uint16(pcm[off]) | uint16(pcm[off+1])<<8)
}
