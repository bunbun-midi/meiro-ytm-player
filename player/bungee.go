// Copyright (C) 2020-2026 Parabola Research Limited
// SPDX-License-Identifier: MPL-2.0
//
// bungee is a translation of Bungee Basic's forward, fixed-pitch time
// stretcher. Pitch stays with the resampler that follows this stretch.
// The analysis window is 4096 samples, the synthesis window is 2048, and
// each grain emits a 512-sample hop. Phase follows spectral peaks, a
// transient merges partials when a peak jumps by more than half again,
// and both channels take one phase from their complex sum.

package player

import (
	"io"
	"math"
)

const (
	bungeeN      = 4096
	bungeeHop    = 512
	bungeeSynthN = 2048
	bungeeLap    = 4096
	bungeeBins   = bungeeN/2 + 1
	bungeeLogHop = 9
)

var (
	bungeeAnalysisWindow  = bungeeWindow(bungeeN, (3*math.Pi/(3*math.Pi+8))/bungeeN)
	bungeeSynthesisWindow = bungeeWindow(bungeeSynthN, 0.25)
)

// bungeeWindow is the inverse transform of a two-bin spectrum, which is the
// Hann shape Bungee uses. The forward transform it later meets is unscaled,
// and the inverse it meets is scaled by the length, so the gain already
// includes that.
func bungeeWindow(n int, gain float64) []float64 {
	w := make([]float64, n)
	for i := range w {
		w[i] = gain * (1 + math.Cos(2*math.Pi*float64(i)/float64(n)))
	}
	return w
}

type bungeePartial struct {
	peak int
	end  int
}

// bungee holds one stretcher's grain history. reset forgets it so the next
// grain starts clean.
type bungee struct {
	primed     bool
	pos        float64
	prevPos    float64
	posError   float64
	speed      float64
	hop        int
	prevPhase  []int16
	prevRot    []int16
	prevEnergy []float32

	phase    []int16
	rotation []int16
	energy   []float32
	partials []bungeePartial
	lap      [bungeeLap][2]float64
	spec     [2][]complex128
	time     []complex128
}

func (b *bungee) reset() {
	*b = bungee{}
}

func (b *bungee) ensure() {
	if b.phase != nil {
		return
	}
	b.phase = make([]int16, bungeeBins+2)
	b.rotation = make([]int16, bungeeBins+2)
	b.energy = make([]float32, bungeeBins+2)
	b.prevPhase = make([]int16, bungeeBins+2)
	b.prevRot = make([]int16, bungeeBins+2)
	b.prevEnergy = make([]float32, bungeeBins+2)
	b.partials = make([]bungeePartial, 0, bungeeBins)
	b.spec[0] = make([]complex128, bungeeN)
	b.spec[1] = make([]complex128, bungeeN)
	b.time = make([]complex128, bungeeN)
}

// placeBungee overlap-adds one Bungee grain into the output buffer.
func (r *rateReader) placeBungee() error {
	r.b.ensure()
	center := r.b.pos
	end := center + float64(bungeeN/2)
	if err := r.fill(int64(math.Ceil(end)) + 2); err != nil {
		r.fail = err
		r.draining = true
		return nil
	}
	available := r.origin + int64(len(r.frames))
	if r.eof && center >= float64(available) {
		r.draining = true
		return nil
	}
	if !r.eof && float64(available) < end {
		return io.ErrNoProgress
	}
	r.inPos = center
	continuous := r.b.specify(r.stretch)
	r.b.analyseGrain(func(at int64) (float64, float64) {
		if at < r.origin || at >= available {
			return 0, 0
		}
		s := r.frames[int(at-r.origin)]
		return s[0], s[1]
	}, continuous)
	hop := r.b.emit()
	r.writeHop(hop)
	r.b.pos += float64(bungeeHop) * r.stretch
	r.inPos = r.b.pos
	r.grainAt += bungeeHop
	r.dropInput(bungeeN/2 + 2)
	return nil
}

func (r *rateReader) writeHop(hop [][2]float64) {
	r.ensureOLA(r.grainAt, len(hop))
	base := int(r.grainAt - r.olaOrigin)
	copy(r.ola[base:], hop)
	if end := r.grainAt + int64(len(hop)); end > r.olaEnd {
		r.olaEnd = end
	}
}

