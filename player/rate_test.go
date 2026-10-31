package player

import (
	"bytes"
	"io"
	"math"
	"testing"
	"time"
)

func TestRateAtUnityMatchesTheSource(t *testing.T) {
	in := sinePCM(440, 500*time.Millisecond)
	out := readRate(t, in, 1, 0)
	inFrames := len(in) / frameBytes
	outFrames := len(out) / frameBytes
	if outFrames < inFrames {
		t.Fatalf("unity playback is %d frames, source is %d", outFrames, inFrames)
	}
	// The last hop fades out, and the first one fades in. Between them the
	// windows sum to one, so the samples are the ones that went in.
	start, end := rateHopOut, inFrames-rateWindow
	var worst int
	for i := start; i < end; i++ {
		diff := abs(int(pcmSample(in, i)) - int(pcmSample(out, i)))
		if diff > worst {
			worst = diff
		}
	}
	if worst > 2 {
		t.Fatalf("unity playback differs from the source by %d", worst)
	}
}

func TestRateChangesPitchAndTempo(t *testing.T) {
	pitched := readRate(t, sinePCM(440, time.Second), 1, 12)
	if got := toneFrequency(pitched); math.Abs(got-880) > 40 {
		t.Fatalf("an octave up is %v Hz, want about 880", got)
	}
	if seconds := pcmSeconds(pitched); seconds < 0.9 || seconds > 1.2 {
		t.Fatalf("an octave up lasts %vs, want about 1s", seconds)
	}

	slowed := readRate(t, sinePCM(440, 200*time.Millisecond), 0.05, 0)
	if seconds := pcmSeconds(slowed); seconds < 3.4 || seconds > 4.6 {
		t.Fatalf("0.2s at 0.05× lasts %vs, want about 4s", seconds)
	}
	if got := toneFrequency(slowed); math.Abs(got-440) > 20 {
		t.Fatalf("slowed tone is %v Hz, want about 440", got)
	}

	faster := readRate(t, sinePCM(440, time.Second), 1.8, 0)
	if seconds := pcmSeconds(faster); seconds < 0.45 || seconds > 0.7 {
		t.Fatalf("1s at 1.8× lasts %vs, want about 0.56s", seconds)
	}
	if got := toneFrequency(faster); math.Abs(got-440) > 20 {
		t.Fatalf("sped-up tone is %v Hz, want about 440", got)
	}
}

func TestRateKeepsTheLevel(t *testing.T) {
	in := sinePCM(440, time.Second)
	want := rmsPCM(in)
	for _, rate := range []struct {
		speed, pitch float64
	}{{1, 0}, {0.5, 0}, {1.8, 0}, {1, 12}, {0.5, -12}} {
		got := rmsPCM(readRate(t, in, rate.speed, rate.pitch))
		// Resampling a tone costs a little level. A stretch that cancelled
		// would fall much further than that.
		limit := 0.2
		if rate.pitch != 0 {
			limit = 0.4
		}
		if math.Abs(got-want) > want*limit {
			t.Errorf("speed %v pitch %v RMS = %v, source = %v", rate.speed, rate.pitch, got, want)
		}
	}
}

func TestChangingRateDoesNotOpenAGapOrJumpThePosition(t *testing.T) {
	in := sinePCM(440, time.Second)
	ctl := newRateControl(1, 0)
	r := newRateReader(bytes.NewReader(in), ctl)
	buf := make([]byte, 4096)
	var out []byte
	changed := false
	for {
		n, err := r.Read(buf)
		out = append(out, buf[:n]...)
		if !changed && len(out)/frameBytes > sampleRate/3 {
			at := int64(len(out) / frameBytes)
			src := r.sourceAt(at)
			ctl.set(1.5, 5)
			if math.Abs(r.sourceAt(at)-src) > 1 {
				t.Fatalf("source position jumped from %v to %v when the rate changed", src, r.sourceAt(at))
			}
			n, err = r.Read(buf)
			out = append(out, buf[:n]...)
			advanced := r.sourceAt(at+int64(n/frameBytes)) - src
			gotFrames := float64(n / frameBytes)
			if advanced < gotFrames-float64(rateHopOut) || advanced > gotFrames*1.5+float64(rateHopOut) {
				t.Fatalf("source advanced by %v frames over %v of output, want between the old and new speeds", advanced, gotFrames)
			}
			changed = true
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if !changed {
		t.Fatal("the rate never changed")
	}
	if gap := silentRun(out); gap >= 0 {
		t.Fatalf("silence of at least 100ms at output frame %d", gap)
	}
	got := r.sourceAt(int64(len(out) / frameBytes))
	want := float64(len(in) / frameBytes)
	if math.Abs(got-want) > float64(sampleRate)/20 {
		t.Fatalf("source position at the end = %v, want %v", got, want)
	}
}

func TestSourcePositionReachesTheEndAtEachSpeed(t *testing.T) {
	in := sinePCM(440, time.Second)
	want := float64(len(in) / frameBytes)
	for _, speed := range []float64{0.05, 0.5, 1, 1.8} {
		r := newRateReader(bytes.NewReader(in), newRateControl(speed, 0))
		out, err := io.ReadAll(r)
		if err != nil {
			t.Fatalf("speed %v: %v", speed, err)
		}
		got := r.sourceAt(int64(len(out) / frameBytes))
		if math.Abs(got-want) > float64(sampleRate)/20 {
			t.Errorf("speed %v source position %v, want %v", speed, got, want)
		}
	}
}

func readRate(t *testing.T, in []byte, speed, pitch float64) []byte {
	t.Helper()
	out, err := io.ReadAll(newRateReader(bytes.NewReader(in), newRateControl(speed, pitch)))
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func pcmSeconds(pcm []byte) float64 {
	return float64(len(pcm)/frameBytes) / sampleRate
}

// toneFrequency is the frequency of a stereo s16le buffer, from rising
// crossings of the left channel over the middle of it.
func toneFrequency(pcm []byte) float64 {
	frames := len(pcm) / frameBytes
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
	off := frame * frameBytes
	return int16(uint16(pcm[off]) | uint16(pcm[off+1])<<8)
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// silentRun is the output frame where 100ms of near-silence begins, ignoring
// the fade at each end, or -1 when the tone never drops out.
func silentRun(pcm []byte) int {
	const quiet = 400
	need := sampleRate / 10
	frames := len(pcm) / frameBytes
	margin := sampleRate / 20
	run := 0
	for i := margin; i < frames-margin; i++ {
		sample := pcmSample(pcm, i)
		if sample > -quiet && sample < quiet {
			run++
			if run >= need {
				return i - run
			}
			continue
		}
		run = 0
	}
	return -1
}
