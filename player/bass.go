package player

import (
	"io"
	"math"
	"sync/atomic"
)

// MaxBassBoost is the loudest the dynamic bass boost will add, in dB.
const MaxBassBoost = 15

// bassBoost is the dynamic bass boost shared by the audio thread and the
// controls. The controls publish a target; the reader owns the filter state.
type bassBoost struct {
	target atomic.Pointer[bassTarget]
}

type bassTarget struct {
	on bool
	db float64
}

func newBassBoost(on bool, db float64) *bassBoost {
	b := &bassBoost{}
	b.Set(on, db)
	return b
}

// Set publishes whether the boost is on and how many dB it may add.
func (b *bassBoost) Set(on bool, db float64) {
	if math.IsNaN(db) || db < 0 {
		db = 0
	}
	if db > MaxBassBoost {
		db = MaxBassBoost
	}
	b.target.Store(&bassTarget{on: on, db: db})
}

// bassReader raises the loudest low notes of a stereo s16le stream, up to
// the published amount, and leaves a passage with no bass peak untouched.
// The detector is the one in JamesDSP's dynamic bass boost: the mix is
// looked at around 500 Hz, and a peaking filter follows the strongest low
// frequency by the difference between the loudest and quietest bands.
type bassReader struct {
	reader io.Reader
	boost  *bassBoost
	dsp    dbb

	pending []byte
	ready   []byte
	scratch [32 * 1024]byte
	readErr error
}

func newBassReader(reader io.Reader, boost *bassBoost) *bassReader {
	r := &bassReader{reader: reader, boost: boost}
	r.dsp.prepare(sampleRate)
	return r
}

func (r *bassReader) Read(out []byte) (int, error) {
	if len(out) == 0 {
		return 0, nil
	}
	if len(r.ready) > 0 {
		return r.emit(out)
	}
	wanted := min(max(len(out)-len(out)%4, 4), len(r.scratch))
	for len(r.pending) < wanted && r.readErr == nil {
		need := min(wanted-len(r.pending)+3, len(r.scratch))
		n, err := r.reader.Read(r.scratch[:need])
		r.pending = append(r.pending, r.scratch[:n]...)
		if err != nil {
			r.readErr = err
		}
		if n == 0 && err == nil {
			return 0, nil
		}
	}
	n := min(len(r.pending), wanted)
	n -= n % 4
	if n > 0 {
		target := r.boost.target.Load()
		if target != nil && target.on && target.db > 0 {
			r.dsp.process(r.pending[:n], target.db)
		}
	} else if r.readErr != nil {
		n = min(len(r.pending), len(out))
	}
	r.ready = r.pending[:n]
	r.pending = r.pending[n:]
	return r.emit(out)
}

func (r *bassReader) emit(out []byte) (int, error) {
	n := copy(out, r.ready)
	r.ready = r.ready[n:]
	if n == 0 {
		if r.readErr != nil {
			return 0, r.readErr
		}
		return 0, nil
	}
	return n, nil
}

// dbb is one stream's dynamic bass boost. Its shape follows dbb.c.
type dbb struct {
	ready bool
	fs    float64
	maxDB float64

	factor int
	gain   float64
	coeffs [8][2]float64
	lpf    [8]biquadMemory
	xn1    float64
	xn2    float64
	buf    []float64
	pos    int

	delay      [16]float64
	fft        [16]float64
	freq       [9]float64
	maxSmooth  float64
	gainSmooth float64
	smoothMax  float64
	boostdB    float64
	svf        svfCoeffs
	z          [2]svfState
	dry        [2]delayLine
}

type biquadMemory struct {
	yn, yn1, yn2 float64
}

type svfCoeffs struct {
	g, pre1, pre2, pre3, k float64
}

type svfState struct {
	z1, z2 float64
}

type delayLine struct {
	in    [1024]float64
	at    int
	out   int
	limit int
}

func (d *dbb) prepare(fs int) {
	if d.ready && d.fs == float64(fs) {
		return
	}
	d.fs = float64(fs)
	factor := int(math.Round(d.fs / 500))
	if factor < 1 {
		factor = 1
	}
	if factor > 960 {
		factor = 960
	}
	d.factor = factor
	d.buf = make([]float64, factor)
	d.pos = 0
	trueFS := d.fs / float64(factor)
	for i := range d.freq {
		d.freq[i] = float64(i) * trueFS / 16
	}
	d.coeffs, d.gain = butterworthDown(factor)
	d.lpf = [8]biquadMemory{}
	d.xn1, d.xn2 = 0, 0
	d.maxSmooth = 1 - math.Exp(-1/(0.0005*d.fs))
	d.gainSmooth = 1 - math.Exp(-1/(0.002*d.fs))
	d.smoothMax = d.freq[1]
	d.boostdB = 0
	lag := factor + int(math.Round(2*d.fs/1000))
	for i := range d.dry {
		d.dry[i].limit = len(d.dry[i].in)
		d.dry[i].set(lag)
	}
	d.svf = svfPeak(d.fs, d.smoothMax, 0)
	d.z = [2]svfState{}
	d.ready = true
}

