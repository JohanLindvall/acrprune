package explore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/JohanLindvall/crprune/internal/pruner"
	"github.com/JohanLindvall/crprune/internal/registry"
	"github.com/JohanLindvall/crprune/internal/registry/registrytest"
	"github.com/gdamore/tcell/v2"
)

func sampleStats() []pruner.RepositoryStats {
	return []pruner.RepositoryStats{{Name: "one", Unique: 100, Total: 200, Count: 20, Tagged: 5, Untagged: 15}, {Name: "two", Unique: 300, Total: 500, Count: 10, Tagged: 4, Untagged: 6}, {Name: "three", Unique: 200, Total: 300, Count: 5}}
}
func press(t *testing.T, a *app, key tcell.Key, r rune) {
	t.Helper()
	quit, err := a.key(t.Context(), tcell.NewEventKey(key, r, tcell.ModNone))
	if quit || err != nil {
		t.Fatalf("key unexpectedly quit: %v", err)
	}
}
func typeText(t *testing.T, a *app, s string) {
	t.Helper()
	for _, r := range s {
		press(t, a, tcell.KeyRune, r)
	}
}
func finishJob(t *testing.T, a *app) result {
	t.Helper()
	if a.job == nil {
		t.Fatal("no job started")
	}
	select {
	case r := <-a.job.done:
		return a.finishJob(r)
	case <-time.After(5 * time.Second):
		t.Fatal("job did not finish")
		return result{}
	}
}
func screenFor(t *testing.T, w, h int) tcell.SimulationScreen {
	t.Helper()
	s := tcell.NewSimulationScreen("UTF-8")
	if err := s.Init(); err != nil {
		t.Fatal(err)
	}
	s.SetSize(w, h)
	t.Cleanup(s.Fini)
	return s
}
func screenText(s tcell.SimulationScreen) string {
	cells, w, _ := s.GetContents()
	var out strings.Builder
	for i, c := range cells {
		if len(c.Runes) == 0 {
			out.WriteByte(' ')
		} else {
			out.WriteString(string(c.Runes))
		}
		if w > 0 && (i+1)%w == 0 {
			out.WriteByte('\n')
		}
	}
	return out.String()
}

func TestSearchSortAndSelection(t *testing.T) {
	a := newApp(sampleStats(), Options{})
	l := a.list()
	l.rows = 2
	if l.current() != "two" {
		t.Fatal(l.current())
	}
	press(t, a, tcell.KeyRune, ' ')
	press(t, a, tcell.KeyRune, '/')
	typeText(t, a, "ONEq") // q is input, not quit.
	if len(l.visible) != 0 {
		t.Fatal(l.visible)
	}
	press(t, a, tcell.KeyBackspace2, 0)
	press(t, a, tcell.KeyEnter, 0)
	if l.current() != "one" || !l.marked["two"] {
		t.Fatal("search lost hidden selection")
	}
	press(t, a, tcell.KeyRune, 'a')
	if !slices.Equal(l.selected(), []string{"one", "two"}) {
		t.Fatal(l.selected())
	}
	press(t, a, tcell.KeyRune, '/')
	press(t, a, tcell.KeyCtrlU, 0)
	press(t, a, tcell.KeyEsc, 0)
	if l.query != "ONE" {
		t.Fatal("escape failed to restore query")
	}
	press(t, a, tcell.KeyEsc, 0)
	if len(l.visible) != 3 {
		t.Fatal(l.visible)
	}
	keep := l.current()
	press(t, a, tcell.KeyRune, 'r')
	if l.current() != keep {
		t.Fatal("sort moved selection to another repository")
	}
	press(t, a, tcell.KeyEnd, 0)
	if l.cursor != 2 || l.offset != 1 {
		t.Fatalf("cursor=%d offset=%d", l.cursor, l.offset)
	}
	press(t, a, tcell.KeyRune, 'c')
	if len(l.marked) != 0 {
		t.Fatal("marks not cleared")
	}
}

