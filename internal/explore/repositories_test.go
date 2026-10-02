package explore

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/JohanLindvall/crprune/internal/pruner"
	"github.com/JohanLindvall/crprune/internal/registry"
	"github.com/JohanLindvall/crprune/internal/registry/registrytest"
	"github.com/gdamore/tcell/v2"
)

func TestRepositoryPatterns(t *testing.T) {
	stats := []pruner.RepositoryStats{{Name: "commons-deploy"}, {Name: "commons-worker"}, {Name: "commonsXdeploy"}, {Name: "team/api"}, {Name: "team.api"}, {Name: "queue"}}
	for _, tt := range []struct {
		mode, pattern string
		want          []string
		invalid       bool
	}{
		{"wildcard", "*", []string{"commons-deploy", "commons-worker", "commonsXdeploy", "queue", "team.api", "team/api"}, false},
		{"wildcard", "commons-*", []string{"commons-deploy", "commons-worker"}, false},
		{"wildcard", "team/???", []string{"team/api"}, false},
		{"wildcard", "*api", []string{"team.api", "team/api"}, false},
		{"wildcard", "team.api", []string{"team.api"}, false},
		{"wildcard", "team[./]api", nil, false},
		{"wildcard", "COMMONS-*", nil, false},
		{"regex", "^commons-(deploy|worker)$", []string{"commons-deploy", "commons-worker"}, false},
		{"regex", "deploy", []string{"commons-deploy", "commonsXdeploy"}, false},
		{"regex", `^team\.api$`, []string{"team.api"}, false},
		{"regex", "[", nil, true},
		{"regex", "", nil, true},
		{"wildcard", "", nil, true},
	} {
		t.Run(tt.mode+"/"+tt.pattern, func(t *testing.T) {
			s := repositorySelector{mode: tt.mode, pattern: tt.pattern}
			s.match(stats)
			if !slices.Equal(s.matches, tt.want) || (s.err != nil) != tt.invalid {
				t.Fatalf("matches=%v err=%v", s.matches, s.err)
			}
		})
	}
}

func TestPatternPreviewUsesOnlyMatchesAndSelectedRules(t *testing.T) {
	for _, mode := range []string{"wildcard", "regex"} {
		t.Run(mode, func(t *testing.T) {
			b := registrytest.New()
			var stats []pruner.RepositoryStats
			for _, name := range []string{"commons-deploy", "commons-worker", "unmatched"} {
				b.Add(name, registrytest.Image(name), registry.Attributes{LastUpdated: time.Now().Add(-48 * time.Hour)})
				stats = append(stats, pruner.RepositoryStats{Name: name})
			}
			b.Add("commons-outside", registrytest.Image("outside"), registry.Attributes{})
			file := RuleFile{Source: "rules.json", Rules: compileRules(t, `[{"repo":"^commons-worker$","untagged":[{"keep":true}]},{"repo":".+","untagged":[{"keep":false}]}]`)}
			a := newApp(stats, Options{Client: testClient(b), RuleFiles: []RuleFile{file}, Filter: "unmatched"})
			press(t, a, tcell.KeyRune, 'P')
			if a.panel != "repositories" || a.job != nil || len(a.selector.matches) != 3 {
				t.Fatal("P did not wait for repository selection")
			}
			pattern := "commons-*"
			if mode == "regex" {
				press(t, a, tcell.KeyTab, 0)
				pattern = "^commons-"
			}
			press(t, a, tcell.KeyCtrlU, 0)
			typeText(t, a, pattern)
			if !slices.Equal(a.selector.matches, []string{"commons-deploy", "commons-worker"}) {
				t.Fatal(a.selector.matches)
			}
			press(t, a, tcell.KeyEnter, 0)
			if a.panel != "rule-picker" || a.job != nil {
				t.Fatal("repository selection bypassed rule choice")
			}
			press(t, a, tcell.KeyEnter, 0)
			if r := finishJob(t, a); r.err != nil {
				t.Fatal(r.err)
			}
			if a.panel != "plan" || len(a.targets) != 1 || a.targets[0].Repository != "commons-deploy" || !strings.Contains(a.scope, pattern) {
				t.Fatal("scope escaped pattern or original rule order", a.scope, a.targets)
			}
			if b.Calls("ListRepositories") != 0 || len(b.Deleted()) != 0 || len(b.DeletedRepositories()) != 0 {
				t.Fatal("selection accessed repositories outside the snapshot or deleted data")
			}
		})
	}
}

