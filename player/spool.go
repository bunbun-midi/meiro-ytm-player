package player

import (
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// errJumped means a seek moved the decoded audio under a read that was
// already in progress. The reader drops what it buffered and reads again.
var errJumped = errors.New("player: seek")

const (
	// seekSnapFrames is how far a cut may move to land on a zero crossing,
	// about 20 ms at 48 kHz.
	seekSnapFrames = sampleRate / 50
	// seekFadeFrames is the short fade Settings offers instead of a hard cut,
	// about 10 ms.
	seekFadeFrames = sampleRate / 100
	// peakFrames is how many PCM frames one waveform column summarises.
	peakFrames = 256
)

// WavePeak is the loudest and quietest sample in one slice of the waveform.
type WavePeak struct {
	Min, Max int16
}

// Waveform is the decoded shape of the stream playing now. Start is the
// file time of Peaks[0]. End is as far as the decode has written.
type Waveform struct {
	Peaks      []WavePeak
	Start, End time.Duration
}

// pcmSpool copies a decode into a temporary file so a seek inside that audio
// moves a cursor instead of starting ffmpeg again. The writer goroutine fills
// the file as fast as the decoder produces samples. Playback reads it at its
// own pace.
type pcmSpool struct {
	mu       sync.Mutex
	cond     *sync.Cond
	file     *os.File
	name     string
	written  int64
	read     int64
	epoch    atomic.Uint64
	closed   bool
	fail     error
	peaks    []wavePeak
	bucket   int
	copyDone chan struct{}
}

type wavePeak struct {
	min, max int16
}

func openSpool() (*pcmSpool, error) {
	f, err := os.CreateTemp("", "meiro-pcm-*.raw")
	if err != nil {
		return nil, fmt.Errorf("player: audio buffer: %w", err)
	}
	s := &pcmSpool{file: f, name: f.Name(), copyDone: make(chan struct{})}
	s.cond = sync.NewCond(&s.mu)
	return s, nil
}

// copy drains r into the file until r ends. It closes copyDone when it
// returns, which is what remove waits for.
func (s *pcmSpool) copy(r io.Reader) {
	defer close(s.copyDone)
	buf := make([]byte, 32*1024)
	var pending []byte
	for {
		n, err := r.Read(buf)
		if n > 0 {
			pending = append(pending, buf[:n]...)
			whole := len(pending) - len(pending)%frameBytes
			if whole > 0 {
				if !s.publish(pending[:whole]) {
					return
				}
				pending = append([]byte(nil), pending[whole:]...)
			}
		}
		if err != nil {
			s.finish(err)
			return
		}
	}
}

func (s *pcmSpool) publish(b []byte) bool {
	s.mu.Lock()
	f := s.file
	off := s.written
	s.mu.Unlock()
	if f == nil {
		return false
	}
	if _, err := f.WriteAt(b, off); err != nil {
		s.finish(err)
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.file == nil || off != s.written {
		return s.file != nil
	}
	s.addPeaks(b)
	s.written += int64(len(b))
	s.cond.Broadcast()
	return true
}

func (s *pcmSpool) addPeaks(b []byte) {
	frames := len(b) / frameBytes
	off := 0
	for frames > 0 {
		if s.bucket == 0 || s.bucket == peakFrames {
			s.peaks = append(s.peaks, wavePeak{min: 32767, max: -32768})
			s.bucket = 0
		}
		room := peakFrames - s.bucket
		n := frames
		if n > room {
			n = room
		}
		pk := &s.peaks[len(s.peaks)-1]
		for i := range n {
			l, r := frameSamples(b[(off+i)*frameBytes:])
			if l < pk.min {
				pk.min = l
			}
			if l > pk.max {
				pk.max = l
			}
			if r < pk.min {
				pk.min = r
			}
			if r > pk.max {
				pk.max = r
			}
		}
		s.bucket += n
		off += n
		frames -= n
	}
}

func frameSamples(b []byte) (left, right int16) {
	left = int16(uint16(b[0]) | uint16(b[1])<<8)
	right = int16(uint16(b[2]) | uint16(b[3])<<8)
	return left, right
}

func (s *pcmSpool) finish(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	if err != nil && !errors.Is(err, io.EOF) && s.fail == nil {
		s.fail = err
	}
	s.cond.Broadcast()
}

// shutdown unblocks a reader waiting for samples that will not come.
func (s *pcmSpool) shutdown() {
	s.mu.Lock()
	s.closed = true
	s.cond.Broadcast()
	s.mu.Unlock()
}

// remove waits for the copy to finish, then deletes the file.
func (s *pcmSpool) remove() {
	if s == nil {
		return
	}
	<-s.copyDone
	s.mu.Lock()
	f := s.file
	s.file = nil
	name := s.name
	s.mu.Unlock()
	if f != nil {
		_ = f.Close()
		_ = os.Remove(name)
	}
}

// Epoch is the seek generation. It changes when Jump moves the cursor.
func (s *pcmSpool) Epoch() uint64 { return s.epoch.Load() }

// Read returns the next decoded bytes. A seek that lands while this read
// waits, or while it copies, comes back as errJumped and no bytes.
func (s *pcmSpool) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	s.mu.Lock()
	epoch := s.epoch.Load()
	for {
		if s.epoch.Load() != epoch {
			s.mu.Unlock()
			return 0, errJumped
		}
		if s.read < s.written {
			break
		}
		if s.closed {
			err := s.fail
			s.mu.Unlock()
			if err == nil {
				err = io.EOF
			}
			return 0, err
		}
		s.cond.Wait()
	}
	off := s.read
	n := len(p)
	if int64(n) > s.written-s.read {
		n = int(s.written - s.read)
	}
	n -= n % frameBytes
	if n <= 0 {
		s.mu.Unlock()
		return 0, io.ErrNoProgress
	}
	s.read += int64(n)
	f := s.file
	s.mu.Unlock()
	if f == nil {
		return 0, io.EOF
	}
	if _, err := f.ReadAt(p[:n], off); err != nil {
		return 0, err
	}
	if s.epoch.Load() != epoch {
		return 0, errJumped
	}
	return n, nil
}

// Jump moves the next read to frame, which is counted from the start of this
// spool. It reports false when that frame has not been written yet.
func (s *pcmSpool) Jump(frame int64) bool {
	if s == nil || frame < 0 {
		return false
	}
	off := frame * frameBytes
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.file == nil || off > s.written {
		return false
	}
	s.read = off
	s.epoch.Add(1)
	s.cond.Broadcast()
	return true
}

// Zero is the zero crossing nearest frame, within about 20 ms, among the
// samples already written. The frame is counted from the start of this spool.
func (s *pcmSpool) Zero(frame int64) (int64, bool) {
	if s == nil || frame < 0 {
		return 0, false
	}
	s.mu.Lock()
	written := s.written / frameBytes
	f := s.file
	s.mu.Unlock()
	if f == nil || written <= 0 || frame >= written {
		return 0, false
	}
	lo := frame - seekSnapFrames
	if lo < 0 {
		lo = 0
	}
	hi := frame + seekSnapFrames
	if hi > written {
		hi = written
	}
	if hi <= lo {
		return frame, true
	}
	buf := make([]byte, (hi-lo)*frameBytes)
	if _, err := f.ReadAt(buf, lo*frameBytes); err != nil {
		return 0, false
	}
	at := nearestZero(buf, int(frame-lo), int(hi-lo))
	return lo + int64(at), true
}

func (s *pcmSpool) snapshot() ([]WavePeak, int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]WavePeak, len(s.peaks))
	for i, pk := range s.peaks {
		out[i] = WavePeak{Min: pk.min, Max: pk.max}
	}
	return out, s.written / frameBytes
}