// specify chooses this grain's integer input hop from the playback stretch,
// carrying the fractional sample into the next grain. The returned flag is
// false for the first grain, which has no phase history to continue.
func (b *bungee) specify(stretch float64) bool {
	b.ensure()
	continuous := b.primed
	b.speed = stretch
	hopIdeal := stretch * float64(bungeeHop)
	if !continuous || math.IsNaN(b.prevPos) {
		b.hop = int(math.Round(hopIdeal))
		b.posError = math.Round(b.pos) - b.pos
		b.primed = true
	} else {
		b.posError -= hopIdeal
		b.hop = int(math.Round(-b.posError))
		b.posError += float64(b.hop)
	}
	b.prevPos = b.pos
	return continuous
}

func (b *bungee) analyseGrain(sample func(int64) (float64, float64), continuous bool) {
	center := int64(math.Round(b.pos))
	half := bungeeN / 2
	for ch := range 2 {
		for i := range half {
			left, right := sample(center + int64(i))
			older, olderR := sample(center - int64(half) + int64(i))
			sTop := left
			sBot := older
			if ch == 1 {
				sTop = right
				sBot = olderR
			}
			b.time[i] = complex(sTop*bungeeAnalysisWindow[i], 0)
			b.time[half+i] = complex(sBot*bungeeAnalysisWindow[half+i], 0)
		}
		copy(b.spec[ch], b.time)
		fft(b.spec[ch], false)
	}
	for i := range bungeeBins {
		sum := b.spec[0][i] + b.spec[1][i]
		re, im := real(sum), imag(sum)
		b.energy[i] = float32(re*re + im*im)
		b.phase[i] = bungeePhase(-math.Atan2(im, re))
	}
	b.partials = bungeeEnumerate(b.partials[:0], bungeeBins, b.energy)
	if continuous {
		b.partials = bungeeSuppress(b.partials, b.energy, b.prevEnergy)
	}
	b.synthesise(continuous)
	copy(b.prevPhase, b.phase)
	copy(b.prevRot, b.rotation)
	copy(b.prevEnergy, b.energy)
}

func (b *bungee) synthesise(continuous bool) {
	bungeeFrequency(b.speed).rotate(bungeeBins, b.rotation, b.phase)
	delta := make([]int16, len(b.partials))
	if continuous && len(b.prevPhase) > 0 {
		t := newBungeeTime(b.hop)
		for i, p := range b.partials {
			offset := b.phase[p.peak] - b.prevPhase[p.peak]
			stretched := t.delta(b.phase[p.peak], b.prevPhase[p.peak], p.peak)
			d := int16(int32(b.prevRot[p.peak]) - int32(offset) + int32(stretched))
			delta[i] = int16(int32(d) - int32(b.rotation[p.peak]))
		}
	} else {
		for i, p := range b.partials {
			delta[i] = int16(-int32(b.rotation[p.peak]))
		}
	}
	n := 0
	for i, p := range b.partials {
		for {
			if n < len(b.rotation) {
				b.rotation[n] = int16(int32(b.rotation[n]) + int32(delta[i]))
			}
			n++
			if n >= p.end {
				break
			}
		}
	}
	if ny := bungeeBins - 1; ny > 0 && ny < len(b.rotation) {
		b.rotation[ny] = b.rotation[ny-1]
	}
	for ch := range 2 {
		for k := range bungeeBins {
			ang := float32(b.rotation[k]) * float32(math.Pi) / 32768
			// The phase above is the conjugate of this transform, so the
			// rotation turns the other way to land on the same waveform.
			turn := complex(float64(math.Cos(float64(ang))), -float64(math.Sin(float64(ang))))
			b.spec[ch][k] *= turn
		}
		b.spec[ch][0] = complex(real(b.spec[ch][0]), 0)
		ny := bungeeN / 2
		b.spec[ch][ny] = complex(real(b.spec[ch][ny]), 0)
		for k := 1; k < ny; k++ {
			b.spec[ch][bungeeN-k] = complex(real(b.spec[ch][k]), -imag(b.spec[ch][k]))
		}
		fft(b.spec[ch], true)
		for i := range bungeeN {
			b.spec[ch][i] *= bungeeN
		}
	}
	b.applySynthesisWindow()
}

