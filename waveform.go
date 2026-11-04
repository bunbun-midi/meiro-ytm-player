package main

import (
	"fmt"
	"time"

	"github.com/egoist/mygo/ui"

	"github.com/elianiva/meiro/internal/bpm"
	"github.com/elianiva/meiro/m3"
	"github.com/elianiva/meiro/player"
)

// waveformPanel is the wide seek bar opened from the button beside Mute.
// Its title drags it. The wheel zooms the waveform under the pointer, and a
// click seeks to the time under the pointer.
func (a *app) waveformPanel(c *ui.Context) {
	if !a.wave.open {
		return
	}
	sc := m3.Of(c).Scheme
	windowW, windowH := c.Size()
	width := windowW - 32
	if width > 920 {
		width = 920
	}
	if width < 320 {
		width = windowW - 16
	}
	const head float32 = 176
	if !a.wave.placed {
		a.wave.x = (windowW - width) / 2
		a.wave.y = windowH - playerHeight - head - 16
		if a.wave.y < 16 {
			a.wave.y = 16
		}
	}
	panel := ui.Column(c.Key("waveform-panel")).Absolute().Width(width).Padding(12, 8).Radius(m3.Large).
		Background(sc.SurfaceContainerHigh).Left(a.wave.x).Top(a.wave.y)
	m3.Elevation(c, panel, 3)
	total := a.waveExtent()
	viewW := width - 24
	if viewW < 1 {
		viewW = 1
	}
	panel.Children(func() {
		bar := ui.Row(c).Height(32).AlignItems(ui.Center).Cursor(ui.CursorMove).Label("Move waveform")
		dx, dy, held := bar.Dragged()
		bar.Children(func() {
			m3.Text(c, m3.LabelMedium, "Waveform").TextColor(sc.OnSurfaceVariant).PassThrough()
			ui.Box(c).Grow(1).PassThrough()
			if m3.IconButton(c, m3.IconButtonSpec{Icon: m3.IconClose, Label: "Close waveform", Size: m3.ExtraSmall32, Key: "waveform-close"}).Clicked() {
				a.wave.open = false
			}
		})
		if held && (dx != 0 || dy != 0) && a.wave.open {
			if !a.wave.placed {
				b := panel.Bounds()
				if b.W > 0 {
					a.wave.x, a.wave.y = b.X, b.Y
				}
				a.wave.placed = true
			}
			a.wave.x += dx
			a.wave.y += dy
			a.wave.x = min(max(a.wave.x, 0), max(windowW-48, 0))
			a.wave.y = min(max(a.wave.y, 0), max(windowH-48, 0))
			a.wave.dragging = true
		} else if a.wave.dragging {
			a.wave.dragging = false
			a.settings.WaveformPlaced = a.wave.placed
			a.settings.WaveformX = float64(a.wave.x)
			a.settings.WaveformY = float64(a.wave.y)
			a.saveSettings()
		}
		wave := a.player.Waveform()
		view := ui.Box(c.Key("waveform-view")).Height(96).Width(viewW).Label("Waveform view").Radius(8).
			Background(sc.SurfaceContainerHighest)
		view.HandleInput(func(ev ui.InputEvent) bool {
			switch ev.Kind {
			case ui.InputPointerMove, ui.InputPointerDown:
				a.wave.hover = true
				a.wave.hoverAt = a.waveTimeAt(ev.X, viewW, total)
				return ev.Kind == ui.InputPointerDown
			case ui.InputPointerUp:
				at := a.waveTimeAt(ev.X, viewW, total)
				if total > 0 && at > total {
					at = total
				}
				if at < 0 {
					at = 0
				}
				a.seek(at)
				return true
			case ui.InputScroll:
				if ev.Precise {
					a.waveZoom(ev.X, viewW, total, ev.DX, true)
				} else {
					a.waveZoom(ev.X, viewW, total, ev.DY, false)
				}
				return true
			case ui.InputZoom:
				dy := float32(40)
				if ev.Scale < 1 {
					dy = -40
				}
				a.waveZoom(ev.X, viewW, total, dy, false)
				return true
			default:
				return false
			}
		})
		view.Draw(func(p *ui.Painter, r ui.Rect) {
			a.paintWaveform(p, r, sc, wave, total)
		})
		if view.Hovered() {
			x, _, _ := view.PointerPosition()
			a.wave.hover = true
			a.wave.hoverAt = a.waveTimeAt(x, viewW, total)
		} else {
			a.wave.hover = false
		}
		readout := a.waveReadout(total)
		m3.Text(c, m3.LabelMedium, readout).NoWrap().TextColor(sc.OnSurface).FontFeatures("tnum")
	})
}

