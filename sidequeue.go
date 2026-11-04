package main

import (
	"strconv"
	"strings"

	"github.com/egoist/mygo/ui"

	"github.com/elianiva/meiro/internal/settings"
	"github.com/elianiva/meiro/m3"
	"github.com/elianiva/meiro/youtube"
)

// The queue beside the rail. Its usual width is what a first open uses.
// Rows stay one line: text that does not fit is cut, without an ellipsis.
const (
	sideQueueDefault float32 = 280
	sideQueueFont    float32 = 11
	sideQueueLine    float32 = 14
	sideQueueRow     float32 = 18
	// sideQueueClip ends a line that does not fit. A zero-width space draws
	// nothing, so the line is cut at the edge of its cell.
	sideQueueClip = "\u200b"
)

// sideQueueWidth is how wide the queue beside the rail is drawn.
func (a *app) sideQueueWidth() float32 {
	w := a.settings.SideQueueWidth
	if w <= 0 {
		return sideQueueDefault
	}
	return float32(w)
}

// sideQueue is the queue beside the navigation rail: artist, title, length
// and album, on one line, with no artwork. Its right edge sets the width.
func (a *app) sideQueue(c *ui.Context) {
	sc := m3.Of(c).Scheme
	w := a.sideQueueWidth()
	panel := ui.Column(c.Key("side-queue")).Width(w).MinWidth(w).MaxWidth(w).Shrink(0).FillHeight().
		Clip().Background(sc.SurfaceContainerLow)
	panel.Children(func() {
		a.sideQueueHeader(c)
		q := &a.queue
		if q.len() == 0 {
			ui.Text(c, "The queue is empty").Padding(2, 6).FontSize(sideQueueFont).
				FixedLineHeight(sideQueueLine).TextColor(sc.OnSurfaceVariant)
		} else {
			a.sideQueueListView(c, q)
		}
		a.sideQueueEdge(c)
	})
}

func (a *app) sideQueueHeader(c *ui.Context) {
	sc := m3.Of(c).Scheme
	colour := sc.OnSurfaceVariant
	ui.Row(c.Key("side-queue-head")).Height(sideQueueRow).Shrink(0).Padding(0, 6).Gap(6).
		AlignItems(ui.Center).Clip().Children(func() {
		a.sideQueuePlain(c, "Artist", colour)
		a.sideQueuePlain(c, "Title", colour)
		ui.Text(c, "Length").Width(40).Shrink(0).FontSize(sideQueueFont).FixedLineHeight(sideQueueLine).
			SingleLine().Ellipsis(sideQueueClip).TextAlign(ui.End).TextColor(colour)
		a.sideQueuePlain(c, "Album", colour)
	})
}

func (a *app) sideQueueListView(c *ui.Context, q *queue) {
	list := &a.sideQueueList
	list.Key = func(row int) any {
		return itemKey("side-queue", q.at(row)) + "#" + strconv.Itoa(row)
	}
	list.Label = func(row int) string { return "Queue track " + strconv.Itoa(row+1) }
	list.Reorder = func(rows []int, to int) {
		if a.queue.moveItems(rows, to) {
			a.queueChanged()
		}
	}
	if a.sideQueueFollow != a.current.VideoID {
		a.sideQueueFollow = a.current.VideoID
		list.ScrollTo(q.index, ui.Start)
	}
	ui.List(c, list, q.len(), func(row int) {
		if row < 0 || row >= q.len() {
			return
		}
		a.sideQueueRow(c, q.at(row), row)
	}).Grow(1).MinHeight(0).Padding(0, 0, 4).FocusRing(false)
}

