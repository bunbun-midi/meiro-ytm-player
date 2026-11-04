package main

import (
	"math"
	"strconv"
	"time"

	"github.com/egoist/mygo/ui"

	"github.com/elianiva/meiro/internal/bpm"
	"github.com/elianiva/meiro/internal/history"
	"github.com/elianiva/meiro/internal/keymap"
	"github.com/elianiva/meiro/m3"
	"github.com/elianiva/meiro/youtube"
)

// playHistory is the popup of recent plays and the menu on one of them.
type playHistory struct {
	open bool
	menu itemMenu
}

// transport builds the controls that start, stop and move through the queue:
// history, shuffle, previous, play or pause, next and repeat. The play button
// is a circle that squares off while the music plays, so the state shows in
// its shape. size is the play button's side.
func (a *app) transport(c *ui.Context, size float32, gap float32) {
	small := m3.IconButtonSpec{Size: m3.Small40}
	ui.Row(c).Gap(gap).AlignItems(ui.Center).Justify(ui.Center).Shrink(0).Children(func() {
		spec := small
		spec.Icon, spec.Label, spec.Key = m3.IconHistory, "Playback history", "history"
		historyButton := m3.IconButton(c, spec)
		a.playbackHistory(c, historyButton)
		spec = small
		spec.Icon, spec.Label, spec.Toggle, spec.Selected, spec.Key = m3.IconShuffle, "Shuffle", true, a.shuffle, "shuffle"
		a.hinted(c, func() {
			if m3.IconButton(c, spec).Clicked() {
				a.setShuffle(!a.shuffle)
			}
		}, keymap.Shuffle)
		spec = small
		spec.Icon, spec.Label, spec.Key = m3.IconPrevious, "Previous", "previous"
		a.hinted(c, func() {
			if m3.IconButton(c, spec).Clicked() {
				a.previous()
			}
		}, keymap.Previous)
		loading := a.loading()
		playing := a.player.Playing() && !loading
		icon, label := m3.IconPlay, "Play"
		switch {
		case loading:
			label = "Loading"
		case playing:
			icon, label = m3.IconPause, "Pause"
		}
		a.hinted(c, func() {
			if m3.IconButton(c, m3.IconButtonSpec{
				Icon: icon, Label: label, Kind: m3.FilledIcon, Dimension: size, Morph: true, Selected: playing, Loading: loading, Key: "play-pause",
			}).Clicked() {
				a.togglePlay()
			}
		}, keymap.PlayPause)
		spec = small
		spec.Icon, spec.Label, spec.Key = m3.IconNext, "Next", "next"
		a.hinted(c, func() {
			if m3.IconButton(c, spec).Clicked() {
				a.advance()
			}
		}, keymap.Next)
		spec = small
		repeatIcon := m3.IconRepeat
		if a.repeat == repeatTrack {
			repeatIcon = m3.IconRepeatOne
		}
		spec.Icon, spec.Label, spec.Toggle, spec.Selected, spec.Key = repeatIcon, "Repeat", true, a.repeat != repeatOff, "repeat"
		a.hinted(c, func() {
			if m3.IconButton(c, spec).Clicked() {
				a.cycleRepeat()
			}
		}, keymap.Repeat)
		spec = small
		item := a.current
		done := item.VideoID != "" && a.isDownloaded(item.VideoID)
		icon, label = m3.IconDownload, "Download"
		if done {
			icon, label = m3.IconDownloadDone, "Downloaded"
		}
		spec.Icon, spec.Label, spec.Key = icon, label, "download-track"
		spec.Disabled = done || !a.canDownload(item)
		spec.Loading = !done && a.downloading(item.VideoID)
		if m3.IconButton(c, spec).Clicked() {
			a.downloadSong(item)
		}
	})
}

