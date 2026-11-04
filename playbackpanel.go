package main

import (
	"math"
	"strconv"

	"github.com/egoist/mygo/ui"

	"github.com/elianiva/meiro/internal/settings"
	"github.com/elianiva/meiro/m3"
	"github.com/elianiva/meiro/player"
)

// playbackNav is the rail button that opens the playback panel. It is not a
// page, so the rail handles it on its own.
const playbackNav = "playback"

// playbackColumn is the speed, pitch and second-voice controls shared by the
// speed button's popup and the panel that can be moved around.
func (a *app) playbackColumn(c *ui.Context, key string, presetW, textSize float32) {
	speed := a.speedFor(a.current)
	pitch := a.pitchFor(a.current)
	ui.Column(c).Gap(12).Children(func() {
		a.rateSlider(c, "Speed", "Playback speed", key+"-speed", player.MinSpeed, player.MaxSpeed, 0.001, speed, speedLabel, a.setSpeedFromSlider, true)
		a.rateSlider(c, "Pitch", "Pitch in semitones", key+"-pitch", player.MinPitch, player.MaxPitch, 0.1, pitch, pitchLabel, a.setPitch, false)
		a.pitchPresets(c, key, pitch, presetW, textSize)
		a.measureJumps(c, key)
		a.targetBPM(c, key)
		if m3.Button(c, m3.ButtonSpec{Label: "Normal", Kind: m3.TextOnly, Size: m3.Small40, Key: key + "-reset"}).Clicked() {
			a.resetPlaybackRate()
		}
		a.secondVoice(c, key, pitch)
	})
}

// secondVoice is the extra pitch, the fader that brings it in, and the
// limiter and meters that keep the two together from distorting.
func (a *app) secondVoice(c *ui.Context, key string, pitch float64) {
	sc := m3.Of(c).Scheme
	ui.Box(c).Height(1).FillWidth().Background(sc.OutlineVariant)
	a.rateSlider(c, "Second pitch", "Second pitch in semitones", key+"-octave", player.MinPitch, player.MaxPitch, 1, a.settings.OctaveSemitones, pitchLabel, a.setOctave, false)
	m3.Text(c, m3.BodySmall, "sounds at "+pitchLabel(pitch+a.settings.OctaveSemitones)).TextColor(sc.OnSurfaceVariant)
	a.rateSlider(c, "Second voice", "Second voice level", key+"-fade", 0, 1, 0.01, a.settings.OctaveFade, func(v float64) string {
		return strconv.Itoa(int(math.Round(v*100))) + "%"
	}, a.setOctaveFade, false)
	ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
		on := a.settings.OctaveCrossfade
		sw := m3.CompactSwitch(c, &on, "Crossfade")
		sw.Tooltip("Fade between the main voice and the second pitch. Off, the main voice stays at full volume and the slider adds the second pitch.")
		if sw.Changed() {
			a.settings.OctaveCrossfade = on
			a.saveSettings()
			a.applyBlend()
		}
		caption := "The main voice stays at full volume. Further right adds the second pitch."
		if on {
			caption = "Left is the main voice and right is the second pitch."
		}
		m3.Text(c, m3.BodySmall, caption).Grow(1).TextColor(sc.OnSurfaceVariant)
	})
	a.spectrumMeter(c)
	a.compressorControls(c, key)
}

func (a *app) spectrumMeter(c *ui.Context) {
	sc := m3.Of(c).Scheme
	level := a.player.Levels()
	ui.Row(c).Height(48).Gap(8).AlignItems(ui.Stretch).Children(func() {
		ui.Box(c).Grow(1).Height(48).Radius(m3.ExtraSmall).Clip().Background(sc.SurfaceContainerHighest).Label("Spectrum").
			Draw(func(p *ui.Painter, r ui.Rect) {
				p.AnimationFrameRate(30)
				n := len(level.Bands)
				if n == 0 {
					return
				}
				const gap float32 = 2
				w := (r.W - gap*float32(n-1)) / float32(n)
				if w < 1 {
					w = 1
				}
				for i, band := range level.Bands {
					h := max(float32(band)*r.H, 1)
					p.Fill(ui.Rect{X: r.X + float32(i)*(w+gap), Y: r.Y + r.H - h, W: w, H: h}, sc.Primary, 1)
				}
			})
		ui.Box(c).Width(18).Height(48).Radius(m3.ExtraSmall).Clip().Background(sc.SurfaceContainerHighest).Label("Level").
			Draw(func(p *ui.Painter, r ui.Rect) {
				p.AnimationFrameRate(30)
				drawMeter(p, r, 0, level.Left, sc.Primary)
				drawMeter(p, r, 1, level.Right, sc.Primary)
			})
	})
}

