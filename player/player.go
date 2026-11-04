// Package player plays one audio stream at a time.
//
// It asks ffmpeg, in a child process, to decode a stream into signed 16-bit
// stereo PCM, and writes those samples to the system audio device. On
// Windows that device is a WASAPI shared stream, and the endpoint can be
// chosen. Elsewhere oto writes them. Decoding in a child process keeps the
// app free of cgo and of codecs of its own, and lets it play anything
// ffmpeg reads.
//
// The zero value is not usable; call New. All methods are safe for
// concurrent use, so the user interface can drive the player from the main
// thread while the output reads samples in the background.
package player

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ebitengine/oto/v3"
)

const (
	// sampleRate, channelCount and bytesPerSample are the PCM format the
	// player asks ffmpeg for and writes to the output. 48 kHz stereo is
	// what most YouTube Music audio decodes from.
	sampleRate     = 48000
	channelCount   = 2
	bytesPerSample = 2
	// bytesPerSecond is the size of one second of that PCM stream.
	bytesPerSecond = sampleRate * channelCount * bytesPerSample
	// userAgent is what ffmpeg sends when it opens the stream. YouTube's
	// media servers answer some requests without one with an error.
	userAgent       = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36"
	httpReadTimeout = 15 * time.Second
	// rangeSize is how many bytes of a remote stream one request asks for,
	// as YouTube's own player and yt-dlp ask.
	rangeSize = 10 << 20
)

// EQFrequencies are the centre frequencies of the equaliser's bands, in Hz.
// Ten ISO one-octave bands cover the audible range a music player is asked
// to shape.
var EQFrequencies = [...]float64{31, 62, 125, 250, 500, 1000, 2000, 4000, 8000, 16000}

// EQBandCount is how many gains SetEQ expects, one per EQFrequencies entry.
const EQBandCount = len(EQFrequencies)

// ErrNoFFmpeg reports that ffmpeg is not on PATH, which the player needs to
// decode a stream.
var ErrNoFFmpeg = errors.New("player: ffmpeg is not installed or not on PATH")

// Player plays one stream at a time.
type Player struct {
	// ctl serialises the calls that replace or end the stream: Play, Seek and
	// Stop. They do slow work (killing and starting ffmpeg, waiting on the
	// audio device), so they run under ctl alone and take mu only to swap the
	// session in or out. The readers the UI asks every frame take mu, and so
	// never wait on that work.
	ctl     sync.Mutex
	mu      sync.Mutex
	session *session
	volume  float64

	// eqBands holds one gain in dB per EQFrequencies entry, nil when the
	// equaliser is off. Guarded by ctl while sessions publish updated targets.
	eqBands []float64
	// bassOn and bassDB are the dynamic bass boost. Guarded by ctl.
	bassOn bool
	bassDB float64
	// speed is how fast streams play, 1 for their own pace, and pitch is
	// how far their key sits from the recording, in semitones. stretch is
	// which time-stretch the speed uses. Written under mu. The audio thread
	// reads the rate published on the session.
	speed   float64
	pitch   float64
	stretch Stretch
	// crossfade is how long the end of a track fades into the start of the
	// next, zero for none. Written under ctl.
	crossfade time.Duration
	// blend is the second voice, and comp the limiter after it. meters is
	// what the playback panel draws. The audio thread reads the copies
	// published onto the session.
	blend  Blend
	comp   Compressor
	meters *meters

	// next is the stream decoding ahead of the end of the current one, and
	// advanced is the tag of the one that took over without a pause, until
	// Advanced reports it.
	next     *session
	advanced string

	lookMu     sync.Mutex
	lookDone   bool
	ffmpegPath string
	lookErr    error
	// rangedHTTP reports whether that ffmpeg can fetch a stream in bounded
	// ranges. It is set with ffmpegPath.
	rangedHTTP bool

	audio *sharedAudio
}

// Option configures a Player.
type Option func(*Player)

// WithFFmpeg makes the player decode with the ffmpeg at path, instead of the
// one on PATH.
func WithFFmpeg(path string) Option {
	return func(p *Player) { p.ffmpegPath = path }
}

// withAudio supplies the audio context used by a Player. It is kept internal
// so tests can replace the operating-system output without exposing it as an
// application configuration surface.
func withAudio(audio *sharedAudio) Option {
	return func(p *Player) { p.audio = audio }
}

// New creates a player that is not yet playing anything. It does not touch
// the audio device or the file system.
func New(options ...Option) *Player {
	p := &Player{
		volume: 1, speed: 1, audio: processAudio,
		blend:  Blend{Rel: 12},
		comp:   DefaultCompressor(),
		meters: newMeters(),
	}
	for _, option := range options {
		option(p)
	}
	// Opening the device can take a long while; do it now, off the thread
	// that will later press play.
	go func() { _, _ = p.audio.context() }()
	return p
}

