package player

import (
	"bytes"
	"io"
	"math"
	"testing"
	"time"
)

func TestBassBoostLiftsLowNotesAndLeavesHighs(t *testing.T) {
	low := sinePCM(80, time.Second)
	high := sinePCM(8000, time.Second)
	boostedLow := throughBass(t, low, true, MaxBassBoost)
	boostedHigh := throughBass(t, high, true, MaxBassBoost)
	if got, base := rmsPCM(boostedLow), rmsPCM(low); got < base*1.5 {
		t.Fatalf("80 Hz RMS = %v, dry = %v, want the low note louder", got, base)
	}
	if got, base := rmsPCM(boostedHigh), rmsPCM(high); math.Abs(got-base) > base*0.25 {
		t.Fatalf("8 kHz RMS = %v, dry = %v, want the high note left alone", got, base)
	}
	// Quiet enough that a 15 dB lift still fits in the sample, so clipping
	// cannot eat the high tone.
	mixed := mixPCM(scalePCM(low, 0.15), scalePCM(high, 0.3))
	boosted := throughBass(t, mixed, true, MaxBassBoost)
	if got, base := toneLevel(boosted, 8000), toneLevel(mixed, 8000); math.Abs(got-base) > base*0.25 {
		t.Fatalf("8 kHz beside a bass note = %v, dry = %v", got, base)
	}
}

func TestBassBoostOffLeavesTheStream(t *testing.T) {
	in := sinePCM(80, 200*time.Millisecond)
	out := throughBass(t, in, false, MaxBassBoost)
	if !bytes.Equal(in, out) {
		t.Fatal("a boost that is off changed the audio")
	}
}

func throughBass(t *testing.T, input []byte, on bool, db float64) []byte {
	t.Helper()
	output := make([]byte, len(input))
	reader := newBassReader(bytes.NewReader(input), newBassBoost(on, db))
	if _, err := io.ReadFull(reader, output); err != nil {
		t.Fatal(err)
	}
	return output
}

func scalePCM(pcm []byte, gain float64) []byte {
	out := make([]byte, len(pcm))
	for offset := 0; offset+1 < len(pcm); offset += 2 {
		sample := int(int16(uint16(pcm[offset]) | uint16(pcm[offset+1])<<8))
		scaled := int(math.Round(float64(sample) * gain))
		if scaled > math.MaxInt16 {
			scaled = math.MaxInt16
		}
		if scaled < math.MinInt16 {
			scaled = math.MinInt16
		}
		out[offset] = byte(scaled)
		out[offset+1] = byte(scaled >> 8)
	}
	return out
}

func mixPCM(a, b []byte) []byte {
	n := min(len(a), len(b))
	out := make([]byte, n)
	for offset := 0; offset+1 < n; offset += 2 {
		left := int(int16(uint16(a[offset]) | uint16(a[offset+1])<<8))
		right := int(int16(uint16(b[offset]) | uint16(b[offset+1])<<8))
		sum := left + right
		if sum > math.MaxInt16 {
			sum = math.MaxInt16
		}
		if sum < math.MinInt16 {
			sum = math.MinInt16
		}
		out[offset] = byte(sum)
		out[offset+1] = byte(sum >> 8)
	}
	return out
}

func toneLevel(pcm []byte, frequency float64) float64 {
	start := sampleRate / 5 * channelCount * bytesPerSample
	var re, im float64
	n := 0
	for frame := start; frame+bytesPerSample*channelCount-1 < len(pcm); frame += bytesPerSample * channelCount {
		sample := int16(uint16(pcm[frame]) | uint16(pcm[frame+1])<<8)
		w := 2 * math.Pi * frequency * float64(n) / sampleRate
		re += float64(sample) * math.Cos(w)
		im += float64(sample) * math.Sin(w)
		n++
	}
	if n == 0 {
		return 0
	}
	return math.Hypot(re, im) / float64(n)
}
