package player

import (
	"errors"
	"io"
	"math"
	"sync/atomic"
)

const (
	// rateWindow and rateHopOut are the overlap-add grain. Half-overlapped
	// Hann windows sum to one, so a speed of 1 leaves the samples as they
	// were once the first hop has played. The hop stays shorter than the
	// window at MaxSpeed, so the next grain still overlaps this one.
	// rateSearch is how far a grain may slide to line up with the one before
	// it. Without that, a faster or slower hop meets the previous grain out
	// of phase and the two cancel.
	rateWindow = 2048
	rateHopOut = 1024
	rateSearch = 256

	frameBytes = channelCount * bytesPerSample

	// pitchSlew is how far the pitch shifter moves toward a new key on each
	// sample, the same step the browser worklet uses, so a slider does not
	// click.
	pitchSlew = 0.002
)

// hannWindow sums to one with a hop of rateHopOut.
var hannWindow = func() [rateWindow]float64 {
	var w [rateWindow]float64
	for i := range w {
		w[i] = 0.5 * (1 - math.Cos(2*math.Pi*float64(i)/rateWindow))
	}
	return w
}()

// rateConfig is one speed, pitch and stretch the audio thread should play.
// The control thread publishes a new one; it never mutates a published value.
type rateConfig struct {
	speed   float64
	pitch   float64
	stretch Stretch
}

// rateControl is shared by the audio thread and whoever moves a slider.
type rateControl struct {
	target atomic.Pointer[rateConfig]
}

func newRateControl(speed, pitch float64) *rateControl {
	c := &rateControl{}
	c.set(speed, pitch)
	return c
}

func (c *rateControl) set(speed, pitch float64) {
	stretch := StretchBungee
	if cur := c.target.Load(); cur != nil {
		stretch = cur.stretch
	}
	c.store(speed, pitch, stretch)
}

// setStretch publishes the time-stretch algorithm, keeping the speed and pitch.
func (c *rateControl) setStretch(stretch Stretch) {
	speed, pitch, _ := c.get()
	c.store(speed, pitch, stretch)
}

func (c *rateControl) store(speed, pitch float64, stretch Stretch) {
	c.target.Store(&rateConfig{speed: speed, pitch: pitch, stretch: knownStretch(stretch)})
}

func (c *rateControl) get() (speed, pitch float64, stretch Stretch) {
	cfg := c.target.Load()
	if cfg == nil {
		return 1, 0, StretchBungee
	}
	speed, pitch, stretch = cfg.speed, cfg.pitch, cfg.stretch
	if !(speed > 0) || math.IsNaN(speed) {
		speed = 1
	}
	if math.IsNaN(pitch) {
		pitch = 0
	}
	return speed, pitch, knownStretch(stretch)
}

// rateSpan maps an output frame to the source frame it represents. Later
// output frames follow at speed source frames each, until the next span.
type rateSpan struct {
	out   int64
	src   float64
	speed float64
}

type rateSpans struct {
	items []rateSpan
}

