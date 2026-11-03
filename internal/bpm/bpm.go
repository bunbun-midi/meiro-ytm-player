// Package bpm finds one tempo for a track and the times of its measures.
// The grid is a single tempo for the whole track, the way a DJ program lays
// a beatgrid over a song: four beats to a measure, lined up with the start
// of the audio.
package bpm

import (
	"math"
	"time"
)

// Grid is the tempo of one track. Phase is the time of a measure start,
// measured from the start of the audio. Beat and Measure are the lengths
// that tempo implies. A 4/4 bar is four beats.
type Grid struct {
	BPM     float64
	Beat    time.Duration
	Measure time.Duration
	Phase   time.Duration
}

// Detect reads mono signed-16 PCM at sampleRate and reports a tempo when
// the audio has a steady beat. The phase is measured from the first sample.
// One tempo is used for the whole clip: it is taken from the steadiest
// stretch, then fitted to the beats themselves so the grid does not drift
// later in the track. When a beat and its double are both present, the one
// nearer a typical song pulse is kept, so a song does not flip between
// half time and double time.
func Detect(pcm []int16, sampleRate int) (Grid, bool) {
	env, low, hop, ok := bandOnsets(pcm, sampleRate)
	if !ok {
		return Grid{}, false
	}
	hopSec := float64(hop) / float64(sampleRate)
	rough, ok := steadiestPeriod(env, hopSec)
	if !ok || rough <= 0 {
		return Grid{}, false
	}
	beatSec, phaseSec, ok := fitBeats(env, low, rough*hopSec, hopSec)
	if !ok || beatSec <= 0 {
		return Grid{}, false
	}
	measure := beatSec * 4
	phaseSec = fold(phaseSec, measure)
	beat := time.Duration(math.Round(beatSec * float64(time.Second)))
	if beat <= 0 {
		return Grid{}, false
	}
	return Grid{
		BPM:     60 / beatSec,
		Beat:    beat,
		Measure: beat * 4,
		Phase:   time.Duration(math.Round(phaseSec * float64(time.Second))),
	}, true
}

// bandOnsets splits the audio into a kick band, a mid band and a high band
// and returns their onset strength per hop, plus the kick band on its own.
// Each band is scaled so a loud hi-hat does not hide the kick.
func bandOnsets(pcm []int16, sampleRate int) (all, low []float64, hop int, ok bool) {
	if sampleRate < 1000 || len(pcm) < sampleRate {
		return nil, nil, 0, false
	}
	hop = int(math.Round(0.005 * float64(sampleRate)))
	if hop < 1 {
		hop = 1
	}
	hops := len(pcm) / hop
	dt := 1 / float64(sampleRate)
	lp := func(fc float64) float64 {
		rc := 1 / (2 * math.Pi * fc)
		return dt / (rc + dt)
	}
	hp := func(fc float64) float64 {
		rc := 1 / (2 * math.Pi * fc)
		return rc / (rc + dt)
	}
	aLow, aMidLP, aMidHP, aHigh := lp(140), lp(2500), hp(200), hp(3500)
	lowE := make([]float64, hops)
	midE := make([]float64, hops)
	highE := make([]float64, hops)
	var yLow, yMidHP, prevMid, yMidLP, yHigh, prevHigh float64
	var eLow, eMid, eHigh float64
	n, hi := 0, 0
	for _, s := range pcm {
		if hi >= hops {
			break
		}
		x := float64(s)
		yLow += aLow * (x - yLow)
		yMidHP = aMidHP * (yMidHP + x - prevMid)
		prevMid = x
		yMidLP += aMidLP * (yMidHP - yMidLP)
		yHigh = aHigh * (yHigh + x - prevHigh)
		prevHigh = x
		eLow += yLow * yLow
		eMid += yMidLP * yMidLP
		eHigh += yHigh * yHigh
		n++
		if n == hop {
			lowE[hi], midE[hi], highE[hi] = eLow, eMid, eHigh
			hi++
			n = 0
			eLow, eMid, eHigh = 0, 0, 0
		}
	}
	low = positiveDiff(lowE)
	mid := normalize(positiveDiff(midE))
	high := normalize(positiveDiff(highE))
	lowN := normalize(append([]float64(nil), low...))
	all = make([]float64, len(low))
	for i := range all {
		all[i] = lowN[i] + mid[i] + high[i]
	}
	return all, low, hop, len(all) > 0
}

