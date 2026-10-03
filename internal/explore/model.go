package explore

import (
	"slices"
	"strings"

	"github.com/JohanLindvall/crprune/internal/pruner"
	"github.com/JohanLindvall/crprune/internal/tui"
	"github.com/gdamore/tcell/v2"
	"github.com/rivo/uniseg"
)

var sortKeys = []string{"unique", "total", "shared", "count", "untagged", "tagged", "running", "newest", "oldest", "name"}
var manifestSortKeys = []string{"newest", "total", "name"}

// listState keeps selection by identity, independently of filtering and sorting.
type listState struct {
	data, visible               []pruner.RepositoryStats
	labels                      map[string]string
	marked                      map[string]bool
	sortKey, query, beforeQuery string
	reverse, searching          bool
	cursor, offset, rows        int
}

func newList(data []pruner.RepositoryStats, key, query string, reverse bool) listState {
	l := listState{data: slices.Clone(data), marked: map[string]bool{}, sortKey: key, query: query, reverse: reverse}
	l.refilter("")
	return l
}

func (l *listState) current() string {
	if len(l.visible) == 0 {
		return ""
	}
	return l.visible[l.cursor].Name
}

func (l *listState) refilter(keep string) {
	l.visible = nil
	query := strings.ToLower(l.query)
	for _, s := range l.data {
		if strings.Contains(strings.ToLower(s.Name+" "+l.labels[s.Name]), query) {
			l.visible = append(l.visible, s)
		}
	}
	// Name breaks ties deterministically, independent of the input file order.
	_ = pruner.SortStatsBy(l.visible, "name")
	_ = pruner.SortStatsBy(l.visible, l.sortKey)
	if l.reverse {
		slices.Reverse(l.visible)
	}
	l.cursor = 0
	if i := slices.IndexFunc(l.visible, func(s pruner.RepositoryStats) bool { return s.Name == keep }); i >= 0 {
		l.cursor = i
	}
	l.clamp()
}

func (l *listState) clamp() {
	l.cursor = max(0, min(l.cursor, len(l.visible)-1))
	rows := max(1, l.rows)
	l.offset = max(0, min(l.offset, len(l.visible)-rows))
	if l.cursor < l.offset {
		l.offset = l.cursor
	}
	if l.cursor >= l.offset+rows {
		l.offset = l.cursor - rows + 1
	}
}

func (l *listState) move(delta int) { l.cursor += delta; l.clamp() }

func (l *listState) selected() []string {
	var selected []string
	for name := range l.marked {
		selected = append(selected, name)
	}
	if len(selected) == 0 && l.current() != "" {
		selected = append(selected, l.current())
	}
	slices.Sort(selected)
	return selected
}

func (l *listState) toggle() {
	if name := l.current(); name != "" {
		if l.marked[name] {
			delete(l.marked, name)
		} else {
			l.marked[name] = true
		}
	}
}

func (l *listState) toggleVisible() {
	all := len(l.visible) > 0
	for _, s := range l.visible {
		all = all && l.marked[s.Name]
	}
	for _, s := range l.visible {
		if all {
			delete(l.marked, s.Name)
		} else {
			l.marked[s.Name] = true
		}
	}
}

func (l *listState) cycleSort(keys []string, delta int) {
	i := max(0, slices.Index(keys, l.sortKey))
	l.sortKey = keys[(i+delta+len(keys))%len(keys)]
	l.refilter(l.current())
}

func (l *listState) navigate(key *tcell.EventKey) bool {
	switch {
	case key.Key() == tcell.KeyUp || key.Rune() == 'k':
		l.move(-1)
	case key.Key() == tcell.KeyDown || key.Rune() == 'j':
		l.move(1)
	case key.Key() == tcell.KeyPgUp:
		l.move(-max(l.rows, 1))
	case key.Key() == tcell.KeyPgDn:
		l.move(max(l.rows, 1))
	case key.Key() == tcell.KeyHome || key.Rune() == 'g':
		l.cursor = 0
		l.clamp()
	case key.Key() == tcell.KeyEnd || key.Rune() == 'G':
		l.cursor = len(l.visible) - 1
		l.clamp()
	default:
		return false
	}
	return true
}

func editText(value *string, key *tcell.EventKey) {
	switch key.Key() {
	case tcell.KeyBackspace, tcell.KeyBackspace2:
		g, start := uniseg.NewGraphemes(*value), 0
		for g.Next() {
			start, _ = g.Positions()
		}
		*value = (*value)[:start]
	case tcell.KeyCtrlU:
		*value = ""
	case tcell.KeyRune:
		text := string(key.Rune())
		if tui.Clean(text) == text && len(*value)+len(text) <= 4096 {
			*value += text
		}
	}
}
