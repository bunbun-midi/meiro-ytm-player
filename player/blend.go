package player

import (
	"io"
	"math"
	"sync/atomic"
)

// Blend is the second copy of the track. Rel is how many semitones it sits
// above the pitch slider, Fade is how far the horizontal fader sits toward
// that copy (0 is only the main voice, 1 is as far right as it goes), and
// Crossfade makes the fader trade one voice for the other. With Crossfade
// off, the main voice stays at full volume and the fader only adds the
// second one.
type Blend struct {
	Rel       float64
	Fade      float64
	Crossfade bool
}

// Compressor is the limiter after the two voices, the bass and the
// equaliser. Threshold, Knee and Makeup are dB. Ratio is the amount of
// reduction above the threshold, 1 for none. Attack and Release are
// milliseconds.
type Compressor struct {
	On        bool
	Threshold float64
	Ratio     float64
	Attack    float64
	Release   float64
	Knee      float64
	Makeup    float64
}

// Level is a snapshot of the sound after the limiter: the loudest recent
// sample on each channel, from 0 to 1, and 32 spectrum bands in the same
// range.
type Level struct {
	Left, Right float32
	Bands       [spectrumBands]float32
}

const spectrumBands = 32

// DefaultCompressor leaves a single track alone and pulls a sum back once
// it passes full scale.
func DefaultCompressor() Compressor {
	return Compressor{On: true, Threshold: 0, Ratio: 20, Attack: 5, Release: 50}
}

// The limiter's ranges. The playback panel's sliders use the same ones.
const (
	MinCompThreshold = -60.0
	MaxCompThreshold = 0.0
	MinCompRatio     = 1.0
	MaxCompRatio     = 20.0
	MinCompAttack    = 0.1
	MaxCompAttack    = 200.0
	MinCompRelease   = 5.0
	MaxCompRelease   = 2000.0
	MinCompKnee      = 0.0
	MaxCompKnee      = 24.0
	MinCompMakeup    = 0.0
	MaxCompMakeup    = 24.0
)

func normalizeBlend(b Blend) Blend {
	if math.IsNaN(b.Rel) {
		b.Rel = 12
	}
	b.Rel = min(max(b.Rel, MinPitch), MaxPitch)
	if math.IsNaN(b.Fade) || b.Fade < 0 {
		b.Fade = 0
	} else if b.Fade > 1 {
		b.Fade = 1
	}
	return b
}

func normalizeComp(c Compressor) Compressor {
	c.Threshold = clampFinite(c.Threshold, MinCompThreshold, MaxCompThreshold)
	c.Ratio = clampFinite(c.Ratio, MinCompRatio, MaxCompRatio)
	c.Attack = clampFinite(c.Attack, MinCompAttack, MaxCompAttack)
	c.Release = clampFinite(c.Release, MinCompRelease, MaxCompRelease)
	c.Knee = clampFinite(c.Knee, MinCompKnee, MaxCompKnee)
	c.Makeup = clampFinite(c.Makeup, MinCompMakeup, MaxCompMakeup)
	return c
}

func clampFinite(v, lo, hi float64) float64 {
	if math.IsNaN(v) {
		return lo
	}
	return min(max(v, lo), hi)
}

// SetBlend retunes the second voice on the streams already open. A fader
// move is a gain change, so it does not restart the decode or leave a gap.
func (p *Player) SetBlend(b Blend) {
	b = normalizeBlend(b)
	p.mu.Lock()
	defer p.mu.Unlock()
	p.blend = b
	p.publishRate()
}

// Blend returns the second voice most recently chosen.
func (p *Player) Blend() Blend {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.blend
}

// SetCompressor publishes the limiter. It takes effect on the next block.
func (p *Player) SetCompressor(c Compressor) {
	c = normalizeComp(c)
	p.mu.Lock()
	defer p.mu.Unlock()
	p.comp = c
	p.publishComp()
}

// Levels is the latest spectrum and stereo peak. The playback panel reads
// it while it draws.
func (p *Player) Levels() Level {
	if p.meters == nil {
		return Level{}
	}
	return p.meters.load()
}

// voiceGains is how loud each voice is at this fader position. Crossfade
// is equal-power, so the middle is not quieter than either end. Otherwise
// the main voice stays at 1 and the fader adds the second.
func voiceGains(fade float64, crossfade bool) (main, oct float64) {
	if fade < 0 {
		fade = 0
	} else if fade > 1 {
		fade = 1
	}
	if !crossfade {
		return 1, fade
	}
	return math.Cos(fade * math.Pi / 2), math.Sin(fade * math.Pi / 2)
}

