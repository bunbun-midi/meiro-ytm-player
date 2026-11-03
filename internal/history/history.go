// Package history keeps what was played on this device, and sums it up for
// the stats page.
package history

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"io"
	"log"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/elianiva/meiro/internal/fsutil"
)

// MaxEntries is the default cap: about a year of heavy listening. The app
// can keep more when the user asks it to. Old entries go first either way.
const MaxEntries = 5000

// Entry is one played track.
type Entry struct {
	VideoID  string    `json:"videoId"`
	Title    string    `json:"title,omitempty"`
	Artist   string    `json:"artist,omitempty"`
	PlayedAt time.Time `json:"playedAt"`
}

// Append adds entries to the end of a history and drops its oldest entries
// beyond MaxEntries, in place.
func Append(history []Entry, entries ...Entry) []Entry {
	return AppendN(history, MaxEntries, entries...)
}

// AppendN adds entries and drops the oldest beyond limit. A limit below 1
// keeps MaxEntries.
func AppendN(history []Entry, limit int, entries ...Entry) []Entry {
	history = append(history, entries...)
	return Trim(history, limit)
}

// Trim drops the oldest entries beyond limit, in place. A limit below 1
// keeps MaxEntries.
func Trim(history []Entry, limit int) []Entry {
	if limit < 1 {
		limit = MaxEntries
	}
	if len(history) > limit {
		history = slices.Delete(history, 0, len(history)-limit)
	}
	return history
}

// Read reads a history file. A missing or unreadable file is an empty
// history rather than an error the user must see.
func Read(path string) []Entry {
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			log.Printf("reading listening history: %v", err)
		}
		return nil
	}
	var entries []Entry
	if err := json.Unmarshal(data, &entries); err != nil {
		log.Printf("listening history in %s is not readable, starting fresh: %v", path, err)
		return nil
	}
	return entries
}

// WriteCSV writes entries as a spreadsheet, oldest play first. The columns
// are when it played, the track, the artist and the video id.
func WriteCSV(w io.Writer, entries []Entry) error {
	out := csv.NewWriter(w)
	out.UseCRLF = true
	if err := out.Write([]string{"Played at", "Title", "Artist", "Video ID"}); err != nil {
		return err
	}
	for _, e := range entries {
		played := ""
		if !e.PlayedAt.IsZero() {
			played = e.PlayedAt.Format(time.RFC3339)
		}
		if err := out.Write([]string{played, e.Title, e.Artist, e.VideoID}); err != nil {
			return err
		}
	}
	out.Flush()
	return out.Error()
}

// Write replaces a history file atomically.
func Write(path string, entries []Entry) error {
	if entries == nil {
		entries = []Entry{} // "[]", not "null"
	}
	data, err := json.Marshal(entries)
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(path, data, 0o600)
}

// Writer writes the history off the main thread, one write at a time, so that
// the writes of two quick plays cannot land out of order and leave the older
// one on disk. A save asked for while another is being written replaces what
// waits behind it. The zero value is ready to use.
type Writer struct {
	mu      sync.Mutex
	path    string
	next    []Entry
	pending bool
	running bool
	done    chan struct{}
}