// Play starts url from its beginning, replacing anything playing. It returns
// once ffmpeg has started, not when the stream ends.
func (p *Player) Play(url string) error {
	return p.PlayContext(context.Background(), url)
}

// PlayContext starts url from its beginning, replacing anything playing. It
// returns once ffmpeg has started, not when the stream ends.
func (p *Player) PlayContext(ctx context.Context, url string) error {
	p.ctl.Lock()
	defer p.ctl.Unlock()
	return p.start(ctx, url, 0, false)
}

// PlayAt is Play from the position at, and held when paused. It is how a
// stream that broke off is picked up again where it stopped.
func (p *Player) PlayAt(url string, at time.Duration, paused bool) error {
	return p.PlayAtContext(context.Background(), url, at, paused)
}

// PlayAtContext is PlayContext from the position at, and held when paused.
func (p *Player) PlayAtContext(ctx context.Context, url string, at time.Duration, paused bool) error {
	p.ctl.Lock()
	defer p.ctl.Unlock()
	return p.start(ctx, url, max(at, 0), paused)
}

// Preload starts decoding url in the background, to follow the stream playing
// the moment that one ends, without the pause of opening it then. tag names
// what it is, as Advanced reports it. It replaces an earlier Preload, and a
// Play, PlayAt, Seek or Stop drops it, as the track it was for may no longer
// be the one that follows. Nothing is preloaded while no stream is loaded.
//
// Only a stream that ends cleanly hands over: one that failed, or whose
// preloaded successor failed to open, ends as it would have, and what follows
// is for the caller to decide.
func (p *Player) Preload(tag, url string) error {
	return p.PreloadContext(context.Background(), tag, url)
}

// PreloadContext is Preload with a caller context for ffmpeg discovery.
func (p *Player) PreloadContext(ctx context.Context, tag, url string) error {
	if strings.TrimSpace(url) == "" {
		return errors.New("player: stream URL is empty")
	}
	ffmpeg, err := p.resolveFFmpeg(ctx)
	if err != nil {
		return err
	}
	p.mu.Lock()
	loaded := p.session != nil
	p.mu.Unlock()
	if !loaded {
		return nil
	}
	// eqBands only changes under ctl, so snapshot it there; Preload itself
	// decodes without the lock, like the rest of the app's calls. Speed and
	// pitch are applied as the audio plays, so the decode is the recording's
	// own pace either way.
	p.ctl.Lock()
	eq, bassOn, bassDB := p.eqBands, p.bassOn, p.bassDB
	p.ctl.Unlock()
	p.mu.Lock()
	speed, pitch, stretch := p.speed, p.pitch, p.stretch
	p.mu.Unlock()
	next, err := p.decode(ffmpeg, url, 0, eq, bassOn, bassDB, speed, pitch, stretch)
	if err != nil {
		return err
	}
	next.tag = tag
	// A slider may have changed EQ, speed or pitch while this stream was
	// decoding. Publish the latest while ctl keeps a newer EQ change from
	// racing the handoff into p.next. The audio is at its own pace, so a
	// speed or pitch moved since still belongs to this stream.
	p.ctl.Lock()
	next.eq.Set(p.eqBands)
	next.bass.Set(p.bassOn, p.bassDB)
	next.fade.setHold(p.crossfade)
	p.mu.Lock()
	if p.session == nil {
		p.mu.Unlock()
		p.ctl.Unlock()
		next.discard()
		return nil
	}
	next.voices.tune(p.speed, p.pitch, p.stretch, p.blend.Rel)
	next.voices.setBlend(p.blend)
	next.voices.storeComp(p.comp)
	next.comp.store(p.comp)
	old := p.next
	p.next = next
	p.mu.Unlock()
	p.ctl.Unlock()
	if old != nil {
		old.discard()
	}
	return nil
}

// Preloaded returns the tag of the stream waiting to follow the current one,
// or an empty string when there is none.
func (p *Player) Preloaded() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.next == nil {
		return ""
	}
	return p.next.tag
}

// CancelPreload drops the preloaded stream, if any.
func (p *Player) CancelPreload() {
	p.mu.Lock()
	next := p.next
	p.next = nil
	p.mu.Unlock()
	if next != nil {
		next.discard()
	}
}

// Advanced returns the tag of the preloaded stream that has taken over from
// the one before it, once, and false when no handover has happened since it
// was last asked.
func (p *Player) Advanced() (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	tag := p.advanced
	p.advanced = ""
	return tag, tag != ""
}