func (b *bungee) applySynthesisWindow() {
	const quadrant = bungeeHop
	const hops = bungeeN / bungeeHop
	for ch := range 2 {
		for i := range 4 {
			wOff := quadrant * (i ^ 2)
			j := (i + hops - 2) % hops
			inOff := quadrant * j
			dst := i * quadrant
			src := (i + 1) * quadrant
			for q := range quadrant {
				sample := real(b.spec[ch][inOff+q]) * bungeeSynthesisWindow[wOff+q]
				if i < 3 {
					b.lap[dst+q][ch] = sample + b.lap[src+q][ch]
				} else {
					b.lap[dst+q][ch] = sample
				}
			}
		}
	}
}

func (b *bungee) emit() [][2]float64 {
	out := make([][2]float64, bungeeHop)
	copy(out, b.lap[:bungeeHop])
	return out
}

func bungeePhase(radians float64) int16 {
	const scale = 65536 / (2 * math.Pi)
	return int16(int32(scale * radians))
}

func bungeeEnumerate(partials []bungeePartial, n int, energy []float32) []bungeePartial {
	saved0, saved1 := energy[n], energy[n+1]
	energy[n], energy[n+1] = -1, 0
	partials = partials[:0]
	m := 1
	for m < n+1 {
		for energy[m] < energy[m+1] {
			m++
		}
		var p bungeePartial
		p.peak = m
		m++
		for !(energy[m] < energy[m+1]) {
			m++
		}
		p.end = m
		m++
		partials = append(partials, p)
	}
	energy[n], energy[n+1] = saved0, saved1
	return partials
}

func bungeeSuppress(partials []bungeePartial, energy, previous []float32) []bungeePartial {
	if len(partials) < 3 {
		return partials
	}
	strongest := 0
	for i := 1; i < len(partials); i++ {
		if energy[partials[i].peak] > energy[partials[strongest].peak] {
			strongest = i
		}
	}
	const k = float32(1.5)
	for i := 1; i < len(partials)-1; i++ {
		if i == strongest {
			continue
		}
		if energy[partials[i].peak] > k*previous[partials[i].peak] {
			if energy[partials[i-1].end] > energy[partials[i].end] {
				partials[i-1].end = partials[i].end
			} else {
				partials[i].end = partials[i-1].end
			}
		}
	}
	return partials
}

type bungeeFreq struct {
	multiplier int32
}

func bungeeFrequency(speed float64) bungeeFreq {
	s := math.Abs(speed) + 1e-20
	mult := int32(math.Round(256 / -s))
	if mult < math.MinInt16 {
		mult = math.MinInt16
	}
	return bungeeFreq{multiplier: mult}
}

func (f bungeeFreq) rotate(n int, rotation, phase []int16) {
	rotation[0] = 0
	for m := 1; m < n; m++ {
		delta := int16(int32(phase[m-1]) - int32(phase[m]))
		x := int32(delta) * f.multiplier
		x >>= 8
		rotation[m] = int16(int32(rotation[m-1]) + x + int32(delta))
	}
}

type bungeeTime struct {
	a          int32
	multiplier int32
}

func newBungeeTime(analysisHop int) bungeeTime {
	const log2Transform = bungeeLogHop + 3
	a := int32(analysisHop) << (32 - log2Transform)
	dividend := int32(1<<bungeeLogHop) << 16
	divisor := int32(analysisHop) << 1
	var multiplier int32
	if divisor != 0 {
		absDiv := divisor
		if absDiv < 0 {
			absDiv = -absDiv
		}
		multiplier = (dividend + absDiv/2) / divisor
	}
	return bungeeTime{a: a, multiplier: multiplier}
}

func (t bungeeTime) delta(phase, previous int16, m int) int16 {
	p := int32(phase) << 16
	q := int32(previous) << 16
	const logS = 29
	da := (p - q) - int32(m)*t.a
	v := (int32(m) << logS) + (da>>15)*t.multiplier
	return int16(v >> 16)
}