// rateReader plays decoded PCM at a speed and pitch that can change while
// it plays. ffmpeg stays at the recording's own pace; stretching and shifting
// happen on the samples oto is about to take, so a slider does not open a
// gap or restart the decode.
//
// Speed is a time stretch. Bungee is the default; a phase vocoder and an
// overlap-add of waveform grains are the other two. The next grain starts
// further along the source when playback is faster. Pitch resamples that
// stretch. To play higher, the stretch runs longer and the resampler reads
// it faster, which brings the length back; lower is the other way around.
// One output frame is one input frame of the resampler, so the pitch change
// does not play a second, delayed copy. At 1× and 0 st the samples are
// copied through: the stretch is not transparent, so it stays out of the
// sound until the speed or the pitch actually leaves that.
type rateReader struct {
	src io.Reader
	ctl *rateControl

	frames  [][2]float64
	origin  int64
	partial []byte
	eof     bool
	// inputEnd is the source length in frames once the input has ended, or
	// -1 before that. The position of the track stops there.
	inputEnd atomic.Int64
	fail     error

	ola       [][2]float64
	olaOrigin int64
	olaEnd    int64

	inPos   float64
	prevIn  float64
	grainAt int64
	emitted int64
	// stretch is how fast the overlap-add is consuming the source for the
	// grain it just placed, in source frames per overlap-add frame. ratio
	// is how many of those frames one output frame uses. Their product is
	// the playback speed, so changing the pitch does not change the pace.
	stretch float64
	ratio   float64
	olaPos  float64

	draining bool
	spans    atomic.Pointer[rateSpans]
	algo     Stretch

	// Phase vocoder state. The audio thread owns it.
	pvSpec   []complex128
	pvPrev   [2][]float64
	pvSynth  [2][]float64
	pvHave   bool
	pvCopied bool
	pvHa     float64
	pvMag    []float64
	pvPhase  []float64
	pvPeaks  []int

	// b is the Bungee Basic stretcher. The audio thread owns it.
	b bungee

	// copying is the straight copy used at 1× and 0 st. copyPos is the next
	// source frame to hand on. flatPitch forces this voice to 0 st, which
	// is how a silent second voice stays out of the stretcher. The audio
	// thread owns both.
	copying   bool
	copyPos   int64
	flatPitch bool

	// landed is the seek generation whose audio this reader is playing.
	// The audio thread writes it when a seek has been applied. played
	// waits for it so a position does not add the old timeline to the new one.
	landed atomic.Uint64
}

func newRateReader(src io.Reader, ctl *rateControl) *rateReader {
	if ctl == nil {
		ctl = newRateControl(1, 0)
	}
	r := &rateReader{src: src, ctl: ctl}
	r.inputEnd.Store(-1)
	speed, pitch, _ := ctl.get()
	r.ratio = pitchRatio(pitch)
	r.stretch = clampSpeed(speed) / r.ratio
	return r
}

func (r *rateReader) set(speed, pitch float64, stretch Stretch) {
	r.ctl.store(speed, pitch, stretch)
}

// sourceAt is the source frame heard at output frame outFrame. outFrame is
// counted in the frames this reader has handed on, which is where the device
// is once its own buffer is subtracted. Safe to call while Read runs.
func (r *rateReader) sourceAt(outFrame int64) float64 {
	if outFrame <= 0 {
		return 0
	}
	cur := r.spans.Load()
	if cur == nil || len(cur.items) == 0 {
		return 0
	}
	items := cur.items
	i := len(items) - 1
	for i > 0 && items[i].out > outFrame {
		i--
	}
	s := items[i]
	src := s.src + float64(outFrame-s.out)*s.speed
	if src < 0 {
		src = 0
	}
	if end := r.inputEnd.Load(); end >= 0 && src > float64(end) {
		src = float64(end)
	}
	return src
}

func (r *rateReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if len(p) < frameBytes {
		return 0, io.ErrShortBuffer
	}
	frames := len(p) / frameBytes
	n := 0
	for n < frames {
		speed, pitch, _ := r.heard()
		if unityPlayback(speed, pitch) {
			if !r.copying {
				r.enterCopy()
			}
			if err := r.copyOne(p[n*frameBytes:]); err != nil {
				if errors.Is(err, errJumped) {
					return 0, err
				}
				if n > 0 {
					return n * frameBytes, nil
				}
				return 0, err
			}
		} else {
			if r.copying {
				r.leaveCopy()
			}
			if err := r.ensure(); err != nil {
				if errors.Is(err, errJumped) {
					return 0, err
				}
				if n > 0 {
					return n * frameBytes, nil
				}
				return 0, err
			}
			r.emit(p[n*frameBytes:])
		}
		n++
	}
	return n * frameBytes, nil
}

// heard is the speed and pitch this voice should play now. A silent second
// voice reports 0 st so it copies, or stretches without a second pitch.
func (r *rateReader) heard() (speed, pitch float64, stretch Stretch) {
	speed, pitch, stretch = r.ctl.get()
	if r.flatPitch {
		pitch = 0
	}
	return speed, pitch, stretch
}

// unityPlayback reports a speed and pitch the ear cannot tell from the
// recording, which is the case the straight copy handles.
func unityPlayback(speed, pitch float64) bool {
	return math.Abs(speed-1) < 1e-4 && math.Abs(pitch) < 1e-4
}