// Resume starts a paused stream again, and does nothing when it plays or has
// ended.
func (p *Player) Resume() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if s := p.session; s != nil && s.paused {
		s.paused = false
		s.out.Play()
	}
}

// Pause holds the stream where it is, keeping a little of it decoded so a
// resume does not stutter.
func (p *Player) Pause() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if s := p.session; s != nil && !s.paused {
		s.paused = true
		s.out.Pause()
	}
}

// Toggle pauses a playing stream and resumes a paused one.
func (p *Player) Toggle() {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.session
	if s == nil {
		return
	}
	if s.paused {
		s.paused = false
		s.out.Play()
		return
	}
	s.paused = true
	s.out.Pause()
}

// Seek moves to at within the stream by restarting the decode there. It
// keeps the stream paused when it was paused.
func (p *Player) Seek(at time.Duration) {
	p.SeekContext(context.Background(), at)
}

// SeekContext is Seek with a caller context for ffmpeg discovery.
func (p *Player) SeekContext(ctx context.Context, at time.Duration) {
	p.ctl.Lock()
	defer p.ctl.Unlock()
	p.mu.Lock()
	s := p.session
	var url string
	var paused bool
	if s != nil {
		url, paused = s.url, s.paused
	}
	p.mu.Unlock()
	if s == nil || at < 0 {
		return
	}
	_ = p.start(ctx, url, at, paused)
}

// Stop ends the stream and forgets it.
func (p *Player) Stop() {
	p.ctl.Lock()
	defer p.ctl.Unlock()
	p.stop()
}

// SetVolume sets the playback gain, 1 for the stream's own level. Values
// below 0 are 0 and above 1 may clip.
func (p *Player) SetVolume(volume float64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.volume = max(volume, 0)
	if p.session != nil {
		p.session.out.SetVolume(p.volume)
	}
}

// SetEQ sets the equaliser's band gains, in dB, one per EQFrequencies entry.
// nil turns the equaliser off. A non-nil slice of another length is ignored.
// The audio reader applies a change to the current and preloaded streams
// without stopping either decode.
func (p *Player) SetEQ(gains []float64) {
	if gains != nil && len(gains) != EQBandCount {
		return
	}
	for _, g := range gains {
		if math.IsNaN(g) || math.IsInf(g, 0) {
			return
		}
	}
	p.ctl.Lock()
	defer p.ctl.Unlock()
	if equalGains(p.eqBands, gains) {
		return
	}
	p.eqBands = append([]float64(nil), gains...)
	p.mu.Lock()
	if p.session != nil {
		p.session.eq.Set(p.eqBands)
	}
	if p.next != nil {
		p.next.eq.Set(p.eqBands)
	}
	p.mu.Unlock()
}

// SetBass sets the dynamic bass boost. db is how many dB it may add, from
// 0 to MaxBassBoost. Off leaves the stream as it is. A change reaches the
// stream playing and the one preloaded without restarting either.
func (p *Player) SetBass(on bool, db float64) {
	if math.IsNaN(db) {
		return
	}
	db = min(max(db, 0), MaxBassBoost)
	p.ctl.Lock()
	defer p.ctl.Unlock()
	p.bassOn, p.bassDB = on, db
	p.mu.Lock()
	if p.session != nil && p.session.bass != nil {
		p.session.bass.Set(on, db)
	}
	if p.next != nil && p.next.bass != nil {
		p.next.bass.Set(on, db)
	}
	p.mu.Unlock()
}

// MaxCrossfade is the longest crossfade SetCrossfade takes. The app opens the
// next track twenty seconds ahead, and a fade can only start once it is open.
const MaxCrossfade = 12 * time.Second

// SetCrossfade sets how long the end of a track fades out while the start of
// the next fades in, and zero turns it off. It is clamped to MaxCrossfade. A
// fade only joins tracks that Preload has opened, and takes effect on the
// stream playing and the one preloaded without restarting either; the stream
// playing gives up the last part of its sound to the fade, so a change to a
// longer fade applies fully from the next track.
func (p *Player) SetCrossfade(d time.Duration) {
	d = min(max(d, 0), MaxCrossfade)
	p.ctl.Lock()
	defer p.ctl.Unlock()
	p.crossfade = d
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.session != nil {
		p.session.fade.setHold(d)
	}
	if p.next != nil {
		p.next.fade.setHold(d)
	}
}

// Crossfade returns the crossfade length, zero when there is none.
func (p *Player) Crossfade() time.Duration {
	p.ctl.Lock()
	defer p.ctl.Unlock()
	return p.crossfade
}

