package history

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSummarizeCountsTheRangeOnly(t *testing.T) {
	now := time.Date(2026, 3, 10, 15, 0, 0, 0, time.UTC)
	entries := []Entry{
		{VideoID: "a", Title: "A", Artist: "X", PlayedAt: now.AddDate(0, 0, -30)}, // outside
		{VideoID: "a", Title: "A", Artist: "X", PlayedAt: now.AddDate(0, 0, -1)},
		{VideoID: "a", Title: "A", Artist: "X", PlayedAt: now},
		{VideoID: "b", Title: "B", Artist: "Y", PlayedAt: now},
	}
	s := Summarize(entries, now, 7)
	if s.Plays != 3 || s.Artists != 2 || len(s.ByDay) != 7 {
		t.Fatalf("summary = %+v", s)
	}
	if s.ByDay[6].Plays != 2 || s.ByDay[5].Plays != 1 {
		t.Errorf("by day = %+v", s.ByDay)
	}
	if s.TopTracks[0].Title != "A" || s.TopTracks[0].Plays != 2 {
		t.Errorf("top tracks = %+v", s.TopTracks)
	}
}

func TestAppendCapsAndWriteReadsBack(t *testing.T) {
	var h []Entry
	for i := range MaxEntries + 3 {
		h = Append(h, Entry{VideoID: string(rune('a' + i%26))})
	}
	if len(h) != MaxEntries {
		t.Fatalf("len = %d", len(h))
	}
	path := filepath.Join(t.TempDir(), "history.json")
	if err := Write(path, h[:2]); err != nil {
		t.Fatal(err)
	}
	if got := Read(path); len(got) != 2 || got[0] != h[0] {
		t.Errorf("read back %+v", got)
	}
}

func TestWriteCSVKeepsCommasAndQuotes(t *testing.T) {
	when := time.Date(2026, 6, 1, 15, 4, 5, 0, time.FixedZone("EDT", -4*3600))
	var buf bytes.Buffer
	err := WriteCSV(&buf, []Entry{
		{VideoID: "a", Title: `Say "hi", friend`, Artist: "One, Two", PlayedAt: when},
		{VideoID: "b", Title: "Plain"},
	})
	if err != nil {
		t.Fatal(err)
	}
	rows, err := csv.NewReader(&buf).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 || rows[0][0] != "Played at" || rows[0][3] != "Video ID" {
		t.Fatalf("rows = %#v", rows)
	}
	if rows[1][0] != when.Format(time.RFC3339) || rows[1][1] != `Say "hi", friend` || rows[1][2] != "One, Two" || rows[1][3] != "a" {
		t.Fatalf("first play = %#v", rows[1])
	}
	if rows[2][0] != "" || rows[2][1] != "Plain" || rows[2][3] != "b" {
		t.Fatalf("second play = %#v", rows[2])
	}
}

func TestWriterKeepsTheLatestSaveAndWaitsForIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.json")
	var w Writer
	for i := range 50 {
		w.Save(path, []Entry{{VideoID: string(rune('a' + i%26)), Title: "t"}, {VideoID: "last", Title: string(rune('0' + i%10))}})
	}
	w.Wait()
	got := Read(path)
	if len(got) != 2 || got[1].Title != "9" {
		t.Errorf("the file holds %+v, want the 50th save", got)
	}
}

func TestWriterCopiesWhatItIsGiven(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.json")
	entries := []Entry{{VideoID: "a"}}
	var w Writer
	w.Save(path, entries)
	entries[0].VideoID = "changed"
	w.Wait()
	if got := Read(path); len(got) != 1 || got[0].VideoID != "a" {
		t.Errorf("the file holds %+v after the caller changed its slice", got)
	}
}

// fullHistory is a history at its cap, with titles and artists the length of
// real ones.
func fullHistory() []Entry {
	var h []Entry
	for i := range MaxEntries {
		h = Append(h, Entry{VideoID: fmt.Sprintf("dQw4w9WgX%02d", i%100), Title: "A fairly long track title (Remastered 2011)", Artist: "Some Artist & Another Artist", PlayedAt: time.Now().Add(-time.Duration(i) * time.Hour)})
	}
	return h
}

// BenchmarkWrite and BenchmarkRead are what a play and a launch cost with the
// history full; the file's size is reported with them.
func BenchmarkWrite(b *testing.B) {
	h, path := fullHistory(), filepath.Join(b.TempDir(), "history.json")
	b.ReportAllocs()
	for b.Loop() {
		if err := Write(path, h); err != nil {
			b.Fatal(err)
		}
	}
	info, _ := os.Stat(path)
	b.ReportMetric(float64(info.Size())/1024, "file-KB")
}

func BenchmarkRead(b *testing.B) {
	path := filepath.Join(b.TempDir(), "history.json")
	if err := Write(path, fullHistory()); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		if got := Read(path); len(got) != MaxEntries {
			b.Fatalf("read %d entries", len(got))
		}
	}
}

func BenchmarkSummarize(b *testing.B) {
	h, now := fullHistory(), time.Now()
	b.ReportAllocs()
	for b.Loop() {
		Summarize(h, now, 30)
	}
}

func TestAppendNKeepsTheRequestedLimit(t *testing.T) {
	var h []Entry
	for _, id := range []string{"a", "b", "c", "d"} {
		h = AppendN(h, 3, Entry{VideoID: id})
	}
	if len(h) != 3 || h[0].VideoID != "b" || h[2].VideoID != "d" {
		t.Fatalf("history = %+v, want the newest 3", h)
	}
}

func TestSearchPagesAndFiltersWithoutReturningEveryMatch(t *testing.T) {
	start := time.Date(2026, 1, 1, 12, 0, 0, 0, time.Local)
	entries := make([]Entry, 250)
	for i := range entries {
		entries[i] = Entry{
			Title: fmt.Sprintf("Track %d", i), Artist: "Someone",
			PlayedAt: start.AddDate(0, 0, i),
		}
	}
	entries[0].Title = "Needle"
	entries[0].Artist = "Hidden"
	entries[10].Artist = "Special Guest"

	page := Search(entries, Query{})
	if page.Total != 250 || len(page.Entries) != PageSize || page.Pages != 3 || page.Entries[0].Title != "Track 249" {
		t.Fatalf("first page = %+v, %d rows, first %q", page, len(page.Entries), page.Entries[0].Title)
	}
	if len(page.Entries) > PageSize {
		t.Fatalf("returned %d rows, the cap is %d", len(page.Entries), PageSize)
	}

	found := Search(entries, Query{Text: "needle"})
	if found.Total != 1 || len(found.Entries) != 1 || found.Entries[0].Artist != "Hidden" {
		t.Fatalf("title search = %+v", found)
	}
	artist := Search(entries, Query{Text: "guest"})
	if artist.Total != 1 || artist.Entries[0].Title != "Track 10" {
		t.Fatalf("artist search = %+v", artist)
	}

	ranged := Search(entries, Query{
		From:  time.Date(2026, 1, 10, 0, 0, 0, 0, time.Local),
		Until: time.Date(2026, 1, 21, 0, 0, 0, 0, time.Local),
	})
	if ranged.Total != 11 || ranged.Pages != 1 || len(ranged.Entries) != 11 {
		t.Fatalf("range = %+v, %d rows", ranged, len(ranged.Entries))
	}
	if ranged.Entries[0].PlayedAt.Day() != 20 || ranged.Entries[len(ranged.Entries)-1].PlayedAt.Day() != 10 {
		t.Fatalf("range order = %v then %v", ranged.Entries[0].PlayedAt, ranged.Entries[len(ranged.Entries)-1].PlayedAt)
	}
}
