package explore

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/JohanLindvall/crprune/internal/pruner"
	"github.com/JohanLindvall/crprune/internal/tui"
	"github.com/gdamore/tcell/v2"
)

type repositorySelector struct {
	pattern, mode string
	preview       bool
	matches       []string
	err           error
	offset, rows  int
}

func (a *app) chooseRepositories(preview bool) {
	a.panel, a.status = "repositories", ""
	a.selector = repositorySelector{pattern: "*", mode: "wildcard", preview: preview}
	a.selector.match(a.repos.data)
}

func (s *repositorySelector) match(stats []pruner.RepositoryStats) {
	s.matches, s.err = nil, nil
	if s.pattern == "" {
		s.err = errors.New("enter a pattern; use * in wildcard mode to select all repositories")
		return
	}
	pattern := s.pattern
	if s.mode == "wildcard" {
		// Wildcards match the entire name, including nested repository paths.
		// All characters except * and ? are literal in this mode.
		pattern = "^(?:" + strings.NewReplacer(`\*`, ".*", `\?`, ".").Replace(regexp.QuoteMeta(pattern)) + ")$"
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		s.err = err
		return
	}
	for _, repo := range stats {
		if re.MatchString(repo.Name) {
			s.matches = append(s.matches, repo.Name)
		}
	}
	slices.Sort(s.matches)
}

func (a *app) selectRepositories(ctx context.Context, key *tcell.EventKey) {
	s := &a.selector
	switch key.Key() {
	case tcell.KeyEsc:
		a.panel, a.status = "", "Repository selection canceled"
		return
	case tcell.KeyEnter:
		if s.err != nil || len(s.matches) == 0 {
			a.status = "Enter a valid pattern matching at least one repository"
			return
		}
		if !s.preview {
			for _, name := range s.matches {
				a.repos.marked[name] = true
			}
			a.panel = ""
			a.status = fmt.Sprintf("Selected %d matching repositories (%d marked); Space toggles, P previews rules", len(s.matches), len(a.repos.marked))
			return
		}
		scope := fmt.Sprintf("rules for %d snapshot repositories matching %s %q", len(s.matches), s.mode, s.pattern)
		if s.mode == "wildcard" && s.pattern == "*" {
			scope = fmt.Sprintf("rules for all %d snapshot repositories (including filtered-out rows)", len(s.matches))
		}
		a.prepare(ctx, request{kind: "rules", repositories: slices.Clone(s.matches)}, scope)
		return
	case tcell.KeyUp:
		s.offset--
	case tcell.KeyDown:
		s.offset++
	case tcell.KeyPgUp:
		s.offset -= max(1, s.rows)
	case tcell.KeyPgDn:
		s.offset += max(1, s.rows)
	case tcell.KeyHome:
		s.offset = 0
	case tcell.KeyEnd:
		s.offset = len(s.matches)
	default:
		if key.Key() == tcell.KeyTab {
			if s.mode == "wildcard" {
				s.mode = "regex"
				if s.pattern == "*" {
					s.pattern = ".*"
				}
			} else {
				s.mode = "wildcard"
				if s.pattern == ".*" {
					s.pattern = "*"
				}
			}
		} else {
			editText(&s.pattern, key)
		}
		s.offset, a.status = 0, ""
		s.match(a.repos.data)
	}
	s.offset = max(0, min(s.offset, len(s.matches)-max(1, s.rows)))
}

func (a *app) drawRepositorySelector(screen tcell.Screen, t tui.Theme) {
	w, h := screen.Size()
	s := &a.selector
	title, action := "SELECT REPOSITORIES BY PATTERN", "select"
	if s.preview {
		title, action = "SELECT REPOSITORIES FOR RULE PREVIEW", "preview"
	}
	tui.Text(screen, 1, 3, w-2, t.Accent, title)
	tui.Text(screen, 1, 4, w-2, t.Muted, "Matches use the full snapshot, including filtered-out rows.")
	help := "Wildcard: * matches any text (including /); ? matches one character. Other characters are literal."
	if s.mode == "regex" {
		help = "Regex: Go regular expression; use ^ and $ to match the entire repository name."
	}
	tui.Text(screen, 1, 5, w-2, t.Muted, help)
	tui.Text(screen, 1, 6, w-2, t.Accent, s.mode+"> "+s.pattern+"▏")
	label := fmt.Sprintf("%d / %d repositories match", len(s.matches), len(a.repos.data))
	style := t.Base.Bold(true)
	if s.err != nil {
		label, style = s.err.Error(), t.Warn
	} else if len(s.matches) == 0 {
		label, style = "No repositories match", t.Warn
	}
	tui.Text(screen, 1, 7, w-2, style, label)
	s.rows = max(1, h-12)
	s.offset = max(0, min(s.offset, len(s.matches)-s.rows))
	for i := s.offset; i < min(len(s.matches), s.offset+s.rows); i++ {
		tui.Text(screen, 2, 9+i-s.offset, w-3, t.Base, s.matches[i])
	}
	tui.Text(screen, 1, h-3, w-2, t.Muted, "↑/↓ PgUp/PgDn Home/End scroll matches · Ctrl-U clears pattern")
	tui.Text(screen, 1, h-2, w-2, t.Warn, a.status)
	tui.Text(screen, 1, h-1, w-2, t.Accent, "Tab wildcard/regex · Enter "+action+" · Esc cancel")
}
