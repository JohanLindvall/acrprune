package explore

import (
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/JohanLindvall/crprune/internal/pruner"
	"github.com/JohanLindvall/crprune/internal/tui"
	"github.com/dustin/go-humanize"
	"github.com/gdamore/tcell/v2"
	"github.com/rivo/uniseg"
)

func (a *app) draw(screen tcell.Screen, now time.Time) {
	t := tui.NewTheme(os.Getenv("NO_COLOR") == "")
	screen.SetStyle(t.Base)
	screen.Clear()
	w, h := screen.Size()
	a.reviewable = w >= 60 && h >= 18
	if w <= 0 || h <= 0 {
		return
	}
	put := func(y int, style tcell.Style, text string) { tui.Text(screen, 1, y, w-2, style, text) }
	tui.Fill(screen, 1, 0, w-2, 1, t.Panel)
	put(0, t.OnPanel(t.Accent), "CRPRUNE  /  STATS EXPLORER")
	mode := "LOCAL SNAPSHOT"
	if a.opts.Client != nil {
		mode = a.opts.Client.Registry
	}
	if w >= 65 {
		tui.Text(screen, max(30, w-2-uniseg.StringWidth(mode)), 0, min(w-32, uniseg.StringWidth(mode)), t.OnPanel(t.Muted), mode)
	}
	put(1, t.Muted, a.opts.Source)
	if a.repository != "" {
		put(1, t.Base.Bold(true), "REPOSITORY IMAGES  /  "+a.repository)
	}
	if a.stale {
		put(2, t.Warn, "Snapshot may be stale after live changes; rescan to refresh statistics")
	}
	switch {
	case a.job != nil:
		a.drawJob(screen, t, now)
	case a.panel == "plan" || a.panel == "confirm":
		a.drawPlan(screen, t)
		if a.panel == "confirm" {
			a.drawConfirmation(screen, t)
		}
	case a.panel == "message" || a.panel == "rule-details" || a.panel == "repository-details":
		a.drawPage(screen, t, a.message)
	case a.panel == "rule-picker":
		a.drawRulePicker(screen, t)
	case a.panel == "repositories":
		a.drawRepositorySelector(screen, t)
	case a.panel == "help":
		a.drawPage(screen, t, []string{
			"KEYBOARD", "",
			"↑ ↓ / j k    Move through repositories, manifests, or preview targets",
			"PgUp / PgDn  Move a page; Home / End jumps to the first / last row",
			"/            Search names, digests, tags, platforms, or image types",
			"Enter / Esc  Accept search / restore previous search; Ctrl-U clears input",
			"s            Choose a sort column; ← / → or Tab cycles columns",
			"r            Reverse sort order; R reloads statistics or live manifests",
			"Enter        Open repository images, or full details for an image",
			"i            Show repository statistics (also available without --registry)",
			"Space        Mark / unmark a row",
			"a / c        Toggle all visible rows / clear all marks",
			"S            Add repository selections by wildcard or regex",
			"m            Browse live images from a repository row or its statistics",
			"d            Preview deletion of marked rows, or the current row",
			"l / L        Choose bundled or local rules / load a JSON rule file",
			"p / P        Preview rules for the current repo / marked repositories",
			"? / Esc      Help / back; q quits; Ctrl-C interrupts", "",
			"Repository images include every listed manifest: tagged and untagged images, multiarch indexes, their platform children, and artifacts or referrers. Enter shows all tags, platforms, full digests, index children and parent indexes. Search for untagged, multiarch, or linux/arm64 to narrow the list. Image details require --registry; statistics files contain only aggregate counts.", "",
			"The rule picker includes bundled examples and JSON files in --rules-dir (default ./rules). Enter selects one complete file; i shows its full descriptions and repository patterns. If no rules are selected, p or P opens the picker before preparing a preview.", "",
			"Use ↑/↓ and Space to select repositories, then P to preview rules for every marked repository, including hidden selections. S adds selections by wildcard (* and ?) or regex (Tab switches mode). Matches come from the full snapshot. With no marks, P opens the pattern selector directly; the default * matches all. A loaded rule only applies where its own repo pattern matches.", "",
			"Deletion requires --registry, a fresh preview, and a typed confirmation. Image indexes select their children too; dependencies of kept images stay protected. Recent images, running images and locks use the same protection as prune. Esc cancels an active operation and waits for requests and lock restores, keeping the previous view. Canceled deletions report confirmed progress and require a new preview.", "",
			"Unique bytes are charged to the first repository that referenced each blob during the original scan. Filtering does not recompute deduplication. Shared means repeated references, not reclaimable bytes. A running count of zero can mean no --running inventory was supplied. Deleting a tag's manifest removes ALL of its tags.", "",
			"This screen never rewrites your statistics file. After deletion, use R in the repository view to rescan live statistics, or run stats again for a file snapshot.",
		})
	case a.panel == "sort":
		lines := []string{"↑/↓ choose · Enter apply", ""}
		for i, key := range a.keys() {
			prefix := "  "
			if i == a.sortCursor {
				prefix = "› "
			}
			lines = append(lines, prefix+key)
		}
		a.drawList(screen, t)
		tui.Dialog(screen, t, "SORT BY", lines)
	case a.panel == "rules":
		a.drawList(screen, t)
		tui.Dialog(screen, t, "LOAD RULE FILE", []string{"Path to a crprune JSON rule file:", "> " + a.input + "▏", "", "Enter loads and validates · Esc cancels", "p applies to current repo · P to marked repos or a pattern", a.status})
	default:
		a.drawList(screen, t)
	}
	screen.Show()
}