func drawMeter(p *ui.Painter, r ui.Rect, channel int, level float32, colour ui.Color) {
	w := float32(6)
	x := r.X + 3 + float32(channel)*(w+2)
	h := max(float32(level)*r.H, 1)
	p.Fill(ui.Rect{X: x, Y: r.Y + r.H - h, W: w, H: h}, colour, 1)
}

func (a *app) compressorControls(c *ui.Context, key string) {
	sc := m3.Of(c).Scheme
	ui.Row(c).AlignItems(ui.Center).Children(func() {
		m3.Text(c, m3.LabelMedium, "Compressor").TextColor(sc.OnSurfaceVariant)
		ui.Box(c).Grow(1)
		on := a.settings.Compressor
		sw := m3.CompactSwitch(c, &on, "Compressor on")
		sw.Tooltip("Hold the two voices under full scale. A single voice is left as it is until it passes the threshold.")
		if sw.Changed() {
			a.settings.Compressor = on
			a.saveSettings()
			a.applyBlend()
		}
	})
	db := func(v float64) string { return strconv.FormatFloat(v, 'f', 1, 64) + " dB" }
	ms := func(v float64) string { return strconv.FormatFloat(v, 'f', 1, 64) + " ms" }
	a.rateSlider(c, "Threshold", "Compressor threshold", key+"-threshold", player.MinCompThreshold, player.MaxCompThreshold, 0.5, a.settings.CompThreshold, db, func(v float64) {
		a.setComp(func(s *settings.Settings) { s.CompThreshold = v })
	}, false)
	a.rateSlider(c, "Ratio", "Compressor ratio", key+"-ratio", player.MinCompRatio, player.MaxCompRatio, 0.5, a.settings.CompRatio, func(v float64) string {
		return strconv.FormatFloat(v, 'f', 1, 64) + ":1"
	}, func(v float64) { a.setComp(func(s *settings.Settings) { s.CompRatio = v }) }, false)
	a.rateSlider(c, "Attack", "Compressor attack", key+"-attack", player.MinCompAttack, player.MaxCompAttack, 0.1, a.settings.CompAttack, ms, func(v float64) {
		a.setComp(func(s *settings.Settings) { s.CompAttack = v })
	}, false)
	a.rateSlider(c, "Release", "Compressor release", key+"-release", player.MinCompRelease, player.MaxCompRelease, 1, a.settings.CompRelease, ms, func(v float64) {
		a.setComp(func(s *settings.Settings) { s.CompRelease = v })
	}, false)
	a.rateSlider(c, "Knee", "Compressor knee", key+"-knee", player.MinCompKnee, player.MaxCompKnee, 0.5, a.settings.CompKnee, db, func(v float64) {
		a.setComp(func(s *settings.Settings) { s.CompKnee = v })
	}, false)
	a.rateSlider(c, "Makeup", "Compressor makeup", key+"-makeup", player.MinCompMakeup, player.MaxCompMakeup, 0.5, a.settings.CompMakeup, db, func(v float64) {
		a.setComp(func(s *settings.Settings) { s.CompMakeup = v })
	}, false)
}

func (a *app) setOctave(semitones float64) {
	semitones = settings.ClampOctave(math.Round(semitones))
	if a.settings.OctaveSemitones == semitones {
		return
	}
	a.settings.OctaveSemitones = semitones
	a.saveSettings()
	a.applyBlend()
}

func (a *app) setOctaveFade(fade float64) {
	fade = settings.ClampUnitInterval(fade)
	if math.Abs(a.settings.OctaveFade-fade) < 1e-6 {
		return
	}
	a.settings.OctaveFade = fade
	a.saveSettings()
	a.applyBlend()
}