// equalGains reports whether two band tables hold the same gains.
func equalGains(a, b []float64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// MinSpeed and MaxSpeed are the ends of the speed slider's track. A drag
// past either end keeps that track's rate until MinDragSpeed or
// MaxDragSpeed. MaxPitch and MinPitch bound SetPitch, in semitones.
const (
	MinSpeed     = 0.05
	MaxSpeed     = 1.8
	MinDragSpeed = 0.01
	MaxDragSpeed = 4
	MinPitch     = -12.0
	MaxPitch     = 12.0
	// VoicePitch is as far as the second voice may sit from the recording,
	// in semitones. The pitch slider stays within MaxPitch. A second voice
	// an octave above a slider at +12 reaches +24.
	VoicePitch = 24.0
)

// SetSpeed sets how fast streams play, 1 for their own pace, keeping their
// pitch. A speed outside MinDragSpeed and MaxDragSpeed is ignored. The stream
// playing goes on from where it is, without a gap, and stays paused when it
// was paused. Positions stay in the stream's own time. Once ctx is done it
// does nothing and returns ctx's error, so a request that was superseded
// cannot undo the one that superseded it.
func (p *Player) SetSpeed(ctx context.Context, speed float64) error {
	if math.IsNaN(speed) || speed < MinDragSpeed || speed > MaxDragSpeed {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.storePlayback(speed, p.pitch)
	return nil
}

// SetPitch sets how far streams sit from the key they were recorded in, in
// semitones, 0 for that key, keeping their speed. A pitch outside MinPitch
// and MaxPitch is ignored. The stream playing goes on from where it is, as
// SetSpeed does.
func (p *Player) SetPitch(ctx context.Context, semitones float64) error {
	if math.IsNaN(semitones) || semitones < MinPitch || semitones > MaxPitch {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.storePlayback(p.speed, semitones)
	return nil
}

// SetPlayback sets the speed and the pitch together. A value outside its
// range ignores the call. The stream playing takes both as it plays, without
// restarting.
func (p *Player) SetPlayback(ctx context.Context, speed, semitones float64) error {
	if math.IsNaN(speed) || speed < MinDragSpeed || speed > MaxDragSpeed || math.IsNaN(semitones) || semitones < MinPitch || semitones > MaxPitch {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.storePlayback(speed, semitones)
	return nil
}

// SetStretch chooses the time-stretch the speed slider uses. The stream
// playing takes it on the next grain, without restarting.
func (p *Player) SetStretch(stretch Stretch) {
	stretch = knownStretch(stretch)
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stretch = stretch
	p.publishRate()
}

// Stretch returns the time-stretch the speed slider uses.
func (p *Player) Stretch() Stretch {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.stretch
}

// storePlayback publishes speed and pitch to the streams already open.
// The caller holds mu.
func (p *Player) storePlayback(speed, semitones float64) {
	p.speed, p.pitch = speed, semitones
	p.publishRate()
}

// publishRate hands the current speed, pitch and stretch to the streams
// already open, and retunes the second voice to match. The caller holds mu.
func (p *Player) publishRate() {
	for _, s := range []*session{p.session, p.next} {
		if s == nil {
			continue
		}
		if s.voices != nil {
			s.voices.tune(p.speed, p.pitch, p.stretch, p.blend.Rel)
			s.voices.setBlend(p.blend)
			continue
		}
		if s.rate != nil {
			s.rate.set(p.speed, p.pitch, p.stretch)
		}
	}
}

// publishComp hands the limiter to the streams already open. The caller holds mu.
func (p *Player) publishComp() {
	for _, s := range []*session{p.session, p.next} {
		if s == nil {
			continue
		}
		if s.voices != nil {
			s.voices.storeComp(p.comp)
		}
		if s.comp != nil {
			s.comp.store(p.comp)
		}
	}
}

// Speed returns how fast streams play, 1 by default.
func (p *Player) Speed() float64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.speed
}

// Pitch returns how far streams sit from their recorded key, in semitones.
func (p *Player) Pitch() float64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.pitch
}

// Volume returns the current gain, 1 by default.
func (p *Player) Volume() float64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.volume
}

// Active reports whether a stream is loaded, playing, paused or ended.
func (p *Player) Active() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.session != nil
}

// Playing reports whether the stream is playing now.
func (p *Player) Playing() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.session
	if s == nil || s.paused || p.endedLocked(s) {
		return false
	}
	// The lock is held; Err on oto objects does not take it.
	return s.out == nil || s.out.Err() == nil
}

// Buffering reports whether the stream is playing but has not yet given any
// sound: ffmpeg is still opening it, or catching up after a seek.
func (p *Player) Buffering() bool {
	p.mu.Lock()
	s := p.session
	paused := s != nil && s.paused
	p.mu.Unlock()
	if s == nil || paused || s.source.read.Load() > 0 {
		return false
	}
	select {
	case <-s.done:
		// Stopped without a sample: Failure says why, unless the reader has
		// yet to take what ffmpeg wrote before it exited.
		return p.pendingSamples(s)
	default:
		return true
	}
}