// pcmFan lets two readers take the same bytes from one stream. Each keeps
// its own cursor. Bytes both have passed are dropped. One audio thread
// pulls both, so the fan has no lock.
type pcmFan struct {
	src     io.Reader
	buf     []byte
	base    int
	pos     [2]int
	err     error
	scratch [8192]byte
}

type fanBranch struct {
	fan *pcmFan
	id  int
}

func (f *pcmFan) branch(id int) io.Reader { return fanBranch{fan: f, id: id} }

func (b fanBranch) Read(p []byte) (int, error) { return b.fan.read(b.id, p) }

func (f *pcmFan) read(id int, p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	want := f.pos[id] + len(p)
	for f.base+len(f.buf) < want && f.err == nil {
		n, err := f.src.Read(f.scratch[:])
		if n > 0 {
			f.buf = append(f.buf, f.scratch[:n]...)
		}
		if err != nil {
			f.err = err
			break
		}
		if n == 0 {
			f.err = io.ErrNoProgress
			break
		}
	}
	have := f.base + len(f.buf)
	if f.pos[id] >= have {
		if f.err != nil {
			return 0, f.err
		}
		return 0, nil
	}
	end := want
	if end > have {
		end = have
	}
	n := copy(p, f.buf[f.pos[id]-f.base:end-f.base])
	f.pos[id] += n
	f.compact()
	if n < len(p) && f.err != nil {
		return n, f.err
	}
	return n, nil
}

func (f *pcmFan) compact() {
	low := f.pos[0]
	if f.pos[1] < low {
		low = f.pos[1]
	}
	drop := low - f.base
	if drop < len(f.scratch) {
		return
	}
	copy(f.buf, f.buf[drop:])
	f.buf = f.buf[:len(f.buf)-drop]
	f.base += drop
}

// voiceMix is the main pitch and the second one, locked to the same output
// frame. Both read one fan of the decoded PCM, and both run for the whole
// track: starting the second one later would miss the samples the first had
// already taken, and stopping it would let it drift. Silence is a gain of
// zero. While that gain is zero the second voice is held at 0 st, so its
// stretcher is not added to the sound the listener hears.
type voiceMix struct {
	main *rateReader
	oct  *rateReader
	live atomic.Pointer[blendLive]

	mainQ, octQ byteQueue
	scratch     [8192]byte
	mainG, octG float64
	ready       bool
	// limit pulls the sum back under full scale before it is stored as
	// 16-bit samples. A later limiter, after the equaliser, catches a
	// boost that would clip again.
	limit compState
}

type blendLive struct {
	fade      float64
	crossfade bool
}

func newVoiceMix(src io.Reader, speed, pitch float64, stretch Stretch, blend Blend) *voiceMix {
	blend = normalizeBlend(blend)
	fan := &pcmFan{src: src}
	main := newRateReader(fan.branch(0), newRateControl(speed, pitch))
	main.set(speed, pitch, stretch)
	rel := clampVoice(pitch + blend.Rel)
	oct := newRateReader(fan.branch(1), newRateControl(speed, rel))
	oct.set(speed, rel, stretch)
	m := &voiceMix{main: main, oct: oct}
	m.setBlend(blend)
	return m
}

func clampVoice(semitones float64) float64 {
	if math.IsNaN(semitones) {
		return 0
	}
	return min(max(semitones, -VoicePitch), VoicePitch)
}

// tune moves both voices. The second one sits Rel above the main pitch.
// Both keep the playback speed, so each output frame is the same moment
// of the recording.
func (m *voiceMix) tune(speed, pitch float64, stretch Stretch, rel float64) {
	if m == nil {
		return
	}
	m.main.set(speed, pitch, stretch)
	m.oct.set(speed, clampVoice(pitch+rel), stretch)
}

func (m *voiceMix) storeComp(cfg Compressor) {
	if m == nil {
		return
	}
	m.limit.store(cfg)
}

func (m *voiceMix) setBlend(b Blend) {
	if m == nil {
		return
	}
	b = normalizeBlend(b)
	m.live.Store(&blendLive{fade: b.Fade, crossfade: b.Crossfade})
}