func (a *app) drawList(screen tcell.Screen, t tui.Theme) {
	w, h := screen.Size()
	l := a.list()
	var total, unique uint64
	var count, tagged, untagged, multiarch, children int
	for _, s := range l.visible {
		total += s.Total
		unique += s.Unique
		count += s.Count
		tagged += s.Tagged
		untagged += s.Untagged
		if a.repository != "" {
			if manifestKind(a.byDigest[s.Name]) == "multiarch" {
				multiarch++
			}
			if len(a.parents[s.Name]) > 0 {
				children++
			}
		}
	}
	top := 9
	if h >= 18 && w >= 55 {
		cardWidth := (w - 4) / 3
		labels := []string{"REPOSITORIES", "UNIQUE BYTES", "MANIFESTS"}
		values := []string{fmt.Sprintf("%d / %d", len(l.visible), len(l.data)), humanize.Bytes(unique), humanize.Comma(int64(count))}
		notes := []string{fmt.Sprintf("%d marked · %d matching", len(l.marked), len(l.visible)), fmt.Sprintf("%s total · %.0f%% shared", humanize.Bytes(total), 100*(1-fraction(unique, total))), fmt.Sprintf("%d tagged · %d untagged", tagged, untagged)}
		if cardWidth < 30 {
			notes[1] = humanize.Bytes(total) + " total"
			notes[2] = fmt.Sprintf("%d untagged", untagged)
		}
		if total == 0 {
			notes[1] = "0 B total · 0% shared"
		}
		if a.repository != "" {
			labels[0], labels[1] = "MATCHING MANIFESTS", "REFERENCED BYTES"
			values[1] = humanize.Bytes(total)
			notes[1] = "Includes repeated blobs"
			labels[2], values[2] = "TAGGED / UNTAGGED", fmt.Sprintf("%d / %d", tagged, untagged)
			notes[2] = fmt.Sprintf("%d multiarch, %d children", multiarch, children)
		}
		for i := range 3 {
			x := 1 + i*(cardWidth+1)
			tui.Fill(screen, x, 3, cardWidth, 3, t.Panel)
			tui.Text(screen, x+1, 3, cardWidth-2, t.OnPanel(t.Muted), labels[i])
			tui.Text(screen, x+1, 4, cardWidth-2, t.OnPanel(t.Accent), values[i])
			tui.Text(screen, x+1, 5, cardWidth-2, t.OnPanel(t.Muted), notes[i])
		}
	} else {
		top = 6
	}
	query := " / search"
	if l.query != "" || l.searching {
		query = " / " + l.query
	}
	style := t.Muted
	if l.searching {
		query += "▏  Enter apply · Esc cancel"
		style = t.Accent
	}
	tui.Text(screen, 1, top-3, w-2, style, query)
	direction := "↓"
	if l.sortKey == "name" || l.sortKey == "oldest" {
		direction = "↑"
	}
	if l.reverse {
		if direction == "↓" {
			direction = "↑"
		} else {
			direction = "↓"
		}
	}
	tui.Text(screen, 1, top-2, w-2, t.Muted, fmt.Sprintf("Sort: %s %s   %d / %d   %d marked", l.sortKey, direction, min(l.cursor+1, len(l.visible)), len(l.visible), len(l.marked)))
	tableWidth := w - 2
	if w >= 120 && h >= 23 {
		tableWidth -= 39
		a.drawInspector(screen, t, tableWidth+3, top-1, 36)
	}
	l.rows = max(1, h-top-3)
	if a.repository != "" {
		l.rows = max(1, l.rows/2)
	}
	l.clamp()
	if a.repository == "" {
		a.drawRepositories(screen, t, 1, top, tableWidth, direction)
	} else {
		a.drawManifests(screen, t, 1, top, tableWidth)
	}
	if len(l.visible) == 0 {
		text := "No repositories in this snapshot"
		if a.repository != "" {
			text = "No manifests in this repository"
		}
		if l.query != "" {
			text = "No matches. Press / to change the search, Ctrl-U to clear it."
		}
		tui.Text(screen, 2, top+1, tableWidth-2, t.Muted, text)
	}
	status := a.status
	if status == "" && a.opts.Client != nil {
		source := a.opts.Client.RuleSource
		if source == "" {
			source = "none selected"
		}
		status = "Rules: " + source + "   l choose · p current · P marked/pattern"
	}
	if status == "" {
		status = "Enter details · Space mark · a mark visible · c clear marks"
	}
	tui.Text(screen, 1, h-2, w-2, t.Muted, status)
	footer := "Enter images  Space mark  S pattern  P rules  d delete  ? help  q quit"
	if a.opts.Client == nil {
		footer = "Enter/i stats  Space mark  S pattern  / search  s sort  ? help  q quit"
	}
	if a.repository != "" {
		footer = "Enter details  Esc repos  i stats  / search  R reload  ? help  q quit"
	}
	tui.Text(screen, 1, h-1, w-2, t.Accent, footer)
}

