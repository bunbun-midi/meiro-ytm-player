package main

import (
	"encoding/json"
	"log"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/egoist/mygo/ui"

	"github.com/elianiva/meiro/internal/fsutil"
	"github.com/elianiva/meiro/m3"
	"github.com/elianiva/meiro/youtube"
)

// localPlaylists is the playlists saved on this computer. They are not the
// playlists on the YouTube account. path is empty when there is nowhere to
// write, and the lists then last for the session.
type localPlaylists struct {
	path  string
	items []localPlaylist
	// expanded is which playlists are open. A click on the name toggles one.
	expanded map[string]bool
	list     *ui.ListState
	menu     struct {
		open bool
		id   string
	}
}

// localPlaylistRow is one line of the Playlists page: the name of a playlist,
// or one of its tracks while it is open.
type localPlaylistRow struct {
	key    string
	header bool
	list   int
	track  int
}

// localPlaylist is one saved queue: a name the listener chose and the tracks
// in the order they had.
type localPlaylist struct {
	ID     string              `json:"id"`
	Name   string              `json:"name"`
	Tracks []youtube.MusicItem `json:"tracks"`
}

type localPlaylistFile struct {
	Playlists []localPlaylist `json:"playlists"`
}

// saveQueueState is the dialog that names the queue and saves it. The app
// holds one while the dialog is open, and nil otherwise.
type saveQueueState struct {
	open bool
	name string
}

// songPlaylistState is the dialog that adds one song to a playlist already
// saved here. query narrows the list to names that contain it.
type songPlaylistState struct {
	open  bool
	query string
	song  youtube.MusicItem
}

type playlistPlace int

const (
	placeNext playlistPlace = iota
	placeEnd
	placeReplace
)

const playlistNameLimit = 80

var playlistSeq uint64

func nextPlaylistID() string {
	playlistSeq++
	return strconv.FormatInt(time.Now().UnixNano(), 36) + "-" + strconv.FormatUint(playlistSeq, 36)
}

// load reads playlists.json. A missing file leaves the lists as they are. A
// file that cannot be read is logged and left unread, so a bad write does not
// wipe a list the session already holds.
func (s *localPlaylists) load() {
	if s.path == "" {
		return
	}
	body, err := os.ReadFile(s.path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("meiro: playlists: %v", err)
		}
		return
	}
	var file localPlaylistFile
	if err := json.Unmarshal(body, &file); err != nil {
		log.Printf("meiro: playlists: %v", err)
		return
	}
	if file.Playlists == nil {
		file.Playlists = []localPlaylist{}
	}
	s.items = file.Playlists
}

