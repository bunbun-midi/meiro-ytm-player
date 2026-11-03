package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/egoist/mygo/ui"

	"github.com/elianiva/meiro/internal/history"
	"github.com/elianiva/meiro/internal/settings"
	"github.com/elianiva/meiro/m3"
)

// historyMemo is the page of history last built, and what it was built from.
// The page rebuilds on every frame, and searching the whole history does not.
type historyMemo struct {
	valid          bool
	n              int
	last           time.Time
	text, from, to string
	page           int
	result         history.Page
}

func (a *app) historyLimit() int {
	return settings.ClampHistoryLimit(a.settings.HistoryLimit)
}

// setHistoryLimit keeps n plays, dropping the oldest, and remembers n.
func (a *app) setHistoryLimit(n int) {
	n = settings.ClampHistoryLimit(n)
	a.settings.HistoryLimit = n
	a.historyLimitText = strconv.Itoa(n)
	a.history = history.Trim(a.history, n)
	a.saveSettings()
	a.saveHistory()
}

// historyMatches is the current page of plays for the search and the dates.
// A match larger than a page is not returned whole.
func (a *app) historyMatches() history.Page {
	var last time.Time
	if n := len(a.history); n > 0 {
		last = a.history[n-1].PlayedAt
	}
	memo := &a.historyView
	if memo.valid && memo.n == len(a.history) && memo.last.Equal(last) &&
		memo.text == a.historySearch && memo.from == a.historyFrom && memo.to == a.historyTo &&
		memo.page == a.historyPageIndex {
		return memo.result
	}
	from, fromOK := parseHistoryDay(a.historyFrom)
	to, toOK := parseHistoryDay(a.historyTo)
	if !fromOK {
		from = time.Time{}
	}
	var until time.Time
	if toOK && !to.IsZero() {
		until = to.AddDate(0, 0, 1)
	}
	result := history.Search(a.history, history.Query{
		Text: a.historySearch, From: from, Until: until, Page: a.historyPageIndex,
	})
	a.historyPageIndex = result.Page
	*memo = historyMemo{
		valid: true, n: len(a.history), last: last,
		text: a.historySearch, from: a.historyFrom, to: a.historyTo,
		page: result.Page, result: result,
	}
	return result
}

// parseHistoryDay reads YYYY-MM-DD. An empty field is the open end of a range.
func parseHistoryDay(text string) (time.Time, bool) {
	text = strings.TrimSpace(text)
	if text == "" {
		return time.Time{}, true
	}
	day, err := time.ParseInLocation("2006-01-02", text, time.Local)
	if err != nil {
		return time.Time{}, false
	}
	return day, true
}

// historyPage lists plays on this device. The whole match is shown when it
// fits on one page; otherwise the page buttons move through it.
func (a *app) historyPage(c *ui.Context) {
	sc := m3.Of(c).Scheme
	ui.Scroll(c).Grow(1).Padding(0, pageGutter, a.clearance()).Children(func() {
		ui.Column(c).MaxWidth(780).Gap(16).Children(func() {
			settingLabel(c, "Listening history", "Search the track or the artist, or limit the list to a range of days.")
			ui.Column(c).Gap(8).Children(func() {
				search := ui.TextInputBase(c.Key("history-search"), &a.historySearch).
					Height(48).Padding(12).Radius(m3.Medium).Background(sc.SurfaceContainerHigh).
					TextColor(sc.OnSurface).FontSize(16).Label("Search history").Placeholder("Track or artist")
				if search.Changed() {
					a.historyPageIndex = 0
				}
				var fromField, toField ui.Element
				ui.Row(c).Gap(12).Children(func() {
					fromField = a.historyDayField(c, "history-from", "From", "First day", &a.historyFrom)
					toField = a.historyDayField(c, "history-to", "To", "Last day", &a.historyTo)
				})
				if fromField.Changed() || toField.Changed() {
					a.historyPageIndex = 0
				}
				if _, ok := parseHistoryDay(a.historyFrom); !ok {
					m3.Text(c, m3.BodyMedium, "Use a date like 2026-01-31. A date that is not ready is left open.").TextColor(sc.Error)
				} else if _, ok := parseHistoryDay(a.historyTo); !ok {
					m3.Text(c, m3.BodyMedium, "Use a date like 2026-01-31. A date that is not ready is left open.").TextColor(sc.Error)
				}
			})
			if !a.historyLoaded {
				a.message(c, m3.IconHistory, "Loading history", "Plays saved on this device are on their way.", "", nil)
				return
			}
			result := a.historyMatches()
			if len(a.history) == 0 {
				a.message(c, m3.IconHistory, "No plays yet", "Songs you listen to in Meiro are listed here.", "", nil)
				return
			}
			m3.Text(c, m3.BodyMedium, historyRangeLabel(result)).TextColor(sc.OnSurfaceVariant)
			if result.Total == 0 {
				a.message(c, m3.IconSearch, "Nothing matches", "Try a different track, artist or range of days.", "", nil)
				return
			}
			if result.Pages > 1 {
				a.historyPager(c, result)
			}
			a.historyRows(c, result)
		})
	})
}

