package main

import (
	"context"
	"time"

	"github.com/elianiva/meiro/internal/settings"
	"github.com/elianiva/meiro/player"
)

// eqLabels names the equaliser's bands the way a music player does.
var eqLabels = [player.EQBandCount]string{"31", "62", "125", "250", "500", "1k", "2k", "4k", "8k", "16k"}

// eqBoostRange is how far a band may be raised or cut, in dB.
const eqBoostRange = 12.0

// eqPresets are the curves an audio player's equaliser usually offers, one
// gain per EQFrequencies entry.
var eqPresets = []struct {
	name  string
	bands []float64
}{
	{"Flat", []float64{0, 0, 0, 0, 0, 0, 0, 0, 0, 0}},
	{"Bass Boost", []float64{7.5, 7, 5.5, 3.5, 1, 0, 0, 0, 0, 0}},
	{"Treble Boost", []float64{0, 0, 0, 0, 0, 1, 2.5, 4, 5.5, 7}},
	{"Vocal", []float64{-2, -1.5, 0, 2.5, 4.5, 4.5, 3, 1.5, 0, -1}},
	{"Acoustic", []float64{4, 3.5, 2, 1, 1.5, 1.5, 2.5, 3, 3.5, 3}},
	{"Pop", []float64{-1.5, -1, 0, 2, 4, 4, 2.5, 0, -1, -1.5}},
	{"Rock", []float64{5.5, 4.5, 3.5, 1.5, -1.5, -1.5, 1.5, 4, 5, 5.5}},
	{"Jazz", []float64{4, 3, 1.5, 2, -1.5, -1.5, 0, 1.5, 3, 4}},
	{"Classical", []float64{4, 3, 2, 0, -1, -1, 0, 2, 3, 4}},
	{"Electronic", []float64{6, 5.5, 2, 0, -2, -1, 0, 2, 4, 5.5}},
}

// eqBands returns the equaliser's gains as the player takes them: exactly
// one per band, nil when the equaliser is off.
func (a *app) eqBands() []float64 {
	if !a.settings.EQOn || len(a.settings.EQBands) != player.EQBandCount {
		return nil
	}
	return a.settings.EQBands
}

// applyBass sends the dynamic bass boost to the player.
func (a *app) applyBass() {
	a.settings.BassDB = settings.ClampBassBoost(a.settings.BassDB)
	a.player.SetBass(a.settings.BassBoostOn, a.settings.BassDB)
}

// applyEQ sends the equaliser's bands to the player and saves the settings.
func (a *app) applyEQ() {
	a.eqDebounce.stop()
	a.player.SetEQ(a.eqBands())
	a.saveSettings()
}

// scheduleEQ saves slider changes once they have been quiet for a moment. The
// player already applies each change live; the delay only avoids repeated
// settings writes while a slider moves.
func (a *app) scheduleEQ() {
	startTask(a, &a.eqDebounce, noTimeout,
		func(ctx context.Context) (struct{}, error) {
			if err := a.sleep(ctx, 250*time.Millisecond); err != nil {
				return struct{}{}, err
			}
			return struct{}{}, nil
		},
		func(_ struct{}, err error) {
			if err != nil {
				return
			}
			a.saveSettings()
		})
}

// loadEQ shapes the player with the bands the settings hold, growing or
// trimming them to the band count the player takes.
func (a *app) loadEQ() {
	a.normalizeEQBands()
	copy(a.eqApplied[:], a.settings.EQBands)
	a.player.SetEQ(a.eqBands())
}

// normalizeEQBands makes the settings' band table exactly the count the
// player takes, filling new bands with silence, and syncs what it has been
// sent with the slider state.
func (a *app) normalizeEQBands() {
	if len(a.settings.EQBands) == player.EQBandCount {
		return
	}
	bands := make([]float64, player.EQBandCount)
	copy(bands, a.settings.EQBands)
	a.settings.EQBands = bands
	copy(a.eqApplied[:], bands)
}