func (m *voiceMix) Read(p []byte) (int, error) {
	if len(p) < frameBytes {
		return 0, nil
	}
	n := len(p) - len(p)%frameBytes
	_, wantOct := m.gains()
	// A silent second voice stays at the recording's pitch. Its samples are
	// multiplied by zero, and keeping it out of the stretcher means a
	// glitch there cannot reach the mix.
	m.oct.flatPitch = wantOct == 0 && m.octG < 1e-6
	errMain := m.mainQ.fill(m.main, n, m.scratch[:])
	errOct := m.octQ.fill(m.oct, n, m.scratch[:])
	have := min(m.mainQ.buffered(), m.octQ.buffered())
	have -= have % frameBytes
	if have == 0 {
		if errMain != nil {
			return 0, errMain
		}
		if errOct != nil {
			return 0, errOct
		}
		return 0, nil
	}
	main := m.mainQ.peek(have)
	oct := m.octQ.peek(have)
	m.mix(p[:have], main, oct)
	m.mainQ.drop(have)
	m.octQ.drop(have)
	if have < n {
		if errMain != nil {
			return have, errMain
		}
		if errOct != nil {
			return have, errOct
		}
	}
	return have, nil
}

// gains is how loud each voice should be for the fader position published
// most recently.
func (m *voiceMix) gains() (main, oct float64) {
	live := m.live.Load()
	fade, cross := 0.0, false
	if live != nil {
		fade, cross = live.fade, live.crossfade
	}
	return voiceGains(fade, cross)
}

func (m *voiceMix) mix(dst, main, oct []byte) {
	wantMain, wantOct := m.gains()
	if !m.ready {
		m.mainG, m.octG = wantMain, wantOct
		m.ready = true
	}
	frames := len(dst) / frameBytes
	for i := range frames {
		m.mainG += (wantMain - m.mainG) * pitchSlew
		m.octG += (wantOct - m.octG) * pitchSlew
		ml, mr := frameAt(main, i)
		ol, orr := frameAt(oct, i)
		left, right := m.limit.apply(ml*m.mainG+ol*m.octG, mr*m.mainG+orr*m.octG)
		putFrame(dst[i*frameBytes:], left, right)
	}
}

// byteQueue keeps output one voice has produced and the mixer has not
// taken yet, so the voice that emits first waits for the other.
type byteQueue struct {
	buf []byte
	off int
}

func (q *byteQueue) buffered() int { return len(q.buf) - q.off }

func (q *byteQueue) fill(r io.Reader, n int, scratch []byte) error {
	q.compact()
	for q.buffered() < n {
		need := n - q.buffered()
		if need > len(scratch) {
			need = len(scratch)
		}
		got, err := r.Read(scratch[:need])
		if got > 0 {
			q.buf = append(q.buf, scratch[:got]...)
		}
		if err != nil {
			return err
		}
		if got == 0 {
			return io.ErrNoProgress
		}
	}
	return nil
}

func (q *byteQueue) peek(n int) []byte { return q.buf[q.off : q.off+n] }

func (q *byteQueue) drop(n int) { q.off += n }

func (q *byteQueue) compact() {
	if q.off < 16384 {
		return
	}
	copy(q.buf, q.buf[q.off:])
	q.buf = q.buf[:len(q.buf)-q.off]
	q.off = 0
}

func frameAt(b []byte, frame int) (left, right float64) {
	off := frame * frameBytes
	l := int16(uint16(b[off]) | uint16(b[off+1])<<8)
	r := int16(uint16(b[off+2]) | uint16(b[off+3])<<8)
	return float64(l) / 32768, float64(r) / 32768
}

// compState is a stereo-linked feedforward compressor. The envelope
// follows the louder channel.
type compState struct {
	cfg             atomic.Pointer[Compressor]
	seen            *Compressor
	attack, release float64
	makeup          float64
	env             float64
}

func (c *compState) store(cfg Compressor) {
	cfg = normalizeComp(cfg)
	c.cfg.Store(&cfg)
}

func (c *compState) apply(left, right float64) (float64, float64) {
	cfg := c.cfg.Load()
	if cfg == nil || !cfg.On {
		return left, right
	}
	if cfg != c.seen {
		c.seen = cfg
		c.attack = math.Exp(-1 / (cfg.Attack / 1000 * sampleRate))
		c.release = math.Exp(-1 / (cfg.Release / 1000 * sampleRate))
		c.makeup = math.Pow(10, cfg.Makeup/20)
	}
	det := math.Max(math.Abs(left), math.Abs(right))
	coeff := c.release
	if det > c.env {
		coeff = c.attack
	}
	c.env = det + (c.env-det)*coeff
	gain := c.makeup * math.Pow(10, -gainReduction(c.env, cfg.Threshold, cfg.Ratio, cfg.Knee)/20)
	return left * gain, right * gain
}