func positiveDiff(energy []float64) []float64 {
	out := make([]float64, len(energy))
	for i := 1; i < len(energy); i++ {
		if d := energy[i] - energy[i-1]; d > 0 {
			out[i] = d
		}
	}
	return out
}

// normalize scales x so its positive values average 1. A silent band stays
// zero and cannot invent a beat.
func normalize(x []float64) []float64 {
	var sum float64
	var n int
	for _, v := range x {
		if v > 0 {
			sum += v
			n++
		}
	}
	if n == 0 {
		return x
	}
	mean := sum / float64(n)
	for i := range x {
		x[i] /= mean
	}
	return x
}

// lagRange is the hop counts for 200 BPM down to 55 BPM, wide enough to see
// a beat and its double before the prior chooses.
func lagRange(hopSeconds float64) (minLag, maxLag int) {
	minLag = int(math.Round(60 / (200 * hopSeconds)))
	maxLag = int(math.Round(60 / (55 * hopSeconds)))
	if minLag < 1 {
		minLag = 1
	}
	if maxLag < minLag {
		maxLag = minLag
	}
	return minLag, maxLag
}

func autocorrelate(onset []float64, minLag, maxLag int) []float64 {
	scores := make([]float64, maxLag+1)
	n := len(onset)
	if maxLag >= n {
		maxLag = n - 1
	}
	for lag := minLag; lag <= maxLag; lag++ {
		var sum float64
		for i := 0; i+lag < n; i++ {
			sum += onset[i] * onset[i+lag]
		}
		if pairs := n - lag; pairs > 0 {
			scores[lag] = sum / float64(pairs)
		}
	}
	return scores
}

// prior favors a tempo near 120 BPM, in octaves, so two equally strong
// pulses an octave apart resolve the same way every time.
func prior(bpm float64) float64 {
	const (
		center = 120.0
		sigma  = 0.6
	)
	if bpm <= 0 {
		return 0
	}
	x := math.Log2(bpm/center) / sigma
	return math.Exp(-0.5 * x * x)
}

// steadiestPeriod is the beat length, in hops, of the stretch whose pulse
// is clearest. A quiet or arrhythmic intro does not get a vote.
func steadiestPeriod(env []float64, hopSec float64) (float64, bool) {
	win := int(math.Round(16 / hopSec))
	step := int(math.Round(8 / hopSec))
	if step < 1 {
		step = 1
	}
	if len(env) < win {
		period, conf, ok := fitPeriod(env, hopSec)
		if !ok || conf < minTempoConf {
			return 0, false
		}
		return period, true
	}
	bestConf, best := -1.0, 0.0
	for start := 0; start+win <= len(env); start += step {
		period, conf, ok := fitPeriod(env[start:start+win], hopSec)
		if ok && conf > bestConf {
			bestConf, best = conf, period
		}
	}
	if bestConf < minTempoConf {
		return 0, false
	}
	return best, true
}

// minTempoConf is how far the winning lag must stand above the others.
const minTempoConf = 1.35

// fitPeriod returns the fractional lag of the tempo and how far it stands
// out. A lag with little of its own pulse cannot win on the prior alone,
// which keeps a slow song from being reported at double its speed.
func fitPeriod(env []float64, hopSec float64) (period, conf float64, ok bool) {
	minLag, maxLag := lagRange(hopSec)
	if len(env) < maxLag*2 {
		return 0, 0, false
	}
	ac := autocorrelate(env, minLag, maxLag*2)
	rawBest := 0.0
	for lag := minLag; lag <= maxLag; lag++ {
		if ac[lag] > rawBest {
			rawBest = ac[lag]
		}
	}
	if rawBest <= 0 {
		return 0, 0, false
	}
	bestLag := minLag
	best := -1.0
	for lag := minLag; lag <= maxLag; lag++ {
		if ac[lag] < rawBest*0.45 {
			continue
		}
		// The period is rarely a whole number of hops, so the pulse one
		// octave down sits on a neighbouring lag, not exactly at 2*lag.
		harmonic := peakNear(ac, lag*2)
		bpm := 60 / (float64(lag) * hopSec)
		if s := (ac[lag] + 0.5*harmonic) * prior(bpm); s > best {
			best, bestLag = s, lag
		}
	}
	base := mean(ac[minLag : maxLag+1])
	if base <= 0 {
		return 0, 0, false
	}
	return refineLag(ac, bestLag), rawBest / base, true
}