// Paused reports whether the stream is loaded and held.
func (p *Player) Paused() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.session != nil && p.session.paused
}

// Ended reports whether the stream has been played to its end.
func (p *Player) Ended() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.session != nil && p.endedLocked(p.session)
}

// Position returns how far playback has come, which leads where the user
// hears by the audio device's buffer, a few tens of milliseconds.
func (p *Player) Position() time.Duration {
	p.mu.Lock()
	s := p.session
	var at time.Duration
	if s != nil {
		at = s.played()
	}
	p.mu.Unlock()
	return at
}

// played is how far into the track this session has come, from where its
// decode began. The samples handed to the device map back to source time,
// and the ones still sitting in the device's buffer have not been heard, so
// they are taken off. A speed chosen halfway through does not move the time
// already played. The caller holds p.mu.
func (s *session) played() time.Duration {
	if s.rate == nil || s.out == nil || s.source == nil {
		return s.offset
	}
	heard := max(s.source.read.Load()-int64(s.out.BufferedSize()), 0) / frameBytes
	return s.offset + time.Duration(s.rate.sourceAt(heard)/float64(sampleRate)*float64(time.Second))
}

// Failure returns what ffmpeg reported when the decode stopped because of an
// error, as an unreachable stream, and an empty string when the stream ended
// or the app stopped it.
func (p *Player) Failure() string {
	p.mu.Lock()
	s := p.session
	stopped := s != nil && s.stopped
	p.mu.Unlock()
	if s == nil || stopped {
		return ""
	}
	if err := p.audioFailure(s); err != nil {
		return "audio device: " + err.Error()
	}
	select {
	case <-s.done:
	default:
		return ""
	}
	message := strings.TrimSpace(s.stderr.String())
	// ffmpeg decodes faster than the track plays, so it exits cleanly a little
	// before the end. A clean exit has no errors on stderr; -xerror makes
	// decode and read failures nonzero, and the stderr check also preserves an
	// error from older ffmpeg builds that exit successfully after one.
	p.mu.Lock()
	finished := s.waitErr == nil && message == "" && s.source.read.Load() > 0
	p.mu.Unlock()
	if finished || p.pendingSamples(s) {
		return ""
	}
	if message == "" {
		message = "ffmpeg stopped"
	}
	return message
}

// StreamFailed reports whether the last decode stopped because the stream
// could not be read, as a connection YouTube's server reset, rather than
// because the audio device stopped working or the stream ended.
func (p *Player) StreamFailed() bool {
	p.mu.Lock()
	s := p.session
	stopped := s != nil && s.stopped
	p.mu.Unlock()
	if s == nil || stopped {
		return false
	}
	if err := p.audioFailure(s); err != nil {
		return false // the sound, not the stream, is what stopped
	}
	select {
	case <-s.done:
	default:
		return false // the decode is still running
	}
	return strings.TrimSpace(s.stderr.String()) != ""
}

// endOfStream says whether a stream has been played to its end. A paused
// stream also finds its device player not playing while the samples it holds
// drain or sit, which is not the track's end: only an unpaused stream with
// the device idle and its data read out says that.
func endOfStream(read int64, eof, devicePlaying, paused bool) bool {
	return read > 0 && eof && !devicePlaying && !paused
}

func (p *Player) endedLocked(s *session) bool {
	return endOfStream(s.source.read.Load(), s.source.eof.Load(), s.out.IsPlaying(), s.paused)
}

// start replaces the current stream with url, decoded from at. The caller
// holds ctl and not mu.
func (p *Player) start(ctx context.Context, url string, at time.Duration, paused bool) error {
	if strings.TrimSpace(url) == "" {
		return errors.New("player: stream URL is empty")
	}
	ffmpeg, err := p.resolveFFmpeg(ctx)
	if err != nil {
		return err
	}
	audioCtx, err := p.audio.context()
	if err != nil {
		return err
	}
	p.stop()

	p.mu.Lock()
	speed, pitch, stretch := p.speed, p.pitch, p.stretch
	p.mu.Unlock()
	s, err := p.decode(ffmpeg, url, at, p.eqBands, p.bassOn, p.bassDB, speed, pitch, stretch)
	if err != nil {
		return err
	}
	s.paused = paused
	s.fade.setHold(p.crossfade)
	s.out = audioCtx.newPlayer(s.source)
	p.mu.Lock()
	// A slider may have moved while ffmpeg was opening. The reader has not
	// started, so this is the rate the first sample plays at.
	s.voices.tune(p.speed, p.pitch, p.stretch, p.blend.Rel)
	s.voices.setBlend(p.blend)
	s.voices.storeComp(p.comp)
	s.comp.store(p.comp)
	s.out.SetVolume(p.volume)
	p.session = s
	p.mu.Unlock()
	if !paused {
		s.out.Play()
	}
	return nil
}