// Save writes entries to path soon. It keeps its own copy of them. Saving
// no entries writes an empty history, which is how the history is cleared.
func (w *Writer) Save(path string, entries []Entry) {
	if path == "" {
		return
	}
	entries = slices.Clone(entries)
	w.mu.Lock()
	w.path, w.next, w.pending = path, entries, true
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
		path, entries := w.path, w.next
		w.next, w.pending = nil, false
		w.mu.Unlock()
		if err := Write(path, entries); err != nil {
			log.Printf("saving listening history: %v", err)
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

// Day is one day of listening.
type Day struct {
	Day   time.Time
	Plays int
}

// Count is the plays of one track or artist.
type Count struct {
	Title  string
	Artist string
	Plays  int
}

// Summary is what the stats page shows.
type Summary struct {
	Plays      int
	Artists    int
	ByDay      []Day
	TopTracks  []Count
	TopArtists []Count
}

// Summarize folds entries into totals, daily counts for the days days ending
// on the day of now, and the five most played tracks and artists.
func Summarize(entries []Entry, now time.Time, days int) Summary {
	var summary Summary
	tracks := make(map[string]*Count)
	artists := make(map[string]*Count)
	cutoff := startOfDay(now).AddDate(0, 0, -days+1)
	summary.ByDay = make([]Day, days)
	for d := range summary.ByDay {
		summary.ByDay[d].Day = cutoff.AddDate(0, 0, d)
	}
	// Days are told apart by their calendar number, so placing a play costs
	// arithmetic instead of formatting its date into a string to look up.
	firstDay := CalendarDay(cutoff)
	for _, e := range entries {
		if e.PlayedAt.Before(cutoff) {
			continue
		}
		summary.Plays++
		key := e.VideoID
		if key == "" {
			key = e.Title
		}
		tc := tracks[key]
		if tc == nil {
			tc = &Count{Title: e.Title, Artist: e.Artist}
			tracks[key] = tc
		}
		tc.Plays++
		if e.Artist != "" {
			ac := artists[e.Artist]
			if ac == nil {
				ac = &Count{Title: e.Artist}
				artists[e.Artist] = ac
			}
			ac.Plays++
		}
		if i := CalendarDay(e.PlayedAt) - firstDay; i >= 0 && i < days {
			summary.ByDay[i].Plays++
		}
	}
	summary.Artists = len(artists)
	summary.TopTracks = top(tracks, 5)
	summary.TopArtists = top(artists, 5)
	return summary
}

// CalendarDay numbers the day t falls on, in its own zone, counting from a
// fixed origin, so consecutive days differ by one whatever the zone's offset.
func CalendarDay(t time.Time) int {
	y, m, d := t.Date()
	return int(time.Date(y, m, d, 0, 0, 0, 0, time.UTC).Unix() / 86400)
}

// top keeps the n highest counts, most plays first.
func top(counts map[string]*Count, n int) []Count {
	all := make([]Count, 0, len(counts))
	for _, c := range counts {
		all = append(all, *c)
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].Plays != all[j].Plays {
			return all[i].Plays > all[j].Plays
		}
		return all[i].Title < all[j].Title
	})
	return all[:min(len(all), n)]
}

// startOfDay is midnight of the day t falls on, in its own zone.
func startOfDay(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, t.Location())
}

// PageSize is how many plays a history page holds. A result no larger than
// this is the whole match; a larger match is split into pages so the caller
// never receives every row at once.
const PageSize = 100

// Query chooses which plays Search returns. From is inclusive and zero means
// no lower bound. Until is exclusive and zero means no upper bound. Text is
// a substring of the track name and the artist, ignoring case. Page is
// zero-based, newest plays first.
type Query struct {
	Text  string
	From  time.Time
	Until time.Time
	Page  int
}

// Page is one page of a search. Total is every match, not just this page.
type Page struct {
	Entries []Entry
	Total   int
	Page    int
	Pages   int
}

// Search returns one page of the plays that match q, newest first. It copies
// at most PageSize entries, however many match.
func Search(entries []Entry, q Query) Page {
	q.Text = strings.ToLower(strings.TrimSpace(q.Text))
	if q.Page < 0 {
		q.Page = 0
	}
	page, total := searchPage(entries, q)
	pages := 1
	if total > PageSize {
		pages = (total + PageSize - 1) / PageSize
	}
	if total > 0 && q.Page >= pages {
		q.Page = pages - 1
		page, total = searchPage(entries, q)
	}
	if total == 0 {
		q.Page = 0
	}
	return Page{Entries: page, Total: total, Page: q.Page, Pages: pages}
}

func searchPage(entries []Entry, q Query) ([]Entry, int) {
	start := q.Page * PageSize
	page := make([]Entry, 0, PageSize)
	total := 0
	for i := len(entries) - 1; i >= 0; i-- {
		if !matchEntry(entries[i], q) {
			continue
		}
		if total >= start && len(page) < PageSize {
			page = append(page, entries[i])
		}
		total++
	}
	return page, total
}

func matchEntry(e Entry, q Query) bool {
	if !q.From.IsZero() && e.PlayedAt.Before(q.From) {
		return false
	}
	if !q.Until.IsZero() && !e.PlayedAt.Before(q.Until) {
		return false
	}
	if q.Text == "" {
		return true
	}
	return strings.Contains(strings.ToLower(e.Title+" "+e.Artist), q.Text)
}
