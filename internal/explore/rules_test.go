package explore

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/JohanLindvall/crprune/internal/registry"
	"github.com/JohanLindvall/crprune/internal/registry/registrytest"
	presets "github.com/JohanLindvall/crprune/rules"
	"github.com/gdamore/tcell/v2"
)

func TestDiscoverRulesIncludesBundledAndLocalFiles(t *testing.T) {
	dir := t.TempDir()
	entries, err := presets.Files.ReadDir(".")
	if err != nil || len(entries) == 0 {
		t.Fatal(entries, err)
	}
	bundled := DiscoverRules(filepath.Join(dir, "missing"))
	if len(bundled) != len(entries) {
		t.Fatal(bundled)
	}
	for _, file := range bundled {
		if file.Err != nil || len(file.Rules) == 0 || !strings.HasPrefix(file.Source, "bundled:") {
			t.Fatalf("bundled file=%+v", file)
		}
	}
	write := func(name, contents string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(contents), 0600); err != nil {
			t.Fatal(err)
		}
	}
	// An unchanged checkout does not duplicate the bundled entries. A changed
	// copy is a distinct choice, never an implicit replacement for a preset.
	name := entries[0].Name()
	data, err := presets.Files.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	write(name, string(data))
	if got := DiscoverRules(dir); len(got) != len(bundled) {
		t.Fatal("identical example duplicated", got)
	}
	write(name, `[{"repo":"^custom$","description":"Custom keep rule","untagged":[{"keep":true}]}]`)
	write("broken.json", `[{"repo":"["}]`)
	write("notes.txt", "not a rule file")
	if err := os.Mkdir(filepath.Join(dir, "subdir.json"), 0700); err != nil {
		t.Fatal(err)
	}
	got := DiscoverRules(dir)
	if len(got) != len(bundled)+2 {
		t.Fatal(got)
	}
	for _, file := range got[len(bundled):] {
		if filepath.Base(file.Source) == "broken.json" {
			if file.Err == nil || file.Rules != nil {
				t.Fatal("invalid rules were accepted", file)
			}
		} else if file.Err != nil || len(file.Rules) != 1 || file.Rules[0].Description != "Custom keep rule" {
			t.Fatal("modified local example was not loaded independently", file)
		}
	}
}

func TestRulePickerPreviewsOnlySelectedFileAndScope(t *testing.T) {
	for _, key := range []rune{'p', 'P'} {
		t.Run(string(key), func(t *testing.T) {
			b := registrytest.New()
			for _, name := range []string{"one", "two", "three", "outside"} {
				b.Add(name, registrytest.Image(name), registry.Attributes{LastUpdated: time.Now().Add(-48 * time.Hour)})
			}
			catalog := []RuleFile{
				{Source: "keep.json", Rules: compileRules(t, `[{"repo":".+","untagged":[{"keep":true}]}]`)},
				{Source: "delete.json", Rules: compileRules(t, `[{"repo":"^(one|two|outside)$","description":"Delete selected repositories","untagged":[{"keep":false}]}]`)},
			}
			c := testClient(b)
			a := newApp(sampleStats(), Options{Client: c, RuleFiles: catalog, Filter: "one"})
			press(t, a, tcell.KeyRune, key)
			if key == 'P' {
				press(t, a, tcell.KeyEnter, 0)
			}
			if a.panel != "rule-picker" || a.job != nil || c.RuleSource != "" {
				t.Fatal("preview did not wait for a rule choice")
			}
			press(t, a, tcell.KeyEnd, 0)
			press(t, a, tcell.KeyRune, 'i')
			if a.panel != "rule-details" || !strings.Contains(strings.Join(a.message, " "), "Delete selected repositories") {
				t.Fatal(a.panel, a.message)
			}
			press(t, a, tcell.KeyEsc, 0)
			press(t, a, tcell.KeyEnter, 0)
			if r := finishJob(t, a); r.err != nil {
				t.Fatal(r.err)
			}
			want := []string{"one"}
			if key == 'P' {
				want = append(want, "two")
			}
			var got []string
			for _, target := range a.targets {
				got = append(got, target.Repository)
			}
			if c.RuleSource != "delete.json" || !slices.Equal(got, want) || a.panel != "plan" {
				t.Fatalf("source=%s targets=%v panel=%s", c.RuleSource, got, a.panel)
			}
			if len(b.Deleted()) != 0 || len(b.DeletedRepositories()) != 0 {
				t.Fatal("choosing a rule bypassed confirmation")
			}
			// Switch to the keep file; the previous deletion rules must not
			// remain active or be combined with this selection.
			press(t, a, tcell.KeyEsc, 0)
			press(t, a, tcell.KeyRune, 'l')
			if a.ruleCursor != 1 {
				t.Fatal("picker did not highlight the active rules")
			}
			press(t, a, tcell.KeyHome, 0)
			press(t, a, tcell.KeyEnter, 0)
			if a.job != nil || c.RuleSource != "keep.json" {
				t.Fatal("choosing rules unexpectedly started work")
			}
			press(t, a, tcell.KeyRune, key)
			if key == 'P' {
				press(t, a, tcell.KeyEnter, 0)
			}
			finishJob(t, a)
			if a.plan != nil || a.message[0] != "NOTHING TO DELETE" {
				t.Fatal(a.plan, a.message)
			}
		})
	}
}

func TestRulePickerInvalidFileAndCancelPreserveSelection(t *testing.T) {
	file := compileRuleFile("broken.json", []byte(`[{"repo":"["}]`), nil)
	c := testClient(registrytest.New())
	c.RuleSource = "active.json"
	c.Rules = compileRules(t, `[{"repo":".+","untagged":[{"keep":true}]}]`)
	a := newApp(sampleStats(), Options{Client: c, RuleFiles: []RuleFile{file}})
	press(t, a, tcell.KeyRune, 'l')
	press(t, a, tcell.KeyHome, 0)
	press(t, a, tcell.KeyEnter, 0)
	if a.panel != "rule-picker" || a.job != nil || c.RuleSource != "active.json" || !strings.Contains(a.status, "invalid rules") {
		t.Fatalf("panel=%s source=%s status=%s", a.panel, c.RuleSource, a.status)
	}
	press(t, a, tcell.KeyEsc, 0)
	if a.panel != "" || a.ruleRequest != nil || c.RuleSource != "active.json" {
		t.Fatal("cancel changed selection or retained a pending preview")
	}
}

func TestRulePickerDrawsAtDifferentSizes(t *testing.T) {
	for _, size := range [][2]int{{120, 30}, {80, 24}, {40, 10}, {1, 1}} {
		s := screenFor(t, size[0], size[1])
		a := newApp(sampleStats(), Options{RuleFiles: DiscoverRules(t.TempDir())})
		press(t, a, tcell.KeyRune, 'l')
		press(t, a, tcell.KeyEnd, 0)
		a.draw(s, time.Now())
		if size[0] >= 80 {
			text := screenText(s)
			if !strings.Contains(text, "CHOOSE RULE FILE") || !strings.Contains(text, "delete_untagged_images.json") || !strings.Contains(text, "Deletes untagged manifests") {
				t.Fatal(text)
			}
		}
	}
}