func (a *app) drawRulePicker(screen tcell.Screen, t tui.Theme) {
	w, h := screen.Size()
	tui.Text(screen, 1, 3, w-2, t.Accent, "CHOOSE RULE FILE")
	scope := "Select one file, then p previews the current repository or P uses marked repositories or a pattern."
	if a.ruleRequest != nil {
		scope = "Select and preview: " + a.ruleScope
	}
	tui.Text(screen, 1, 4, w-2, t.Muted, scope)
	details := min(7, max(0, h-12))
	a.ruleRows = max(1, h-8-details)
	a.ruleOffset = max(0, min(a.ruleOffset, len(a.opts.RuleFiles)-a.ruleRows))
	if a.ruleCursor < a.ruleOffset {
		a.ruleOffset = a.ruleCursor
	}
	if a.ruleCursor >= a.ruleOffset+a.ruleRows {
		a.ruleOffset = a.ruleCursor - a.ruleRows + 1
	}
	for i := a.ruleOffset; i < min(len(a.opts.RuleFiles), a.ruleOffset+a.ruleRows); i++ {
		file := a.opts.RuleFiles[i]
		style, label := t.Base, "  "+file.Source
		if file.Err != nil {
			label += " [invalid]"
			style = t.Warn
		}
		if a.opts.Client != nil && file.Source == a.opts.Client.RuleSource {
			label += " [selected]"
		}
		if i == a.ruleCursor {
			style, label = t.Selected, "› "+label[2:]
			tui.Fill(screen, 1, 6+i-a.ruleOffset, w-2, 1, style)
		}
		tui.Text(screen, 1, 6+i-a.ruleOffset, w-2, style, label)
	}
	if len(a.opts.RuleFiles) == 0 {
		tui.Text(screen, 1, 6, w-2, t.Muted, "No rule files available. Press L to load a file.")
	} else if details > 0 {
		var lines []string
		for _, line := range ruleDetails(a.opts.RuleFiles[a.ruleCursor]) {
			lines = append(lines, tui.Wrap(line, w-4)...)
		}
		for i, line := range lines[:min(len(lines), details)] {
			tui.Text(screen, 2, h-2-details+i, w-4, t.Muted, line)
		}
	}
	tui.Text(screen, 1, h-2, w-2, t.Warn, a.status)
	tui.Text(screen, 1, h-1, w-2, t.Accent, "↑/↓ choose · Enter select · i details · L load file · Esc cancel")
}