// nearestZero is the frame in pcm closest to target whose neighbours change
// sign, or the quietest frame when the window never crosses zero. pcm is
// interleaved stereo s16le. window is how far, in frames, the search may look.
func nearestZero(pcm []byte, target, window int) int {
	frames := len(pcm) / frameBytes
	if frames == 0 {
		return 0
	}
	if target < 0 {
		target = 0
	}
	if target >= frames {
		target = frames - 1
	}
	lo := target - window
	if lo < 0 {
		lo = 0
	}
	hi := target + window
	if hi > frames {
		hi = frames
	}
	best := target
	bestD := window + 1
	found := false
	prev := sampleSum(pcm, lo)
	if prev == 0 {
		best, bestD, found = lo, absInt(lo-target), true
	}
	for i := lo + 1; i < hi; i++ {
		cur := sampleSum(pcm, i)
		if cur == 0 || (prev < 0 && cur > 0) || (prev > 0 && cur < 0) {
			cand := i
			if cur != 0 && absInt(i-1-target) <= absInt(i-target) {
				cand = i - 1
			}
			if d := absInt(cand - target); d < bestD {
				best, bestD, found = cand, d, true
			}
		}
		prev = cur
	}
	if found {
		return best
	}
	quiet := int(^uint(0) >> 1)
	for i := lo; i < hi; i++ {
		if e := sampleEnergy(pcm, i); e < quiet {
			quiet = e
			best = i
		}
	}
	return best
}

func sampleSum(pcm []byte, frame int) int32 {
	off := frame * frameBytes
	l, r := frameSamples(pcm[off:])
	return int32(l) + int32(r)
}

func sampleEnergy(pcm []byte, frame int) int {
	off := frame * frameBytes
	l, r := frameSamples(pcm[off:])
	if l < 0 {
		l = -l
	}
	if r < 0 {
		r = -r
	}
	return int(l) + int(r)
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

func fadeOutPCM(pcm []byte) {
	frames := len(pcm) / frameBytes
	if frames <= 1 {
		return
	}
	for i := range frames {
		gain := float64(frames-1-i) / float64(frames-1)
		scaleFrame(pcm[i*frameBytes:], gain)
	}
}

// fadeInPCM raises the first total frames of pcm from silence to full level.
func fadeInPCM(pcm []byte, total int) {
	frames := len(pcm) / frameBytes
	if total <= 1 || frames == 0 {
		return
	}
	n := frames
	if n > total {
		n = total
	}
	for i := range n {
		scaleFrame(pcm[i*frameBytes:], float64(i+1)/float64(total))
	}
}

func scaleFrame(pcm []byte, gain float64) {
	if len(pcm) < frameBytes || gain == 1 {
		return
	}
	for c := range channelCount {
		off := c * bytesPerSample
		s := int16(uint16(pcm[off]) | uint16(pcm[off+1])<<8)
		v := int32(math.Round(float64(s) * gain))
		if v > 32767 {
			v = 32767
		} else if v < -32768 {
			v = -32768
		}
		pcm[off] = byte(uint16(int16(v)))
		pcm[off+1] = byte(uint16(int16(v)) >> 8)
	}
}

func durationFrames(d time.Duration) int64 {
	if d <= 0 {
		return 0
	}
	return int64(d.Seconds() * sampleRate)
}

func framesDuration(frames float64) time.Duration {
	if frames <= 0 {
		return 0
	}
	return time.Duration(frames * float64(time.Second) / sampleRate)
}
