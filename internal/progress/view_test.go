package progress

import (
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/uniseg"
)

// simScreen returns an initialized simulated terminal of the given size.
func simScreen(t *testing.T, w, h int) tcell.SimulationScreen {
	t.Helper()
	screen := tcell.NewSimulationScreen("UTF-8")
	if err := screen.Init(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(screen.Fini)
	screen.SetSize(w, h)
	return screen
}

// rowText returns the first n cells of a row, a wide character once for the
// two cells it covers.
func rowText(screen tcell.SimulationScreen, y, n int) string {
	cells, w, _ := screen.GetContents()
	var text strings.Builder
	for x := 0; x < n; x++ {
		runes := cells[y*w+x].Runes
		text.WriteString(string(runes))
		if uniseg.StringWidth(string(runes)) == 2 {
			x++
		}
	}
	return text.String()
}

// TestDrawTextTruncates: text is cut to its width at a character boundary,
// wide characters included, with an ellipsis in the last cell it uses, and
// never reaches past its width.
func TestDrawTextTruncates(t *testing.T) {
	tests := []struct {
		text  string
		width int
		want  string // the cells up to one past the width; untouched ones are dots
	}{
		{"hello", 7, "hello..."},
		{"hello world", 5, "hell…."},
		{"界界界", 6, "界界界."},
		{"界界界", 5, "界界…."},
		{"a界界", 3, "a….."},                        // a wide character does not fit in the one cell left
		{"e\u0301e\u0301e\u0301", 2, "e\u0301…."}, // a combining mark stays with its letter
		{"👍🏽👍🏽", 3, "👍🏽…."},
		{"🇸🇪🇸🇪x", 4, "🇸🇪….."},
		{"\u0301abc", 3, "abc."}, // a lone combining mark takes no cell
		{"tab\there", 8, "tab here."},
		{"abc", 0, "."},
	}
	for _, tt := range tests {
		screen := simScreen(t, 20, 1)
		screen.Fill('.', tcell.StyleDefault)
		drawText(screen, 0, 0, tt.width, tcell.StyleDefault, tt.text)
		screen.Show()
		if got := rowText(screen, 0, tt.width+1); got != tt.want {
			t.Errorf("drawText(%q, width %d) = %q, want %q", tt.text, tt.width, got, tt.want)
		}
	}
}

// TestDrawKeepsBorders: long lines of any kind stop short of the right-hand
// column, as they start after the left-hand one.
func TestDrawKeepsBorders(t *testing.T) {
	now := time.Now()
	long := strings.Repeat("界a👍🏽", 60)
	s := Snapshot{Started: now, Repository: long, Phase: long, Repositories: 1, Logs: []Entry{{Time: now, Level: slog.LevelWarn, Text: long}}}
	for _, v := range []viewState{{logs: true}, {help: true}} {
		screen := simScreen(t, 80, 30)
		draw(screen, s, Options{Operation: "prune", Registry: long}, &v, now)
		cells, w, h := screen.GetContents()
		for y := range h {
			if left, right := cells[y*w].Runes, cells[y*w+w-1].Runes; len(left) > 0 && left[0] != ' ' || len(right) > 0 && right[0] != ' ' {
				t.Errorf("row %d reaches the border: %q", y, rowText(screen, y, w))
			}
		}
	}
}

func TestDrawVariants(t *testing.T) {
	now := time.Now()
	s := Snapshot{Started: now.Add(-time.Minute), Repository: "team/app", Repositories: 4, Completed: 1, Kept: 20, Bytes: 2_000_000,
		Warnings: 1, RetryUntil: now.Add(30 * time.Second),
		Logs:        []Entry{{Time: now, Level: slog.LevelInfo, Text: "Processed app"}},
		WarningLogs: []Entry{{Time: now, Level: slog.LevelWarn, Text: "Retrying request"}},
	}
	tests := []struct {
		name           string
		opts           Options
		view           viewState
		want, unwanted []string
	}{
		{"statistics", Options{Operation: "statistics"}, viewState{logs: true},
			[]string{"STATISTICS", "SCANNED  20 manifests", "UNIQUE  2.0 MB", "Processed app"}, []string{"SELECTED"}},
		{"live delete", Options{Operation: "prune"}, viewState{logs: true},
			[]string{"LIVE DELETE", "KEEP  20", "API retry scheduled in 30s"}, nil},
		{"help", Options{Operation: "prune", DryRun: true}, viewState{logs: true, help: true},
			[]string{"KEYBOARD", "PgUp/PgDn move a page", "200 messages and 100 warnings"}, []string{"ACTIVITY"}},
		{"warnings only", Options{Operation: "prune"}, viewState{logs: true, warningsOnly: true},
			[]string{"ACTIVITY · warnings only", "Retrying request"}, []string{"Processed app", "scrolled"}},
		{"activity hidden", Options{Operation: "prune"}, viewState{},
			nil, []string{"ACTIVITY", "Processed app"}},
		{"canceling", Options{Operation: "prune"}, viewState{canceling: true},
			[]string{"Stopping: waiting for in-flight requests"}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			screen := simScreen(t, 100, 30)
			draw(screen, s, tt.opts, &tt.view, now)
			text := screenText(screen)
			for _, want := range tt.want {
				if !strings.Contains(text, want) {
					t.Errorf("missing %q:\n%s", want, text)
				}
			}
			for _, unwanted := range tt.unwanted {
				if strings.Contains(text, unwanted) {
					t.Errorf("unexpected %q:\n%s", unwanted, text)
				}
			}
		})
	}
}

