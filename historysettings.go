package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"

	"github.com/elianiva/meiro/internal/fsutil"
	"github.com/elianiva/meiro/internal/history"
	"github.com/elianiva/meiro/internal/settings"
	"github.com/elianiva/meiro/m3"
)

// historyCard is where the user sees what the listening history holds, how
// many plays to keep, and clears it. Clearing asks twice: it cannot be undone.
func (a *app) historyCard(c *ui.Context) {
	sc := m3.Of(c).Scheme
	a.settingsCard(c, "Listening history", m3.IconHistory, func() {
		ui.Row(c).Gap(16).AlignItems(ui.Center).Children(func() {
			body := fmt.Sprintf("%d plays are kept on this device, for the stats. Meiro keeps the latest %d.", len(a.history), a.historyLimit())
			if len(a.history) == 0 {
				body = "No plays are kept on this device."
			}
			settingLabel(c, "Plays", body).Grow(1)
			switch {
			case len(a.history) == 0:
				a.confirmClearHistory = false
				m3.Button(c, m3.ButtonSpec{Label: "Clear history", Kind: m3.Tonal, Size: m3.Medium56, Disabled: true, Key: "clear-history"})
			case !a.confirmClearHistory:
				if m3.Button(c, m3.ButtonSpec{Label: "Clear history", Kind: m3.Tonal, Size: m3.Medium56, Key: "clear-history"}).Clicked() {
					a.confirmClearHistory = true
				}
			default:
				ui.Row(c).Gap(8).Children(func() {
					if m3.Button(c, m3.ButtonSpec{Label: "Keep", Kind: m3.Outlined, Size: m3.Medium56, Key: "keep-history"}).Clicked() {
						a.confirmClearHistory = false
					}
					if m3.Button(c, m3.ButtonSpec{Label: "Delete all plays", Kind: m3.Filled, Size: m3.Medium56, Key: "confirm-clear-history"}).Clicked() {
						a.confirmClearHistory = false
						a.clearHistory()
					}
				})
			}
		})
		ui.Column(c).Gap(8).Children(func() {
			settingLabel(c, "Plays to keep", "Enter a whole number and press Enter. Older plays are dropped first. Up to "+strconv.Itoa(settings.MaxHistoryLimit)+".")
			ui.Row(c).Gap(12).AlignItems(ui.Center).Children(func() {
				input := ui.TextInputBase(c.Key("history-limit"), &a.historyLimitText).
					Width(128).Height(48).Shrink(0).Padding(12).Radius(m3.Medium).Background(sc.SurfaceContainerHigh).
					TextColor(sc.OnSurface).FontSize(16).Label("Number of plays to keep").Placeholder(strconv.Itoa(history.MaxEntries))
				m3.Text(c, m3.BodyMedium, "plays").Shrink(0).TextColor(sc.OnSurfaceVariant)
				if input.Submitted() {
					n, err := strconv.Atoi(strings.TrimSpace(a.historyLimitText))
					if err != nil || n < 1 || n > settings.MaxHistoryLimit {
						a.historyLimitError = "Enter a whole number from 1 to " + strconv.Itoa(settings.MaxHistoryLimit) + "."
					} else {
						a.historyLimitError = ""
						a.setHistoryLimit(n)
					}
				}
			})
			if a.historyLimitError != "" {
				m3.Text(c, m3.BodyMedium, a.historyLimitError).TextColor(sc.Error)
			}
		})
		ui.Row(c).Gap(16).AlignItems(ui.Center).Children(func() {
			settingLabel(c, "Listening stats", "Plays per day, most played tracks and artists.").Grow(1)
			if m3.Button(c, m3.ButtonSpec{Label: "Show stats", Kind: m3.Tonal, Size: m3.Medium56, Key: "show-stats"}).Clicked() {
				a.navigate(routeStats)
			}
		})
		ui.Row(c).Gap(16).AlignItems(ui.Center).Children(func() {
			settingLabel(c, "Every play", "The track, the artist and when it played. A long list is split into pages.").Grow(1)
			if m3.Button(c, m3.ButtonSpec{Label: "Show history", Kind: m3.Tonal, Size: m3.Medium56, Key: "show-history"}).Clicked() {
				a.navigate(routeHistory)
			}
		})
		ui.Row(c).Gap(16).AlignItems(ui.Center).Children(func() {
			settingLabel(c, "Export", "A spreadsheet of every play kept on this device.").Grow(1)
			if m3.Button(c, m3.ButtonSpec{
				Label: "Export history", Kind: m3.Tonal, Size: m3.Medium56, Key: "export-history",
				Disabled: len(a.history) == 0 || !a.historyLoaded || a.historyExporting,
			}).Clicked() {
				a.exportHistory()
			}
		})
	})
}

// exportHistory asks where to save a spreadsheet of every play kept on
// this device, then writes it. Cancelling the dialog leaves no file.
func (a *app) exportHistory() {
	if a.historyExporting || !a.historyLoaded || len(a.history) == 0 || a.pickSavePath == nil {
		return
	}
	entries := slices.Clone(a.history)
	home, _ := os.UserHomeDir()
	name := "meiro-history.csv"
	if home != "" {
		name = filepath.Join(home, name)
	}
	parent := a.desktop.window()
	a.historyExporting = true
	type saved struct {
		path string
		err  string
	}
	spawn(a, func() saved {
		path, err := a.pickSavePath(mygo.SaveDialogOptions{
			Parent: parent, Title: "Export listening history", DefaultPath: name,
			ButtonLabel: "Export", CreateDirectories: true,
			Filters: []mygo.FileFilter{{Name: "CSV", Extensions: []string{"csv"}}},
		})
		if err != nil {
			return saved{err: err.Error()}
		}
		if path == "" {
			return saved{}
		}
		if !strings.HasSuffix(strings.ToLower(path), ".csv") {
			path += ".csv"
		}
		var buf bytes.Buffer
		if err := history.WriteCSV(&buf, entries); err != nil {
			return saved{err: err.Error()}
		}
		if err := fsutil.WriteFileAtomic(path, buf.Bytes(), 0o644); err != nil {
			return saved{err: err.Error()}
		}
		return saved{path: path}
	}, func(s saved) {
		a.historyExporting = false
		if s.err != "" {
			a.notice = "Could not export listening history: " + s.err
			return
		}
		if s.path == "" {
			return
		}
		plays := "plays"
		if len(entries) == 1 {
			plays = "play"
		}
		a.notice = fmt.Sprintf("Exported %d %s.", len(entries), plays)
	})
}
