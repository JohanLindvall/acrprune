package progress

import (
	"fmt"
	"log/slog"
	"time"

	"github.com/JohanLindvall/crprune/internal/tui"
	"github.com/dustin/go-humanize"
	"github.com/gdamore/tcell/v2"
)

// draw renders a snapshot, recording the activity panel's geometry in v.
func draw(screen tcell.Screen, s Snapshot, opts Options, v *viewState, now time.Time) {
	t := tui.NewTheme(v.color)
	screen.SetStyle(t.Base)
	screen.Clear()
	w, h := screen.Size()
	if w < 1 || h < 1 {
		return
	}
	put := func(y int, style tcell.Style, text string) { tui.Text(screen, 1, y, w-2, style, text) }
	mode, modeStyle := "STATISTICS", t.Accent
	if opts.Operation == "prune" {
		mode, modeStyle = "LIVE DELETE", t.Danger
		if opts.DryRun {
			mode, modeStyle = "DRY RUN", t.Good
		}
	}
	tui.Fill(screen, 1, 0, w-2, 1, t.Panel)
	put(0, t.OnPanel(modeStyle), "CRPRUNE  /  "+mode)
	put(1, t.Muted, opts.Registry)
	elapsed := max(now.Sub(s.Started), 0).Round(time.Second)
	phase := s.Phase
	if phase == "" {
		phase = "Waiting for progress"
	}
	if v.canceling {
		phase = "Stopping: waiting for in-flight requests"
	}
	spinner := []rune("◐◓◑◒")[now.UnixMilli()/125%4]
	if w < 45 || h < 18 {
		put(3, t.Accent, fmt.Sprintf("%c %s", spinner, phase))
		put(4, t.Base, s.Repository)
		put(5, t.Base, fmt.Sprintf("%d/%d repos  %d fetched", s.Completed, s.Repositories, s.Fetched))
		if h >= 10 {
			if opts.Operation == "prune" {
				put(7, modeStyle, fmt.Sprintf("Selected %d  Deleted %d", s.Selected, s.Deleted))
			} else {
				put(7, modeStyle, fmt.Sprintf("Scanned %d  Unique %s", s.Kept, humanize.Bytes(s.Bytes)))
			}
		}
		put(h-1, t.Muted, "q cancel  |  enlarge for details")
	} else {
		put(3, t.Base.Bold(true), fmt.Sprintf("%d / %d repositories    elapsed %s", s.Completed, s.Repositories, elapsed))
		fraction := 0.0
		if s.Repositories > 0 {
			fraction = float64(s.Completed) / float64(s.Repositories)
		}
		tui.Meter(screen, 1, 4, w-10, fraction, t.Accent, t.Border)
		tui.Text(screen, w-8, 4, 6, t.Accent, fmt.Sprintf("%5.1f%%", fraction*100))
		current := s.Repository
		if current == "" {
			current = "Waiting for repository listing"
		}
		put(6, t.Base.Bold(true), current)
		put(7, t.Accent, fmt.Sprintf("%c %s", spinner, phase))
		put(8, t.Muted, fmt.Sprintf("Fetched %d / %d listed manifests    cache hits %d", s.Fetched, s.Listed, s.Cached))
		tui.Fill(screen, 1, 9, w-2, 2, t.Panel)
		if opts.Operation == "prune" {
			put(9, t.OnPanel(t.Good), fmt.Sprintf("KEEP  %d     SELECTED  %d     DELETED  %d", s.Kept, s.Selected, s.Deleted))
			put(10, t.OnPanel(t.Muted), "Estimated removable blobs  "+humanize.Bytes(s.Bytes)+"  (within repositories)")
		} else {
			put(9, t.OnPanel(t.Good), fmt.Sprintf("SCANNED  %d manifests     UNIQUE  %s", s.Kept, humanize.Bytes(s.Bytes)))
			put(10, t.OnPanel(t.Muted), "Completed repositories are included in the statistics snapshot")
		}
		style := t.Muted
		if s.Warnings+s.Denied+s.Skipped > 0 {
			style = t.Warn
		}
		put(12, style, fmt.Sprintf("Warnings %d    denied %d    skipped %d", s.Warnings, s.Denied, s.Skipped))
		if s.RetryUntil.After(now) {
			put(13, t.Warn, "API retry scheduled in "+s.RetryUntil.Sub(now).Round(time.Second).String()+"; other requests may continue")
		}
		if v.logs && !v.help && h > 18 {
			title := "ACTIVITY"
			entries, first := s.Logs, s.DroppedLogs
			if v.warningsOnly {
				title += " · warnings only"
				entries, first = s.WarningLogs, s.DroppedWarnings
			}
			rows := h - 18
			end := v.pin(first, first+len(entries), rows) - first
			if v.scrolled {
				title += " · scrolled (End to follow)"
			} else {
				title += " · following"
			}
			tui.Rule(screen, 1, 15, w-2, t, title)
			if len(entries) == 0 {
				message := "Waiting for activity…"
				if v.warningsOnly {
					message = "No warnings so far"
				}
				put(16, t.Muted, message)
			}
			for i, entry := range entries[max(0, end-rows):end] {
				style := t.Base
				if entry.Level >= slog.LevelWarn {
					style = t.Warn
				}
				if entry.Level >= slog.LevelError {
					style = t.Danger
				}
				tui.Text(screen, 1, 16+i, 8, t.Muted, entry.Time.Format("15:04:05"))
				tui.Text(screen, 10, 16+i, w-11, style, fmt.Sprintf("%-5s  %s", entry.Level, entry.Text))
			}
		}
		put(h-1, t.Muted, "q cancel   l activity   w warnings   ↑/↓ scroll   End follow   ? help")
	}
	if v.help {
		tui.Dialog(screen, t, "KEYBOARD", []string{
			"q / Ctrl-C  Cancel safely and wait for active requests to stop",
			"l           Show or hide activity", "w           Toggle warnings-only activity",
			"↑ ↓ / j k   Scroll activity; PgUp/PgDn move a page",
			"Home / End  Oldest activity / follow latest", "? / Esc     Show / close help", "",
			"JSON stays on stdout. Progress uses the controlling terminal.",
			fmt.Sprintf("Retains the latest %d messages and %d warnings; --progress=plain keeps all.", logLimit, warningLimit),
		})
	}
	screen.Show()
}

var drawText = tui.Text