// enterCopy resumes at the source frame the listener is on.
func (r *rateReader) enterCopy() {
	r.copying = true
	r.ratio = 1
	r.stretch = 1
	if r.emitted == 0 {
		r.copyPos = 0
		return
	}
	src := r.inPos
	if at := r.sourceAt(r.emitted); at > 0 {
		src = at
	}
	r.copyPos = int64(math.Round(src))
	if r.copyPos < r.origin {
		r.copyPos = r.origin
	}
}

// leaveCopy starts the stretcher at the frame the copy had reached, so
// moving the speed or the pitch does not jump back to the start.
func (r *rateReader) leaveCopy() {
	pos := float64(r.copyPos)
	r.copying = false
	r.inPos = pos
	r.prevIn = pos
	r.grainAt = r.emitted
	r.olaPos = float64(r.emitted)
	r.ola = nil
	r.olaOrigin = r.emitted
	r.olaEnd = r.emitted
	r.b.reset()
	r.b.pos = pos
	r.pvHave = false
	r.pvCopied = false
	r.pvHa = 0
	r.draining = false
}

// resetJump forgets every sample this reader has buffered, so the next read
// is the audio a seek just landed on. The control's speed and pitch stay.
func (r *rateReader) resetJump() {
	r.frames = nil
	r.origin = 0
	r.partial = nil
	r.eof = false
	r.inputEnd.Store(-1)
	r.fail = nil
	r.ola = nil
	r.olaOrigin = 0
	r.olaEnd = 0
	r.inPos = 0
	r.prevIn = 0
	r.grainAt = 0
	r.emitted = 0
	r.olaPos = 0
	r.draining = false
	r.copying = false
	r.copyPos = 0
	r.spans.Store(nil)
	r.b.reset()
	r.pvHave = false
	r.pvCopied = false
	r.pvHa = 0
	speed, pitch, _ := r.heard()
	r.ratio = pitchRatio(pitch)
	if r.ratio == 0 {
		r.ratio = 1
	}
	r.stretch = clampSpeed(speed) / r.ratio
}

// copyOne writes the next source frame unchanged.
func (r *rateReader) copyOne(dst []byte) error {
	if err := r.fill(r.copyPos + 1); err != nil {
		return err
	}
	rel := int(r.copyPos - r.origin)
	if rel < 0 || rel >= len(r.frames) {
		if r.eof {
			return io.EOF
		}
		return io.ErrNoProgress
	}
	sample := r.frames[rel]
	putFrame(dst, sample[0], sample[1])
	r.noteSpan(r.emitted, float64(r.copyPos), 1)
	r.copyPos++
	r.emitted++
	r.inPos = float64(r.copyPos)
	// Keep a window behind the play head so leaving the copy can stretch
	// from here instead of from silence.
	r.dropInput(bungeeN/2 + 8)
	return nil
}

// ensure has the next output sample's overlap-add frames ready, or reports
// that the stream has ended.
func (r *rateReader) ensure() error {
	for {
		if r.pastEnd() {
			if r.fail != nil {
				return r.fail
			}
			return io.EOF
		}
		if r.sampleReady() {
			return nil
		}
		if r.draining {
			if r.fail != nil {
				return r.fail
			}
			return io.EOF
		}
		if err := r.placeGrain(); err != nil {
			return err
		}
	}
}

// pastEnd reports that the source has been heard through to its end. The
// last grain's fade continues past that, and playing it makes the track long.
func (r *rateReader) pastEnd() bool {
	if r.inputEnd.Load() < 0 || r.spans.Load() == nil {
		return false
	}
	return r.emitted >= r.finishAt(math.MaxInt64)
}

// sampleReady reports that the overlap-add buffer holds the frame the
// resampler is about to read. The next grain is mixed in before the read
// cursor reaches it, so until the input ends the readable frames stop at
// the next grain.
func (r *rateReader) sampleReady() bool {
	idx := int64(math.Floor(r.olaPos))
	if idx < r.olaOrigin || idx >= r.olaOrigin+int64(len(r.ola)) {
		return false
	}
	if r.draining {
		return idx < r.olaEnd
	}
	return idx < r.grainAt
}