// scrubber is the position slider: a wave that travels while the music plays
// and goes flat when it stops. It follows the player until the user takes it,
// and seeks when they let go. Times stand at both ends.
func (a *app) scrubber(c *ui.Context) {
	sc := m3.Of(c).Scheme
	known := a.total > 0
	total := a.total.Seconds()
	if !known {
		total, a.scrub = 1, 0
	}
	a.scrub = min(a.scrub, total)
	ui.Row(c).Gap(10).AlignItems(ui.Center).Children(func() {
		m3.Text(c, m3.LabelMedium, clock(time.Duration(a.scrub*float64(time.Second)))).
			MinWidth(40).NoWrap().TextAlign(ui.End).Shrink(0).TextColor(sc.OnSurfaceVariant).FontFeatures("tnum")
		ui.Column(c.Key("scrub-track")).Grow(1).MinWidth(40).Gap(0).AlignItems(ui.Stretch).Children(func() {
			// The slider's hit target reaches the marks above it, so a click
			// on a mark is also a slider change. The mark wins: it seeks to
			// the measure, and the slider does not seek again on that click.
			var onMark bool
			if grid, ok := a.currentGrid(); ok && known {
				onMark = a.measureTickMarks(c, grid, grid.Measures(a.total), sc)
			}
			slider := m3.Slider(c, &a.scrub, 0, total, m3.SliderSpec{
				Label: "Position", Key: "scrubber", Wavy: true, Waving: a.player.Playing(), Disabled: !known,
			})
			// The slider grows inside a row. In this column that would
			// stretch it vertically, so the track keeps its own height.
			slider.Grow(0)
			if onMark {
				a.scrubMark = true
				a.scrub = a.seekTarget.Seconds()
				return
			}
			if a.scrubMark {
				a.scrub = a.seekTarget.Seconds()
				if !slider.Pressed() {
					a.scrubMark = false
				}
				return
			}
			if slider.Pressed() {
				a.scrubbing = true
			}
			if a.scrubbing && !slider.Pressed() {
				a.scrubbing = false
				a.seek(time.Duration(a.scrub * float64(time.Second)))
			} else if slider.Changed() && !slider.Pressed() {
				// Arrow keys, Home and End change the value without a pointer press.
				a.seek(time.Duration(a.scrub * float64(time.Second)))
			}
		})
		if grid, ok := a.currentGrid(); ok && known {
			speed := a.player.Speed()
			if speed <= 0 || math.IsNaN(speed) {
				speed = 1
			}
			text := formatBPM(grid.BPM) + " · " + formatBPM(grid.BPM*speed) + " BPM"
			m3.Text(c, m3.LabelMedium, text).Shrink(0).NoWrap().TextColor(sc.OnSurfaceVariant).
				Tooltip("The track's tempo, and that tempo at the current speed.")
		}
		m3.Text(c, m3.LabelMedium, clock(a.total)).MinWidth(40).NoWrap().Shrink(0).TextColor(sc.OnSurfaceVariant).FontFeatures("tnum")
	})
}

// formatBPM prints a tempo. A whole number stays whole.
func formatBPM(v float64) string {
	if math.Abs(v-math.Round(v)) < 0.05 {
		return strconv.FormatFloat(math.Round(v), 'f', 0, 64)
	}
	return strconv.FormatFloat(v, 'f', 1, 64)
}

// measureTickMarks paints the start of each measure above the scrubber. A
// click seeks to the nearest one. The tempo is drawn beside this strip.
// It reports whether that click happened.
func (a *app) measureTickMarks(c *ui.Context, grid bpm.Grid, marks []time.Duration, sc m3.Scheme) bool {
	// Taller than the slider's hit slop, so a click on a mark reaches the
	// marks and not the slider underneath.
	bar := ui.ButtonBase(c.Key("measure-ticks")).Height(20).Label("Measure starts").Cursor(ui.CursorPointer)
	bar.Background(ui.Transparent)
	bar.Draw(func(p *ui.Painter, r ui.Rect) {
		if a.total <= 0 || r.W <= 0 {
			return
		}
		for _, at := range marks {
			x := r.X + r.W*float32(float64(at)/float64(a.total))
			p.Fill(ui.Rect{X: x - 0.75, Y: r.Y, W: 1.5, H: r.H}, sc.Primary, 1)
		}
	})
	if !bar.Clicked() {
		return false
	}
	x, _, _ := bar.PointerPosition()
	if w := bar.Bounds().W; w > 0 {
		at := time.Duration(float64(x/w) * float64(a.total))
		if target, ok := grid.Nearest(at, a.total); ok {
			a.scrub = target.Seconds()
			a.seek(target)
		}
	}
	return true
}

// volumeIcon is the speaker shape for the current volume.
func (a *app) volumeIcon() *ui.SVG {
	icon := m3.IconVolumeUp
	switch {
	case a.volume == 0:
		icon = m3.IconVolumeOff
	case a.volume < 45:
		icon = m3.IconVolumeDown
	}
	return icon
}