// decode starts ffmpeg on url from at, and the goroutine that reaps it. The
// session's reader stretches and shifts the PCM as it reaches the output.
// The session has no output yet: start gives it one, and a handoff takes over
// the one before it.
func (p *Player) decode(ffmpeg, url string, at time.Duration, eq []float64, bassOn bool, bassDB, speed, pitch float64, stretch Stretch) (*session, error) {
	cmd := exec.Command(ffmpeg, streamArgs(url, at, p.rangedHTTP)...) //nolint:noctx // the player ends ffmpeg itself when the stream is replaced or stopped
	hideWindow(cmd)
	pipe, writer, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("player: create audio pipe: %w", err)
	}
	cmd.Stdout = writer
	stderr := &boundedBuffer{limit: 4096}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		_ = pipe.Close()
		_ = writer.Close()
		return nil, fmt.Errorf("player: start ffmpeg: %w", err)
	}
	_ = writer.Close()
	s := &session{
		url: url, cmd: cmd, pipe: pipe, stderr: stderr,
		offset: at, done: make(chan struct{}), quit: make(chan struct{}), eq: newEqualizer(eq),
		bass: newBassBoost(bassOn, bassDB),
	}
	p.mu.Lock()
	blend, comp := p.blend, p.comp
	p.mu.Unlock()
	// Two readers share the pipe through a fan, so a second pitch stays on
	// the same source samples as the main voice. The main reader remains
	// the position clock: its output frames are the ones the device plays.
	s.voices = newVoiceMix(pipe, speed, pitch, stretch, blend)
	s.voices.storeComp(comp)
	s.rate = s.voices.main
	s.comp = newCompReader(newEQReader(newBassReader(s.voices, s.bass), s.eq), comp, p.meters)
	s.fade = newFadeReader(s.comp, func() *session { return p.upcoming(s) })
	s.source = newCountingReader(s.fade, func() io.Reader { return p.handoff(s) })
	go p.reap(s)
	return s, nil
}

// streamArgs is the ffmpeg command line that decodes url from at into signed
// 16-bit stereo PCM on stdout, at the recording's own pace and key. Speed,
// pitch and the equaliser run as live layers after decode. ranged is whether
// ffmpeg can fetch a remote stream in bounded ranges.
func streamArgs(url string, at time.Duration, ranged bool) []string {
	args := []string{"-hide_banner", "-loglevel", "error", "-nostdin", "-xerror"}
	if isHTTP(url) {
		// A remote stream, or a manifest it names, cannot reach local files.
		args = append(args,
			"-protocol_whitelist", "http,https,httpproxy,tcp,tls,crypto",
			"-reconnect", "1", "-reconnect_streamed", "1", "-reconnect_delay_max", "5",
			"-rw_timeout", strconv.FormatInt(httpReadTimeout.Microseconds(), 10),
			"-user_agent", userAgent,
		)
		if ranged {
			// YouTube serves one request for a whole long file, such as a
			// podcast episode, at about twice the speed it plays, which a
			// slow moment of the network turns into a stall. Bounded ranges
			// come at full speed.
			args = append(args, "-request_size", strconv.Itoa(rangeSize))
		}
	}
	if at > 0 {
		args = append(args, "-ss", strconv.FormatFloat(at.Seconds(), 'f', 3, 64))
	}
	args = append(args, "-i", url)
	args = append(args,
		"-vn", "-f", "s16le", "-acodec", "pcm_s16le",
		"-ac", strconv.Itoa(channelCount), "-ar", strconv.Itoa(sampleRate),
	)
	return append(args, "pipe:1")
}

// isHTTP reports whether rawURL is read over HTTP, the only protocol that
// defines the reconnect and user-agent options streamArgs adds. A cached
// track is a plain path, and ffmpeg refuses an input given an option its
// protocol does not define, so it must not receive them. Add a scheme here to
// give it the same options.
func isHTTP(rawURL string) bool {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http", "https":
		return true
	default:
		return false
	}
}

// reap observes process completion independently from the audio reader, so a
// paused session cannot prevent an ffmpeg exit from being noticed. The pipe is
// ours rather than Wait's, so the samples still in it survive the exit; reap
// closes it once the reader has taken them all, or the app has stopped.
func (p *Player) reap(s *session) {
	waitErr := s.cmd.Wait()
	p.mu.Lock()
	s.waitErr = waitErr
	p.mu.Unlock()
	close(s.done)
	select {
	case <-s.source.finished:
	case <-s.quit:
	}
	_ = s.pipe.Close()
}