func (d *dbb) process(pcm []byte, maxDB float64) {
	d.prepare(sampleRate)
	if maxDB != d.maxDB {
		d.maxDB = maxDB
	}
	for frame := 0; frame+3 < len(pcm); frame += 4 {
		left := sampleAt(pcm, frame)
		right := sampleAt(pcm, frame+2)
		d.buf[d.pos] = (left + right) * 0.5
		d.pos++
		if d.pos == d.factor {
			d.analyse()
			d.pos = 0
		}
		dl := d.dry[0].push(left)
		dr := d.dry[1].push(right)
		yl, yr := d.svf.filter(dl, dr, &d.z)
		putSample(pcm, frame, yl)
		putSample(pcm, frame+2, yr)
	}
}

func (d *dbb) analyse() {
	copy(d.delay[1:], d.delay[:15])
	d.delay[0] = d.decimate(d.buf)
	d.fft = [16]float64{}
	order := [...]int{0, 8, 4, 12, 2, 10, 6, 14, 1, 9, 5, 13, 3, 11, 7, 15}
	for i, src := range order {
		d.fft[src] = d.delay[i]
	}
	fht16(&d.fft)
	peak := math.Abs(d.fft[0])
	floor := peak
	maxF := d.freq[0]
	for i := 1; i < 9; i++ {
		sym := 16 - i
		re := (d.fft[i] + d.fft[sym]) * 0.5
		im := (d.fft[i] - d.fft[sym]) * 0.5
		mag := math.Hypot(re, im)
		if mag > peak {
			peak = mag
			maxF = d.freq[i]
		}
		if mag < floor {
			floor = mag
		}
	}
	hi := 20 * math.Log10(peak+1e-20)
	lo := 20 * math.Log10(floor+1e-20)
	clamp := hi - lo
	if clamp > d.maxDB {
		clamp = d.maxDB
	}
	if clamp < 0 {
		clamp = 0
	}
	d.boostdB = clamp*d.gainSmooth + d.boostdB*(1-d.gainSmooth)
	d.smoothMax = maxF*d.maxSmooth + d.smoothMax*(1-d.maxSmooth)
	d.svf = svfPeak(d.fs, d.smoothMax, d.boostdB)
}

func (d *dbb) decimate(in []float64) float64 {
	var out float64
	for _, x := range in {
		out = d.lowpass(x) * d.gain
	}
	return out
}

func (d *dbb) lowpass(x float64) float64 {
	yn := x + 2*d.xn1 + d.xn2 + d.coeffs[0][0]*d.lpf[0].yn1 + d.coeffs[0][1]*d.lpf[0].yn2
	d.lpf[0].yn = yn
	for i := 1; i < len(d.lpf); i++ {
		yn = d.lpf[i-1].yn + 2*d.lpf[i-1].yn1 + d.lpf[i-1].yn2 + d.coeffs[i][0]*d.lpf[i].yn1 + d.coeffs[i][1]*d.lpf[i].yn2
		d.lpf[i].yn = yn
	}
	for i := range d.lpf {
		d.lpf[i].yn2 = d.lpf[i].yn1
		d.lpf[i].yn1 = d.lpf[i].yn
	}
	d.xn2 = d.xn1
	d.xn1 = x
	return d.lpf[len(d.lpf)-1].yn
}

func (line *delayLine) set(lag int) {
	line.at = 0
	if lag > line.limit-1 {
		line.out = 1
		return
	}
	line.out = -lag
	for line.out < 0 {
		line.out += line.limit
	}
}

func (line *delayLine) push(sample float64) float64 {
	line.in[line.at] = sample
	line.at++
	if line.at == line.limit {
		line.at = 0
	}
	last := line.in[line.out]
	line.out++
	if line.out >= line.limit {
		line.out = 0
	}
	return last
}