type column struct {
	key, label string
	width      int
}

var columns = []column{{"unique", "UNIQUE", 10}, {"total", "TOTAL", 10}, {"shared", "SHARED", 9}, {"count", "COUNT", 9}, {"untagged", "UNTAGGED", 10}, {"tagged", "TAGGED", 9}, {"running", "RUNNING", 9}, {"newest", "NEWEST", 12}, {"oldest", "OLDEST", 12}}

func visibleColumns(width int, key string) ([]column, int) {
	budget := max(0, width-24)
	chosen := map[string]bool{}
	// Keep the active sort column visible even on a narrow terminal.
	for _, c := range columns {
		if c.key == key && c.width <= budget {
			chosen[c.key] = true
			budget -= c.width
		}
	}
	for _, c := range columns {
		if !chosen[c.key] && c.width <= budget {
			chosen[c.key] = true
			budget -= c.width
		}
	}
	var result []column
	nameWidth := width - 4
	for _, c := range columns {
		if chosen[c.key] {
			result = append(result, c)
			nameWidth -= c.width
		}
	}
	return result, max(1, nameWidth)
}

func (a *app) drawRepositories(screen tcell.Screen, t tui.Theme, x, y, width int, direction string) {
	l := &a.repos
	cols, nameWidth := visibleColumns(width, l.sortKey)
	tui.Rule(screen, x, y-1, width, t, "")
	name := "REPOSITORY"
	if l.sortKey == "name" {
		name += " " + direction
	}
	tui.Text(screen, x+4, y-1, nameWidth, t.Accent, name)
	xcol := x + 4 + nameWidth
	for _, c := range cols {
		label, style := c.label, t.Muted
		if c.key == l.sortKey {
			label += direction
			style = t.Accent
		}
		tui.Text(screen, xcol, y-1, c.width-1, style, label)
		xcol += c.width
	}
	for i := l.offset; i < min(len(l.visible), l.offset+l.rows); i++ {
		s := l.visible[i]
		row, style := y+i-l.offset, t.Base
		if i == l.cursor {
			style = t.Selected
			tui.Fill(screen, x, row, width, 1, style)
			tui.Text(screen, x, row, 1, style, "›")
		}
		if l.marked[s.Name] {
			tui.Text(screen, x+2, row, 1, style, "✓")
		}
		tui.Text(screen, x+4, row, nameWidth-1, style, s.Name)
		xcol := x + 4 + nameWidth
		for _, c := range cols {
			tui.Text(screen, xcol, row, c.width-1, style, statValue(s, c.key))
			xcol += c.width
		}
	}
}

func (a *app) drawManifests(screen tcell.Screen, t tui.Theme, x, y, width int) {
	l := &a.manifests
	nameWidth := width
	if width >= 28 {
		nameWidth -= 10
	}
	if width >= 50 {
		nameWidth -= 11
	}
	if width >= 75 {
		nameWidth -= 12
	}
	tui.Rule(screen, x, y-1, width, t, "")
	tui.Text(screen, x+4, y-1, nameWidth-4, t.Accent, "TAGS / DIGEST")
	if width >= 28 {
		tui.Text(screen, x+nameWidth, y-1, 9, t.Muted, "TYPE")
	}
	if width >= 50 {
		tui.Text(screen, x+nameWidth+10, y-1, 10, t.Muted, "SIZE")
	}
	if width >= 75 {
		tui.Text(screen, x+nameWidth+21, y-1, 11, t.Muted, "UPDATED")
	}
	for i := l.offset; i < min(len(l.visible), l.offset+l.rows); i++ {
		s := l.visible[i]
		m := a.byDigest[s.Name]
		row, style, secondary := y+2*(i-l.offset), t.Base, t.Muted
		if i == l.cursor {
			style, secondary = t.Selected, t.Selected
			tui.Fill(screen, x, row, width, 2, style)
			tui.Text(screen, x, row, 1, style, "›")
		}
		if l.marked[s.Name] {
			tui.Text(screen, x+2, row, 1, style, "✓")
		}
		label := strings.Join(m.Tags, ", ")
		if label == "" {
			label = m.Digest
		}
		tui.Text(screen, x+4, row, nameWidth-5, style, label)
		if width >= 28 {
			tui.Text(screen, x+nameWidth, row, 9, style, manifestKind(m))
		}
		if width >= 50 {
			tui.Text(screen, x+nameWidth+10, row, 10, style, humanize.Bytes(s.Total))
		}
		if width >= 75 {
			tui.Text(screen, x+nameWidth+21, row, 11, style, date(m.LastUpdated))
		}
		tui.Text(screen, x+4, row+1, width-4, secondary, a.manifestSummary(m))
	}
}