func (a *app) historyDayField(c *ui.Context, key, label, placeholder string, text *string) ui.Element {
	sc := m3.Of(c).Scheme
	return ui.TextInputBase(c.Key(key), text).Grow(1).Height(48).Padding(12).Radius(m3.Medium).
		Background(sc.SurfaceContainerHigh).TextColor(sc.OnSurface).FontSize(16).
		Label(label).Placeholder(placeholder)
}

func (a *app) historyRows(c *ui.Context, result history.Page) {
	sc := m3.Of(c).Scheme
	ui.Column(c).Gap(4).Children(func() {
		for i, entry := range result.Entries {
			name := entry.Title
			if name == "" {
				name = "Unknown track"
			}
			key := fmt.Sprintf("hist-%d-%d", result.Page, i)
			row := ui.ButtonBase(c.Key(key)).Padding(8, 4).Radius(m3.Medium).
				Cursor(ui.CursorPointer).Label(name).AlignItems(ui.Center)
			row.Background(m3.StateFill(ui.Transparent, sc.OnSurface, row.Hovered(), row.Pressed(), row.FocusVisible()))
			var anchor ui.Element
			var has bool
			row.Children(func() {
				ui.Column(c).Grow(1).MinWidth(0).Children(func() {
					m3.EmphasizedText(c, m3.BodyLarge, name).SingleLine()
					if entry.Artist != "" {
						m3.Text(c, m3.BodySmall, entry.Artist).SingleLine().TextColor(sc.OnSurfaceVariant)
					}
				})
				m3.Text(c, m3.BodySmall, entry.PlayedAt.Local().Format("Jan 2, 2006 3:04 PM")).
					Shrink(0).TextColor(sc.OnSurfaceVariant)
				anchor, has = a.historyMenu.pointerAnchor(c, key)
			})
			pointer := row.RightClicked()
			if pointer || row.Shortcut(ui.Shift, ui.KeyF10) {
				x, y, _ := row.PointerPosition()
				a.historyMenu.show(key, pointer, x, y)
			}
			if a.historyMenu.owns(key) {
				a.historyListMenu(c, a.historyMenu.anchor(row, anchor, has), key, entry)
			}
		}
	})
}

// historyListMenu queues a play from the listening history.
func (a *app) historyListMenu(c *ui.Context, anchor ui.Element, key string, entry history.Entry) {
	if !a.historyMenu.owns(key) {
		return
	}
	item := historyTrack(entry)
	m3.Menu(c, anchor, &a.historyMenu.open, 200, func() {
		if m3.MenuItem(c, "Add Next", m3.IconNext).Clicked() {
			a.enqueue(item, true)
			a.historyMenu.close()
		}
		if m3.MenuItem(c, "Add to Queue", m3.IconQueue).Clicked() {
			a.enqueue(item, false)
			a.historyMenu.close()
		}
	})
}

func (a *app) historyPager(c *ui.Context, result history.Page) {
	ui.Row(c).Gap(12).AlignItems(ui.Center).Children(func() {
		if m3.Button(c, m3.ButtonSpec{
			Label: "Previous page", Kind: m3.Outlined, Size: m3.Small40, Key: "history-prev",
			Disabled: result.Page == 0,
		}).Clicked() {
			a.historyPageIndex = result.Page - 1
		}
		m3.Text(c, m3.BodyMedium, fmt.Sprintf("Page %d of %d", result.Page+1, result.Pages)).
			TextColor(m3.Of(c).Scheme.OnSurfaceVariant)
		if m3.Button(c, m3.ButtonSpec{
			Label: "Next page", Kind: m3.Outlined, Size: m3.Small40, Key: "history-next",
			Disabled: result.Page+1 >= result.Pages,
		}).Clicked() {
			a.historyPageIndex = result.Page + 1
		}
	})
}

func historyRangeLabel(result history.Page) string {
	if result.Total == 0 {
		return "No plays match."
	}
	if result.Pages == 1 {
		if result.Total == 1 {
			return "1 play"
		}
		return strconv.Itoa(result.Total) + " plays"
	}
	first := result.Page*history.PageSize + 1
	last := first + len(result.Entries) - 1
	return fmt.Sprintf("%d–%d of %d", first, last, result.Total)
}