// waveformButton opens the floating waveform. It sits to the left of Mute.
func (a *app) waveformButton(c *ui.Context) {
	button := m3.IconButton(c, m3.IconButtonSpec{Icon: m3.IconWaveform, Label: "Waveform", Key: "waveform"})
	if button.Clicked() {
		a.wave.open = !a.wave.open
	}
}

// muteButton is the speaker button, which mutes or restores the volume.
func (a *app) muteButton(c *ui.Context) ui.Element {
	button := m3.IconButton(c, m3.IconButtonSpec{Icon: a.volumeIcon(), Label: "Mute", Key: "mute"})
	if button.Clicked() {
		a.toggleMute()
	}
	return button
}

// volumeSlider is the volume setting, horizontal in the player bar and
// vertical in the narrow bar's hover panel.
func (a *app) volumeSlider(c *ui.Context, vertical bool) ui.Element {
	v := a.volume
	slider := m3.Slider(c, &v, 0, 100, m3.SliderSpec{Label: "Volume", Key: "volume", Vertical: vertical})
	if slider.Changed() {
		a.setVolume(v)
	}
	// Save after a pointer drag, or after each keyboard edit.
	switch {
	case slider.Pressed():
		a.nowPlaying.volumeHeld = true
	case a.nowPlaying.volumeHeld:
		a.nowPlaying.volumeHeld = false
		a.saveSettings()
	case slider.Changed():
		a.saveSettings()
	}
	return slider
}

// volumeControl is the speaker button, which mutes, and the slider beside it.
func (a *app) volumeControl(c *ui.Context, sliderWidth float32) {
	a.nowPlaying.volumePopup = false
	ui.Row(c).Gap(8).AlignItems(ui.Center).Shrink(0).Children(func() {
		a.waveformButton(c)
		a.hinted(c, func() {
			a.muteButton(c)
		}, keymap.Mute)
		if sliderWidth <= 0 {
			return
		}
		a.hinted(c, func() {
			ui.Row(c).Width(sliderWidth).Children(func() {
				a.volumeSlider(c, false)
			})
		}, keymap.VolumeUp, keymap.VolumeDown)
	})
}

// compactVolumeControl keeps the mute button in the narrow player bar and
// reveals an upright volume slider while the button or its panel is hovered.
func (a *app) compactVolumeControl(c *ui.Context) {
	sc := m3.Of(c).Scheme
	ui.Row(c).Shrink(0).Children(func() {
		a.waveformButton(c)
		var anchor ui.Element
		a.hinted(c, func() { anchor = a.muteButton(c) }, keymap.Mute)

		open := anchor.Hovered() || anchor.FocusVisible() || a.nowPlaying.volumePopup
		var slider ui.Element
		panel := ui.PopoverBase(c, anchor, &open, func(panel ui.Element) {
			panel.Key("volume-popup").Width(64).Padding(8).Radius(m3.Large).Margin(0).Background(sc.SurfaceContainerHigh)
			m3.Elevation(c, panel, 3)
			a.hinted(c, func() {
				ui.Column(c).Width(48).Height(128).Center().Children(func() {
					slider = a.volumeSlider(c, true)
				})
			}, keymap.VolumeUp, keymap.VolumeDown)
		})
		keepOpen := anchor.Hovered() || anchor.FocusVisible() || panel.Hovered() || slider.FocusVisible()
		if next := open && keepOpen; next != a.nowPlaying.volumePopup {
			a.nowPlaying.volumePopup = next
			c.Invalidate()
		}
	})
}

// speedLabel names a playback speed as "1.5×".
func speedLabel(speed float64) string {
	return strconv.FormatFloat(speed, 'f', -1, 64) + "×"
}

// pitchLabel names a pitch shift as "+1.5 st".
func pitchLabel(semitones float64) string {
	text := strconv.FormatFloat(semitones, 'f', -1, 64)
	if semitones > 0 {
		text = "+" + text
	}
	return text + " st"
}

