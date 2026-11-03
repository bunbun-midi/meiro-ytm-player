// Package offline keeps the music the user downloads to play without a
// network: the audio files, and an index of the songs, albums and playlists
// they belong to.
package offline

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/elianiva/meiro/internal/audio"
	"github.com/elianiva/meiro/internal/fsutil"
	"github.com/elianiva/meiro/internal/proc"
	"github.com/elianiva/meiro/youtube"
)

// indexName is the file in the downloads folder that lists what it holds.
const indexName = "downloads.json"

// Library is what has been downloaded. The newest songs and collections come
// first. Its methods are for one goroutine at a time.
type Library struct {
	Songs       []Song       `json:"songs"`
	Collections []Collection `json:"collections"`

	// index is the place of each song in Songs, by video ID.
	index map[string]int
}

// Song is a downloaded track and the file that holds its audio.
type Song struct {
	Item youtube.MusicItem `json:"item"`
	// File is the name of the audio file in the downloads folder.
	File string `json:"file"`
	// Single says the user downloaded the song by itself, so removing an
	// album or a playlist that holds it keeps it.
	Single bool `json:"single,omitempty"`
}

// Collection is a downloaded album or playlist: the item that opens its page,
// and the video IDs of its tracks, in order.
type Collection struct {
	Item   youtube.MusicItem `json:"item"`
	Tracks []string          `json:"tracks"`
}

// ID is the browse ID of the collection's page.
func (c Collection) ID() string { return c.Item.Target().ID }

// Read reads the index of the downloads folder dir. A missing or unreadable
// index is an empty library. It also removes what downloads an earlier run
// stopped left behind.
func Read(dir string) Library {
	proc.SweepStale(dir, nil, ".audio-")
	var lib Library
	data, err := os.ReadFile(filepath.Join(dir, indexName))
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		log.Printf("reading downloads: %v", err)
		return lib
	default:
		if err := json.Unmarshal(data, &lib); err != nil {
			log.Printf("downloads index in %s is not readable, starting fresh: %v", dir, err)
			return Library{}
		}
	}
	lib.reindex()
	lib.sweep(dir)
	return lib
}

// sweep deletes the audio files in dir that the library does not list: the
// files of downloads that were stopped as they finished.
func (l *Library) sweep(dir string) {
	listed := make(map[string]bool, len(l.Songs))
	for _, song := range l.Songs {
		listed[song.File] = true
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		name := entry.Name()
		key, _, _ := strings.Cut(name, ".")
		if entry.IsDir() || listed[name] || len(key) != sha256.Size*2 {
			continue
		}
		if _, err := hex.DecodeString(key); err != nil {
			continue
		}
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			log.Printf("removing unlisted download %s: %v", name, err)
		}
	}
}

// Write replaces the index of the downloads folder dir atomically.
func Write(dir string, lib Library) error {
	if lib.Songs == nil {
		lib.Songs = []Song{}
	}
	if lib.Collections == nil {
		lib.Collections = []Collection{}
	}
	data, err := json.Marshal(lib)
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(filepath.Join(dir, indexName), data, 0o600)
}

func (l *Library) reindex() {
	l.index = make(map[string]int, len(l.Songs))
	for i, song := range l.Songs {
		l.index[song.Item.VideoID] = i
	}
}

// Song returns the downloaded song with a video ID.
func (l *Library) Song(videoID string) (Song, bool) {
	i, ok := l.index[videoID]
	if !ok {
		return Song{}, false
	}
	return l.Songs[i], true
}

// Add puts a song first in the library, in place of the same song if it is
// there already.
func (l *Library) Add(song Song) {
	songs := make([]Song, 0, len(l.Songs)+1)
	songs = append(songs, song)
	for _, s := range l.Songs {
		if s.Item.VideoID != song.Item.VideoID {
			songs = append(songs, s)
		}
	}
	l.Songs = songs
	l.reindex()
}

// Remove takes a song out of the library and returns its file name. The
// albums and playlists that hold it keep its place, and show it again if it
// is downloaded again.
func (l *Library) Remove(videoID string) (string, bool) {
	song, ok := l.Song(videoID)
	if !ok {
		return "", false
	}
	l.Songs = slices.DeleteFunc(slices.Clone(l.Songs), func(s Song) bool { return s.Item.VideoID == videoID })
	l.reindex()
	return song.File, true
}

// Collection returns the downloaded album or playlist with a browse ID.
func (l *Library) Collection(id string) (Collection, bool) {
	for _, c := range l.Collections {
		if c.ID() == id {
			return c, true
		}
	}
	return Collection{}, false
}