func (a *app) sideQueueRow(c *ui.Context, item youtube.MusicItem, index int) {
	sc := m3.Of(c).Scheme
	playing := index == a.queue.index && item.VideoID != "" && item.VideoID == a.current.VideoID
	titleColour, subColour := sc.OnSurface, sc.OnSurfaceVariant
	if playing {
		titleColour, subColour = sc.OnSecondaryContainer, sc.OnSecondaryContainer
	}
	// A saved copy is drawn in the theme's own colour, which is the
	// saturated one, including on the row that is playing.
	if a.isDownloaded(item.VideoID) {
		titleColour = sc.Primary
	}
	row := ui.Row(c.Key("side-queue-row-"+strconv.Itoa(index))).Height(sideQueueRow).Shrink(0).
		Padding(0, 6).Gap(6).AlignItems(ui.Center).Clip()
	if playing {
		row.Background(sc.SecondaryContainer)
	}
	row.Children(func() {
		a.sideQueueArtists(c, item, index, subColour)
		a.sideQueueTitle(c, item, index, titleColour)
		length := ui.Text(c, item.Duration).Width(40).Shrink(0).FontSize(sideQueueFont).FixedLineHeight(sideQueueLine).
			SingleLine().Ellipsis(sideQueueClip).TextAlign(ui.End).TextColor(subColour).FontFeatures("tnum")
		a.offerSideMenu(c, length, index, "side-length-"+strconv.Itoa(index))
		a.sideQueueAlbum(c, item, index, subColour)
	})
	a.offerSideMenu(c, row, index, "side-row-"+strconv.Itoa(index))
}

func (a *app) sideQueueArtists(c *ui.Context, item youtube.MusicItem, index int, colour ui.Color) {
	parts := artistLinks(item)
	ui.Row(c).Grow(1).Basis(0).MinWidth(0).Clip().AlignItems(ui.Center).Children(func() {
		if len(parts) == 0 {
			a.sideQueuePlain(c, "—", colour)
			return
		}
		for i, part := range parts {
			if i > 0 {
				ui.Text(c, ", ").Shrink(0).FontSize(sideQueueFont).FixedLineHeight(sideQueueLine).TextColor(colour)
			}
			if !part.Target().Opens() {
				text := ui.Text(c, part.Text).Shrink(1).MinWidth(0).FontSize(sideQueueFont).FixedLineHeight(sideQueueLine).
					SingleLine().Ellipsis(sideQueueClip).TextColor(colour)
				a.offerSideMenu(c, text, index, "side-artist-"+strconv.Itoa(index)+"-"+strconv.Itoa(i))
				continue
			}
			link := ui.Link(c.Key("side-artist-"+strconv.Itoa(index)+"-"+strconv.Itoa(i)), part.Text, "").
				Grow(1).Basis(0).MinWidth(0).FontSize(sideQueueFont).FixedLineHeight(sideQueueLine).
				SingleLine().Ellipsis(sideQueueClip).TextColor(colour).
				Label("Go to artist: " + part.Text)
			if link.Clicked() {
				a.openPlayerLink(part)
			}
			a.offerSideMenu(c, link, index, "side-artist-"+strconv.Itoa(index)+"-"+strconv.Itoa(i))
		}
	})
}

func (a *app) sideQueueTitle(c *ui.Context, item youtube.MusicItem, index int, colour ui.Color) {
	title := strings.TrimSpace(item.Title)
	if title == "" {
		title = "Unknown track"
	}
	b := ui.ButtonBase(c.Key("side-title-" + strconv.Itoa(index))).Grow(1).Basis(0).MinWidth(0).
		Height(sideQueueRow).Clip().Cursor(ui.CursorPointer).Label(title)
	b.Children(func() {
		text := ui.Text(c, title).FontSize(sideQueueFont).FixedLineHeight(sideQueueLine).
			SingleLine().Ellipsis(sideQueueClip).Grow(1).MinWidth(0).TextColor(colour).PassThrough()
		if index == a.queue.index {
			text.FontWeight(600)
		}
	})
	if b.Clicked() {
		if index == a.queue.index {
			a.togglePlay()
			return
		}
		a.playFrom(item, a.queue.items, index, youtube.UpNextOptions{}, a.queue.source)
	}
	a.offerSideMenu(c, b, index, "side-title-"+strconv.Itoa(index))
}