// waveExtent is the length the waveform is zoomed against: the track, or
// as far as the decode has reached when the length is not known yet.
func (a *app) waveExtent() time.Duration {
	if a.total > 0 {
		return a.total
	}
	if a.player != nil {
		if end := a.player.Waveform().End; end > 0 {
			return end
		}
	}
	return 30 * time.Second
}

// waveWindow is the time at the left of the waveform and how much it shows.
func (a *app) waveWindow(total time.Duration) (origin, span time.Duration) {
	if total <= 0 {
		total = 30 * time.Second
	}
	span = a.wave.span
	if span <= 0 || span > total {
		span = total
	}
	const minSpan = 100 * time.Millisecond
	if span < minSpan {
		span = minSpan
	}
	origin = a.wave.origin
	if origin < 0 {
		origin = 0
	}
	if maxOrigin := total - span; origin > maxOrigin {
		origin = max(maxOrigin, 0)
	}
	return origin, span
}

// waveTimeAt is the track time under a pointer x on a view of viewW.
func (a *app) waveTimeAt(x, viewW float32, total time.Duration) time.Duration {
	origin, span := a.waveWindow(total)
	if viewW <= 0 {
		return origin
	}
	frac := float64(x / viewW)
	if frac < 0 {
		frac = 0
	}
	if frac > 1 {
		frac = 1
	}
	return origin + time.Duration(frac*float64(span))
}

// waveZoom zooms the waveform around x. A negative dy, a wheel notch up,
// shows less of the track. A precise touchpad pans instead.
func (a *app) waveZoom(x, viewW float32, total time.Duration, dy float32, pan bool) {
	origin, span := a.waveWindow(total)
	frac := 0.5
	if viewW > 0 {
		frac = float64(x / viewW)
		if frac < 0 {
			frac = 0
		}
		if frac > 1 {
			frac = 1
		}
	}
	if pan {
		if viewW > 0 {
			origin += time.Duration(float64(dy) / float64(viewW) * float64(span))
		}
	} else if dy != 0 {
		at := origin + time.Duration(frac*float64(span))
		next := span
		if dy < 0 {
			next = time.Duration(float64(span) / 1.25)
		} else {
			next = time.Duration(float64(span) * 1.25)
		}
		const minSpan = 100 * time.Millisecond
		if next < minSpan {
			next = minSpan
		}
		if total > 0 && next > total {
			next = total
		}
		origin = at - time.Duration(frac*float64(next))
		span = next
	}
	if origin < 0 {
		origin = 0
	}
	if total > 0 && origin > total-span {
		origin = max(total-span, 0)
	}
	a.wave.origin = origin
	a.wave.span = span
}

func (a *app) waveReadout(total time.Duration) string {
	at := a.playbackPosition()
	hover := a.wave.hover
	if hover {
		at = a.wave.hoverAt
	}
	if grid, ok := a.currentGrid(); ok {
		bar, beat := barAndBeat(grid, at)
		if !hover {
			return fmt.Sprintf("%d.%d", bar, beat)
		}
		return formatStep(beatIndex(grid, a.playbackPosition()), beatIndex(grid, at)) + " · " + fmt.Sprintf("%d.%d", bar, beat)
	}
	if !hover {
		return clock(at)
	}
	return signedClock(at-a.playbackPosition()) + " · " + clock(at)
}