func TestDeletionNeedsExactConfirmationAndCanBeCanceled(t *testing.T) {
	b := registrytest.New()
	b.Add("one", registrytest.Image("one"), registry.Attributes{Tags: []string{"old", "alias"}, LastUpdated: time.Now().Add(-48 * time.Hour)})
	a := newApp(sampleStats()[:1], Options{Client: testClient(b)})
	a.reviewable = true
	press(t, a, tcell.KeyRune, 'd')
	finishJob(t, a)
	if a.panel != "plan" || len(a.targets) != 1 || len(a.targets[0].Tags) != 2 {
		t.Fatalf("preview=%s %+v", a.panel, a.targets)
	}
	if b.Calls("DeleteRepository") != 0 {
		t.Fatal("preview deleted")
	}
	press(t, a, tcell.KeyRune, 'c')
	press(t, a, tcell.KeyEnter, 0)
	typeText(t, a, "yes")
	press(t, a, tcell.KeyEnter, 0)
	if a.job != nil || b.Calls("DeleteRepository") != 0 {
		t.Fatal("default or wrong confirmation deleted")
	}
	press(t, a, tcell.KeyEsc, 0)
	press(t, a, tcell.KeyEsc, 0)
	if a.plan != nil || a.panel != "" {
		t.Fatal("cancel retained executable plan")
	}
	press(t, a, tcell.KeyRune, 'd')
	finishJob(t, a)
	press(t, a, tcell.KeyRune, 'c')
	typeText(t, a, a.confirmPhrase())
	press(t, a, tcell.KeyEnter, 0)
	r := finishJob(t, a)
	if r.err != nil || !slices.Equal(b.DeletedRepositories(), []string{"one"}) || !a.stale {
		t.Fatalf("result=%+v stale=%t", r, a.stale)
	}
	if a.plan != nil {
		t.Fatal("successful plan retained")
	}
}

func TestAllRulesPatternUsesSnapshotNotFilter(t *testing.T) {
	b := registrytest.New()
	for _, name := range []string{"one", "two", "three", "outside"} {
		b.Add(name, registrytest.Image(name), registry.Attributes{LastUpdated: time.Now().Add(-48 * time.Hour)})
	}
	c := testClient(b)
	c.Rules = compileRules(t, `[{"repo":".+","untagged":[{"keep":false}]}]`)
	a := newApp(sampleStats(), Options{Client: c, Filter: "one"})
	press(t, a, tcell.KeyRune, 'P')
	press(t, a, tcell.KeyEnter, 0)
	finishJob(t, a)
	if len(a.targets) != 3 || !strings.Contains(a.scope, "all 3 snapshot repositories") {
		t.Fatalf("scope=%s targets=%+v", a.scope, a.targets)
	}
	for _, target := range a.targets {
		if target.Repository == "outside" {
			t.Fatal("scope escaped snapshot")
		}
	}
	press(t, a, tcell.KeyEsc, 0)
	press(t, a, tcell.KeyRune, 'p')
	finishJob(t, a)
	if len(a.targets) != 1 || a.targets[0].Repository != "one" {
		t.Fatal(a.targets)
	}
}

func TestRulesLoadAndReloadErrorsKeepState(t *testing.T) {
	a := newApp(sampleStats(), Options{Client: testClient(registrytest.New()), Reload: func() ([]pruner.RepositoryStats, error) { return nil, errors.New("broken snapshot") }})
	path := filepath.Join(t.TempDir(), "rules.json")
	if err := os.WriteFile(path, []byte(`[{"repo":"^one$","untagged":[{"keep":false}]}]`), 0600); err != nil {
		t.Fatal(err)
	}
	press(t, a, tcell.KeyRune, 'L')
	typeText(t, a, path)
	press(t, a, tcell.KeyEnter, 0)
	finishJob(t, a)
	if len(a.opts.Client.Rules) != 1 || a.opts.Client.RuleSource != path {
		t.Fatal("rules not loaded")
	}
	press(t, a, tcell.KeyRune, 'R')
	finishJob(t, a)
	if len(a.repos.data) != 3 || !strings.Contains(strings.Join(a.message, " "), "broken snapshot") {
		t.Fatal("reload lost snapshot or error")
	}
}

func TestLiveStatisticsReloadPreservesViewAndClearsStaleness(t *testing.T) {
	b := registrytest.New()
	b.Add("one", registrytest.Image("one"), registry.Attributes{})
	a := newApp(sampleStats(), Options{Client: testClient(b), LiveStats: true, Sort: "name", Filter: "one", Reverse: true})
	a.stale = true
	a.repos.marked["two"] = true
	press(t, a, tcell.KeyRune, 'R')
	if r := finishJob(t, a); r.err != nil {
		t.Fatal(r.err)
	}
	if len(a.repos.data) != 1 || a.repos.data[0].Count != 1 || a.repos.current() != "one" || a.stale {
		t.Fatalf("repos=%+v stale=%t", a.repos, a.stale)
	}
	if a.repos.sortKey != "name" || a.repos.query != "one" || !a.repos.reverse || len(a.repos.marked) != 0 {
		t.Fatalf("view state=%+v", a.repos)
	}
	if err := b.DeleteRepository(t.Context(), "one"); err != nil {
		t.Fatal(err)
	}
	press(t, a, tcell.KeyRune, 'R')
	if r := finishJob(t, a); r.err != nil || len(a.repos.data) != 0 {
		t.Fatal(a.repos.data, r.err)
	}
}