// speedControl is the button that shows how fast the current track plays and
// opens the sliders for its speed and pitch; key tells the player's buttons
// apart.
func (a *app) speedControl(c *ui.Context, key string) {
	sc := m3.Of(c).Scheme
	speed := a.speedFor(a.current)
	pitch := a.pitchFor(a.current)
	anchor := m3.Button(c, m3.ButtonSpec{Label: speedLabel(speed), Kind: m3.TextOnly, Size: m3.Small40, Key: key})
	if pitch != 0 {
		anchor.Tooltip("Pitch " + pitchLabel(pitch))
	}
	// A panel ignores the press that lands on the button that opened it, so
	// the button closes it, as the account menu's button does.
	wasOpen := a.nowPlaying.speedMenu == key
	if anchor.Clicked() {
		a.playbackPanel.open = false
		if wasOpen {
			a.nowPlaying.speedMenu = ""
		} else {
			a.nowPlaying.speedMenu = key
		}
	}
	if a.playbackPanel.open || a.nowPlaying.speedMenu != key {
		return
	}
	open := true
	presetW, textSize := pitchPresetMetrics(c)
	const (
		presetGap float32 = 2
		padX      float32 = 8
	)
	// Width includes the padding. The row is a 0 button beside twelve
	// semitone buttons, and 336 is as wide as a popup can be in the tray's
	// mini player and still sit 4 DIPs inside that window.
	inner := 13*presetW + 12*presetGap
	panelW := max(float32(280), inner+padX*2)
	ui.PopoverBase(c, anchor, &open, func(panel ui.Element) {
		panel.Key("playback-"+key).Width(panelW).Padding(12, padX).Radius(m3.Large).Margin(8, 0, 0, 0).
			Background(sc.SurfaceContainerHigh)
		m3.Elevation(c, panel, 3)
		ui.Column(c).Gap(8).Children(func() {
			ui.Row(c).AlignItems(ui.Center).Children(func() {
				m3.Text(c, m3.LabelMedium, "Playback").TextColor(sc.OnSurfaceVariant)
				ui.Box(c).Grow(1)
				if m3.Button(c, m3.ButtonSpec{Label: "Detach", Kind: m3.TextOnly, Size: m3.ExtraSmall32, Key: key + "-detach"}).Clicked() {
					a.playbackPanel.open = true
					a.nowPlaying.speedMenu = ""
				}
			})
			ui.Scroll(c).MaxHeight(520).Grow(1).Children(func() {
				a.playbackColumn(c, key, presetW, textSize)
			})
		})
	})
	if !open {
		a.nowPlaying.speedMenu = ""
	}
}

// semitoneLabel names an integer pitch the way the preset buttons do,
// "+5", "0" or "−5". The minus is the typographic minus, not a hyphen.
func semitoneLabel(semitones int) string {
	switch {
	case semitones > 0:
		return "+" + strconv.Itoa(semitones)
	case semitones < 0:
		return "−" + strconv.Itoa(-semitones)
	default:
		return "0"
	}
}

// pitchPresetMetrics is the width and type size of one semitone button.
// The width fits the widest label, "+12" or "−12", with a little air around
// the glyphs. The size steps down only when that row, plus the 0 button,
// would no longer fit in the tray's mini player.
func pitchPresetMetrics(c *ui.Context) (width, size float32) {
	const (
		gap      float32 = 2
		padX     float32 = 8
		panelMax float32 = 336
	)
	for _, size := range []float32{m3.LabelSmall.Size, 10, 9} {
		widest := max(measurePreset(c, "+12", size), measurePreset(c, "−12", size)) + 4
		inner := 13*widest + 12*gap
		if inner+padX*2 <= panelMax || size == 9 {
			return widest, size
		}
	}
	return 0, 9
}

func measurePreset(c *ui.Context, text string, size float32) float32 {
	w, _ := c.MeasureText(0, ui.Span{
		Text: text, Size: size, Weight: m3.LabelSmall.Emphasis, Features: "tnum",
	})
	return w
}

// pitchPresets is the 0 button beside two rows of keys, +1 through +12 and
// −1 through −12. The one that matches the current pitch is lit. Choosing
// one sets the pitch and leaves the speed alone.
func (a *app) pitchPresets(c *ui.Context, key string, pitch float64, buttonW, textSize float32) {
	ui.Row(c.Key(key + "-presets")).Gap(2).AlignItems(ui.Stretch).Label("Semitone presets").Children(func() {
		a.pitchPreset(c, key, 0, pitch, buttonW, textSize, 62, false)
		ui.Column(c).Grow(1).Gap(6).Children(func() {
			for _, direction := range []int{1, -1} {
				row := "up"
				if direction < 0 {
					row = "down"
				}
				ui.Row(c.Key(key + "-presets-" + row)).Gap(2).Children(func() {
					for amount := 1; amount <= 12; amount++ {
						a.pitchPreset(c, key, direction*amount, pitch, buttonW, textSize, 28, true)
					}
				})
			}
		})
	})
}