// placeGrain mixes one more hop into the overlap-add buffer. The hop is
// placed before its samples are handed on, so the overlap with the grain
// before it is already mixed in.
func (r *rateReader) placeGrain() error {
	speed, pitch, algo := r.heard()
	speed = clampSpeed(speed)
	target := pitchRatio(pitch)
	if r.emitted == 0 || r.ratio == 0 {
		r.ratio = target
	}
	// A higher key stretches the source more slowly, so the resampler can
	// read the result faster and land back on the same length.
	r.stretch = speed / r.ratio
	if algo != r.algo {
		r.algo = algo
		r.pvHave = false
		r.pvCopied = false
		r.pvHa = 0
		r.b.reset()
	}
	switch algo {
	case StretchOverlapAdd:
		return r.placeOverlap()
	case StretchPhaseVocoder:
		return r.placeVocoder()
	default:
		return r.placeBungee()
	}
}

func (r *rateReader) placeOverlap() error {
	hopIn := float64(rateHopOut) * r.stretch
	need := int64(math.Floor(r.inPos)) + rateSearch + rateWindow + 2
	ok, err := r.readyInput(need)
	if !ok {
		return err
	}
	// inPos is where this grain would start to keep the speed exact. It may
	// slide a few milliseconds so it adds to the grain already playing
	// instead of against it. The next grain is still measured from inPos,
	// so the slide does not change the pace.
	chosen := r.inPos
	if r.grainAt > 0 {
		chosen = r.align(r.inPos)
	}
	r.addGrain(chosen, r.grainAt)
	r.prevIn = chosen
	r.inPos += hopIn
	r.grainAt += rateHopOut
	r.dropInput(rateSearch + rateWindow)
	return nil
}

// readyInput reports whether the source holds the samples the next frame
// needs. A false ok with a nil error means the stream is draining.
func (r *rateReader) readyInput(need int64) (bool, error) {
	if err := r.fill(need); err != nil {
		r.fail = err
		r.draining = true
		return false, nil
	}
	available := r.origin + int64(len(r.frames))
	if r.eof && r.inPos >= float64(available) {
		r.draining = true
		return false, nil
	}
	if !r.eof && available < need {
		return false, io.ErrNoProgress
	}
	return true, nil
}

func (r *rateReader) fill(until int64) error {
	for !r.eof && r.origin+int64(len(r.frames)) < until {
		var buf [8 * 1024]byte
		n, err := r.src.Read(buf[:])
		if errors.Is(err, errJumped) {
			return errJumped
		}
		if n > 0 {
			r.appendPCM(buf[:n])
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				r.markEnd()
				return nil
			}
			r.markEnd()
			return err
		}
		if n == 0 {
			return io.ErrNoProgress
		}
	}
	return nil
}

func (r *rateReader) markEnd() {
	r.eof = true
	if r.inputEnd.Load() < 0 {
		r.inputEnd.Store(r.origin + int64(len(r.frames)))
	}
}

func (r *rateReader) appendPCM(b []byte) {
	if len(r.partial) > 0 {
		combined := make([]byte, 0, len(r.partial)+len(b))
		combined = append(combined, r.partial...)
		combined = append(combined, b...)
		r.partial = r.partial[:0]
		b = combined
	}
	frames := len(b) / frameBytes
	for i := range frames {
		off := i * frameBytes
		left := int16(uint16(b[off]) | uint16(b[off+1])<<8)
		right := int16(uint16(b[off+2]) | uint16(b[off+3])<<8)
		r.frames = append(r.frames, [2]float64{float64(left) / 32768, float64(right) / 32768})
	}
	if rem := b[frames*frameBytes:]; len(rem) > 0 {
		r.partial = append(r.partial[:0], rem...)
	}
}