func fht16(a *[16]float64) {
	combine := func(i0, i1, i2, i3 int) {
		alpha, beta := a[i0], a[i1]
		beta2, alpha1 := a[i2], a[i3]
		alpha2 := alpha + beta
		y1 := alpha - beta
		y2 := beta2 + alpha1
		y3 := beta2 - alpha1
		a[i0] = alpha2 + y2
		a[i2] = alpha2 - y2
		a[i1] = y1 + y3
		a[i3] = y1 - y3
	}
	combine(0, 1, 2, 3)
	combine(4, 5, 6, 7)
	combine(8, 9, 10, 11)
	combine(12, 13, 14, 15)
	pair := func(i, j int) {
		alpha, beta := a[i], a[j]
		a[i] = alpha + beta
		a[j] = alpha - beta
	}
	pair(0, 4)
	pair(2, 6)
	alpha := a[1]
	beta := 0.707106769 * (a[5] + a[7])
	beta2 := 0.707106769 * (a[5] - a[7])
	a[1] = alpha + beta
	a[5] = alpha - beta
	alpha = a[3]
	a[3] = alpha + beta2
	a[7] = alpha - beta2
	pair(8, 12)
	pair(10, 14)
	alpha = a[9]
	beta = 0.707106769 * (a[13] + a[15])
	beta2 = 0.707106769 * (a[13] - a[15])
	a[9] = alpha + beta
	a[13] = alpha - beta
	alpha = a[11]
	a[11] = alpha + beta2
	a[15] = alpha - beta2
	pair(0, 8)
	pair(4, 12)
	alpha1, alpha2 := a[1], a[7]
	beta = a[9]*0.923879504 + a[15]*0.382683426
	beta2 = a[9]*0.382683426 - a[15]*0.923879504
	a[1] = alpha1 + beta
	a[9] = alpha1 - beta
	a[7] = alpha2 + beta2
	a[15] = alpha2 - beta2
	alpha1 = a[10] * 0.707106769
	alpha2 = a[14] * 0.707106769
	beta = alpha1 + alpha2
	beta2 = alpha1 - alpha2
	alpha1, alpha2 = a[2], a[6]
	a[2] = alpha1 + beta
	a[10] = alpha1 - beta
	a[6] = alpha2 + beta2
	a[14] = alpha2 - beta2
	alpha1, alpha2 = a[3], a[5]
	beta = a[11]*0.382683426 + a[13]*0.923879504
	beta2 = a[11]*0.923879504 - a[13]*0.382683426
	a[3] = alpha1 + beta
	a[11] = alpha1 - beta
	a[5] = alpha2 + beta2
	a[13] = alpha2 - beta2
}

func svfPeak(fs, cutoff, boostDB float64) svfCoeffs {
	if cutoff < 1 {
		cutoff = 1
	}
	if cutoff > fs/2-1 {
		cutoff = fs/2 - 1
	}
	t := 1 / fs
	wa := (2 / t) * math.Tan(cutoff*2*math.Pi*t/2)
	g := wa * t / 2
	q := 1 / (2 * (1 - 0.75))
	r := 1 / (2 * q)
	k := math.Pow(10, boostDB/20) - 1
	return svfCoeffs{
		g:    g,
		pre1: 2*r + g,
		pre2: 1 / (1 + 2*r*g + g*g),
		pre3: 2 * r,
		k:    k,
	}
}

func (c svfCoeffs) filter(x1, x2 float64, z *[2]svfState) (float64, float64) {
	return c.one(x1, &z[0]), c.one(x2, &z[1])
}

func (c svfCoeffs) one(x float64, z *svfState) float64 {
	hp := (x - c.pre1*z.z1 - z.z2) * c.pre2
	bp := hp*c.g + z.z1
	lp := bp*c.g + z.z2
	ubp := c.pre3 * bp
	z.z1 = c.g*hp + bp
	z.z2 = c.g*bp + lp
	return x + ubp*c.k
}

func butterworthDown(factor int) ([8][2]float64, float64) {
	const order = 16
	fs := float64(2 * factor)
	wc := 2 * math.Tan(math.Pi*0.89/fs)
	type c128 struct{ re, im float64 }
	poles := make([]c128, order)
	index := 0
	for k := 0; k < (order+1)/2; k++ {
		theta := float64(2*k+1) * math.Pi / (2 * order)
		real, imag := -math.Sin(theta), math.Cos(theta)
		poles[index+k] = c128{real, imag}
		poles[index+k+1] = c128{real, -imag}
		index++
	}
	for i := range poles {
		poles[i].re *= wc
		poles[i].im *= wc
	}
	gain := math.Pow(wc, float64(len(poles)))
	pre := gain
	for i := range poles {
		gain *= blt(&poles[i].re, &poles[i].im)
	}
	overall := pre * (pre / gain)
	var coeffs [8][2]float64
	for i := 0; i+1 < len(poles); i += 2 {
		re := -(poles[i].re + poles[i+1].re)
		prod := poles[i].re*poles[i+1].re - poles[i].im*poles[i+1].im
		n := i / 2
		coeffs[n][0] = -re
		coeffs[n][1] = -prod
	}
	return coeffs, overall
}

func blt(re, im *float64) float64 {
	sRe, sIm := *re, *im
	real := 2 - sRe
	imag := -sIm
	den := real*real + imag*imag
	*re = ((2+sRe)*real + sIm*imag) / den
	*im = (sIm*real - (2+sRe)*imag) / den
	return math.Hypot(real, imag)
}

func sampleAt(pcm []byte, offset int) float64 {
	sample := int16(uint16(pcm[offset]) | uint16(pcm[offset+1])<<8)
	return float64(sample) / 32768
}

func putSample(pcm []byte, offset int, value float64) {
	scaled := int(math.Round(value * 32768))
	if scaled > math.MaxInt16 {
		scaled = math.MaxInt16
	}
	if scaled < math.MinInt16 {
		scaled = math.MinInt16
	}
	pcm[offset] = byte(scaled)
	pcm[offset+1] = byte(scaled >> 8)
}
