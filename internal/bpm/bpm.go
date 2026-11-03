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
func Detect(pcm []int16, sampleRate int) (Grid, bool) {
	if sampleRate < 1000 || len(pcm) < sampleRate {
		return Grid{}, false
	}
	const hopSeconds = 0.005
	hop := int(math.Round(hopSeconds * float64(sampleRate)))
	if hop < 1 {
		return Grid{}, false
	}
	hops := len(pcm) / hop
	minLag, maxLag := lagRange(float64(hop) / float64(sampleRate))
	if hops < maxLag*2 {
		return Grid{}, false
	}
	onset := onsetStrength(pcm, hop, hops)
	scores := autocorrelate(onset, minLag, maxLag)
	best := minLag
	for lag := minLag + 1; lag <= maxLag; lag++ {
		if scores[lag] > scores[best] {
			best = lag
		}
	}
	if scores[best] <= 0 || scores[best] < meanScore(scores, minLag, maxLag)*2 {
		return Grid{}, false
	}
	hopDur := float64(hop) / float64(sampleRate)
	chosen := chooseLag(scores, minLag, maxLag, best, hopDur)
	phaseHop := downbeat(onset, chosen)
	beat := time.Duration(float64(chosen*hop) / float64(sampleRate) * float64(time.Second))
	if beat <= 0 {
		return Grid{}, false
	}
	return Grid{
		BPM:     60 / beat.Seconds(),
		Beat:    beat,
		Measure: beat * 4,
		Phase:   time.Duration(float64(phaseHop*hop) / float64(sampleRate) * float64(time.Second)),
	}, true
}

// lagRange is the hop counts for 180 BPM down to 70 BPM.
func lagRange(hopSeconds float64) (minLag, maxLag int) {
	minLag = int(math.Round(60 / (180 * hopSeconds)))
	maxLag = int(math.Round(60 / (70 * hopSeconds)))
	if minLag < 1 {
		minLag = 1
	}
	if maxLag < minLag {
		maxLag = minLag
	}
	return minLag, maxLag
}

func onsetStrength(pcm []int16, hop, hops int) []float64 {
	energy := make([]float64, hops)
	for i := 0; i < hops; i++ {
		var sum float64
		chunk := pcm[i*hop : (i+1)*hop]
		for _, s := range chunk {
			v := float64(s)
			sum += v * v
		}
		energy[i] = sum
	}
	onset := make([]float64, hops)
	for i := 1; i < hops; i++ {
		if d := energy[i] - energy[i-1]; d > 0 {
			onset[i] = d
		}
	}
	return onset
}

func autocorrelate(onset []float64, minLag, maxLag int) []float64 {
	scores := make([]float64, maxLag+1)
	n := len(onset)
	for lag := minLag; lag <= maxLag; lag++ {
		var sum float64
		for i := 0; i+lag < n; i++ {
			sum += onset[i] * onset[i+lag]
		}
		pairs := n - lag
		if pairs > 0 {
			scores[lag] = sum / float64(pairs)
		}
	}
	return scores
}

func meanScore(scores []float64, minLag, maxLag int) float64 {
	var sum float64
	for lag := minLag; lag <= maxLag; lag++ {
		sum += scores[lag]
	}
	return sum / float64(maxLag-minLag+1)
}

// chooseLag keeps a tempo between 80 and 160 BPM when half or double the
// winning lag is nearly as strong. A tempo already in that range stays.
func chooseLag(scores []float64, minLag, maxLag, best int, hopSeconds float64) int {
	if musical(60 / (float64(best) * hopSeconds)) {
		return best
	}
	for _, lag := range []int{best / 2, best * 2} {
		if lag < minLag || lag > maxLag || lag >= len(scores) {
			continue
		}
		if scores[lag] < scores[best]*0.85 {
			continue
		}
		if musical(60 / (float64(lag) * hopSeconds)) {
			return lag
		}
	}
	return best
}

func musical(tempo float64) bool { return tempo >= 80 && tempo <= 160 }

// downbeat is the hop of a measure start: the beat phase, then the strongest
// of the four beats in a bar.
func downbeat(onset []float64, beatLag int) int {
	bestOff, bestSum := 0, -1.0
	for off := 0; off < beatLag; off++ {
		sum := strideSum(onset, off, beatLag)
		if sum > bestSum {
			bestSum = sum
			bestOff = off
		}
	}
	bar := beatLag * 4
	down, downSum := bestOff, -1.0
	for k := 0; k < 4; k++ {
		off := bestOff + k*beatLag
		sum := strideSum(onset, off, bar)
		if sum > downSum {
			downSum = sum
			down = off
		}
	}
	return down % bar
}

func strideSum(onset []float64, off, step int) float64 {
	if step <= 0 {
		return 0
	}
	var sum float64
	for i := off; i < len(onset); i += step {
		sum += onset[i]
	}
	return sum
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