// handoff is what the output reads on when the stream from ends: the preloaded
// stream, now the current one, in the same output, so that the sound goes on
// with no restart of it. It returns nil when there is nothing to hand over to,
// or from did not end cleanly.
func (p *Player) handoff(from *session) io.Reader {
	select {
	case <-from.done:
	case <-from.quit:
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	next := p.upcomingLocked(from)
	// A stream that was faded into must be the one that takes over; another
	// preloaded since would start over from its beginning.
	if next == nil || from.fade.mixing && from.fade.follower != nil && from.fade.follower != next {
		return nil
	}
	next.out, next.paused = from.out, from.paused
	p.session, p.next, p.advanced = next, nil, next.tag
	return next.source
}

// upcoming is the preloaded stream that will follow from when it has ended
// cleanly, or nil. The fade reads the start of it before the handover.
func (p *Player) upcoming(from *session) *session {
	select {
	case <-from.done:
	case <-from.quit:
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.upcomingLocked(from)
}

// upcomingLocked is upcoming for a caller that holds p.mu, and has waited for
// from to finish.
func (p *Player) upcomingLocked(from *session) *session {
	next := p.next
	if p.session != from || next == nil || from.waitErr != nil || from.stderr.String() != "" {
		return nil
	}
	select {
	case <-next.done:
		if next.waitErr != nil || next.stderr.String() != "" {
			p.next = nil
			next.discard()
			return nil
		}
	default:
	}
	return next
}

// pendingSamples reports whether ffmpeg exited cleanly without the reader
// having taken anything yet, so its output may still sit in the pipe.
func (p *Player) pendingSamples(s *session) bool {
	p.mu.Lock()
	clean := s.waitErr == nil
	p.mu.Unlock()
	if !clean || s.stderr.String() != "" || s.source.read.Load() > 0 {
		return false
	}
	select {
	case <-s.source.finished:
		return false
	default:
		return true
	}
}

// stop ends the current stream, if any. The caller holds ctl and not mu: the
// session is detached under mu, and the slow teardown runs outside it.
func (p *Player) stop() {
	p.mu.Lock()
	s, next := p.session, p.next
	p.session, p.next, p.advanced = nil, nil, ""
	if s != nil {
		s.stopped = true
	}
	p.mu.Unlock()
	if next != nil {
		next.discard()
	}
	if s == nil {
		return
	}
	close(s.quit)
	// ffmpeg goes first, and the pipe with it. The output may be blocked
	// reading a stream that has stalled, and PauseAndStopReading waits for
	// that read, on the thread of whoever pressed stop.
	if s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
	}
	_ = s.pipe.Close()
	s.out.PauseAndStopReading()
}

// discard ends a decode that never reached the output.
func (s *session) discard() {
	close(s.quit)
	if s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
	}
	_ = s.pipe.Close()
}

func (p *Player) resolveFFmpeg(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	p.lookMu.Lock()
	defer p.lookMu.Unlock()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if p.lookDone {
		if p.lookErr != nil {
			return "", ErrNoFFmpeg
		}
		return p.ffmpegPath, nil
	}
	path := p.ffmpegPath
	if path == "" {
		var err error
		path, err = exec.LookPath("ffmpeg")
		if err != nil {
			p.lookErr = err
			p.lookDone = true
			return "", ErrNoFFmpeg
		}
	}
	ranged, err := supportsRanges(ctx, path)
	if err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	p.ffmpegPath = path
	p.rangedHTTP = ranged
	p.lookDone = true
	return p.ffmpegPath, nil
}

// supportsRanges reports whether the ffmpeg at path has the HTTP option
// request_size, which older builds lack. Those refuse a command line
// that names it, so it is only passed to an ffmpeg that lists it.
func supportsRanges(ctx context.Context, path string) (bool, error) {
	parentCtx := ctx
	ctx, cancel := context.WithTimeout(parentCtx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "-hide_banner", "-h", "protocol=http")
	hideWindow(cmd)
	help, err := cmd.Output()
	if err := parentCtx.Err(); err != nil {
		return false, err
	}
	return err == nil && bytes.Contains(help, []byte("-request_size")), nil
}

// output is the part of an oto player a session drives.
type output interface {
	Play()
	Pause()
	SetVolume(float64)
	BufferedSize() int
	IsPlaying() bool
	PauseAndStopReading()
	Err() error
}

// device is the process-wide audio context.
type device interface {
	newPlayer(io.Reader) output
	Err() error
}