func peakNear(ac []float64, lag int) float64 {
	best := 0.0
	for d := -1; d <= 1; d++ {
		i := lag + d
		if i > 0 && i < len(ac) && ac[i] > best {
			best = ac[i]
		}
	}
	return best
}

func refineLag(ac []float64, k int) float64 {
	if k <= 0 || k+1 >= len(ac) {
		return float64(k)
	}
	a, b, c := ac[k-1], ac[k], ac[k+1]
	den := a - 2*b + c
	if den == 0 {
		return float64(k)
	}
	delta := 0.5 * (a - c) / den
	if delta > 1 || delta < -1 {
		delta = 0
	}
	return float64(k) + delta
}

func mean(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	var s float64
	for _, x := range v {
		s += x
	}
	return s / float64(len(v))
}

// fitBeats turns a rough beat length into a period and a downbeat measured
// from the start of the audio. The period is a straight line through the
// detected beats, so it still lands on them at the end of a long track.
func fitBeats(env, low []float64, rough, hopSec float64) (beat, phase float64, ok bool) {
	if rough <= 0 {
		return 0, 0, false
	}
	phase = align(env, rough, hopSec)
	peaks := peaksOf(env, rough/hopSec)
	period := rough
	for pass := 0; pass < 3 && len(peaks) >= 8; pass++ {
		var xs, ys []float64
		for _, p := range peaks {
			t := float64(p) * hopSec
			pred := phase + math.Round((t-phase)/period)*period
			if pass > 0 && math.Abs(t-pred) > period*0.15 {
				continue
			}
			xs = append(xs, math.Round((t-phase)/period))
			ys = append(ys, t)
		}
		if len(xs) < 8 {
			break
		}
		a, b, good := linreg(xs, ys)
		if !good || b <= 0 || math.Abs(b-rough)/rough > 0.08 {
			break
		}
		phase, period = a, b
	}
	phase = downbeat(low, env, phase, period, hopSec)
	return period, phase, true
}

// align is the time of a beat in the first period, the offset where the
// onsets pile up.
func align(env []float64, period, hopSec float64) float64 {
	hops := period / hopSec
	if hops < 2 {
		return 0
	}
	steps := int(hops)
	bestOff, best := 0.0, -1.0
	for s := 0; s < steps; s++ {
		if sc := pulseScore(env, float64(s), hops); sc > best {
			best, bestOff = sc, float64(s)
		}
	}
	return bestOff * hopSec
}

func pulseScore(env []float64, offset, period float64) float64 {
	if period <= 0 || len(env) < 2 {
		return 0
	}
	var sum float64
	for t := offset; t < float64(len(env)-1); t += period {
		i := int(t)
		if i < 0 {
			continue
		}
		frac := t - float64(i)
		sum += env[i]*(1-frac) + env[i+1]*frac
	}
	return sum
}

// downbeat moves phase onto the kick, or onto the strongest beat of the bar
// when the kick band is empty. phase and beat are in seconds.
func downbeat(low, all []float64, phase, beat, hopSec float64) float64 {
	band := low
	if sum(low) <= 0 {
		band = all
	}
	bestK, best := 0, -1.0
	bar := beat * 4 / hopSec
	for k := 0; k < 4; k++ {
		off := (phase + float64(k)*beat) / hopSec
		if sc := pulseScore(band, off, bar); sc > best {
			best, bestK = sc, k
		}
	}
	return fold(phase+float64(bestK)*beat, beat*4)
}

func sum(v []float64) float64 {
	var s float64
	for _, x := range v {
		s += x
	}
	return s
}