// save writes the lists. With nowhere to write, they stay in memory.
func (s *localPlaylists) save() error {
	if s.path == "" {
		return nil
	}
	body, err := json.MarshalIndent(localPlaylistFile{Playlists: s.items}, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')
	return fsutil.WriteFileAtomic(s.path, body, 0o644)
}

func (s *localPlaylists) indexOf(id string) int {
	for i := range s.items {
		if s.items[i].ID == id {
			return i
		}
	}
	return -1
}

func clipPlaylistName(name string) string {
	name = strings.TrimSpace(name)
	if utf8.RuneCountInString(name) <= playlistNameLimit {
		return name
	}
	return string([]rune(name)[:playlistNameLimit])
}

func playlistNameMatches(name, query string) bool {
	query = strings.TrimSpace(query)
	if query == "" {
		return true
	}
	return strings.Contains(strings.ToLower(name), strings.ToLower(query))
}

func trackCountLabel(n int) string {
	if n == 1 {
		return "1 track"
	}
	return strconv.Itoa(n) + " tracks"
}

// queueSnapshot is the queue beside the rail, without rows that have no video.
func (a *app) queueSnapshot() []youtube.MusicItem {
	var tracks []youtube.MusicItem
	for i := 0; i < a.queue.len(); i++ {
		item := a.queue.at(i)
		if item.VideoID == "" {
			continue
		}
		tracks = append(tracks, item)
	}
	return tracks
}

func (a *app) wrotePlaylists(notice string) {
	if err := a.localLists.save(); err != nil {
		log.Printf("meiro: playlists: %v", err)
		a.notice = "Could not write the playlists"
		return
	}
	a.notice = notice
}

// openSaveQueue asks for a name and saves the queue beside the rail.
func (a *app) openSaveQueue() {
	if len(a.queueSnapshot()) == 0 {
		a.notice = "The queue is empty"
		return
	}
	a.songPlaylist = nil
	a.saveQueue = &saveQueueState{open: true}
}

// openSongToPlaylist asks which saved playlist should gain this song.
func (a *app) openSongToPlaylist(item youtube.MusicItem) {
	if item.VideoID == "" {
		a.notice = "That song cannot be saved"
		return
	}
	a.saveQueue = nil
	a.songPlaylist = &songPlaylistState{open: true, song: item}
}

func (a *app) commitSaveQueue() {
	d := a.saveQueue
	if d == nil {
		return
	}
	name := clipPlaylistName(d.name)
	if name == "" {
		return
	}
	tracks := a.queueSnapshot()
	if len(tracks) == 0 {
		a.notice = "The queue is empty"
		a.saveQueue = nil
		return
	}
	list := localPlaylist{ID: nextPlaylistID(), Name: name, Tracks: tracks}
	a.localLists.items = append(a.localLists.items, list)
	a.localLists.expand(list.ID, true)
	a.saveQueue = nil
	a.wrotePlaylists("Saved “" + name + "”")
}

func (a *app) addSongToLocal(id string) {
	d := a.songPlaylist
	if d == nil || d.song.VideoID == "" {
		return
	}
	i := a.localLists.indexOf(id)
	if i < 0 {
		a.notice = "That playlist is gone"
		a.songPlaylist = nil
		return
	}
	list := &a.localLists.items[i]
	for _, track := range list.Tracks {
		if track.VideoID == d.song.VideoID {
			a.notice = "“" + songTitle(d.song) + "” is already in “" + list.Name + "”"
			a.songPlaylist = nil
			return
		}
	}
	list.Tracks = append(list.Tracks, d.song)
	name := list.Name
	title := songTitle(d.song)
	a.songPlaylist = nil
	a.wrotePlaylists("Added “" + title + "” to “" + name + "”")
}

func (a *app) pickTypedPlaylist() {
	d := a.songPlaylist
	if d == nil {
		return
	}
	id := ""
	n := 0
	for i := range a.localLists.items {
		if !playlistNameMatches(a.localLists.items[i].Name, d.query) {
			continue
		}
		n++
		id = a.localLists.items[i].ID
	}
	if n == 1 {
		a.addSongToLocal(id)
	}
}

func songTitle(item youtube.MusicItem) string {
	title := strings.TrimSpace(item.Title)
	if title == "" {
		return "Unknown track"
	}
	return title
}

// placeLocalPlaylist puts a saved playlist into the queue that is playing.
// An empty queue starts the playlist, which is the same list the rail shows.
func (a *app) placeLocalPlaylist(id string, place playlistPlace) {
	i := a.localLists.indexOf(id)
	a.localLists.menu.open = false
	if i < 0 {
		return
	}
	list := a.localLists.items[i]
	tracks := make([]youtube.MusicItem, 0, len(list.Tracks))
	for _, item := range list.Tracks {
		if item.VideoID != "" {
			tracks = append(tracks, item)
		}
	}
	if len(tracks) == 0 {
		a.notice = "That playlist is empty"
		return
	}
	if place == placeReplace || a.queue.len() == 0 || a.current.VideoID == "" {
		a.playFrom(tracks[0], tracks, 0, youtube.UpNextOptions{}, list.Name)
		a.notice = "Now playing “" + list.Name + "”"
		return
	}
	playNext := place == placeNext
	a.queue.insertAll(tracks, playNext, a.settings.AutoPlay)
	a.queueChanged()
	if playNext {
		a.notice = "“" + list.Name + "” will play next"
		return
	}
	a.notice = "“" + list.Name + "” was added to the queue"
}

func (a *app) removeLocalPlaylist(id string) {
	i := a.localLists.indexOf(id)
	a.localLists.menu.open = false
	if i < 0 {
		return
	}
	name := a.localLists.items[i].Name
	a.localLists.items = slices.Delete(a.localLists.items, i, i+1)
	a.localLists.expand(id, false)
	a.wrotePlaylists("Removed “" + name + "”")
}

func (a *app) saveQueueDialog(c *ui.Context) {
	d := a.saveQueue
	if d == nil {
		return
	}
	if !d.open {
		a.saveQueue = nil
		return
	}
	sc := m3.Of(c).Scheme
	m3.Dialog(c, &d.open, 480, func() {
		m3.EmphasizedText(c, m3.HeadlineSmall, "Save playlist")
		m3.Text(c, m3.BodyMedium, "The queue beside the rail is saved on this computer.").TextColor(sc.OnSurfaceVariant)
		box := ui.Column(c.Key("local-playlist-name")).Padding(12, 16).Radius(m3.Large).Background(sc.SurfaceContainerHighest)
		if box.FocusWithin() {
			box.Border(2, sc.Primary)
		}
		box.Children(func() {
			ui.TextInputBase(c.Key("local-playlist-name-field"), &d.name).
				Placeholder("Playlist name").Label("Name").TextColor(sc.OnSurface).FontSize(16).AutoFocus().
				OnSubmit(a.commitSaveQueue)
		})
		ui.Row(c).Gap(8).Justify(ui.End).AlignItems(ui.Center).Children(func() {
			if m3.Button(c, m3.ButtonSpec{Label: "Cancel", Kind: m3.TextOnly, Key: "cancel-local-playlist"}).Clicked() {
				a.saveQueue = nil
			}
			if m3.Button(c, m3.ButtonSpec{Label: "Save", Disabled: strings.TrimSpace(d.name) == "", Key: "save-local-playlist"}).Clicked() {
				a.commitSaveQueue()
			}
		})
	})
}

func (a *app) songPlaylistDialog(c *ui.Context) {
	d := a.songPlaylist
	if d == nil {
		return
	}
	if !d.open {
		a.songPlaylist = nil
		return
	}
	sc := m3.Of(c).Scheme
	m3.Dialog(c, &d.open, 480, func() {
		m3.EmphasizedText(c, m3.HeadlineSmall, "Song to Playlist")
		m3.Text(c, m3.BodyMedium, "Add “"+songTitle(d.song)+"” to a playlist on this computer.").TextColor(sc.OnSurfaceVariant).SingleLine()
		if len(a.localLists.items) == 0 {
			m3.Text(c, m3.BodyMedium, "No playlists yet. All To Playlist saves the queue beside the rail.").TextColor(sc.OnSurfaceVariant)
			if m3.Button(c, m3.ButtonSpec{Label: "Close", Kind: m3.TextOnly, Key: "close-song-playlist"}).Clicked() {
				a.songPlaylist = nil
			}
			return
		}
		box := ui.Column(c.Key("local-playlist-search")).Padding(12, 16).Radius(m3.Large).Background(sc.SurfaceContainerHighest)
		if box.FocusWithin() {
			box.Border(2, sc.Primary)
		}
		box.Children(func() {
			ui.TextInputBase(c.Key("local-playlist-search-field"), &d.query).
				Placeholder("Playlist name").Label("Find a playlist").TextColor(sc.OnSurface).FontSize(16).AutoFocus().
				OnSubmit(a.pickTypedPlaylist)
		})
		var picks []localPlaylist
		for i := range a.localLists.items {
			if playlistNameMatches(a.localLists.items[i].Name, d.query) {
				picks = append(picks, a.localLists.items[i])
			}
		}
		if len(picks) == 0 {
			m3.Text(c, m3.BodyMedium, "No playlist matches.").TextColor(sc.OnSurfaceVariant)
			if m3.Button(c, m3.ButtonSpec{Label: "Close", Kind: m3.TextOnly, Key: "close-song-playlist"}).Clicked() {
				a.songPlaylist = nil
			}
			return
		}
		ui.Scroll(c).MaxHeight(280).FillWidth().Children(func() {
			ui.Column(c).FillWidth().Gap(2).Children(func() {
				for _, list := range picks {
					a.playlistPick(c, list)
				}
			})
		})
	})
}

func (a *app) playlistPick(c *ui.Context, list localPlaylist) {
	sc := m3.Of(c).Scheme
	row := ui.ButtonBase(c.Key("pick-"+list.ID)).FillWidth().Height(40).Padding(0, 8).
		Radius(m3.Medium).Cursor(ui.CursorPointer).Label(list.Name)
	row.Background(m3.StateFill(ui.Transparent, sc.OnSurface, row.Hovered(), row.Pressed(), row.FocusVisible()))
	row.Children(func() {
		ui.Row(c).FillWidth().AlignItems(ui.Center).Gap(8).Children(func() {
			ui.Text(c, list.Name).Grow(1).MinWidth(0).SingleLine().Ellipsis("…").PassThrough()
			ui.Text(c, trackCountLabel(len(list.Tracks))).Shrink(0).TextColor(sc.OnSurfaceVariant).PassThrough()
		})
	})
	if row.Clicked() {
		a.addSongToLocal(list.ID)
	}
}

// expand opens or closes a playlist. Closing forgets it.
func (s *localPlaylists) expand(id string, open bool) {
	if s.expanded == nil {
		s.expanded = map[string]bool{}
	}
	if open {
		s.expanded[id] = true
		return
	}
	delete(s.expanded, id)
}

// localPlaylistRows is the page: each playlist's name, and its tracks while
// it is open. The list builds the lines in view, so a long playlist can be
// scrolled to its end.
func (a *app) localPlaylistRows() []localPlaylistRow {
	var rows []localPlaylistRow
	for i := range a.localLists.items {
		list := a.localLists.items[i]
		rows = append(rows, localPlaylistRow{key: "h-" + list.ID, header: true, list: i})
		if !a.localLists.expanded[list.ID] {
			continue
		}
		for t := range list.Tracks {
			rows = append(rows, localPlaylistRow{key: "t-" + list.ID + "-" + strconv.Itoa(t), list: i, track: t})
		}
	}
	return rows
}

// localPlaylistsPage lists the playlists saved on this computer. A click on a
// name opens or closes it. Its menu puts the whole list into the queue that
// is playing, which is the list beside the rail.
func (a *app) localPlaylistsPage(c *ui.Context) {
	if len(a.localLists.items) == 0 {
		ui.Column(c).Fill().Center().Children(func() {
			a.message(c, m3.IconPlaylist, "No playlists yet", "Right-click the queue beside the rail and choose All To Playlist.", "", nil)
		})
		return
	}
	if a.localLists.list == nil {
		a.localLists.list = &ui.ListState{}
	}
	rows := a.localPlaylistRows()
	a.localLists.list.Key = func(i int) any {
		if i >= len(rows) {
			return nil
		}
		return rows[i].key
	}
	a.localLists.list.Label = func(i int) string {
		if i >= len(rows) || rows[i].list >= len(a.localLists.items) {
			return ""
		}
		list := a.localLists.items[rows[i].list]
		if rows[i].header {
			return list.Name
		}
		if rows[i].track >= len(list.Tracks) {
			return ""
		}
		return "Saved " + songTitle(list.Tracks[rows[i].track])
	}
	ui.List(c, a.localLists.list, len(rows), func(i int) {
		if i >= len(rows) || rows[i].list >= len(a.localLists.items) {
			return
		}
		list := a.localLists.items[rows[i].list]
		if rows[i].header {
			a.localPlaylistHeader(c, list)
			return
		}
		if rows[i].track >= len(list.Tracks) {
			return
		}
		a.localPlaylistTrack(c, list, list.Tracks[rows[i].track], rows[i].track)
	}).Grow(1).MinHeight(0).Padding(8, pageGutter, a.clearance())
}

func (a *app) localPlaylistHeader(c *ui.Context, list localPlaylist) {
	sc := m3.Of(c).Scheme
	open := a.localLists.expanded[list.ID]
	row := ui.ButtonBase(c.Key("local-playlist-"+list.ID)).FillWidth().Height(56).Padding(8, 4).
		Radius(m3.Medium).Cursor(ui.CursorPointer).Label(list.Name).AlignItems(ui.Center)
	fill := m3.StateFill(ui.Transparent, sc.OnSurface, row.Hovered(), row.Pressed(), row.FocusVisible())
	if open {
		fill = sc.SecondaryContainer
	}
	row.Background(fill)
	icon := m3.IconExpandMore
	if open {
		icon = m3.IconExpandLess
	}
	row.Children(func() {
		ui.Icon(c, icon).FontSize(24).TextColor(sc.OnSurface).Shrink(0).PassThrough()
		ui.Column(c).Grow(1).MinWidth(0).Children(func() {
			m3.EmphasizedText(c, m3.BodyLarge, list.Name).SingleLine().PassThrough()
			m3.Text(c, m3.BodySmall, trackCountLabel(len(list.Tracks))).SingleLine().TextColor(sc.OnSurfaceVariant).PassThrough()
		})
	})
	if row.Clicked() {
		a.localLists.expand(list.ID, !open)
	}
	if row.RightClicked() {
		a.sideMenu.open = false
		a.localLists.menu.open = true
		a.localLists.menu.id = list.ID
	}
	if a.localLists.menu.open && a.localLists.menu.id == list.ID {
		m3.Menu(c, row, &a.localLists.menu.open, 220, func() {
			if m3.MenuItem(c, "Add Next", m3.IconNext).Clicked() {
				a.placeLocalPlaylist(list.ID, placeNext)
			}
			if m3.MenuItem(c, "Add to Queue", m3.IconQueue).Clicked() {
				a.placeLocalPlaylist(list.ID, placeEnd)
			}
			if m3.MenuItem(c, "Replace List", m3.IconPlay).Clicked() {
				a.placeLocalPlaylist(list.ID, placeReplace)
			}
			if m3.MenuItem(c, "Remove", m3.IconDelete).Clicked() {
				a.removeLocalPlaylist(list.ID)
			}
		})
	}
}

func (a *app) localPlaylistTrack(c *ui.Context, list localPlaylist, item youtube.MusicItem, index int) {
	sc := m3.Of(c).Scheme
	title := songTitle(item)
	line := title
	if artist := shownArtist(item); artist != "" {
		line += " — " + artist
	}
	if item.Duration != "" {
		line += "    " + item.Duration
	}
	ui.Row(c.Key("saved-"+list.ID+"-"+strconv.Itoa(index))).FillWidth().Height(32).Padding(0, 16).
		AlignItems(ui.Center).Label("Saved " + title).Children(func() {
		m3.Text(c, m3.BodyMedium, line).SingleLine().TextColor(sc.OnSurface)
	})
}