func (a *app) setComp(edit func(*settings.Settings)) {
	edit(&a.settings)
	a.saveSettings()
	a.applyBlend()
}

// applyBlend publishes the second voice and the limiter. Both take effect
// on the next block of audio, without a gap.
func (a *app) applyBlend() {
	s := a.settings
	a.player.SetBlend(player.Blend{Rel: s.OctaveSemitones, Fade: s.OctaveFade, Crossfade: s.OctaveCrossfade})
	a.player.SetCompressor(player.Compressor{
		On: s.Compressor, Threshold: s.CompThreshold, Ratio: s.CompRatio,
		Attack: s.CompAttack, Release: s.CompRelease, Knee: s.CompKnee, Makeup: s.CompMakeup,
	})
}

// playbackPanelView is the playback panel when it is open on its own, above
// Settings or wherever it was dragged. The speed button's popup is separate
// and can hand off to this one.
func (a *app) playbackPanelView(c *ui.Context) {
	if !a.playbackPanel.open {
		return
	}
	sc := m3.Of(c).Scheme
	presetW, textSize := pitchPresetMetrics(c)
	width := playbackWidth(presetW)
	_, windowH := c.Size()
	body := windowH - 160
	if body < 240 {
		body = 240
	}
	panel := ui.Column(c.Key("playback-panel")).Absolute().Width(width).Padding(12, 8).Radius(m3.Large).
		Background(sc.SurfaceContainerHigh)
	if a.playbackPanel.placed {
		panel.Left(a.playbackPanel.x).Top(a.playbackPanel.y)
	} else {
		// To the right of the rail. The body scrolls, so the title stays on
		// screen while the rest extends down toward Settings.
		panel.Left(m3.RailCollapsed + 8).Top(48)
	}
	m3.Elevation(c, panel, 3)
	panel.Children(func() {
		bar := ui.Row(c).Height(32).AlignItems(ui.Center).Cursor(ui.CursorMove).Label("Move playback")
		dx, dy, held := bar.Dragged()
		bar.Children(func() {
			m3.Text(c, m3.LabelMedium, "Playback").TextColor(sc.OnSurfaceVariant).PassThrough()
			ui.Box(c).Grow(1).PassThrough()
			if m3.IconButton(c, m3.IconButtonSpec{Icon: m3.IconClose, Label: "Close playback", Size: m3.ExtraSmall32, Key: "playback-close"}).Clicked() {
				a.playbackPanel.open = false
			}
		})
		if held && (dx != 0 || dy != 0) && a.playbackPanel.open {
			if !a.playbackPanel.placed {
				b := panel.Bounds()
				if b.W > 0 {
					a.playbackPanel.x, a.playbackPanel.y = b.X, b.Y
				} else {
					_, h := c.Size()
					a.playbackPanel.x = m3.RailCollapsed + 8
					a.playbackPanel.y = max(h-body-96, 24)
				}
				a.playbackPanel.placed = true
			}
			if a.playbackPanel.placed {
				a.playbackPanel.x += dx
				a.playbackPanel.y += dy
				w, h := c.Size()
				a.playbackPanel.x = min(max(a.playbackPanel.x, 0), max(w-48, 0))
				a.playbackPanel.y = min(max(a.playbackPanel.y, 0), max(h-48, 0))
			}
			a.playbackPanel.dragging = true
		} else if a.playbackPanel.dragging {
			a.playbackPanel.dragging = false
			a.settings.PlaybackPlaced = a.playbackPanel.placed
			a.settings.PlaybackX = float64(a.playbackPanel.x)
			a.settings.PlaybackY = float64(a.playbackPanel.y)
			a.saveSettings()
		}
		ui.Scroll(c).Height(body).Children(func() {
			a.playbackColumn(c, "playback", presetW, textSize)
		})
	})
}

func playbackWidth(presetW float32) float32 {
	const (
		gap  float32 = 2
		padX float32 = 8
	)
	inner := 13*presetW + 12*gap
	return max(float32(280), inner+padX*2)
}