// AddCollection puts an album or a playlist first in the library, in place of
// the same one if it is there already.
func (l *Library) AddCollection(c Collection) {
	collections := make([]Collection, 0, len(l.Collections)+1)
	collections = append(collections, c)
	for _, other := range l.Collections {
		if other.ID() != c.ID() {
			collections = append(collections, other)
		}
	}
	l.Collections = collections
}

// RemoveCollection takes an album or a playlist out of the library, with the
// songs that were downloaded only for it. It returns the files of those songs.
func (l *Library) RemoveCollection(id string) []string {
	removed, ok := l.Collection(id)
	if !ok {
		return nil
	}
	l.Collections = slices.DeleteFunc(slices.Clone(l.Collections), func(c Collection) bool { return c.ID() == id })
	var files []string
	for _, videoID := range removed.Tracks {
		song, ok := l.Song(videoID)
		if !ok || song.Single || l.InCollection(videoID) {
			continue
		}
		if file, ok := l.Remove(videoID); ok {
			files = append(files, file)
		}
	}
	return files
}

// InCollection reports whether a downloaded album or playlist holds a track.
func (l *Library) InCollection(videoID string) bool {
	for _, c := range l.Collections {
		if slices.Contains(c.Tracks, videoID) {
			return true
		}
	}
	return false
}

// Tracks returns the songs of a collection that are downloaded, in its order.
func (l *Library) Tracks(c Collection) []youtube.MusicItem {
	var items []youtube.MusicItem
	for _, videoID := range c.Tracks {
		if song, ok := l.Song(videoID); ok {
			items = append(items, song.Item)
		}
	}
	return items
}

// Download saves the audio at streamURL into dir under the name
// "Artist - Song", in format. format is mp3, m4a, opus, flac, or original,
// which keeps the file YouTube sent. An MP3, and the other encoded formats,
// are tagged with the artist, the title and the album. It returns the file's
// name.
func Download(ctx context.Context, dir, streamURL, format string, item youtube.MusicItem) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	temp, err := audio.Fetch(ctx, streamURL, dir)
	if err != nil {
		return "", err
	}
	defer fsutil.RemoveAll(filepath.Dir(temp))
	return install(ctx, dir, temp, format, item)
}

// install moves source into dir, encoding it when format asks, and returns
// the file's name. A name already in the folder gains " (2)", then " (3)".
func install(ctx context.Context, dir, source, format string, item youtube.MusicItem) (string, error) {
	name, err := uniqueName(dir, SongFileName(item, fileExt(format, source)))
	if err != nil {
		return "", err
	}
	dest := filepath.Join(dir, name)
	if format == "" || format == "original" {
		if err := os.Rename(source, dest); err != nil {
			return "", err
		}
		return name, nil
	}
	part := strings.TrimSuffix(dest, filepath.Ext(dest)) + ".part" + filepath.Ext(dest)
	if err := encode(ctx, source, part, format, item); err != nil {
		_ = os.Remove(part)
		return "", err
	}
	if err := os.Rename(part, dest); err != nil {
		_ = os.Remove(part)
		return "", err
	}
	return name, nil
}

// SongFileName is "Artist - Song.ext", or "Song.ext" when the song names no
// artist. Characters a file name cannot hold are left out.
func SongFileName(item youtube.MusicItem, ext string) string {
	title := strings.TrimSpace(item.Title)
	if title == "" {
		title = "Unknown track"
	}
	artist := strings.TrimSpace(item.Artists)
	if artist == "" {
		artist = strings.TrimSpace(item.Artist)
	}
	base := title
	if artist != "" {
		base = artist + " - " + title
	}
	base = sanitizeFileName(base)
	if base == "" {
		base = "Unknown track"
	}
	ext = strings.TrimPrefix(strings.TrimSpace(ext), ".")
	if ext == "" {
		ext = "audio"
	}
	return base + "." + ext
}

func fileExt(format, source string) string {
	switch format {
	case "mp3", "m4a", "opus", "flac":
		return format
	default:
		ext := strings.TrimPrefix(filepath.Ext(source), ".")
		if ext == "" {
			return "audio"
		}
		return ext
	}
}

func sanitizeFileName(name string) string {
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || strings.ContainsRune(`<>:"/\|?*`, r) {
			return ' '
		}
		return r
	}, name)
	name = strings.Join(strings.Fields(name), " ")
	name = strings.TrimRight(name, ". ")
	if utf8.RuneCountInString(name) > 120 {
		name = strings.TrimRight(string([]rune(name)[:120]), ". ")
	}
	if windowsReserved(name) {
		name = "_" + name
	}
	return name
}

