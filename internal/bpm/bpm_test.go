package bpm

import (
	"math"
	"math/rand"
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
	if math.Abs(grid.BPM-tempo) > 0.5 {
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

func TestTempoHoldsForAMinute(t *testing.T) {
	const tempo = 123.4
	phase := 180 * time.Millisecond
	pcm := clicks(22050, tempo, phase, 60, 4)
	grid, ok := Detect(pcm, 22050)
	if !ok {
		t.Fatal("no tempo")
	}
	if math.Abs(grid.BPM-tempo) > 0.2 {
		t.Fatalf("BPM = %v, want %v", grid.BPM, tempo)
	}
	if d := grid.Phase - phase; d < -25*time.Millisecond || d > 25*time.Millisecond {
		t.Fatalf("phase = %v, want %v", grid.Phase, phase)
	}
	// Two hundred beats later the grid is still on the click, not a
	// rounded tempo that has walked off it.
	beats := 200.0
	want := phase + time.Duration(beats*60/tempo*float64(time.Second))
	got := grid.Phase + 200*grid.Beat
	if d := got - want; d < -25*time.Millisecond || d > 25*time.Millisecond {
		t.Fatalf("beat 200 at %v, want %v", got, want)
	}
}

func TestBackbeatDoesNotHalveOrShiftTheGrid(t *testing.T) {
	const rate = 22050
	pcm := make([]int16, rate*12)
	beat := 500 * time.Millisecond
	for i := 0; i < 24; i++ {
		at := time.Duration(i) * beat
		thump(pcm, rate, at, 70, 40, 9000)
		if i%2 == 1 {
			thump(pcm, rate, at, 2500, 12, 22000)
		}
	}
	grid, ok := Detect(pcm, rate)
	if !ok {
		t.Fatal("no tempo")
	}
	if math.Abs(grid.BPM-120) > 1 {
		t.Fatalf("BPM = %v, want about 120", grid.BPM)
	}
	off := math.Mod(grid.Phase.Seconds(), beat.Seconds())
	if off > beat.Seconds()/2 {
		off = beat.Seconds() - off
	}
	// The bar line sits on a kick, which is on the even beats, not on the snare.
	if off > 0.08 {
		t.Fatalf("phase %v is %.0fms off the kick", grid.Phase, off*1000)
	}
}

func TestIntroDoesNotSetTheTempo(t *testing.T) {
	const rate = 22050
	lead := 5 * time.Second
	body := clicks(rate, 100, 0, 16, 4)
	pcm := make([]int16, rate*5+len(body))
	copy(pcm[rate*5:], body)
	grid, ok := Detect(pcm, rate)
	if !ok {
		t.Fatal("no tempo")
	}
	if math.Abs(grid.BPM-100) > 1 {
		t.Fatalf("BPM = %v, want about 100", grid.BPM)
	}
	// The phase is folded into one measure, so the first click is a measure
	// start even though it is several seconds into the file.
	at, found := grid.Nearest(lead, lead+16*time.Second)
	if !found {
		t.Fatal("no measure near the first beat")
	}
	if d := at - lead; d < -40*time.Millisecond || d > 40*time.Millisecond {
		t.Fatalf("measure at %v, want the first beat at %v (phase %v)", at, lead, grid.Phase)
	}
}

func TestOffbeatHatsStayOnTheKick(t *testing.T) {
	const rate = 22050
	pcm := make([]int16, rate*12)
	beat := 500 * time.Millisecond
	for i := 0; i < 48; i++ {
		thump(pcm, rate, time.Duration(i)*beat/2, 6000, 8, 7000)
	}
	for i := 0; i < 24; i++ {
		thump(pcm, rate, time.Duration(i)*beat, 70, 40, 16000)
	}
	grid, ok := Detect(pcm, rate)
	if !ok {
		t.Fatal("no tempo")
	}
	if math.Abs(grid.BPM-120) > 1 {
		t.Fatalf("BPM = %v, want 120 rather than the hat rate", grid.BPM)
	}
}

func TestNoiseHasNoTempo(t *testing.T) {
	pcm := make([]int16, 22050*8)
	r := rand.New(rand.NewSource(1))
	for i := range pcm {
		pcm[i] = int16(r.Intn(2000) - 1000)
	}
	if _, ok := Detect(pcm, 22050); ok {
		t.Fatal("noise looked like a beat")
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

// thump adds a decaying tone, a kick when hz is low and a snare when it is high.
func thump(pcm []int16, rate int, at time.Duration, hz float64, ms int, amp int16) {
	n := rate * ms / 1000
	s0 := int(at.Seconds() * float64(rate))
	for i := 0; i < n && s0+i < len(pcm); i++ {
		env := 1 - float64(i)/float64(n)
		add := float64(amp) * env * math.Sin(2*math.Pi*hz*float64(i)/float64(rate))
		v := int(pcm[s0+i]) + int(add)
		if v > 32767 {
			v = 32767
		}
		if v < -32768 {
			v = -32768
		}
		pcm[s0+i] = int16(v)
	}
}