func (r *rateReader) sampleAt(pos float64) (float64, float64) {
	rel := pos - float64(r.origin)
	if len(r.frames) == 0 || rel < 0 {
		return 0, 0
	}
	i := int(rel)
	if i >= len(r.frames) {
		return 0, 0
	}
	s0 := r.frames[i]
	frac := rel - float64(i)
	if frac == 0 || i+1 >= len(r.frames) {
		return s0[0], s0[1]
	}
	s1 := r.frames[i+1]
	return s0[0]*(1-frac) + s1[0]*frac, s0[1]*(1-frac) + s1[1]*frac
}

func (r *rateReader) addGrain(inPos float64, outPos int64) {
	r.ensureOLA(outPos, rateWindow)
	base := int(outPos - r.olaOrigin)
	for i := range rateWindow {
		w := hannWindow[i]
		left, right := r.sampleAt(inPos + float64(i))
		r.ola[base+i][0] += left * w
		r.ola[base+i][1] += right * w
	}
	if end := outPos + rateWindow; end > r.olaEnd {
		r.olaEnd = end
	}
}

func (r *rateReader) ensureOLA(from int64, n int) {
	end := from + int64(n)
	have := r.olaOrigin + int64(len(r.ola))
	if end > have {
		r.ola = append(r.ola, make([][2]float64, int(end-have))...)
	}
}

func (r *rateReader) noteSpan(out int64, src, speed float64) {
	cur := r.spans.Load()
	var prev []rateSpan
	if cur != nil {
		prev = cur.items
	}
	if len(prev) > 0 && math.Float64bits(prev[len(prev)-1].speed) == math.Float64bits(speed) {
		return
	}
	// The device and a crossfade can still be playing output from a few
	// seconds ago. Spans older than that are never asked for again.
	keepFrom := r.emitted - int64(sampleRate)*14
	start := 0
	if keepFrom > 0 {
		for start+1 < len(prev) && prev[start+1].out < keepFrom {
			start++
		}
	}
	items := make([]rateSpan, len(prev)-start+1)
	copy(items, prev[start:])
	items[len(items)-1] = rateSpan{out: out, src: src, speed: speed}
	r.spans.Store(&rateSpans{items: items})
}

// align slides nominal so the new grain resembles the overlap left by the
// previous one. A small penalty keeps a grain from wandering off when
// several alignments match, which is what leaves a speed of 1 identical to
// the source.
func (r *rateReader) align(nominal float64) float64 {
	template := r.prevIn + float64(rateHopOut)
	best := 0
	bestScore := -2.0
	consider := func(offset int) {
		score := r.correlate(template, nominal+float64(offset))
		score -= 0.15 * math.Abs(float64(offset)) / rateSearch
		if score > bestScore {
			bestScore = score
			best = offset
		}
	}
	for offset := -rateSearch; offset <= rateSearch; offset += 4 {
		consider(offset)
	}
	for offset := best - 4; offset <= best+4; offset++ {
		if offset < -rateSearch || offset > rateSearch {
			continue
		}
		consider(offset)
	}
	return nominal + float64(best)
}

func (r *rateReader) correlate(a, b float64) float64 {
	const step = 8
	var dot, ea, eb float64
	for i := 0; i < rateHopOut; i += step {
		a0, a1 := r.sampleAt(a + float64(i))
		b0, b1 := r.sampleAt(b + float64(i))
		dot += a0*b0 + a1*b1
		ea += a0*a0 + a1*a1
		eb += b0*b0 + b1*b1
	}
	if ea < 1e-8 || eb < 1e-8 {
		return 0
	}
	return dot / math.Sqrt(ea*eb)
}

// finishAt stops the output where the source runs out. Past that, the last
// grain is only its fade, and playing it makes every track a little long.
func (r *rateReader) finishAt(ready int64) int64 {
	end := r.inputEnd.Load()
	if end < 0 {
		return ready
	}
	cur := r.spans.Load()
	if cur == nil || len(cur.items) == 0 {
		return ready
	}
	s := cur.items[len(cur.items)-1]
	if s.speed <= 0 {
		return ready
	}
	remain := float64(end) - s.src
	var limit int64
	if remain <= 0 {
		limit = s.out
	} else {
		limit = s.out + int64(math.Ceil(remain/s.speed))
	}
	if limit < ready {
		return limit
	}
	return ready
}