func (a *app) pitchPreset(c *ui.Context, key string, semitones int, pitch float64, buttonW, textSize, height float32, grow bool) {
	sc := m3.Of(c).Scheme
	label := semitoneLabel(semitones)
	name := "Set pitch to " + label + " semitones"
	delta := pitch - float64(semitones)
	if delta < 0 {
		delta = -delta
	}
	chosen := delta < 0.001
	bg, fg := sc.SurfaceContainerHighest, sc.OnSurface
	if chosen {
		bg, fg = sc.Secondary, sc.OnSecondary
	}
	b := ui.ButtonBase(c.Key(key + "-st-" + strconv.Itoa(semitones))).Shrink(0).MinWidth(buttonW).Height(height).
		Radius(m3.ExtraSmall).Cursor(ui.CursorPointer).Label(name).Tooltip(name).
		Justify(ui.Center).AlignItems(ui.Center)
	if grow {
		b.Grow(1)
	}
	b.Background(m3.StateFill(bg, fg, b.Hovered(), b.Pressed(), b.FocusVisible())).TextColor(fg)
	b.Children(func() {
		m3.LabelSmall.Style(ui.Text(c, label), true).Grow(1).TextAlign(ui.Center).
			TextColor(fg).NoWrap().FontFeatures("tnum").LetterSpacing(0).FontSize(textSize)
	})
	if b.Clicked() {
		a.setPitch(float64(semitones))
	}
}

// measureJump is one button under the semitone presets.
type measureJump struct {
	n    float64
	text string
	name string
}

// The two rows of measure jumps: forward, then back by the same amounts.
var measureJumpRows = [][]measureJump{
	{
		{1, "+1", "Jump forward 1 measure"},
		{0.5, "+1/2", "Jump forward 1/2 measure"},
		{0.25, "+1/4", "Jump forward 1/4 measure"},
		{0.125, "+1/8", "Jump forward 1/8 measure"},
		{2, "+2", "Jump forward 2 measures"},
		{4, "+4", "Jump forward 4 measures"},
		{8, "+8", "Jump forward 8 measures"},
	},
	{
		{-1, "−1", "Jump back 1 measure"},
		{-0.5, "−1/2", "Jump back 1/2 measure"},
		{-0.25, "−1/4", "Jump back 1/4 measure"},
		{-0.125, "−1/8", "Jump back 1/8 measure"},
		{-2, "−2", "Jump back 2 measures"},
		{-4, "−4", "Jump back 4 measures"},
		{-8, "−8", "Jump back 8 measures"},
	},
}

// measureJumps adds or subtracts measures of the current tempo. The switch
// beside the rows lands on the measure grid instead.
func (a *app) measureJumps(c *ui.Context, key string) {
	_, ready := a.currentGrid()
	ui.Row(c.Key(key + "-measures")).Gap(8).AlignItems(ui.Center).Children(func() {
		ui.Column(c).Grow(1).Gap(4).Children(func() {
			for row, jumps := range measureJumpRows {
				ui.Row(c.Key(key + "-measures-" + strconv.Itoa(row))).Gap(2).Children(func() {
					for _, jump := range jumps {
						a.measureJumpButton(c, key, jump, ready)
					}
				})
			}
		})
		on := a.settings.MeasureSnap
		sw := m3.CompactSwitch(c, &on, "Snap to measures")
		sw.Tooltip("Land on the measure grid instead of jumping from here")
		if sw.Changed() {
			a.settings.MeasureSnap = on
			a.saveSettings()
		}
	})
}

