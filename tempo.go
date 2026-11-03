package main

import (
	"context"
	"encoding/binary"
	"errors"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/elianiva/meiro/internal/bpm"
	"github.com/elianiva/meiro/internal/proc"
)

// beatState is the tempo of the track it was read from. done is set when a
// tempo was found, or when the audio was heard and had no steady beat. A
// read that failed before any audio arrived leaves done clear, so the next
// time the track starts it is tried again.
type beatState struct {
	id   string
	grid bpm.Grid
	ok   bool
	done bool
}

// How much of a track is read to find its tempo, from the start, so the
// phase lines up with the beginning of the audio. A minute covers an intro
// and a steady stretch after it.
const (
	tempoSeconds = 90
	tempoRate    = 22050
	tempoTimeout = 2 * time.Minute
)

// setBPMDetection turns tempo analysis on or off and reads the track that
// is already playing when it comes on.
func (a *app) setBPMDetection(on bool) {
	a.settings.BPMDetection = on
	a.saveSettings()
	a.beatTask.stop()
	a.beats = beatState{}
	if on {
		a.detectTempo(a.current.VideoID, a.playbackURL)
	}
}

// detectTempo reads the tempo of id from its audio. Episodes are left
// alone, and a track already analysed is not read again.
func (a *app) detectTempo(id, rawURL string) {
	if !a.settings.BPMDetection || id == "" || rawURL == "" || a.current.IsEpisode() {
		return
	}
	if a.beats.id == id && a.beats.done {
		return
	}
	startTask(a, &a.beatTask, tempoTimeout, func(ctx context.Context) (bpm.Grid, error) {
		return readTempo(ctx, rawURL)
	}, func(grid bpm.Grid, err error) {
		if a.current.VideoID != id {
			return
		}
		// A read that never got any audio can be tried again. A track that
		// was heard and has no beat stays without one until detection is
		// turned off and on.
		if err != nil {
			if errors.Is(err, errNoBeat) {
				log.Printf("tempo: video_id=%s: no steady beat", id)
				a.beats = beatState{id: id, done: true}
			}
			return
		}
		log.Printf("tempo: video_id=%s bpm=%.1f", id, grid.BPM)
		a.beats = beatState{id: id, grid: grid, ok: true, done: true}
		a.applyTargetBPM()
	})
}

// currentGrid is the tempo of the track playing, when detection is on and
// the analysis belongs to that track.
func (a *app) currentGrid() (bpm.Grid, bool) {
	if !a.settings.BPMDetection || a.current.IsEpisode() || a.beats.id != a.current.VideoID || !a.beats.ok {
		return bpm.Grid{}, false
	}
	return a.beats.grid, true
}

// jumpMeasures moves playback by n measures of the current tempo. n may be
// negative or a fraction. The end of the track, when it is known, is as far
// as a jump goes.
func (a *app) jumpMeasures(n float64) {
	grid, ok := a.currentGrid()
	if !ok {
		return
	}
	next := grid.Jump(a.playbackPosition(), n, a.settings.MeasureSnap)
	if a.total > 0 && next > a.total {
		next = a.total
	}
	a.seek(next)
}

func readTempo(ctx context.Context, rawURL string) (bpm.Grid, error) {
	ffmpeg, err := proc.Tool("ffmpeg")
	if err != nil {
		return bpm.Grid{}, err
	}
	out, readErr := readTempoPCM(ctx, ffmpeg, rawURL)
	if len(out) < tempoRate*2*8 {
		if readErr != nil {
			return bpm.Grid{}, readErr
		}
		return bpm.Grid{}, errNoBeat
	}
	pcm := make([]int16, len(out)/2)
	for i := range pcm {
		pcm[i] = int16(binary.LittleEndian.Uint16(out[i*2:]))
	}
	grid, ok := bpm.Detect(pcm, tempoRate)
	if !ok {
		return bpm.Grid{}, errNoBeat
	}
	return grid, nil
}

// readTempoPCM decodes the opening of the track twice if the first read
// comes back short. Whatever audio did arrive is kept: a stream that dies
// after half a minute still has a tempo in it.
func readTempoPCM(ctx context.Context, ffmpeg, rawURL string) ([]byte, error) {
	var best []byte
	var last error
	for attempt := 0; attempt < 2; attempt++ {
		if err := ctx.Err(); err != nil {
			return best, err
		}
		buf, err := proc.Command(ctx, ffmpeg, tempoArgs(rawURL)...).Output()
		if len(buf) > len(best) {
			best = buf
		}
		last = err
		if err == nil && len(buf) >= tempoRate*2*8 {
			return buf, nil
		}
	}
	return best, last
}

// errNoBeat is a tempo that could not be read. The audio address is left
// out of it, because that address can carry a signature.
var errNoBeat = errors.New("tempo: no steady beat")

func tempoArgs(rawURL string) []string {
	args := []string{"-hide_banner", "-loglevel", "error", "-nostdin"}
	if strings.HasPrefix(rawURL, "http://") || strings.HasPrefix(rawURL, "https://") {
		args = append(args,
			"-protocol_whitelist", "http,https,httpproxy,tcp,tls,crypto",
			"-reconnect", "1", "-reconnect_streamed", "1", "-reconnect_delay_max", "2",
			"-rw_timeout", strconv.FormatInt((20*time.Second).Microseconds(), 10),
			"-user_agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36",
		)
	}
	args = append(args,
		"-t", strconv.Itoa(tempoSeconds),
		"-i", rawURL,
		"-vn", "-ac", "1", "-ar", "22050",
		"-f", "s16le", "pipe:1",
	)
	return args
}
