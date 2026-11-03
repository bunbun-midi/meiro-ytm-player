package main

import (
	"testing"

	"github.com/egoist/mygo/ui"

	"github.com/elianiva/meiro/player"
)

func TestOutputDeviceSetting(t *testing.T) {
	a := newTestApp(t)
	a.outputDevices = func() ([]player.OutputDevice, error) {
		return []player.OutputDevice{
			{ID: "speakers", Name: "Speakers"},
			{ID: "hdmi", Name: "HDMI"},
		}, nil
	}
	a.router.Push("/settings")
	a.settingsSearch.query = "output"
	tt := ui.NewTester(a.view, 1000, 4400)
	tt.Frame()
	if err := tt.Click("System default"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if err := tt.Click("HDMI"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if a.settings.OutputDevice != "hdmi" {
		t.Fatalf("saved %q", a.settings.OutputDevice)
	}
	if a.player.OutputDeviceID() != "hdmi" {
		t.Fatalf("player stored %q", a.player.OutputDeviceID())
	}
	if a.outputMenu {
		t.Fatal("the menu stayed open")
	}
	if !tt.HasText("HDMI") {
		t.Fatalf("the button did not show the chosen device: %q", tt.Texts())
	}
}

func TestOutputDeviceHiddenWithoutAList(t *testing.T) {
	a := newTestApp(t)
	a.router.Push("/settings")
	a.settingsSearch.query = "output"
	tt := ui.NewTester(a.view, 1000, 4400)
	tt.Frame()
	if err := tt.Click("System default"); err == nil {
		t.Fatal("the output device was shown without a device list")
	}
}

func TestMissingOutputDeviceSaysSo(t *testing.T) {
	a := newTestApp(t)
	a.outputDevices = func() ([]player.OutputDevice, error) {
		return []player.OutputDevice{{ID: "speakers", Name: "Speakers"}}, nil
	}
	a.settings.OutputDevice = "gone"
	a.refreshOutputDevices()
	a.router.Push("/settings")
	a.settingsSearch.query = "output"
	tt := ui.NewTester(a.view, 1000, 4400)
	tt.Frame()
	if !tt.HasText("That device is unavailable, so Meiro uses the system default.") {
		t.Fatalf("missing device was not explained: %q", tt.Texts())
	}
	if !tt.HasText("Unavailable device") {
		t.Fatalf("the button did not say the device is gone: %q", tt.Texts())
	}
}