var entryPattern = regexp.MustCompile(`entry \d+`)

// logEntries returns activity entries "entry <from>" to "entry <to-1>".
func logEntries(from, to int) []Entry {
	var entries []Entry
	for i := from; i < to; i++ {
		entries = append(entries, Entry{Level: slog.LevelInfo, Text: fmt.Sprintf("entry %02d", i)})
	}
	return entries
}

// TestActivityScrolling: the activity panel scrolls within its entries by
// line and by page, and a scrolled view stays on the entries it shows while
// others arrive.
func TestActivityScrolling(t *testing.T) {
	screen := simScreen(t, 100, 30) // 12 rows of activity
	v := &viewState{logs: true}
	s := Snapshot{Logs: logEntries(0, 50)}
	press := func(key tcell.Key, r rune) {
		v.key(tcell.NewEventKey(key, r, tcell.ModNone))
	}
	shows := func(step, first, last string, scrolled bool) {
		t.Helper()
		draw(screen, s, Options{Operation: "prune"}, v, time.Now())
		text := screenText(screen)
		visible := entryPattern.FindAllString(text, -1)
		if len(visible) == 0 || visible[0] != first || visible[len(visible)-1] != last || strings.Contains(text, "scrolled") != scrolled {
			t.Fatalf("%s: shows %v, scrolled=%v; want %s to %s, scrolled=%v", step, visible, strings.Contains(text, "scrolled"), first, last, scrolled)
		}
	}

	shows("following", "entry 38", "entry 49", false)
	press(tcell.KeyHome, 0)
	shows("Home", "entry 00", "entry 11", true)
	press(tcell.KeyRune, 'j')
	shows("j after Home", "entry 01", "entry 12", true)
	press(tcell.KeyPgDn, 0)
	shows("PgDn", "entry 13", "entry 24", true)
	press(tcell.KeyPgUp, 0)
	shows("PgUp", "entry 01", "entry 12", true)
	press(tcell.KeyUp, 0)
	press(tcell.KeyRune, 'k') // already at the top
	press(tcell.KeyDown, 0)
	shows("↓ after scrolling past the top", "entry 01", "entry 12", true)
	press(tcell.KeyPgDn, 0)
	press(tcell.KeyPgDn, 0)
	shows("two pages down", "entry 25", "entry 36", true)

	s = Snapshot{Logs: logEntries(10, 70), DroppedLogs: 10}
	shows("entries arrived and the oldest were dropped", "entry 25", "entry 36", true)
	s = Snapshot{Logs: logEntries(30, 90), DroppedLogs: 30}
	shows("the entries shown were dropped", "entry 30", "entry 41", true)

	press(tcell.KeyEnd, 0)
	shows("End", "entry 78", "entry 89", false)
	press(tcell.KeyPgDn, 0)
	shows("PgDn while following", "entry 78", "entry 89", false)
	press(tcell.KeyPgUp, 0)
	press(tcell.KeyPgDn, 0)
	shows("back down to the newest entry", "entry 78", "entry 89", false)

	// Warnings-only has its own entries; switching follows them.
	press(tcell.KeyPgUp, 0)
	press(tcell.KeyRune, 'w')
	s.WarningLogs = logEntries(0, 3)
	shows("warnings only", "entry 00", "entry 02", false)
	press(tcell.KeyUp, 0)
	shows("nothing to scroll", "entry 00", "entry 02", false)
	press(tcell.KeyEsc, 0)
	press(tcell.KeyRune, '?')
	press(tcell.KeyRune, 'l')
	if !v.help || v.logs || !v.warningsOnly {
		t.Errorf("state = %+v", *v)
	}
	if press := v.key(tcell.NewEventKey(tcell.KeyCtrlC, 0, tcell.ModCtrl)); !press || !v.canceling {
		t.Error("Ctrl-C should cancel")
	}
}
