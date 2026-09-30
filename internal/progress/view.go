package progress

import (
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/dustin/go-humanize"
	"github.com/gdamore/tcell/v2"
	"github.com/rivo/uniseg"
)

// draw renders a snapshot, recording the activity panel's geometry in v.
func draw(screen tcell.Screen, s Snapshot, opts Options, v *viewState, now time.Time) {
	screen.Clear()
	w, h := screen.Size()
	if w < 1 || h < 1 {
		return
	}
	base := tcell.StyleDefault
	accent, good, warn := base.Bold(true), base, base.Bold(true)
	if v.color {
		accent = accent.Foreground(tcell.ColorTeal)
		good = good.Foreground(tcell.ColorGreen)
		warn = warn.Foreground(tcell.ColorOlive)
	}
	put := func(y int, style tcell.Style, text string) { drawText(screen, 1, y, w-2, style, text) }
	mode := "STATISTICS"
	if opts.Operation == "prune" {
		mode = "LIVE DELETE"
		if opts.DryRun {
			mode = "DRY RUN"
		}
	}
	put(0, accent, "ACRPRUNE  /  "+mode+"  /  "+opts.Registry)
	elapsed := max(now.Sub(s.Started), 0).Round(time.Second)
	phase := s.Phase
	if v.canceling {
		phase = "Stopping: waiting for in-flight requests"
	}
	spinner := []rune("◐◓◑◒")[now.UnixMilli()/125%4]
	put(2, base.Bold(true), fmt.Sprintf("%c %s", spinner, phase))
	if w < 45 || h < 12 {
		put(3, base, s.Repository)
		put(4, base, fmt.Sprintf("%d/%d repos  %d fetched", s.Completed, s.Repositories, s.Fetched))
		put(h-1, warn, "q cancel  |  enlarge for details")
		screen.Show()
		return
	}
	put(3, base, fmt.Sprintf("%d / %d repositories    elapsed %s", s.Completed, s.Repositories, elapsed))
	barWidth := min(w-12, 70)
	filled := 0
	if s.Repositories > 0 {
		filled = min(barWidth, barWidth*s.Completed/s.Repositories)
	}
	put(4, accent, "["+strings.Repeat("━", filled)+strings.Repeat("─", barWidth-filled)+"]")
	current := s.Repository
	if current == "" {
		current = "Waiting for repository listing"
	}
	put(6, base.Bold(true), current)
	put(7, base, fmt.Sprintf("Fetched %d / %d listed manifests    cache hits %d", s.Fetched, s.Listed, s.Cached))
	if opts.Operation == "prune" {
		put(9, good, fmt.Sprintf("KEEP  %d     SELECTED  %d     DELETED  %d", s.Kept, s.Selected, s.Deleted))
		put(10, base, "Estimated removable blobs  "+humanize.Bytes(s.Bytes)+"  (within repositories)")
	} else {
		put(9, good, fmt.Sprintf("SCANNED  %d manifests     UNIQUE  %s", s.Kept, humanize.Bytes(s.Bytes)))
		put(10, base, "Each completed repository is included in the statistics snapshot")
	}
	put(12, warn, fmt.Sprintf("Warnings %d    denied %d    skipped %d", s.Warnings, s.Denied, s.Skipped))
	if s.RetryUntil.After(now) {
		put(13, warn, "API retry scheduled in "+s.RetryUntil.Sub(now).Round(time.Second).String()+"; other requests may continue")
	}
	footer := "q cancel   l activity   w warnings   ↑/↓ scroll   End follow   ? help"
	put(h-1, base, footer)
	if v.help {
		lines := []string{
			"KEYBOARD", "q / Ctrl-C  Cancel safely and wait for active requests to stop",
			"l           Show or hide activity", "w           Toggle warnings-only activity",
			"↑ ↓ / j k   Scroll activity; PgUp/PgDn move a page",
			"Home / End  Oldest activity / follow latest", "? / Esc     Show / close help",
			"JSON stays on stdout. Progress uses the controlling terminal.",
			fmt.Sprintf("Retains the latest %d messages and %d warnings; --progress=plain keeps all.", logLimit, warningLimit),
		}
		for i, line := range lines {
			if y := 15 + i; y < h-2 {
				put(y, base, line)
			}
		}
	} else if v.logs && h > 18 {
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
		}
		put(15, accent, title)
		for i, entry := range entries[max(0, end-rows):end] {
			style := base
			if entry.Level >= slog.LevelWarn {
				style = warn
			}
			put(16+i, style, entry.Time.Format("15:04:05")+" "+entry.Level.String()+"  "+entry.Text)
		}
	}
	screen.Show()
}

// drawText writes one line of text from x, clipped to width cells and to the
// screen, and ends it with an ellipsis when it does not fit.
func drawText(screen tcell.Screen, x, y, width int, style tcell.Style, text string) {
	w, h := screen.Size()
	if y < 0 || y >= h || width < 1 {
		return
	}
	end := min(x+width, w)
	text = clean(text)
	truncated := uniseg.StringWidth(text) > end-x
	if truncated {
		end-- // reserve the final cell for an ellipsis
	}
	graphemes := uniseg.NewGraphemes(text)
	for graphemes.Next() {
		cw := graphemes.Width()
		if cw == 0 {
			continue
		}
		if x+cw > end {
			break
		}
		runes := graphemes.Runes()
		screen.SetContent(x, y, runes[0], runes[1:], style)
		x += cw
	}
	if truncated {
		screen.SetContent(x, y, '…', nil, style)
	}
}