func (a *app) sideQueueAlbum(c *ui.Context, item youtube.MusicItem, index int, colour ui.Color) {
	part := albumLink(item)
	if strings.TrimSpace(part.Text) == "" {
		text := ui.Text(c, "—").Grow(1).Basis(0).MinWidth(0).FontSize(sideQueueFont).FixedLineHeight(sideQueueLine).
			SingleLine().Ellipsis(sideQueueClip).TextColor(colour)
		a.offerSideMenu(c, text, index, "side-album-"+strconv.Itoa(index))
		return
	}
	if !part.Target().Opens() {
		text := ui.Text(c, part.Text).Grow(1).Basis(0).MinWidth(0).FontSize(sideQueueFont).FixedLineHeight(sideQueueLine).
			SingleLine().Ellipsis(sideQueueClip).TextColor(colour)
		a.offerSideMenu(c, text, index, "side-album-"+strconv.Itoa(index))
		return
	}
	link := ui.Link(c.Key("side-album-"+strconv.Itoa(index)), part.Text, "").
		Grow(1).Basis(0).MinWidth(0).FontSize(sideQueueFont).FixedLineHeight(sideQueueLine).
		SingleLine().Ellipsis(sideQueueClip).TextColor(colour).
		Label("Go to album: " + part.Text)
	if link.Clicked() {
		a.openPlayerLink(part)
	}
	a.offerSideMenu(c, link, index, "side-album-"+strconv.Itoa(index))
}

func (a *app) sideQueuePlain(c *ui.Context, text string, colour ui.Color) {
	ui.Text(c, text).Grow(1).Basis(0).MinWidth(0).FontSize(sideQueueFont).FixedLineHeight(sideQueueLine).
		SingleLine().Ellipsis(sideQueueClip).TextColor(colour)
}

// offerSideMenu opens Play and Download when the secondary button hits
// this part of the row. The menu is anchored to that part.
func (a *app) offerSideMenu(c *ui.Context, e ui.Element, index int, key string) {
	if e.RightClicked() {
		a.sideMenu.open = true
		a.sideMenu.index = index
		a.sideMenu.key = key
	}
	if !a.sideMenu.open || a.sideMenu.index != index || a.sideMenu.key != key {
		return
	}
	if index < 0 || index >= a.queue.len() {
		a.sideMenu.open = false
		return
	}
	item := a.queue.at(index)
	m3.Menu(c, e, &a.sideMenu.open, 200, func() {
		if m3.MenuItem(c, "Play", m3.IconPlay).Label("Play track").Clicked() {
			a.sideMenu.open = false
			if index == a.queue.index && item.VideoID == a.current.VideoID {
				a.togglePlay()
				return
			}
			a.playFrom(item, a.queue.items, index, youtube.UpNextOptions{}, a.queue.source)
		}
		switch {
		case a.isDownloaded(item.VideoID):
			if m3.MenuItem(c, "Downloaded", m3.IconDownloadDone).Clicked() {
				a.sideMenu.open = false
			}
		case a.canDownload(item):
			if m3.MenuItem(c, "Download", m3.IconDownload).Clicked() {
				a.sideMenu.open = false
				a.downloadSong(item)
			}
		}
	})
}

func (a *app) sideQueueEdge(c *ui.Context) {
	edge := ui.Box(c.Key("side-queue-resize")).Absolute().Top(0).Bottom(0).Right(-3).Width(6).
		Cursor(ui.CursorResizeEW).Label("Resize playlist")
	dx, _, held := edge.Dragged()
	if held {
		a.sideQueueSizing = true
		if dx != 0 {
			a.settings.SideQueueWidth = settings.ClampSideQueueWidth(float64(a.sideQueueWidth()) + float64(dx))
		}
		return
	}
	if a.sideQueueSizing {
		a.sideQueueSizing = false
		a.saveSettings()
	}
}

func artistLinks(item youtube.MusicItem) []youtube.BylinePart {
	var parts []youtube.BylinePart
	if item.Byline != nil {
		for _, part := range item.Byline.Parts {
			if part.Target().Kind == youtube.TargetArtist && strings.TrimSpace(part.Text) != "" {
				parts = append(parts, part)
			}
		}
	}
	if len(parts) == 0 && strings.TrimSpace(item.Artist) != "" {
		parts = []youtube.BylinePart{{Text: item.Artist, BrowseID: item.ArtistID()}}
	}
	return parts
}

func albumLink(item youtube.MusicItem) youtube.BylinePart {
	if item.Byline != nil {
		for _, part := range item.Byline.Parts {
			if part.Target().Kind == youtube.TargetAlbum && strings.TrimSpace(part.Text) != "" {
				return part
			}
		}
	}
	return youtube.BylinePart{Text: item.Album}
}
