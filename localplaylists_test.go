package main

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/egoist/mygo/ui"

	"github.com/elianiva/meiro/youtube"
)

func TestLocalPlaylistListsEveryTrackAndCollapses(t *testing.T) {
	a := newTestApp(t)
	tracks := make([]youtube.MusicItem, 40)
	for i := range tracks {
		tracks[i] = youtube.MusicItem{VideoID: strconv.Itoa(i), Title: "Track " + strconv.Itoa(i), Artists: "Ada"}
	}
	a.localLists.items = []localPlaylist{{ID: "road", Name: "Road", Tracks: tracks}}
	a.localLists.expand("road", true)
	a.router.Push("/playlists")
	tt := ui.NewTester(a.view, 1000, 700)
	tt.Frame()

	found := false
	for range 40 {
		if _, ok := tt.Find("Saved Track 39"); ok {
			found = true
			break
		}
		tt.Scroll(600, 300, 0, 500)
		tt.Frame()
	}
	if !found {
		t.Fatalf("the last track is not in the list: %q", tt.Texts())
	}
	for range 40 {
		if _, ok := tt.Find("Road"); ok {
			break
		}
		tt.Scroll(600, 300, 0, -500)
		tt.Frame()
	}
	if err := tt.Click("Road"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if tt.HasText("Track 0") {
		t.Fatal("the playlist stayed open")
	}
	if err := tt.Click("Road"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if _, ok := tt.Find("Saved Track 0"); !ok {
		t.Fatal("the playlist did not open")
	}
}

func TestLocalPlaylistsRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "playlists.json")
	s := localPlaylists{path: path, items: []localPlaylist{{
		ID: "a", Name: "Road", Tracks: tracks("one"),
	}}}
	if err := s.save(); err != nil {
		t.Fatal(err)
	}
	var got localPlaylists
	got.path = path
	got.load()
	if len(got.items) != 1 || got.items[0].Name != "Road" || got.items[0].Tracks[0].VideoID != "one" {
		t.Fatalf("%+v", got.items)
	}

	kept := localPlaylists{path: path, items: []localPlaylist{{ID: "keep", Name: "Keep"}}}
	if err := os.WriteFile(path, []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	kept.load()
	if len(kept.items) != 1 || kept.items[0].ID != "keep" {
		t.Fatalf("a bad file replaced the list: %+v", kept.items)
	}
	if clipPlaylistName("  Road  ") != "Road" {
		t.Fatal(clipPlaylistName("  Road  "))
	}
	long := strings.Repeat("n", playlistNameLimit+5)
	if got := clipPlaylistName(long); len([]rune(got)) != playlistNameLimit {
		t.Fatalf("clipped to %d runes", len([]rune(got)))
	}
	if !playlistNameMatches("Morning", "morn") || playlistNameMatches("Road", "morn") {
		t.Fatal("name filter")
	}
}

func TestLocalPlaylistsSaveTheQueueAndPlayItBack(t *testing.T) {
	a := newTestApp(t)
	a.localLists.path = filepath.Join(t.TempDir(), "playlists.json")
	a.settings.RailExpanded = true
	a.settings.AutoPlay = false
	first := youtube.MusicItem{VideoID: "one", Title: "First Song", Artists: "Ada"}
	second := youtube.MusicItem{VideoID: "two", Title: "Second Song", Artists: "Bea"}
	a.current = first
	a.queue.replace(first, []youtube.MusicItem{first, second}, 0, "Home")

	tt := ui.NewTester(a.view, 1280, 900)
	tt.Frame()
	library, ok := tt.Find("Library")
	playlists, pok := tt.Find("Playlists")
	downloads, dok := tt.Find("Downloads")
	if !ok || !pok || !dok || !(library.Y < playlists.Y && playlists.Y < downloads.Y) {
		t.Fatalf("Playlists is not under Library: library %v playlists %v downloads %v", library, playlists, downloads)
	}

	if err := tt.RightClick("First Song"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if err := tt.Click("Song to Playlist"); err != nil {
		t.Fatalf("no song action: %v in %q", err, tt.Texts())
	}
	tt.Frame()
	if !tt.HasText("No playlists yet") {
		t.Fatalf("empty picker: %q", tt.Texts())
	}
	tt.Key(0, ui.KeyEscape)
	tt.Frame()

	if err := tt.RightClick("First Song"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if err := tt.Click("All To Playlist"); err != nil {
		t.Fatalf("no save action: %v in %q", err, tt.Texts())
	}
	tt.Frame()
	if err := tt.Click("Name"); err != nil {
		t.Fatalf("no name field: %v in %q", err, tt.Texts())
	}
	tt.Type("Road")
	tt.Frame()
	if err := tt.Click("Save"); err != nil {
		t.Fatalf("no save: %v in %q", err, tt.Texts())
	}
	tt.Frame()
	if !tt.HasText("Saved “Road”") {
		t.Fatalf("notice: %q", tt.Texts())
	}
	if len(a.localLists.items) != 1 || a.localLists.items[0].Name != "Road" || len(a.localLists.items[0].Tracks) != 2 {
		t.Fatalf("%+v", a.localLists.items)
	}
	var stored localPlaylists
	stored.path = a.localLists.path
	stored.load()
	if len(stored.items) != 1 || stored.items[0].Name != "Road" || len(stored.items[0].Tracks) != 2 {
		t.Fatalf("disk %+v", stored.items)
	}

	if err := tt.Click("Playlists"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if !tt.HasText("Road") || !tt.HasText("2 tracks") {
		t.Fatalf("page: %q", tt.Texts())
	}
	if _, ok := tt.Find("Saved Second Song"); !ok {
		t.Fatalf("the playlist did not list its songs: %q", tt.Texts())
	}

	if err := tt.RightClick("Road"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if err := tt.Click("Add Next"); err != nil {
		t.Fatalf("no add next: %v in %q", err, tt.Texts())
	}
	tt.Frame()
	if got := ids(a.queue.items); !slices.Equal(got, []string{"one", "one", "two", "two"}) || a.queue.index != 0 {
		t.Fatalf("add next: %v index %d", got, a.queue.index)
	}

	if err := tt.RightClick("Road"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if err := tt.Click("Add to Queue"); err != nil {
		t.Fatalf("no add to queue: %v in %q", err, tt.Texts())
	}
	tt.Frame()
	if got := ids(a.queue.items); !slices.Equal(got, []string{"one", "one", "two", "two", "one", "two"}) {
		t.Fatalf("add to queue: %v", got)
	}

	if err := tt.RightClick("Road"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if err := tt.Click("Replace List"); err != nil {
		t.Fatalf("no replace: %v in %q", err, tt.Texts())
	}
	tt.Frame()
	if got := ids(a.queue.items); !slices.Equal(got, []string{"one", "two"}) {
		t.Fatalf("replace: %v", got)
	}

	if err := tt.RightClick("Road"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if err := tt.Click("Remove"); err != nil {
		t.Fatalf("no remove: %v in %q", err, tt.Texts())
	}
	tt.Frame()
	if !tt.HasText("No playlists yet") || len(a.localLists.items) != 0 {
		t.Fatalf("removed: %d %q", len(a.localLists.items), tt.Texts())
	}
	stored.items = nil
	stored.load()
	if len(stored.items) != 0 {
		t.Fatalf("disk still has %+v", stored.items)
	}
}

func TestSongToPlaylistFiltersByName(t *testing.T) {
	a := newTestApp(t)
	a.localLists.path = filepath.Join(t.TempDir(), "playlists.json")
	a.settings.RailExpanded = true
	a.settings.AutoPlay = false
	song := youtube.MusicItem{VideoID: "two", Title: "Second Song", Artists: "Bea"}
	a.current = song
	a.queue.replace(song, []youtube.MusicItem{song}, 0, "Home")

	tt := ui.NewTester(a.view, 1280, 900)
	tt.Frame()
	a.localLists.items = []localPlaylist{
		{ID: "road", Name: "Road", Tracks: tracks("old")},
		{ID: "morning", Name: "Morning"},
	}
	tt.Frame()
	if err := tt.RightClick("Second Song"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if err := tt.Click("Song to Playlist"); err != nil {
		t.Fatalf("no song action: %v in %q", err, tt.Texts())
	}
	tt.Frame()
	if !tt.HasText("Road") || !tt.HasText("Morning") {
		t.Fatalf("picker: %q", tt.Texts())
	}
	if err := tt.Click("Find a playlist"); err != nil {
		t.Fatalf("no search: %v in %q", err, tt.Texts())
	}
	tt.Type("morn")
	tt.Frame()
	if tt.HasText("Road") {
		t.Fatalf("filter kept Road: %q", tt.Texts())
	}
	if err := tt.Click("Morning"); err != nil {
		t.Fatalf("no morning: %v in %q", err, tt.Texts())
	}
	tt.Frame()
	if len(a.localLists.items[1].Tracks) != 1 || a.localLists.items[1].Tracks[0].VideoID != "two" {
		t.Fatalf("morning: %+v", a.localLists.items[1].Tracks)
	}
	if len(a.localLists.items[0].Tracks) != 1 || a.localLists.items[0].Tracks[0].VideoID != "old" {
		t.Fatalf("road changed: %+v", a.localLists.items[0].Tracks)
	}
	if !tt.HasText("Added “Second Song” to “Morning”") {
		t.Fatalf("notice: %q", tt.Texts())
	}

	if err := tt.RightClick("Second Song"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if err := tt.Click("Song to Playlist"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if err := tt.Click("Morning"); err != nil {
		t.Fatalf("no morning again: %v in %q", err, tt.Texts())
	}
	tt.Frame()
	if len(a.localLists.items[1].Tracks) != 1 {
		t.Fatalf("duplicate added: %+v", a.localLists.items[1].Tracks)
	}
	if !tt.HasText("already in") {
		t.Fatalf("notice: %q", tt.Texts())
	}

	var stored localPlaylists
	stored.path = a.localLists.path
	stored.load()
	if len(stored.items) != 2 || len(stored.items[1].Tracks) != 1 || stored.items[1].Tracks[0].VideoID != "two" {
		t.Fatalf("disk %+v", stored.items)
	}
}