func (a *app) measureJumpButton(c *ui.Context, key string, jump measureJump, ready bool) {
	sc := m3.Of(c).Scheme
	b := ui.ButtonBase(c.Key(key + "-meas-" + jump.text)).Height(28).Grow(1).MinWidth(0).
		Radius(m3.ExtraSmall).Cursor(ui.CursorPointer).Label(jump.name).Tooltip(jump.name).
		Justify(ui.Center).AlignItems(ui.Center)
	fg := sc.OnSurface
	if !ready {
		b.Opacity(0.38)
		fg = sc.OnSurface.Alpha(0.38)
	}
	b.Background(m3.StateFill(sc.SurfaceContainerHighest, fg, b.Hovered(), b.Pressed(), b.FocusVisible())).TextColor(fg)
	b.Children(func() {
		m3.LabelSmall.Style(ui.Text(c, jump.text), true).TextAlign(ui.Center).NoWrap().
			TextColor(fg).FontFeatures("tnum").LetterSpacing(0).FontSize(10)
	})
	if b.Clicked() && ready {
		a.jumpMeasures(jump.n)
	}
}

// targetBPM is the tempo a new track is sped up or slowed down to, and the
// switch that turns that off. The number is kept when Enter is pressed.
func (a *app) targetBPM(c *ui.Context, key string) {
	sc := m3.Of(c).Scheme
	ui.Row(c.Key(key + "-target")).Gap(8).AlignItems(ui.Center).Children(func() {
		m3.Text(c, m3.BodyMedium, "Target").Shrink(0).TextColor(sc.OnSurface)
		input := ui.TextInputBase(c.Key(key+"-target-bpm"), &a.targetBPMText).
			Width(72).Height(40).Shrink(0).Padding(8).Radius(m3.Small).
			Background(sc.SurfaceContainerHighest).TextColor(sc.OnSurface).FontSize(14).
			Label("Target BPM").Placeholder("BPM")
		m3.Text(c, m3.LabelMedium, "BPM").Shrink(0).TextColor(sc.OnSurfaceVariant)
		ui.Box(c).Grow(1)
		on := a.settings.TargetBPMOn
		sw := m3.CompactSwitch(c, &on, "Target BPM on")
		sw.Tooltip("Set the speed of each new track so its tempo matches. Moving the speed slider keeps the speed you chose for this track.")
		if input.Submitted() {
			a.commitTargetBPM()
		}
		if sw.Changed() {
			a.settings.TargetBPMOn = on
			a.saveSettings()
			if on {
				a.commitTargetBPM()
			}
		}
	})
}

// rateSlider is one labeled slider of the playback panel. A change applies
// as the knob moves, so the sound follows the pointer. fine lets a Ctrl-drag
// of the speed slider move by the factor in settings.
func (a *app) rateSlider(c *ui.Context, title, label, key string, lo, hi, step float64, value float64, format func(float64) string, commit func(float64), fine bool) {
	sc := m3.Of(c).Scheme
	ui.Row(c).AlignItems(ui.Center).Children(func() {
		m3.Text(c, m3.BodyMedium, title).TextColor(sc.OnSurface)
		ui.Box(c).Grow(1)
		m3.Text(c, m3.LabelMedium, format(value)).TextColor(sc.OnSurfaceVariant)
	})
	before := value
	spec := m3.SliderSpec{Label: label, Key: key, Step: step}
	if fine {
		ctrl := c.Modifiers()&ui.Ctrl != 0
		spec.Adjust = func(absolute float64, pressed bool) float64 {
			return a.scaleSpeedDrag(key, before, absolute, pressed, ctrl)
		}
	}
	slider := m3.Slider(c, &value, lo, hi, spec)
	if fine {
		slider.Tooltip("Hold Ctrl and drag to move more slowly")
	}
	// Past the ends the track's own value stays put, so a drag there still
	// has to be stored when Adjust moved it.
	if value != before || slider.Changed() {
		commit(value)
	}
}