type otoDevice struct{ ctx *oto.Context }

func (d otoDevice) newPlayer(r io.Reader) output { return d.ctx.NewPlayer(r) }
func (d otoDevice) Err() error                   { return d.ctx.Err() }

// sharedAudio opens one playback device for the process and remembers the
// outcome. A later UseOutputDevice retargets that device when the platform
// can. Oto allows one context per process and cannot retry a failed open, so
// a failure is kept rather than hidden behind a second attempt that would
// only report "context is already created".
type sharedAudio struct {
	open    func() (device, error)
	once    sync.Once
	started atomic.Bool
	dev     device
	err     error
}

// processAudio is the context every Player shares.
var processAudio = &sharedAudio{open: openOutput}

func openOto() (device, error) {
	ctx, ready, err := oto.NewContext(&oto.NewContextOptions{
		SampleRate:      sampleRate,
		ChannelCount:    channelCount,
		Format:          oto.FormatSignedInt16LE,
		ApplicationName: "Meiro",
	})
	if err != nil {
		return nil, err
	}
	<-ready
	return otoDevice{ctx}, nil
}

// context opens the audio device on first use, and reports why it could not.
func (a *sharedAudio) context() (device, error) {
	a.once.Do(func() {
		// Mark the open as started before calling open, so a concurrent
		// UseOutputDevice either stores its endpoint in time for this open
		// to read it, or sees the open and retargets the device afterwards.
		a.started.Store(true)
		dev, err := a.open()
		if err != nil {
			a.err = fmt.Errorf("player: open the audio device: %w", err)
			return
		}
		a.dev = dev
	})
	return a.dev, a.err
}

// opened reports whether context has begun opening the device.
func (a *sharedAudio) opened() bool { return a.started.Load() }

// audioFailure returns why the device or the session's output stopped working,
// or nil. The caller must not hold p.mu.
func (p *Player) audioFailure(s *session) error {
	if s == nil || s.out == nil {
		return nil
	}
	if err := s.out.Err(); err != nil {
		return err
	}
	if dev, err := p.audio.context(); err == nil && dev != nil {
		return dev.Err()
	}
	return nil
}

// session is one decode: a child ffmpeg, the pipe it writes PCM to, and the
// oto player reading that pipe.
type session struct {
	url string
	// tag names a preloaded stream to the caller that asked for it.
	tag    string
	cmd    *exec.Cmd
	pipe   io.ReadCloser
	stderr *boundedBuffer
	source *countingReader
	eq     *equalizer
	bass   *bassBoost
	fade   *fadeReader
	out    output
	done   chan struct{}
	// quit is closed when the app stops the decode.
	quit chan struct{}

	// offset is where in the track this decode began. rate stretches and
	// shifts the decoded audio as it plays.
	offset time.Duration
	rate   *rateReader
	// voices mixes the main pitch with the second one. comp limits the sum.
	voices *voiceMix
	comp   *compReader
	paused bool
	// stopped is set when the app, not ffmpeg, ended the decode.
	stopped bool
	waitErr error
}

// countingReader counts the bytes oto has taken from the pipe, and remembers
// the end of the stream.
type countingReader struct {
	reader io.Reader
	read   atomic.Int64
	eof    atomic.Bool
	// finished is closed when a read fails or reaches the end.
	finished chan struct{}
	once     sync.Once
	// follow is asked for the stream to carry on with when this one ends,
	// and nil means none. Once one is given, reads go on to it.
	follow func() io.Reader
	onward io.Reader
}

func newCountingReader(reader io.Reader, follow func() io.Reader) *countingReader {
	return &countingReader{reader: reader, finished: make(chan struct{}), follow: follow}
}

func (c *countingReader) Read(p []byte) (int, error) {
	if c.onward != nil {
		return c.onward.Read(p)
	}
	n, err := c.reader.Read(p)
	c.read.Add(int64(n))
	if err != nil {
		if errors.Is(err, io.EOF) {
			c.eof.Store(true)
		}
		c.once.Do(func() { close(c.finished) })
		if n == 0 && errors.Is(err, io.EOF) && c.follow != nil {
			if onward := c.follow(); onward != nil {
				c.onward = onward
				return c.onward.Read(p)
			}
		}
	}
	return n, err
}

// boundedBuffer keeps the first limit bytes written to it, which is enough
// of ffmpeg's error output to explain a failed decode.
type boundedBuffer struct {
	mu    sync.Mutex
	buf   []byte
	limit int
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if room := b.limit - len(b.buf); room > 0 {
		b.buf = append(b.buf, p[:min(room, len(p))]...)
	}
	return len(p), nil
}

func (b *boundedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}