func (r *rateReader) dropInput(keepBehind int) {
	// The next grain still needs the samples behind inPos, including room
	// for the overlap-add search.
	keep := int64(math.Floor(r.inPos)) - int64(keepBehind)
	drop := int(keep - r.origin)
	if drop <= 0 || drop >= len(r.frames) {
		return
	}
	copy(r.frames, r.frames[drop:])
	r.frames = r.frames[:len(r.frames)-drop]
	r.origin += int64(drop)
}

func (r *rateReader) dropOutput() {
	// Keep the sample under the read cursor, and the one before it, so a
	// fractional read can still blend them. Also keep the next grain's start:
	// it is mixed in after this read, and dropping it makes that mix land
	// before the buffer. Drop a hop at a time so this is not a copy on every
	// sample.
	keep := int64(math.Floor(r.olaPos)) - 1
	if keep > r.grainAt {
		keep = r.grainAt
	}
	drop := int(keep - r.olaOrigin)
	if drop < pvHop || drop >= len(r.ola) {
		return
	}
	copy(r.ola, r.ola[drop:])
	r.ola = r.ola[:len(r.ola)-drop]
	r.olaOrigin += int64(drop)
}

func (r *rateReader) emit(dst []byte) {
	speed, pitch, _ := r.heard()
	speed = clampSpeed(speed)
	target := pitchRatio(pitch)
	if r.emitted == 0 || r.ratio == 0 {
		r.ratio = target
	} else {
		r.ratio += (target - r.ratio) * pitchSlew
	}
	r.track(speed)
	left, right := r.olaAt(r.olaPos)
	r.olaPos += r.ratio
	r.emitted++
	r.dropOutput()
	putFrame(dst, left, right)
}

// track records where the source is for the output frame about to be
// handed on, when the pace changed. Later frames follow at that pace.
func (r *rateReader) track(speed float64) {
	src := r.inPos + (r.olaPos-float64(r.grainAt))*r.stretch
	if src < 0 {
		src = 0
	}
	r.noteSpan(r.emitted, src, speed)
}

// olaAt is the overlap-add sample at pos, blending the two frames it falls
// between. An exact frame is that frame, which is what keeps a speed of 1
// and a pitch of 0 identical to the stretch.
func (r *rateReader) olaAt(pos float64) (float64, float64) {
	if len(r.ola) == 0 {
		return 0, 0
	}
	rel := pos - float64(r.olaOrigin)
	i := int(math.Floor(rel))
	if i < 0 || i >= len(r.ola) {
		return 0, 0
	}
	s0 := r.ola[i]
	frac := rel - float64(i)
	if frac == 0 || i+1 >= len(r.ola) {
		return s0[0], s0[1]
	}
	s1 := r.ola[i+1]
	return s0[0]*(1-frac) + s1[0]*frac, s0[1]*(1-frac) + s1[1]*frac
}

func clampSpeed(speed float64) float64 {
	if speed < MinDragSpeed {
		return MinDragSpeed
	}
	if speed > MaxDragSpeed {
		return MaxDragSpeed
	}
	return speed
}

// pitchRatio is how much faster the resampler reads to raise the key by
// semitones. 0 stays at 1, so the resampler copies samples across. The
// slider stays within MaxPitch; a second voice may reach VoicePitch.
func pitchRatio(semitones float64) float64 {
	if semitones > VoicePitch {
		semitones = VoicePitch
	} else if semitones < -VoicePitch {
		semitones = -VoicePitch
	}
	if semitones == 0 {
		return 1
	}
	return math.Pow(2, semitones/12)
}

func putFrame(dst []byte, left, right float64) {
	l := floatToInt16(left)
	rr := floatToInt16(right)
	dst[0] = byte(l)
	dst[1] = byte(uint16(l) >> 8)
	dst[2] = byte(rr)
	dst[3] = byte(uint16(rr) >> 8)
}

func floatToInt16(v float64) int16 {
	scaled := math.Round(v * 32768)
	switch {
	case scaled > 32767:
		return 32767
	case scaled < -32768:
		return -32768
	default:
		return int16(scaled)
	}
}