// trackLine is the name of the current track over who made it, or the
// reason it does not play.
func (a *app) trackLine(c *ui.Context, title, subtitle m3.Role, links bool) ui.Element {
	sc := m3.Of(c).Scheme
	return ui.Column(c).Gap(1).MinWidth(0).Children(func() {
		m3.EmphasizedText(c, title, a.current.Title).SingleLine().TextColor(sc.OnSurface)
		line, colour := a.current.Subtitle, sc.OnSurfaceVariant
		if a.playErr != "" {
			line, colour = firstLine(a.playErr), sc.Error
		}
		if links && a.playErr == "" {
			parts := a.current.PlayerByline()
			subtitle.Style(ui.RichText(c.Key(a.current.VideoID)), false).MaxLines(2).TextColor(colour).Children(func() {
				for i, part := range parts {
					if !part.Target().Opens() {
						ui.Text(c.Key(i), part.Text)
						continue
					}
					link := ui.Link(c.Key(i), part.Text, "").TextColor(colour).Disabled(a.nowPlaying.closing)
					label := "Go to artist: "
					if part.Target().Kind == youtube.TargetAlbum {
						label = "Go to album: "
					}
					link.Label(label + part.Text).Tooltip(label + part.Text)
					if link.Clicked() {
						a.openPlayerLink(part)
					}
				}
			})
			return
		}
		m3.Text(c, subtitle, line).SingleLine().TextColor(colour)
	})
}

// openPlayerLink keeps audio and the queue untouched. The full player finishes
// its closing animation before navigating; the bar opens the page directly.
func (a *app) openPlayerLink(part youtube.BylinePart) {
	if a.nowPlaying.open {
		a.nowPlaying.navigateTo = &part
		a.nowPlaying.closing = true
		return
	}
	a.openTarget(part.Target(), detail{title: part.Text})
}

// playerBar is the floating player: what plays on the left, the transport and
// the scrubber in the middle, volume and the full-screen player on the right.
func (a *app) playerBar(c *ui.Context) {
	sc := m3.Of(c).Scheme
	w, _ := c.Size()
	wide := w > 1180
	roomy := w > 800
	bar := ui.Row(c.Key("player")).Absolute().Left(playerGutter).Right(playerGutter).Bottom(playerGutter).
		Height(playerHeight).Padding(10, 20, 10, 12).Gap(16).AlignItems(ui.Center).
		Radius(m3.ExtraLarge).Background(sc.SurfaceContainerHigh)
	m3.Elevation(c, bar, 3)
	slide := m3.SpatialDefault.Motion()
	slide.Enter = &ui.Motion{Y: 48}
	slide.Position = true
	bar.Transition(slide)
	bar.Children(func() {
		info := ui.ButtonBase(c.Key("player-info"))
		info.Grow(1).Basis(0).MinWidth(0).Gap(14).AlignItems(ui.Center).Radius(m3.Large).Padding(0, 8, 0, 0).
			Cursor(ui.CursorPointer).Label("Open the player").
			Background(m3.StateFill(ui.Transparent, sc.OnSurface, info.Hovered(), info.Pressed(), info.FocusVisible()))
		info.Children(func() {
			a.art(c, a.current.Thumbnail, playerArt, 64, m3.Large, func() {
				if a.current.IsVideo() {
					m3.VideoBadge(c, 64)
				}
			})
			a.trackLine(c, m3.TitleSmall, m3.BodySmall, true).Grow(1)
		})
		if info.Clicked() {
			a.nowPlaying.open, a.nowPlaying.closing = true, false
			a.loadPanel()
		}

		// In a narrow window the track keeps its share: the transport takes
		// what it needs and the buttons on the right take no more than they
		// are wide.
		transportGrow := float32(2)
		if !roomy {
			transportGrow = 1
		}
		ui.Column(c).Grow(transportGrow).Basis(0).MinWidth(220).MaxWidth(580).Gap(0).Children(func() {
			a.transport(c, 48, 4)
			a.scrubber(c)
		})

		// The buttons on the right never overflow the bar: when their share is
		// too narrow for them, the track's share gives way.
		end := ui.Row(c).Justify(ui.End).AlignItems(ui.Center).Gap(8)
		if roomy {
			end.Grow(1).Basis(0)
		} else {
			end.Shrink(0)
		}
		end.Children(func() {
			if wide {
				a.volumeControl(c, 108)
			} else {
				a.compactVolumeControl(c)
			}
			a.speedControl(c, "bar-speed")
			if m3.IconButton(c, m3.IconButtonSpec{Icon: m3.IconQueue, Label: "Up next", Key: "bar-queue"}).Clicked() {
				a.openPlayer(tabQueue, true)
			}
			if m3.IconButton(c, m3.IconButtonSpec{Icon: m3.IconExpandLess, Label: "Open the player", Key: "bar-expand"}).Clicked() {
				a.openPlayer(a.nowPlaying.tab, false)
			}
		})
	})
}