func (a *app) drawInspector(screen tcell.Screen, t tui.Theme, x, y, width int) {
	l := a.list()
	if len(l.visible) == 0 {
		return
	}
	s := l.visible[l.cursor]
	tui.Rule(screen, x, y, width, t, "DETAILS")
	if a.repository != "" {
		m := a.byDigest[s.Name]
		lines := []string{tags(m.Tags), "", manifestKind(m), manifestPlatforms(m), humanize.Bytes(s.Total) + " own referenced", date(m.LastUpdated),
			fmt.Sprintf("%d children · %d parent indexes", len(m.Manifests), len(a.parents[m.Digest])), fmt.Sprintf("Locked: %t", m.IsLocked()), "", m.Digest, "", "Enter for full image details"}
		_, h := screen.Size()
		for i, line := range lines {
			if y+2+i >= h-2 {
				break
			}
			tui.Text(screen, x, y+2+i, width, t.Base, line)
		}
		return
	}
	tui.Text(screen, x, y+2, width, t.Base.Bold(true), s.Name)
	tui.Text(screen, x, y+4, width, t.Accent, humanize.Bytes(s.Unique)+" unique")
	tui.Meter(screen, x, y+5, width, fraction(s.Unique, s.Total), t.Accent, t.Border)
	tui.Text(screen, x, y+6, width, t.Muted, fmt.Sprintf("%s total · %.1f%% shared", humanize.Bytes(s.Total), s.Shared*100))
	tui.Text(screen, x, y+8, width, t.Good, fmt.Sprintf("%d tagged / %d untagged", s.Tagged, s.Untagged))
	tui.Meter(screen, x, y+9, width, fraction(uint64(max(0, s.Tagged)), uint64(max(0, s.Count))), t.Good, t.Border)
	for i, line := range []string{fmt.Sprintf("Running  %d (if inventoried)", s.Running), "Newest   " + date(s.Newest), "Oldest   " + date(s.Oldest)} {
		tui.Text(screen, x, y+11+i, width, t.Muted, line)
	}
}

func statValue(s pruner.RepositoryStats, key string) string {
	switch key {
	case "unique":
		return humanize.Bytes(s.Unique)
	case "total":
		return humanize.Bytes(s.Total)
	case "shared":
		return fmt.Sprintf("%.1f%%", s.Shared*100)
	case "count":
		return humanize.Comma(int64(s.Count))
	case "tagged":
		return humanize.Comma(int64(s.Tagged))
	case "untagged":
		return humanize.Comma(int64(s.Untagged))
	case "running":
		return humanize.Comma(int64(s.Running))
	case "newest":
		return date(s.Newest)
	case "oldest":
		return date(s.Oldest)
	}
	return s.Name
}
func date(t time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	return t.UTC().Format("2006-01-02")
}
func timestamp(t time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	return t.UTC().Format(time.RFC3339)
}
func fraction(n, d uint64) float64 {
	if d == 0 {
		return 0
	}
	return min(1, float64(n)/float64(d))
}

func (a *app) details() {
	l := a.list()
	if len(l.visible) == 0 {
		return
	}
	s := l.visible[l.cursor]
	if a.repository != "" {
		m := a.byDigest[s.Name]
		a.showMessage("IMAGE / MANIFEST DETAILS", a.manifestDetails(m)...)
		return
	}
	a.repositoryDetails()
}