func windowsReserved(name string) bool {
	upper := strings.ToUpper(name)
	switch upper {
	case "CON", "PRN", "AUX", "NUL":
		return true
	}
	if len(upper) == 4 && (strings.HasPrefix(upper, "COM") || strings.HasPrefix(upper, "LPT")) {
		return upper[3] >= '1' && upper[3] <= '9'
	}
	return false
}

func uniqueName(dir, name string) (string, error) {
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	candidate := name
	for n := 2; n < 1000; n++ {
		_, err := os.Stat(filepath.Join(dir, candidate))
		if errors.Is(err, os.ErrNotExist) {
			return candidate, nil
		}
		if err != nil {
			return "", err
		}
		candidate = stem + " (" + fmt.Sprint(n) + ")" + ext
	}
	return "", errors.New("could not find a free file name")
}

// encode writes source as format at dest and tags it with the song's artist,
// title and album.
func encode(ctx context.Context, source, dest, format string, item youtube.MusicItem) error {
	ffmpeg, err := proc.Tool("ffmpeg")
	if err != nil {
		return err
	}
	args := []string{"-hide_banner", "-loglevel", "error", "-nostdin", "-y", "-i", source, "-map_metadata", "-1", "-vn"}
	switch format {
	case "mp3":
		args = append(args, "-codec:a", "libmp3lame", "-q:a", "0", "-id3v2_version", "3")
	case "m4a":
		args = append(args, "-codec:a", "aac", "-b:a", "256k")
	case "opus":
		args = append(args, "-codec:a", "libopus", "-b:a", "192k")
	case "flac":
		args = append(args, "-codec:a", "flac")
	default:
		return errors.New("unknown download format")
	}
	args = append(args, tagArgs(item)...)
	args = append(args, dest)
	out, err := proc.Command(ctx, ffmpeg, args...).CombinedOutput()
	if err != nil {
		reason := lastLine(out)
		if reason == "" {
			return errors.New("could not save the audio")
		}
		return errors.New("could not save the audio: " + reason)
	}
	return nil
}

func tagArgs(item youtube.MusicItem) []string {
	title := strings.TrimSpace(item.Title)
	if title == "" {
		title = "Unknown track"
	}
	artist := strings.TrimSpace(item.Artists)
	if artist == "" {
		artist = strings.TrimSpace(item.Artist)
	}
	var args []string
	add := func(key, value string) {
		value = cleanTag(value)
		if value == "" {
			return
		}
		args = append(args, "-metadata", key+"="+value)
	}
	add("title", title)
	add("artist", artist)
	add("album", item.Album)
	return args
}

func cleanTag(value string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || unicode.IsControl(r) {
			return -1
		}
		return r
	}, strings.TrimSpace(value))
}

func lastLine(out []byte) string {
	lines := strings.FieldsFunc(string(out), func(r rune) bool { return r == '\n' || r == '\r' })
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line != "" {
			if len(line) > 200 {
				line = line[:200]
			}
			return line
		}
	}
	return ""
}

// Writer writes the index off the main thread, one write at a time, so two
// quick changes cannot land out of order. A save asked for while another is
// being written replaces what waits behind it. The zero value is ready to use.
type Writer struct {
	mu      sync.Mutex
	dir     string
	next    Library
	pending bool
	running bool
	done    chan struct{}
}

// Save writes lib to the downloads folder dir soon. The library must not be
// changed in place afterwards: its methods replace the slices they change.
func (w *Writer) Save(dir string, lib Library) {
	if dir == "" {
		return
	}
	w.mu.Lock()
	w.dir, w.next, w.pending = dir, lib, true
	if w.running {
		w.mu.Unlock()
		return
	}
	w.running = true
	w.done = make(chan struct{})
	done := w.done
	w.mu.Unlock()
	go w.drain(done)
}

func (w *Writer) drain(done chan struct{}) {
	defer close(done)
	for {
		w.mu.Lock()
		if !w.pending {
			w.running = false
			w.mu.Unlock()
			return
		}
		dir, lib := w.dir, w.next
		w.next, w.pending = Library{}, false
		w.mu.Unlock()
		if err := Write(dir, lib); err != nil {
			log.Printf("saving downloads: %v", err)
		}
	}
}

// Wait returns when every save asked for so far is on disk.
func (w *Writer) Wait() {
	w.mu.Lock()
	if !w.running {
		w.mu.Unlock()
		return
	}
	done := w.done
	w.mu.Unlock()
	<-done
}