// historyPopupLimit is how many recent plays the player button lists.
const historyPopupLimit = 40

// playbackHistory opens the recent plays beside the button that asked for them.
func (a *app) playbackHistory(c *ui.Context, anchor ui.Element) {
	sc := m3.Of(c).Scheme
	wasOpen := a.plays.open
	if anchor.Clicked() {
		if wasOpen {
			a.plays.open = false
			a.plays.menu.close()
		} else {
			a.plays.open = true
			a.loadHistory()
		}
	}
	if !a.plays.open {
		return
	}
	open := true
	ui.PopoverBase(c, anchor, &open, func(panel ui.Element) {
		panel.Key("playback-history").Width(280).Padding(8).Radius(m3.Large).Margin(8, 0, 0, 0).
			Background(sc.SurfaceContainerHigh)
		m3.Elevation(c, panel, 3)
		plays := a.recentPlays()
		if len(plays) == 0 {
			m3.Text(c, m3.BodyMedium, "Nothing played yet").TextColor(sc.OnSurfaceVariant).Padding(8)
			return
		}
		ui.Scroll(c).Key("playback-history-list").MaxHeight(360).Children(func() {
			for i, entry := range plays {
				a.historyPlayRow(c, i, entry)
			}
		})
	})
	if !open {
		a.plays.open = false
		a.plays.menu.close()
	}
}

// recentPlays is the newest plays, up to historyPopupLimit.
func (a *app) recentPlays() []history.Entry {
	n := len(a.history)
	if n > historyPopupLimit {
		n = historyPopupLimit
	}
	out := make([]history.Entry, n)
	for i := range out {
		out[i] = a.history[len(a.history)-1-i]
	}
	return out
}

func (a *app) historyPlayRow(c *ui.Context, i int, entry history.Entry) {
	sc := m3.Of(c).Scheme
	title := entry.Title
	if title == "" {
		title = "Unknown track"
	}
	key := "played-" + strconv.Itoa(i) + "-" + entry.VideoID
	row := ui.ButtonBase(c.Key(key)).Height(48).PaddingX(8).Radius(m3.Medium).
		Cursor(ui.CursorPointer).Label(title).AlignItems(ui.Center)
	row.Background(m3.StateFill(ui.Transparent, sc.OnSurface, row.Hovered(), row.Pressed(), row.FocusVisible()))
	var anchor ui.Element
	var has bool
	row.Children(func() {
		ui.Column(c).Grow(1).MinWidth(0).Children(func() {
			m3.Text(c, m3.BodyMedium, title).SingleLine().TextColor(sc.OnSurface)
			if entry.Artist != "" {
				m3.Text(c, m3.BodySmall, entry.Artist).SingleLine().TextColor(sc.OnSurfaceVariant)
			}
		})
		anchor, has = a.plays.menu.pointerAnchor(c, key)
	})
	pointer := row.RightClicked()
	if pointer || row.Shortcut(ui.Shift, ui.KeyF10) {
		x, y, _ := row.PointerPosition()
		a.plays.menu.show(key, pointer, x, y)
	}
	if a.plays.menu.owns(key) {
		a.historyPlayMenu(c, a.plays.menu.anchor(row, anchor, has), key, entry)
	}
}

// historyPlayMenu queues the play from the video id the history recorded.
func (a *app) historyPlayMenu(c *ui.Context, anchor ui.Element, key string, entry history.Entry) {
	if !a.plays.menu.owns(key) {
		return
	}
	item := historyTrack(entry)
	m3.Menu(c, anchor, &a.plays.menu.open, 200, func() {
		if m3.MenuItem(c, "Add Next", m3.IconNext).Clicked() {
			a.enqueue(item, true)
			a.plays.menu.close()
		}
		if m3.MenuItem(c, "Add to Queue", m3.IconQueue).Clicked() {
			a.enqueue(item, false)
			a.plays.menu.close()
		}
	})
}

// historyTrack is the track a history entry can be queued as. VideoID is
// the string recorded for that play.
func historyTrack(entry history.Entry) youtube.MusicItem {
	title := entry.Title
	if title == "" {
		title = "Unknown track"
	}
	return youtube.MusicItem{
		VideoID:  entry.VideoID,
		Title:    title,
		Subtitle: entry.Artist,
		Kind:     youtube.KindTrack,
	}
}