func TestLiveStatisticsPartialAndFailedScans(t *testing.T) {
	for _, partial := range []bool{false, true} {
		t.Run(fmt.Sprint(partial), func(t *testing.T) {
			b := registrytest.New()
			b.Add("one", registrytest.Image("one"), registry.Attributes{})
			b.Add("two", registrytest.Image("two"), registry.Attributes{})
			b.Fail = func(_, repository string) error {
				if !partial || repository == "two" {
					return registrytest.Forbidden("statistics")
				}
				return nil
			}
			a := newApp(sampleStats(), Options{Client: testClient(b), LiveStats: true})
			a.stale = true
			press(t, a, tcell.KeyRune, 'R')
			if r := finishJob(t, a); r.err == nil || !a.stale || a.panel != "message" {
				t.Fatalf("err=%v stale=%t panel=%s", r.err, a.stale, a.panel)
			}
			if partial {
				if len(a.repos.data) != 1 || a.repos.data[0].Name != "one" || a.message[0] != "PARTIAL STATISTICS" {
					t.Fatal(a.repos.data, a.message)
				}
			} else if !slices.Equal(a.repos.data, sampleStats()) || a.message[0] != "ACTION FAILED" {
				t.Fatal("failed scan lost the previous snapshot", a.repos.data, a.message)
			}
		})
	}
}

func TestExplorerDrawResponsiveNoColorAndEmpty(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	for _, size := range [][2]int{{150, 40}, {120, 30}, {80, 24}, {60, 18}, {40, 10}, {1, 1}, {0, 0}} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			screen := screenFor(t, size[0], size[1])
			a := newApp(sampleStats(), Options{Source: "file\x1b[2J\n界.json"})
			for _, panel := range []string{"", "help", "sort", "message"} {
				a.panel = panel
				a.message = []string{"DETAILS", strings.Repeat("界", 200)}
				a.draw(screen, time.Now())
				text := screenText(screen)
				if strings.ContainsRune(text, '\x1b') {
					t.Fatal("terminal control sequence rendered")
				}
				if size[0] >= 80 && panel == "" && !strings.Contains(text, "REPOSITORIES") {
					t.Fatal(text)
				}
				cells, _, _ := screen.GetContents()
				for _, c := range cells {
					fg, bg, _ := c.Style.Decompose()
					if fg != tcell.ColorDefault || bg != tcell.ColorDefault {
						t.Fatal("NO_COLOR ignored")
					}
				}
			}
			a.panel = ""
			a.repos = newList(nil, "unique", "", false)
			a.draw(screen, time.Now())
		})
	}
}

// An opt-in render artifact lets visual QA inspect exactly the cells tcell
// draws, using the supplied sample without making it a test dependency.
func TestRenderSample(t *testing.T) {
	out := os.Getenv("CRPRUNE_RENDER_DIR")
	if out == "" {
		t.Skip("set CRPRUNE_RENDER_DIR for sample render")
	}
	stats := sampleStats()
	for _, size := range [][2]int{{140, 34}, {80, 24}} {
		s := screenFor(t, size[0], size[1])
		a := newApp(stats, Options{Source: "sample statistics"})
		a.draw(s, time.Now())
		cells, w, h := s.GetContents()
		type cell struct {
			Text          string
			FG, BG        int32
			Bold, Reverse bool
		}
		var rendered []cell
		for _, c := range cells {
			fg, bg, attrs := c.Style.Decompose()
			rendered = append(rendered, cell{string(c.Runes), fg.Hex(), bg.Hex(), attrs&tcell.AttrBold != 0, attrs&tcell.AttrReverse != 0})
		}
		data, _ := json.Marshal(struct {
			Width, Height int
			Cells         []cell
		}{w, h, rendered})
		if err := os.WriteFile(filepath.Join(out, fmt.Sprintf("explore-%dx%d.json", w, h)), data, 0600); err != nil {
			t.Fatal(err)
		}
		t.Log("\n" + screenText(s))
	}
}

func TestReadOnlyActionsCannotStartWork(t *testing.T) {
	for _, key := range "mdpPL" {
		a := newApp(sampleStats(), Options{Source: "stats.json"})
		press(t, a, tcell.KeyRune, key)
		if a.job != nil || a.panel != "message" || !strings.Contains(strings.Join(a.message, " "), "--registry") {
			t.Fatalf("key=%c", key)
		}
	}
}

