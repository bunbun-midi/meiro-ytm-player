package player

import (
	"bytes"
	"io"
	"math"
	"testing"
	"time"
)

func TestVoiceGains(t *testing.T) {
	main, oct := voiceGains(0, false)
	if main != 1 || oct != 0 {
		t.Fatalf("fader left, add: %v %v", main, oct)
	}
	main, oct = voiceGains(1, false)
	if main != 1 || oct != 1 {
		t.Fatalf("fader right, add: %v %v", main, oct)
	}
	main, oct = voiceGains(0, true)
	if main != 1 || oct != 0 {
		t.Fatalf("fader left, crossfade: %v %v", main, oct)
	}
	main, oct = voiceGains(1, true)
	if math.Abs(main) > 1e-9 || math.Abs(oct-1) > 1e-9 {
		t.Fatalf("fader right, crossfade: %v %v", main, oct)
	}
	main, oct = voiceGains(0.5, true)
	if math.Abs(main*main+oct*oct-1) > 1e-9 {
		t.Fatalf("centre crossfade is not equal power: %v %v", main, oct)
	}
}

func TestFanGivesBothReadersTheSameBytes(t *testing.T) {
	src := sinePCM(440, 50*time.Millisecond)
	fan := &pcmFan{src: bytes.NewReader(src)}
	left, right := fan.branch(0), fan.branch(1)
	bufL, bufR := make([]byte, 1024), make([]byte, 1024)
	var gotL, gotR []byte
	for {
		n, err := left.Read(bufL)
		gotL = append(gotL, bufL[:n]...)
		m, errR := right.Read(bufR[:n])
		gotR = append(gotR, bufR[:m]...)
		if m != n {
			t.Fatalf("second reader got %d, first got %d", m, n)
		}
		if err == io.EOF || errR == io.EOF {
			break
		}
		if err != nil || errR != nil {
			t.Fatal(err, errR)
		}
	}
	if !bytes.Equal(gotL, src) || !bytes.Equal(gotR, src) {
		t.Fatalf("fan changed the stream: %d %d, source %d", len(gotL), len(gotR), len(src))
	}
}

func TestTwoPitchesStayInStep(t *testing.T) {
	src := sinePCM(220, 400*time.Millisecond)
	mix := newVoiceMix(bytes.NewReader(src), 1, 0, StretchOverlapAdd, Blend{Rel: 12, Fade: 1, Crossfade: true})
	out := readAll(t, mix)
	// Crossfade fully right: only the octave. Its zero crossings in the
	// settled half should be about twice a copy at the original pitch.
	plain := readAll(t, newRateReader(bytes.NewReader(src), newRateControl(1, 0)))
	if delta := len(out) - len(plain); delta < -sampleRate/20*frameBytes || delta > sampleRate/20*frameBytes {
		t.Fatalf("octave output length %d, plain %d", len(out), len(plain))
	}
	low := crossings(plain[len(plain)/2:])
	high := crossings(out[len(out)/2:])
	if high < low*3/2 || high > low*5/2 {
		t.Fatalf("octave crossings %d, original %d", high, low)
	}
}

func TestNormalMixIsTheRecording(t *testing.T) {
	src := sinePCM(440, 200*time.Millisecond)
	for i := 0; i < len(src); i += frameBytes {
		src[i+2], src[i+3] = 0, 0
	}
	mix := newVoiceMix(bytes.NewReader(append([]byte(nil), src...)), 1, 0, StretchBungee, Blend{Rel: 12, Fade: 0})
	got := readAll(t, mix)
	if !bytes.Equal(got, src) {
		t.Fatalf("a closed second voice changed the recording: %d vs %d", len(got), len(src))
	}
}

func TestMainVoiceIsUnchangedWhenTheSecondIsSilent(t *testing.T) {
	src := sinePCM(440, 300*time.Millisecond)
	plain := readAll(t, paced(bytes.NewReader(append([]byte(nil), src...)), 1, -7))
	mix := newVoiceMix(bytes.NewReader(src), 1, -7, StretchOverlapAdd, Blend{Rel: 12, Fade: 0})
	got := readAll(t, mix)
	if !bytes.Equal(got, plain) {
		t.Fatalf("a silent second voice changed the main one: %d vs %d", len(got), len(plain))
	}
}

func TestLimiterSettlesASumNearFullScale(t *testing.T) {
	var state compState
	state.store(DefaultCompressor())
	var peak float64
	for i := range sampleRate / 5 {
		left, _ := state.apply(1.6, 1.6)
		if i > sampleRate/20 && math.Abs(left) > peak {
			peak = math.Abs(left)
		}
	}
	if peak > 1.05 {
		t.Fatalf("limited peak %v", peak)
	}
	if peak < 0.9 {
		t.Fatalf("limiter dulled a full-scale sum to %v", peak)
	}
	var quiet compState
	quiet.store(Compressor{On: false, Ratio: 4, Threshold: -20, Attack: 5, Release: 50})
	left, _ := quiet.apply(0.25, 0.25)
	if math.Abs(left-0.25) > 1e-9 {
		t.Fatalf("a limiter that is off changed the sample to %v", left)
	}
}

func TestSoftKneeEasesIn(t *testing.T) {
	hard := gainReduction(math.Pow(10, 0/20), 0, 4, 0)
	if hard != 0 {
		t.Fatalf("hard knee at the threshold reduced by %v dB", hard)
	}
	soft := gainReduction(1, 0, 4, 6)
	if soft <= 0 || soft > 1 {
		t.Fatalf("soft knee at the threshold reduced by %v dB", soft)
	}
}

func TestSpectrumHearsATone(t *testing.T) {
	m := newMeters()
	const freq = 8 * sampleRate / fftSize
	for i := range fftSize * 4 {
		s := math.Sin(2 * math.Pi * float64(freq) * float64(i) / sampleRate)
		m.sample(s, s)
	}
	level := m.load()
	loudest, at := float32(0), 0
	for i, band := range level.Bands {
		if band > loudest {
			loudest, at = band, i
		}
	}
	// Bin 8 falls in band 1 (bins 5–8) or band 2 (bins 9–12). The Hann
	// window spreads it, so either neighbour counts.
	if at < 1 || at > 2 || loudest < 0.1 {
		t.Fatalf("tone landed in band %d at %v, bands %v", at, loudest, level.Bands)
	}
	if level.Left < 0.5 || level.Right < 0.5 {
		t.Fatalf("level meter %v %v", level.Left, level.Right)
	}
}

func paced(src io.Reader, speed, pitch float64) io.Reader {
	r := newRateReader(src, newRateControl(speed, pitch))
	r.set(speed, pitch, StretchOverlapAdd)
	return r
}

func readAll(t *testing.T, r io.Reader) []byte {
	t.Helper()
	var out []byte
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		out = append(out, buf[:n]...)
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			t.Fatal("reader stalled")
		}
	}
}

func crossings(pcm []byte) int {
	frames := len(pcm) / frameBytes
	n := 0
	var prev float64
	for i := range frames {
		left, _ := frameAt(pcm, i)
		if prev <= 0 && left > 0 {
			n++
		}
		prev = left
	}
	return n
}
