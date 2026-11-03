package bpm

import (
	"math"
	"testing"
	"time"
)

func TestDetectsAClickTrack(t *testing.T) {
	const (
		rate    = 22050
		tempo   = 120.0
		seconds = 8.0
	)
	phase := 200 * time.Millisecond
	pcm := clicks(rate, tempo, phase, seconds, 4)
	grid, ok := Detect(pcm, rate)
	if !ok {
		t.Fatal("a steady click track produced no tempo")
	}
	if math.Abs(grid.BPM-tempo) > 1 {
		t.Errorf("BPM = %v, want about %v", grid.BPM, tempo)
	}
	if d := grid.Phase - phase; d < -20*time.Millisecond || d > 20*time.Millisecond {
		t.Errorf("phase = %v, want %v", grid.Phase, phase)
	}
	if grid.Measure < 1900*time.Millisecond || grid.Measure > 2100*time.Millisecond {
		t.Errorf("measure = %v, want about 2s", grid.Measure)
	}
}

func TestDetectRejectsSilence(t *testing.T) {
	if _, ok := Detect(make([]int16, 22050*8), 22050); ok {
		t.Fatal("silence looked like a beat")
	}
}

func TestChooseLagPrefersAMusicalOctave(t *testing.T) {
	hop := 110.0 / 22050
	scores := make([]float64, 201)
	scores[200] = 10
	scores[100] = 9
	if got := chooseLag(scores, 50, 200, 200, hop); got != 100 {
		t.Fatalf("lag %d, want the octave inside 80–160 BPM", got)
	}
	scores[100] = 1
	if got := chooseLag(scores, 50, 200, 200, hop); got != 200 {
		t.Fatalf("a weak octave moved the lag to %d", got)
	}
	scores[100] = 10
	scores[50] = 9
	if got := chooseLag(scores, 50, 200, 100, hop); got != 100 {
		t.Fatalf("a tempo already in range moved to lag %d", got)
	}
}

func TestJumpLandsOnTheGridWhenAsked(t *testing.T) {
	g := Grid{Measure: 2 * time.Second}
	free := []struct {
		pos, want time.Duration
		n         float64
	}{
		{4700 * time.Millisecond, 6700 * time.Millisecond, 1},
		{4 * time.Second, 6 * time.Second, 1},
		{4700 * time.Millisecond, 2700 * time.Millisecond, -1},
		{100 * time.Millisecond, 0, -1},
	}
	for _, tt := range free {
		if got := g.Jump(tt.pos, tt.n, false); got != tt.want {
			t.Errorf("free %+v from %v = %v, want %v", tt.n, tt.pos, got, tt.want)
		}
	}
	snap := []struct {
		pos, want time.Duration
		n         float64
	}{
		{4700 * time.Millisecond, 6 * time.Second, 1},
		{4 * time.Second, 6 * time.Second, 1},
		{4700 * time.Millisecond, 4 * time.Second, -1},
		{4 * time.Second, 2 * time.Second, -1},
		{4700 * time.Millisecond, 5 * time.Second, 0.5},
		{7500 * time.Millisecond, 8 * time.Second, 2},
	}
	for _, tt := range snap {
		if got := g.Jump(tt.pos, tt.n, true); got != tt.want {
			t.Errorf("snap %+v from %v = %v, want %v", tt.n, tt.pos, got, tt.want)
		}
	}
}

func TestMeasuresFollowThePhase(t *testing.T) {
	g := Grid{Phase: 200 * time.Millisecond, Measure: 2 * time.Second}
	got := g.Measures(5 * time.Second)
	want := []time.Duration{200 * time.Millisecond, 2200 * time.Millisecond, 4200 * time.Millisecond}
	if len(got) != len(want) {
		t.Fatalf("measures = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("measure %d = %v, want %v", i, got[i], want[i])
		}
	}
	at, ok := g.Nearest(2400*time.Millisecond, 5*time.Second)
	if !ok || at != 2200*time.Millisecond {
		t.Fatalf("nearest = %v, %v, want 2.2s", at, ok)
	}
}

// clicks is a mono click every beat, with a louder click every accent-th beat.
func clicks(rate int, tempo float64, phase time.Duration, seconds float64, accent int) []int16 {
	n := int(seconds * float64(rate))
	pcm := make([]int16, n)
	beat := 60 / tempo
	for i := 0; ; i++ {
		at := phase.Seconds() + float64(i)*beat
		if at >= seconds {
			break
		}
		sample := int(at * float64(rate))
		amp := int16(8000)
		if accent > 0 && i%accent == 0 {
			amp = 24000
		}
		for k := 0; k < 6 && sample+k < n; k++ {
			pcm[sample+k] = amp
		}
	}
	return pcm
}