func fold(phase, measure float64) float64 {
	if measure <= 0 {
		return 0
	}
	phase = math.Mod(phase, measure)
	if phase < 0 {
		phase += measure
	}
	return phase
}

func peaksOf(env []float64, periodHops float64) []int {
	maxV := 0.0
	for _, v := range env {
		if v > maxV {
			maxV = v
		}
	}
	if maxV <= 0 {
		return nil
	}
	thresh := maxV * 0.3
	gap := int(periodHops * 0.5)
	if gap < 1 {
		gap = 1
	}
	var peaks []int
	last := -gap
	for i := 1; i < len(env)-1; i++ {
		if env[i] < thresh || env[i] < env[i-1] || env[i] < env[i+1] {
			continue
		}
		if i-last < gap {
			if len(peaks) > 0 && env[i] > env[peaks[len(peaks)-1]] {
				peaks[len(peaks)-1] = i
				last = i
			}
			continue
		}
		peaks = append(peaks, i)
		last = i
	}
	return peaks
}

func linreg(xs, ys []float64) (a, b float64, ok bool) {
	n := float64(len(xs))
	if len(xs) < 2 || len(xs) != len(ys) {
		return 0, 0, false
	}
	var sx, sy, sxx, sxy float64
	for i := range xs {
		sx += xs[i]
		sy += ys[i]
		sxx += xs[i] * xs[i]
		sxy += xs[i] * ys[i]
	}
	den := n*sxx - sx*sx
	if den == 0 {
		return 0, 0, false
	}
	b = (n*sxy - sx*sy) / den
	a = (sy - b*sx) / n
	return a, b, true
}

// Measures lists the measure starts from the beginning of the audio through
// duration, including one that falls at time zero.
func (g Grid) Measures(duration time.Duration) []time.Duration {
	if g.Measure <= 0 || duration <= 0 {
		return nil
	}
	start := g.Phase
	if start >= g.Measure {
		start -= time.Duration(int64(start/g.Measure)) * g.Measure
	}
	for start < 0 {
		start += g.Measure
	}
	var out []time.Duration
	for at := start; at < duration && len(out) < 10000; at += g.Measure {
		if at >= 0 {
			out = append(out, at)
		}
	}
	return out
}

// Nearest is the measure start closest to pos, inside a track of duration.
func (g Grid) Nearest(pos, duration time.Duration) (time.Duration, bool) {
	marks := g.Measures(duration)
	if len(marks) == 0 {
		return 0, false
	}
	best := marks[0]
	bestD := distance(pos, best)
	for _, at := range marks[1:] {
		if d := distance(pos, at); d < bestD {
			best, bestD = at, d
		}
	}
	return best, true
}

func distance(a, b time.Duration) time.Duration {
	if a < b {
		return b - a
	}
	return a - b
}

// Jump moves pos by n measures. n may be a fraction, such as a half or a
// quarter. With snap, the landing point is the next grid line of that
// spacing strictly after pos when n is positive, and the previous one
// strictly before pos when n is negative. A position already on a line
// moves a full step. Without snap, the time is added to pos. A result
// before the start of the track is zero.
func (g Grid) Jump(pos time.Duration, n float64, snap bool) time.Duration {
	if g.Measure <= 0 || n == 0 {
		return pos
	}
	step := time.Duration(math.Round(n * float64(g.Measure)))
	if !snap {
		if next := pos + step; next > 0 {
			return next
		}
		return 0
	}
	unit := step
	if unit < 0 {
		unit = -unit
	}
	if unit == 0 {
		return pos
	}
	diff := int64(pos) - int64(g.Phase)
	u := int64(unit)
	var k int64
	if n > 0 {
		k = divFloor(diff, u) + 1
	} else {
		k = divCeil(diff, u) - 1
	}
	target := int64(g.Phase) + k*u
	if target < 0 {
		return 0
	}
	return time.Duration(target)
}

func divFloor(a, b int64) int64 {
	q := a / b
	if a%b != 0 && (a < 0) != (b < 0) {
		q--
	}
	return q
}

func divCeil(a, b int64) int64 {
	q := a / b
	if a%b != 0 && (a < 0) == (b < 0) {
		q++
	}
	return q
}
