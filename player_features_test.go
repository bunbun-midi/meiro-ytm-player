package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"

	"github.com/elianiva/meiro/internal/bpm"
	"github.com/elianiva/meiro/internal/history"
	"github.com/elianiva/meiro/internal/playstate"
	"github.com/elianiva/meiro/internal/settings"
	"github.com/elianiva/meiro/youtube"
)

func TestGlobalShortcutCheckbox(t *testing.T) {
	a := newTestApp(t)
	a.saver.Path = filepath.Join(t.TempDir(), "settings.json")
	a.router.Push("/settings")
	tt := ui.NewTester(a.view, 1000, 5400)
	tt.Frame()
	if tt.HasText("Global Shuffle") {
		t.Fatal("shuffle offers a global shortcut")
	}
	if err := tt.Click("Global Play or pause"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if !a.settings.GlobalKeys["playPause"] {
		t.Fatalf("global keys = %v", a.settings.GlobalKeys)
	}
	a.saver.Wait()
	if got := settings.Load(a.saver.Path).GlobalKeys["playPause"]; !got {
		t.Fatal("the global shortcut was not saved")
	}
	if err := tt.Click("Global Play or pause"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if a.settings.GlobalKeys["playPause"] {
		t.Fatal("the checkbox did not turn the global shortcut off")
	}
}

func TestBPMDetectionSwitch(t *testing.T) {
	a := newTestApp(t)
	a.router.Push("/settings")
	tt := ui.NewTester(a.view, 1000, 4400)
	tt.Frame()
	if a.settings.BPMDetection {
		t.Fatal("BPM detection starts on")
	}
	if err := tt.Click("BPM detection on"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if !a.settings.BPMDetection {
		t.Fatal("the switch did not turn BPM detection on")
	}
}

func TestMeasureJumpsAndTicksSeek(t *testing.T) {
	a := newTestApp(t)
	a.current = tracks("song")[0]
	a.total = 8 * time.Second
	a.settings.BPMDetection = true
	a.beats = beatState{
		id: "song", ok: true, done: true,
		grid: bpm.Grid{BPM: 120, Beat: 500 * time.Millisecond, Measure: 2 * time.Second},
	}
	tt := ui.NewTester(a.view, 1000, 700)
	tt.Frame()
	if _, ok := tt.Find("Measure starts"); !ok {
		t.Fatal("the seek bar has no measure marks")
	}
	if err := tt.Click("1×"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	for _, label := range []string{
		"Jump forward 1 measure", "Jump forward 1/2 measure", "Jump forward 1/4 measure",
		"Jump forward 1/8 measure", "Jump forward 2 measures", "Jump forward 4 measures",
		"Jump forward 8 measures", "Jump back 1 measure", "Jump back 1/2 measure",
		"Jump back 1/4 measure", "Jump back 1/8 measure", "Jump back 2 measures",
		"Jump back 4 measures", "Jump back 8 measures",
	} {
		box, ok := tt.Find(label)
		if !ok || box.Y < 0 || box.Y+box.H > 700 || box.X < 0 || box.X+box.W > 1000 {
			t.Errorf("%s at %v, found %v", label, box, ok)
		}
	}
	if err := tt.Click("Jump forward 1 measure"); err != nil {
		t.Fatalf("no measure jump: %v in %q", err, tt.Texts())
	}
	if a.seekTarget != 2*time.Second {
		t.Fatalf("forward jump landed at %v, want 2s", a.seekTarget)
	}
	if err := tt.Click("Snap to measures"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if !a.settings.MeasureSnap {
		t.Fatal("the switch did not snap jumps to the measure grid")
	}
	if err := tt.Click("Measure starts"); err != nil {
		t.Fatal(err)
	}
	if a.seekTarget != 4*time.Second {
		t.Fatalf("clicking the marks landed at %v, want 4s", a.seekTarget)
	}
	if !tt.HasText("120 · 120 BPM") {
		t.Fatalf("tempo readout = %q", tt.Texts())
	}
	if err := a.player.SetSpeed(context.Background(), 1.5); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if !tt.HasText("120 · 180 BPM") {
		t.Fatalf("adjusted tempo readout = %q", tt.Texts())
	}
}

func TestEpisodeHasNoMeasureGrid(t *testing.T) {
	a := newTestApp(t)
	a.current = tracks("ep")[0]
	a.current.Kind = youtube.KindEpisode
	a.settings.BPMDetection = true
	a.beats = beatState{id: "ep", ok: true, grid: bpm.Grid{Measure: time.Second}}
	if _, ok := a.currentGrid(); ok {
		t.Fatal("an episode used a measure grid")
	}
}

func TestPlaybackHistoryQueuesTheRecordedTrack(t *testing.T) {
	a := newTestApp(t)
	a.queue.restore(playstate.State{Items: tracks("a", "b"), Index: 0, RecStart: -1})
	a.current = a.queue.current()
	a.history = []history.Entry{{VideoID: "old-1", Title: "Old Song", Artist: "Someone"}}
	a.historyLoaded = true
	tt := ui.NewTester(a.view, 1000, 700)
	tt.Frame()
	if err := tt.Click("Playback history"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if err := tt.RightClick("Old Song"); err != nil {
		t.Fatalf("no history row: %v in %q", err, tt.Texts())
	}
	tt.Frame()
	if err := tt.Click("Add Next"); err != nil {
		t.Fatalf("no Add Next: %v in %q", err, tt.Texts())
	}
	if a.queue.at(1).VideoID != "old-1" {
		t.Fatalf("next is %s, want old-1", a.queue.at(1).VideoID)
	}
	if err := tt.RightClick("Old Song"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if err := tt.Click("Add to Queue"); err != nil {
		t.Fatalf("no Add to Queue: %v in %q", err, tt.Texts())
	}
	if got := a.queue.at(a.queue.len() - 1).VideoID; got != "old-1" {
		t.Fatalf("end of the queue is %s, want old-1", got)
	}
}

func TestHistoryPageQueuesTheRecordedTrack(t *testing.T) {
	a := newTestApp(t)
	a.queue.restore(playstate.State{Items: tracks("a", "b"), Index: 0, RecStart: -1})
	a.current = a.queue.current()
	a.historyLoaded = true
	a.history = []history.Entry{{
		VideoID: "old-1", Title: "Old Song", Artist: "Someone",
		PlayedAt: time.Date(2026, 6, 1, 15, 0, 0, 0, time.Local),
	}}
	a.navigate(routeHistory)
	tt := ui.NewTester(a.view, 1000, 700)
	tt.Frame()
	if err := tt.RightClick("Old Song"); err != nil {
		t.Fatalf("no history row: %v in %q", err, tt.Texts())
	}
	tt.Frame()
	if err := tt.Click("Add Next"); err != nil {
		t.Fatalf("no Add Next: %v in %q", err, tt.Texts())
	}
	if a.queue.at(1).VideoID != "old-1" {
		t.Fatalf("next is %s, want old-1", a.queue.at(1).VideoID)
	}
}

func TestDownloadFormatDefaultsToMP3(t *testing.T) {
	a := newTestApp(t)
	a.saver.Path = filepath.Join(t.TempDir(), "settings.json")
	a.router.Push("/settings")
	tt := ui.NewTester(a.view, 1000, 1600)
	tt.Frame()
	if err := tt.Click("Search settings"); err != nil {
		t.Fatal(err)
	}
	tt.Type("mp3")
	tt.Frame()
	if err := tt.Click("M4A"); err != nil {
		t.Fatalf("no format choice: %v in %q", err, tt.Texts())
	}
	tt.Frame()
	if a.settings.DownloadFormat != settings.DownloadM4A {
		t.Fatalf("format = %q", a.settings.DownloadFormat)
	}
	a.saver.Wait()
	if got := settings.Load(a.saver.Path).DownloadFormat; got != settings.DownloadM4A {
		t.Fatalf("saved format = %q", got)
	}
}

func TestDownloadButtonSavesTheCurrentSong(t *testing.T) {
	a := newTestApp(t)
	a.openDownloads(t.TempDir())
	a.current = tracks("song")[0]
	a.current.Title = "Saved Song"
	tt := ui.NewTester(a.view, 1000, 700)
	tt.Frame()
	if err := tt.Click("Download"); err != nil {
		t.Fatalf("no download button: %v in %q", err, tt.Texts())
	}
	if !tt.HasText("Saved Song") {
		t.Fatalf("the download was not reported: %q", tt.Texts())
	}
}