func (a *app) repositoryDetails() {
	name := a.repository
	if name == "" {
		name = a.repos.current()
	}
	i := slices.IndexFunc(a.repos.data, func(s pruner.RepositoryStats) bool { return s.Name == name })
	if i < 0 {
		return
	}
	s := a.repos.data[i]
	images := "Enter or m: browse all live images, including untagged manifests and index children."
	if a.opts.Client == nil {
		images = "This snapshot contains aggregate statistics. Reopen with --registry to browse individual images."
	}
	lines := []string{s.Name, images, ""}
	for _, key := range sortKeys {
		if key != "name" {
			lines = append(lines, fmt.Sprintf("%-10s %s", key, statValue(s, key)))
		}
	}
	lines = append(lines, "", fmt.Sprintf("Exact bytes: unique %d / total %d", s.Unique, s.Total), "Newest: "+timestamp(s.Newest), "Oldest: "+timestamp(s.Oldest), "",
		"Unique bytes are attributed by the original scan order. They are not a reclaimable-storage estimate. A zero running count may mean no inventory was supplied.")
	a.showMessage("REPOSITORY STATISTICS", lines...)
	a.panel = "repository-details"
}

func (a *app) drawPage(screen tcell.Screen, t tui.Theme, paragraphs []string) {
	w, h := screen.Size()
	var lines []string
	for _, p := range paragraphs {
		lines = append(lines, tui.Wrap(p, w-4)...)
	}
	rows := max(1, h-5)
	a.panelOffset = max(0, min(a.panelOffset, len(lines)-rows))
	for i, line := range lines[a.panelOffset:min(len(lines), a.panelOffset+rows)] {
		style := t.Base
		if a.panelOffset+i == 0 {
			style = t.Accent
		}
		tui.Text(screen, 2, 3+i, w-4, style, line)
	}
	footer := "↑/↓ scroll · PgUp/PgDn page · Esc back · q quit"
	if a.panel == "repository-details" && a.opts.Client != nil {
		footer = "Enter/m images · ↑/↓ scroll · PgUp/PgDn page · Esc back · q quit"
	}
	tui.Text(screen, 1, h-1, w-2, t.Muted, footer)
}

func (a *app) drawPlan(screen tcell.Screen, t tui.Theme) {
	w, h := screen.Size()
	s := a.plan.Summary()
	tui.Text(screen, 1, 3, w-2, t.Warn, "DELETION PREVIEW  /  "+a.opts.Client.Registry)
	tui.Text(screen, 1, 4, w-2, t.Base, a.scope)
	tui.Text(screen, 1, 5, w-2, t.Warn, fmt.Sprintf("%d manifests · %d whole repositories · %s estimated blobs", s.DeletedManifests, s.Repositories-s.KeptRepositories, humanize.Bytes(s.DeletedBytes)))
	tui.Text(screen, 1, 6, w-2, t.Muted, fmt.Sprintf("Grace %s · include locked %t · warnings %d (w to read)", a.opts.Client.KeepYounger, a.opts.Client.IncludeLocked, a.tracker.Snapshot().Warnings))
	if strings.HasPrefix(a.scope, "rules for") {
		tui.Text(screen, 1, 7, w-2, t.Muted, "Rule file: "+a.opts.Client.RuleSource)
	}
	tui.Rule(screen, 1, 8, w-2, t, fmt.Sprintf("TARGET %d / %d · every tag shown is removed", a.planCursor+1, len(a.targets)))
	a.planRows = max(1, (h-12)/2)
	a.planOffset = max(0, min(a.planOffset, len(a.targets)-a.planRows))
	if a.planCursor < a.planOffset {
		a.planOffset = a.planCursor
	}
	if a.planCursor >= a.planOffset+a.planRows {
		a.planOffset = a.planCursor - a.planRows + 1
	}
	for i := a.planOffset; i < min(len(a.targets), a.planOffset+a.planRows); i++ {
		target := a.targets[i]
		style, y := t.Base, 9+2*(i-a.planOffset)
		if i == a.planCursor {
			style = t.Selected
			tui.Fill(screen, 1, y, w-2, 2, style)
		}
		label := target.Repository
		if target.WholeRepository {
			label += " [whole repository]"
		}
		if target.Unlock {
			label += " [unlock]"
		}
		tui.Text(screen, 2, y, w-4, style, label+"  "+tags(target.Tags))
		tui.Text(screen, 2, y+1, w-4, style, target.Digest)
	}
	tui.Text(screen, 1, h-2, w-2, t.Muted, "No changes yet. Enter shows full target details. Changed snapshots are refused.")
	tui.Text(screen, 1, h-1, w-2, t.Accent, "↑/↓ review   c confirm deletion   w warnings   Esc discard   q quit")
	if !a.reviewable {
		tui.Text(screen, 1, h-1, w-2, t.Warn, "Enlarge to at least 60 × 18 to review and confirm")
	}
}

