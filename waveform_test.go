package main

import (
	"testing"
	"time"

	"github.com/egoist/mygo/ui"

	"github.com/elianiva/meiro/internal/bpm"
)

func TestBarBeatMatchesTheFirstTwoAbletonFields(t *testing.T) {
	grid := bpm.Grid{BPM: 60, Beat: time.Second, Measure: 4 * time.Second}
	bar, beat := barAndBeat(grid, 10*time.Second)
	if bar != 3 || beat != 3 {
		t.Fatalf("10s is %d.%d, want 3.3", bar, beat)
	}
	if got := formatStep(beatIndex(grid, 0), beatIndex(grid, 10*time.Second)); got != "+2.2" {
		t.Fatalf("step to 10s = %s, want +2.2", got)
	}
	if got := formatStep(beatIndex(grid, 10*time.Second), beatIndex(grid, 0)); got != "-2.2" {
		t.Fatalf("step back = %s, want -2.2", got)
	}
	// One whole bar is four beats, written +1.0.
	if got := formatStep(0, 4); got != "+1.0" {
		t.Fatalf("one bar = %s, want +1.0", got)
	}
}

func TestWaveformButtonOpensAZoomablePanel(t *testing.T) {
	a, _ := playbackApp(t)
	a.current = tracks("song")[0]
	a.total = 60 * time.Second
	a.settings.BPMDetection = true
	a.beats = beatState{
		id: "song", ok: true, done: true,
		grid: bpm.Grid{BPM: 120, Beat: 500 * time.Millisecond, Measure: 2 * time.Second},
	}
	tt := ui.NewTester(a.view, 1280, 700)
	tt.Frame()
	wave, ok := tt.Find("Waveform")
	if !ok {
		t.Fatalf("the player bar has no waveform button: %q", tt.Texts())
	}
	mute, ok := tt.Find("Mute")
	if !ok {
		t.Fatal("the player bar has no mute button")
	}
	if wave.X >= mute.X {
		t.Fatalf("waveform at %v is not left of mute at %v", wave.X, mute.X)
	}
	if err := tt.Click("Waveform"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if _, ok := tt.Find("Move waveform"); !ok {
		t.Fatalf("the waveform panel did not open: %q", tt.Texts())
	}
	view, ok := tt.Find("Waveform view")
	if !ok {
		t.Fatal("the panel has no waveform")
	}
	tt.Move(view.X+view.W/2, view.Y+view.H/2)
	tt.Frame()
	if !tt.HasText("+15.0") || !tt.HasText("16.1") {
		t.Fatalf("hover does not show the bar and beat: %q", tt.Texts())
	}
	tt.ClickAt(view.X+view.W/2, view.Y+view.H/2)
	tt.Frame()
	if a.seekTarget < 25*time.Second || a.seekTarget > 35*time.Second {
		t.Fatalf("click seeked to %v, want about 30s", a.seekTarget)
	}
	tt.Wheel(view.X+20, view.Y+view.H/2, 0, -40)
	tt.Frame()
	if a.wave.span <= 0 || a.wave.span >= 60*time.Second {
		t.Fatalf("the wheel did not zoom the waveform, span = %v", a.wave.span)
	}
	if err := tt.Click("Close waveform"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if _, ok := tt.Find("Move waveform"); ok {
		t.Fatal("close left the waveform panel open")
	}
}

func TestFadeSeeksSwitch(t *testing.T) {
	a := newTestApp(t)
	a.router.Push("/settings")
	tt := ui.NewTester(a.view, 1000, 4400)
	tt.Frame()
	if a.settings.SeekFade {
		t.Fatal("seek fades start on")
	}
	if err := tt.Click("Fade seeks on"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if !a.settings.SeekFade {
		t.Fatal("the switch did not turn seek fades on")
	}
}
