package tui

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"
)

func TestWrap(t *testing.T) {
	for _, tt := range []struct {
		text  string
		width int
		want  []string
	}{
		{"", 8, []string{""}},
		{"short", 8, []string{"short"}},
		{"one two three", 7, []string{"one two", "three"}},
		{"abcdefgh", 3, []string{"abc", "def", "gh"}},
		{"a   bc", 3, []string{"a", "bc"}},
		{"界界a", 2, []string{"界", "界", "a"}},
		{"界ab", 1, []string{"界", "a", "b"}},
		{"ab", 0, []string{"a", "b"}},
		{"e\u0301e\u0301", 1, []string{"e\u0301", "e\u0301"}},
		{"👩\u200d💻abc", 2, []string{"👩\u200d💻", "ab", "c"}},
		{"a\nb\u202ec", 8, []string{"a b c"}},
	} {
		if got := Wrap(tt.text, tt.width); !slices.Equal(got, tt.want) {
			t.Errorf("Wrap(%q, %d) = %q, want %q", tt.text, tt.width, got, tt.want)
		}
	}
}

func BenchmarkWrapLongText(b *testing.B) {
	for _, size := range []int{1000, 10000, 50000} {
		b.Run(fmt.Sprint(size), func(b *testing.B) {
			text := strings.Repeat("manifest ", size/9)
			b.ReportAllocs()
			for b.Loop() {
				_ = Wrap(text, 76)
			}
		})
	}
}

func TestCleanPreservesJoinersAndRemovesControls(t *testing.T) {
	text := "👩\u200d💻 می\u200cروم\x1b\n\u202e\u2066"
	if got := Clean(text); got != "👩\u200d💻 می\u200cروم    " {
		t.Fatalf("Clean = %q", got)
	}
}

func TestTextPreservesGraphemesWhenClipping(t *testing.T) {
	s := tcell.NewSimulationScreen("UTF-8")
	if err := s.Init(); err != nil {
		t.Fatal(err)
	}
	defer s.Fini()
	s.SetSize(6, 1)
	Text(s, 0, 0, 4, tcell.StyleDefault, "👩\u200d💻abcd")
	s.Show()
	cells, _, _ := s.GetContents()
	if got := string(cells[0].Runes); got != "👩\u200d💻" {
		t.Errorf("emoji split apart: %q", got)
	}
	if string(cells[2].Runes) != "a" || string(cells[3].Runes) != "…" {
		t.Fatal("incorrect clipping", cells)
	}
}

func FuzzWrap(f *testing.F) {
	for _, seed := range []string{"", "one two three", "界a", "👩\u200d💻", "a\x1b\n\u202eb", "a  b"} {
		f.Add(seed, uint8(3))
	}
	f.Fuzz(func(t *testing.T, text string, width uint8) {
		lines := Wrap(text, int(width))
		// Wrapping may remove boundary spaces but must retain every other
		// character, including a cluster wider than the terminal.
		withoutSpaces := func(s string) string { return strings.ReplaceAll(s, " ", "") }
		if withoutSpaces(strings.Join(lines, "")) != withoutSpaces(Clean(text)) {
			t.Fatalf("lost text: %q -> %q", text, lines)
		}
	})
}
