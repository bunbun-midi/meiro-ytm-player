package player

import "math"

// Phase vocoder time stretch. The synthesis hop is a quarter of the window,
// so four Hann windows overlap and their sum is constant. pvWindow is that
// Hann divided by the sum, which makes a speed of 1 add back to the samples
// that went in once the first window has filled.
const (
	pvN   = 2048
	pvHop = 512
)

// pvWindow sums to one with a hop of pvHop.
var pvWindow = func() [pvN]float64 {
	var w [pvN]float64
	for i := range w {
		w[i] = 0.25 * (1 - math.Cos(2*math.Pi*float64(i)/pvN))
	}
	return w
}()

// Stretch chooses how the speed slider stretches time.
type Stretch int

const (
	// StretchBungee is Bungee Basic's phase vocoder. It is the default.
	StretchBungee Stretch = iota
	// StretchPhaseVocoder keeps the recording's pitch by resynthesising each
	// frequency's phase on its own.
	StretchPhaseVocoder
	// StretchOverlapAdd repeats and overlaps grains of the waveform, sliding
	// each one so it lines up with the grain before it.
	StretchOverlapAdd
)

func knownStretch(stretch Stretch) Stretch {
	switch stretch {
	case StretchBungee, StretchPhaseVocoder, StretchOverlapAdd:
		return stretch
	default:
		return StretchBungee
	}
}

func (r *rateReader) placeVocoder() error {
	need := int64(math.Floor(r.inPos)) + pvN + 2
	ok, err := r.readyInput(need)
	if !ok {
		return err
	}
	r.synthesize(r.inPos, r.grainAt)
	r.inPos += float64(pvHop) * r.stretch
	r.grainAt += pvHop
	r.dropInput(pvN)
	return nil
}

// synthesize overlap-adds one phase-vocoder frame. At the recorded speed the
// spectrum is copied through, so the window sum reconstructs the samples.
// Any other speed advances each bin's phase by how far that frame moved.
func (r *rateReader) synthesize(inPos float64, outPos int64) {
	if r.pvSpec == nil {
		bins := pvN/2 + 1
		r.pvSpec = make([]complex128, pvN)
		r.pvPrev = [2][]float64{make([]float64, bins), make([]float64, bins)}
		r.pvSynth = [2][]float64{make([]float64, bins), make([]float64, bins)}
		r.pvMag = make([]float64, bins)
		r.pvPhase = make([]float64, bins)
		r.pvPeaks = make([]int, 0, bins)
	}
	r.ensureOLA(outPos, pvN)
	base := int(outPos - r.olaOrigin)
	ha := float64(pvHop) * r.stretch
	identity := math.Abs(r.stretch-1) < 1e-4
	for ch := range 2 {
		for i := range pvN {
			left, right := r.sampleAt(inPos + float64(i))
			sample := left
			if ch == 1 {
				sample = right
			}
			r.pvSpec[i] = complex(sample*pvWindow[i], 0)
		}
		fft(r.pvSpec, false)
		if !identity {
			r.shiftPhases(ch)
		}
		fft(r.pvSpec, true)
		for i := range pvN {
			r.ola[base+i][ch] += real(r.pvSpec[i])
		}
	}
	r.pvHave = true
	r.pvCopied = identity
	r.pvHa = ha
	if end := outPos + pvN; end > r.olaEnd {
		r.olaEnd = end
	}
}

// shiftPhases moves each spectral peak so the hop lands on the stretched
// time, and keeps the phase around that peak the way the analysis found it.
// A bin rotated on its own drifts off its neighbours and the overlap cancels.
// The upper half is then mirrored so the waveform stays real.
func (r *rateReader) shiftPhases(ch int) {
	bins := pvN/2 + 1
	for k := range bins {
		re := real(r.pvSpec[k])
		im := imag(r.pvSpec[k])
		r.pvMag[k] = math.Hypot(re, im)
		r.pvPhase[k] = math.Atan2(im, re)
	}
	restart := !r.pvHave || r.pvCopied || r.pvHa == 0
	r.pvPeaks = r.pvPeaks[:0]
	for k := range bins {
		left, right := 0.0, 0.0
		if k > 0 {
			left = r.pvMag[k-1]
		}
		if k+1 < bins {
			right = r.pvMag[k+1]
		}
		if r.pvMag[k] > 0 && r.pvMag[k] >= left && r.pvMag[k] > right {
			r.pvPeaks = append(r.pvPeaks, k)
		}
	}
	if len(r.pvPeaks) == 0 {
		r.pvPeaks = append(r.pvPeaks, 0)
	}
	for _, k := range r.pvPeaks {
		synth := r.pvPhase[k]
		if !restart {
			// The forward transform's phase falls as a frame moves later, so
			// the expected advance is minus the bin frequency.
			omega := 2 * math.Pi * float64(k) / pvN
			delta := princarg(r.pvPhase[k] - r.pvPrev[ch][k] + omega*r.pvHa)
			freq := -omega + delta/r.pvHa
			synth = princarg(r.pvSynth[ch][k] + freq*float64(pvHop))
		}
		r.pvSynth[ch][k] = synth
	}
	for i, peak := range r.pvPeaks {
		from := 0
		if i > 0 {
			from = (r.pvPeaks[i-1]+peak)/2 + 1
		}
		to := bins
		if i+1 < len(r.pvPeaks) {
			to = (peak+r.pvPeaks[i+1])/2 + 1
		}
		for k := from; k < to; k++ {
			synth := princarg(r.pvSynth[ch][peak] + r.pvPhase[k] - r.pvPhase[peak])
			r.pvPrev[ch][k] = r.pvPhase[k]
			r.pvSynth[ch][k] = synth
			r.pvSpec[k] = complex(r.pvMag[k]*math.Cos(synth), r.pvMag[k]*math.Sin(synth))
		}
	}
	r.pvSpec[0] = complex(real(r.pvSpec[0]), 0)
	r.pvSpec[pvN/2] = complex(real(r.pvSpec[pvN/2]), 0)
	for k := 1; k < pvN/2; k++ {
		r.pvSpec[pvN-k] = complex(real(r.pvSpec[k]), -imag(r.pvSpec[k]))
	}
}

func princarg(p float64) float64 {
	return p - 2*math.Pi*math.Round(p/(2*math.Pi))
}

// fft is a radix-2 transform. inverse divides by the length, so the two
// passes bring a buffer back to itself.
func fft(a []complex128, inverse bool) {
	n := len(a)
	for i, j := 1, 0; i < n; i++ {
		bit := n >> 1
		for j&bit != 0 {
			j &^= bit
			bit >>= 1
		}
		j |= bit
		if i < j {
			a[i], a[j] = a[j], a[i]
		}
	}
	for length := 2; length <= n; length <<= 1 {
		ang := 2 * math.Pi / float64(length)
		if inverse {
			ang = -ang
		}
		wlen := complex(math.Cos(ang), math.Sin(ang))
		for i := 0; i < n; i += length {
			w := 1 + 0i
			half := length / 2
			for j := 0; j < half; j++ {
				u := a[i+j]
				v := a[i+j+half] * w
				a[i+j] = u + v
				a[i+j+half] = u - v
				w *= wlen
			}
		}
	}
	if inverse {
		scale := 1 / float64(n)
		for i := range a {
			a[i] *= complex(scale, 0)
		}
	}
}