func TestMarkedRulesIncludeHiddenSelections(t *testing.T) {
	b := registrytest.New()
	for _, name := range []string{"one", "two", "three", "outside"} {
		b.Add(name, registrytest.Image(name), registry.Attributes{LastUpdated: time.Now().Add(-48 * time.Hour)})
	}
	c := testClient(b)
	c.Rules = compileRules(t, `[{"repo":".+","untagged":[{"keep":false}]}]`)
	a := newApp(sampleStats(), Options{Client: c, Sort: "name"})
	press(t, a, tcell.KeyRune, ' ') // one
	press(t, a, tcell.KeyDown, 0)
	press(t, a, tcell.KeyRune, ' ') // three
	press(t, a, tcell.KeyUp, 0)
	press(t, a, tcell.KeyRune, ' ') // unmark one
	press(t, a, tcell.KeyRune, 'S')
	press(t, a, tcell.KeyTab, 0)
	press(t, a, tcell.KeyCtrlU, 0)
	typeText(t, a, "^two$")
	press(t, a, tcell.KeyEnter, 0)
	if a.panel != "" || a.job != nil || !slices.Equal(a.repos.selected(), []string{"three", "two"}) {
		t.Fatal("regex did not add to manual selection", a.repos.marked)
	}
	press(t, a, tcell.KeyRune, '/')
	typeText(t, a, "one")
	press(t, a, tcell.KeyEnter, 0)
	press(t, a, tcell.KeyRune, 'P')
	if r := finishJob(t, a); r.err != nil {
		t.Fatal(r.err)
	}
	var names []string
	for _, target := range a.targets {
		names = append(names, target.Repository)
	}
	if !slices.Equal(names, []string{"three", "two"}) || !strings.Contains(a.scope, "2 marked") {
		t.Fatal("P ignored marks, included the cursor, or lost hidden selections", names, a.scope)
	}
	if len(b.Deleted()) != 0 || len(b.DeletedRepositories()) != 0 {
		t.Fatal("selection bypassed confirmation")
	}
}

func TestPatternSelectionRejectsInvalidOrEmptyMatchesAndCanCancel(t *testing.T) {
	a := newApp(sampleStats(), Options{Client: testClient(registrytest.New())})
	a.repos.marked["one"] = true
	for _, pattern := range []string{"[", "^missing$", "", "q"} {
		press(t, a, tcell.KeyRune, 'S')
		press(t, a, tcell.KeyTab, 0)
		press(t, a, tcell.KeyCtrlU, 0)
		typeText(t, a, pattern) // q is editable pattern text, not quit.
		press(t, a, tcell.KeyEnter, 0)
		if a.panel != "repositories" || a.job != nil || !slices.Equal(a.repos.selected(), []string{"one"}) {
			t.Fatal("invalid or empty match changed selection", a.panel, a.repos.marked)
		}
		press(t, a, tcell.KeyEsc, 0)
		if a.panel != "" || a.job != nil || !slices.Equal(a.repos.selected(), []string{"one"}) {
			t.Fatal("Esc changed selection")
		}
	}
}

func TestRepositorySelectorScrollingAndResizing(t *testing.T) {
	var stats []pruner.RepositoryStats
	for i := range 50 {
		stats = append(stats, pruner.RepositoryStats{Name: fmt.Sprintf("repo-%02d", i)})
	}
	a := newApp(stats, Options{})
	press(t, a, tcell.KeyRune, 'S')
	for _, size := range [][2]int{{120, 30}, {80, 24}, {40, 10}, {1, 1}, {0, 0}} {
		s := screenFor(t, size[0], size[1])
		a.draw(s, time.Now())
		press(t, a, tcell.KeyEnd, 0)
		a.draw(s, time.Now())
		if size[0] >= 80 {
			text := screenText(s)
			for _, want := range []string{"SELECT REPOSITORIES", "wildcard> *", "50 / 50", "repo-49", "Tab wildcard/regex"} {
				if !strings.Contains(text, want) {
					t.Fatalf("missing %q in:\n%s", want, text)
				}
			}
		}
		press(t, a, tcell.KeyHome, 0)
		a.draw(s, time.Now())
		if a.selector.offset != 0 {
			t.Fatal("Home did not return to the first match")
		}
	}
}