// compReader limits the signal after the equaliser and feeds the spectrum
// and the level meter from what it let through.
type compReader struct {
	src    io.Reader
	state  compState
	meters *meters
}

func newCompReader(src io.Reader, cfg Compressor, meters *meters) *compReader {
	c := &compReader{src: src, meters: meters}
	c.store(cfg)
	return c
}

func (c *compReader) store(cfg Compressor) { c.state.store(cfg) }

func (c *compReader) Read(p []byte) (int, error) {
	n, err := c.src.Read(p)
	if n >= frameBytes {
		c.process(p[:n-n%frameBytes])
	}
	return n, err
}

func (c *compReader) process(p []byte) {
	frames := len(p) / frameBytes
	for i := range frames {
		left, right := frameAt(p, i)
		left, right = c.state.apply(left, right)
		putFrame(p[i*frameBytes:], left, right)
		if c.meters != nil {
			c.meters.sample(left, right)
		}
	}
}

// gainReduction is how many dB to take off a level of levelDB. A hard knee
// does nothing at or below the threshold. A soft knee eases in across Knee
// dB centred on the threshold.
func gainReduction(level, threshold, ratio, knee float64) float64 {
	if level <= 0 || ratio <= 1 {
		return 0
	}
	levelDB := 20 * math.Log10(level)
	slope := 1 - 1/ratio
	if knee <= 0 {
		if levelDB <= threshold {
			return 0
		}
		return (levelDB - threshold) * slope
	}
	below := threshold - knee/2
	if levelDB <= below {
		return 0
	}
	above := threshold + knee/2
	if levelDB >= above {
		return (levelDB - threshold) * slope
	}
	x := levelDB - below
	return slope * (x * x) / (2 * knee)
}

// meters keeps two snapshots so the audio thread can fill one while the
// panel reads the other. No block allocates.
type meters struct {
	slots  [2]Level
	cur    atomic.Uint32
	buf    [fftSize]float64
	spec   [fftSize]complex128
	fill   int
	blockL float64
	blockR float64
	heldL  float64
	heldR  float64
	held   [spectrumBands]float64
}

const fftSize = 256

func newMeters() *meters { return &meters{} }

func (m *meters) sample(left, right float64) {
	m.buf[m.fill] = (left + right) / 2
	m.fill++
	if a := math.Abs(left); a > m.blockL {
		m.blockL = a
	}
	if a := math.Abs(right); a > m.blockR {
		m.blockR = a
	}
	if m.fill < fftSize {
		return
	}
	m.fill = 0
	m.publish()
}

func (m *meters) publish() {
	m.heldL = followPeak(m.heldL, m.blockL)
	m.heldR = followPeak(m.heldR, m.blockR)
	m.blockL, m.blockR = 0, 0
	for i := range m.spec {
		w := 0.5 - 0.5*math.Cos(2*math.Pi*float64(i)/fftSize)
		m.spec[i] = complex(m.buf[i]*w, 0)
	}
	fft(m.spec[:], false)
	var bands [spectrumBands]float32
	// Bins 1 through 128, four bins to a band. A full-scale cosine lands
	// near N/2, which the division brings back to about 1.
	for band := range spectrumBands {
		var sum float64
		for k := range 4 {
			bin := 1 + band*4 + k
			re, im := real(m.spec[bin]), imag(m.spec[bin])
			sum += math.Hypot(re, im)
		}
		mag := sum / 4 / (fftSize / 2)
		if mag > m.held[band] {
			m.held[band] = mag
		} else {
			m.held[band] = m.held[band]*0.6 + mag*0.4
		}
		v := m.held[band]
		if v > 1 {
			v = 1
		}
		bands[band] = float32(v)
	}
	i := 1 - m.cur.Load()
	m.slots[i] = Level{Left: unit32(m.heldL), Right: unit32(m.heldR), Bands: bands}
	m.cur.Store(i)
}

func (m *meters) load() Level { return m.slots[m.cur.Load()] }

func followPeak(held, next float64) float64 {
	if next >= held {
		return next
	}
	return held*0.85 + next*0.15
}

func unit32(v float64) float32 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return float32(v)
}