// barAndBeat is the 1-indexed bar and beat of at, the first two numbers of
// a bars.beats display. Four beats make a bar.
func barAndBeat(g bpm.Grid, at time.Duration) (bar, beat int) {
	n := beatIndex(g, at)
	return n/4 + 1, n%4 + 1
}

// beatIndex counts beats from the grid's phase. Time before the phase is 0.
func beatIndex(g bpm.Grid, at time.Duration) int {
	if g.Beat <= 0 {
		return 0
	}
	rel := at - g.Phase
	if rel < 0 {
		return 0
	}
	return int(rel / g.Beat)
}

// formatStep prints a move of whole beats as bars.beats, with a sign.
// Four beats carry into the next bar, so one bar is +1.0.
func formatStep(from, to int) string {
	d := to - from
	sign := "+"
	if d < 0 {
		sign = "-"
		d = -d
	}
	return fmt.Sprintf("%s%d.%d", sign, d/4, d%4)
}

func signedClock(d time.Duration) string {
	if d < 0 {
		return "-" + clock(-d)
	}
	return "+" + clock(d)
}

func (a *app) paintWaveform(p *ui.Painter, r ui.Rect, sc m3.Scheme, wave player.Waveform, total time.Duration) {
	if r.W <= 0 || r.H <= 0 {
		return
	}
	origin, span := a.waveWindow(total)
	mid := r.Y + r.H/2
	p.Line(r.X, mid, r.X+r.W, mid, 1, sc.OutlineVariant)
	cols := int(r.W)
	if cols < 1 {
		cols = 1
	}
	for i := range cols {
		t0 := origin + time.Duration(float64(span)*float64(i)/float64(cols))
		t1 := origin + time.Duration(float64(span)*float64(i+1)/float64(cols))
		pk, ok := waveColumn(wave, t0, t1)
		if !ok {
			continue
		}
		amp := peakAmp(pk, r.H)
		x := r.X + float32(i)
		p.Fill(ui.Rect{X: x, Y: mid - amp, W: 1, H: max(amp*2, 1)}, sc.Primary, 0)
	}
	if a.wave.hover && span > 0 {
		frac := float32(float64(a.wave.hoverAt-origin) / float64(span))
		if frac >= 0 && frac <= 1 {
			x := r.X + r.W*frac
			p.Line(x, r.Y, x, r.Y+r.H, 1, sc.Secondary)
		}
	}
	pos := a.playbackPosition()
	if span > 0 && pos >= origin && pos <= origin+span {
		x := r.X + r.W*float32(float64(pos-origin)/float64(span))
		p.Line(x, r.Y, x, r.Y+r.H, 2, sc.OnSurface)
	}
}

func waveColumn(wave player.Waveform, t0, t1 time.Duration) (player.WavePeak, bool) {
	if len(wave.Peaks) == 0 || wave.End <= wave.Start || t1 <= wave.Start || t0 >= wave.End {
		return player.WavePeak{}, false
	}
	span := float64(wave.End - wave.Start)
	i0 := int(float64(t0-wave.Start) / span * float64(len(wave.Peaks)))
	i1 := int(float64(t1-wave.Start) / span * float64(len(wave.Peaks)))
	if i0 < 0 {
		i0 = 0
	}
	if i1 >= len(wave.Peaks) {
		i1 = len(wave.Peaks) - 1
	}
	if i0 >= len(wave.Peaks) {
		return player.WavePeak{}, false
	}
	if i1 < i0 {
		i1 = i0
	}
	pk := wave.Peaks[i0]
	for _, p := range wave.Peaks[i0 : i1+1] {
		if p.Min < pk.Min {
			pk.Min = p.Min
		}
		if p.Max > pk.Max {
			pk.Max = p.Max
		}
	}
	return pk, true
}

func peakAmp(pk player.WavePeak, h float32) float32 {
	v := pk.Max
	if -pk.Min > v {
		v = -pk.Min
	}
	if v < 0 {
		v = 0
	}
	amp := float32(v) / 32768 * (h/2 - 2)
	if amp < 1 {
		amp = 1
	}
	return amp
}