func TestCtrlCInterruptsAndQQuits(t *testing.T) {
	a := newApp(nil, Options{})
	quit, err := a.key(t.Context(), tcell.NewEventKey(tcell.KeyCtrlC, 0, tcell.ModCtrl))
	if !quit || !errors.Is(err, context.Canceled) {
		t.Fatal(quit, err)
	}
	quit, err = a.key(t.Context(), tcell.NewEventKey(tcell.KeyRune, 'q', tcell.ModNone))
	if !quit || err != nil {
		t.Fatal(quit, err)
	}
}

func TestPreviewAndConfirmationRemainReviewableAfterResize(t *testing.T) {
	b := registrytest.New()
	var names []string
	for i := range 50 {
		names = append(names, fmt.Sprintf("long-tag-%02d", i))
	}
	digest := b.Add("one", registrytest.Image("one"), registry.Attributes{Tags: names, LastUpdated: time.Now().Add(-48 * time.Hour)})
	a := newApp(sampleStats()[:1], Options{Client: testClient(b)})
	s := screenFor(t, 80, 24)
	a.draw(s, time.Now())
	press(t, a, tcell.KeyRune, 'd')
	finishJob(t, a)
	a.draw(s, time.Now())
	if text := screenText(s); !strings.Contains(text, digest) || !strings.Contains(text, "DELETION PREVIEW") {
		t.Fatal(text)
	}
	press(t, a, tcell.KeyEnter, 0)
	press(t, a, tcell.KeyEnd, 0)
	a.draw(s, time.Now())
	if text := screenText(s); !strings.Contains(text, "long-tag-49") {
		t.Fatal("full tags cannot be reviewed:\n" + text)
	}
	press(t, a, tcell.KeyEsc, 0)
	press(t, a, tcell.KeyRune, 'c')
	a.draw(s, time.Now())
	if text := screenText(s); !strings.Contains(text, a.confirmPhrase()) || !strings.Contains(text, "CONFIRM LIVE DELETION") {
		t.Fatal(text)
	}
	typeText(t, a, a.confirmPhrase())
	for _, size := range [][2]int{{0, 0}, {40, 10}} {
		s.SetSize(size[0], size[1])
		a.draw(s, time.Now())
		press(t, a, tcell.KeyEnter, 0)
		if a.job != nil || b.Calls("DeleteRepository") != 0 {
			t.Fatal("confirmed on a screen too small to review")
		}
	}
	s.SetSize(80, 24)
	a.draw(s, time.Now())
	press(t, a, tcell.KeyEnter, 0)
	r := finishJob(t, a)
	if r.err != nil || r.outcome.DeletedManifests != 1 {
		t.Fatal(r)
	}
}

func TestBrowseSelectDeleteAndReloadLiveManifest(t *testing.T) {
	b := registrytest.New()
	old := time.Now().Add(-48 * time.Hour)
	remove := b.Add("one", registrytest.Image("obsolete"), registry.Attributes{Tags: []string{"obsolete", "alias"}, LastUpdated: old})
	keep := b.Add("one", registrytest.Image("release"), registry.Attributes{Tags: []string{"release"}, LastUpdated: old})
	a := newApp(sampleStats()[:1], Options{Client: testClient(b)})
	a.reviewable = true
	press(t, a, tcell.KeyRune, 'm')
	finishJob(t, a)
	if a.repository != "one" || len(a.manifests.data) != 2 {
		t.Fatal("manifest browser did not open")
	}
	press(t, a, tcell.KeyRune, '/')
	typeText(t, a, "obsolete")
	press(t, a, tcell.KeyEnter, 0)
	if a.manifests.current() != remove {
		t.Fatal("tag search selected the wrong image")
	}
	press(t, a, tcell.KeyRune, ' ')
	press(t, a, tcell.KeyRune, 'd')
	finishJob(t, a)
	if len(a.targets) != 1 || a.targets[0].Digest != remove || a.targets[0].WholeRepository {
		t.Fatal(a.targets)
	}
	press(t, a, tcell.KeyRune, 'c')
	typeText(t, a, a.confirmPhrase())
	press(t, a, tcell.KeyEnter, 0)
	finishJob(t, a)
	press(t, a, tcell.KeyEsc, 0)
	press(t, a, tcell.KeyRune, 'R')
	finishJob(t, a)
	if len(a.manifests.data) != 1 || a.manifests.current() != keep {
		t.Fatal("live reload did not reflect the deletion")
	}
	press(t, a, tcell.KeyEsc, 0)
	if a.repository != "" || len(a.repos.data) != 1 {
		t.Fatal("return to snapshot failed")
	}
}