func (a *app) drawJob(screen tcell.Screen, t tui.Theme, now time.Time) {
	w, h := screen.Size()
	s := a.tracker.Snapshot()
	title := a.job.title
	if a.job.stopping {
		title = "Stopping: waiting for requests and lock restores"
	}
	spinner := []rune("◐◓◑◒")[now.UnixMilli()/125%4]
	tui.Text(screen, 1, 3, w-2, t.Accent, fmt.Sprintf("%c %s", spinner, title))
	tui.Text(screen, 1, 5, w-2, t.Base, s.Repository)
	tui.Text(screen, 1, 6, w-2, t.Muted, s.Phase)
	tui.Text(screen, 1, 8, w-2, t.Base, fmt.Sprintf("Repositories %d / %d · Fetched %d / %d", s.Completed, s.Repositories, s.Fetched, s.Listed))
	tui.Text(screen, 1, 9, w-2, t.Base, fmt.Sprintf("Selected %d · Deleted %d · Warnings %d", s.Selected, s.Deleted, s.Warnings))
	if s.RetryUntil.After(now) {
		tui.Text(screen, 1, 10, w-2, t.Warn, "Retry in "+s.RetryUntil.Sub(now).Round(time.Second).String())
	}
	tui.Rule(screen, 1, 12, w-2, t, "ACTIVITY")
	rows := max(0, h-15)
	for i, e := range s.Logs[max(0, len(s.Logs)-rows):] {
		tui.Text(screen, 1, 13+i, w-2, t.Muted, e.Time.Format("15:04:05")+" "+e.Text)
	}
	tui.Text(screen, 1, h-1, w-2, t.Muted, "Esc cancel action · q cancel and quit · active requests finish before closing")
}

func (a *app) drawConfirmation(screen tcell.Screen, t tui.Theme) {
	w, h := screen.Size()
	tui.Fill(screen, 1, 3, w-2, h-3, t.Base)
	s := a.plan.Summary()
	put := func(y int, style tcell.Style, text string) { tui.Text(screen, 1, y, w-2, style, text) }
	put(3, t.Danger, "CONFIRM LIVE DELETION")
	put(5, t.Warn, fmt.Sprintf("Delete %d manifests and %d whole repositories.", s.DeletedManifests, s.Repositories-s.KeptRepositories))
	put(6, t.Base, "Scope: "+a.scope)
	put(7, t.Muted, "Every tag on the reviewed manifests will be removed.")
	put(9, t.Base, "Type this exact phrase, then press Enter:")
	lines := tui.Wrap(a.confirmPhrase(), w-4)
	a.reviewable = a.reviewable && len(lines) <= h-14
	for i, line := range lines {
		if 10+i < h-4 {
			tui.Text(screen, 2, 10+i, w-4, t.Accent, line)
		}
	}
	put(h-4, t.Warn, "> "+tailText(a.input+"▏", w-5))
	put(h-3, t.Warn, a.status)
	put(h-1, t.Muted, "Enter executes deletion · Esc returns to the preview")
	if !a.reviewable {
		put(h-2, t.Warn, "Enlarge the terminal to read the full confirmation")
	}
}

func tailText(text string, width int) string {
	if width <= 1 {
		return "…"
	}
	cells := uniseg.StringWidth(text)
	if cells <= width {
		return text
	}
	g := uniseg.NewGraphemes(text)
	for g.Next() {
		cells -= g.Width()
		if cells <= width-1 {
			_, end := g.Positions()
			return "…" + text[end:]
		}
	}
	return text
}
