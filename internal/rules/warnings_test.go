package rules

import (
	"regexp"
	"slices"
	"strings"
	"testing"
)

// TestWarnings covers rules that are valid but can never apply: pruning uses
// only the first repository rule matching a repository and, within it, the
// first matching tagged or untagged rule, so rules placed after a catch-all
// silently protect nothing.
func TestWarnings(t *testing.T) {
	tests := []struct {
		name  string
		rules string
		want  []string
	}{
		{"literal rule after a catch-all repository rule",
			`[{"repo": ".+", "tagged": [{"match_older": "30d", "keep": false}]}, {"repo": "^prod$", "tagged": [{"tag": "^v", "keep": true}]}]`,
			[]string{`rule 2 never applies: rule 1 (repo ".+") before it matches every repository, and only the first matching rule applies to a repository`}},
		{"regex rule after a catch-all repository rule",
			`[{"description": "everything", "repo": "^.*$"}, {"description": "prod", "repo": "^prod-"}]`,
			[]string{`rule 2 ("prod") never applies: rule 1 ("everything", repo "^.*$") before it matches every repository, and only the first matching rule applies to a repository`}},
		{"literal rule shadowed by a regex",
			`[{"repo": "^prod"}, {"repo": "^dev$"}, {"description": "keep prod", "repo": "^prod$"}]`,
			[]string{`rule 3 ("keep prod") never applies: rule 1 (repo "^prod") before it also matches repository "prod", and only the first matching rule applies to a repository`}},
		{"literal rule repeated",
			`[{"repo": "^app$"}, {"repo": "^app$"}]`,
			[]string{`rule 2 never applies: rule 1 (repo "^app$") before it also matches repository "app", and only the first matching rule applies to a repository`}},
		{"lists of a rule that never applies",
			`[{"repo": ".+"}, {"repo": "^app$", "tagged": [{"keep": false}, {"tag": "^v"}]}]`,
			[]string{`rule 2 never applies: rule 1 (repo ".+") before it matches every repository, and only the first matching rule applies to a repository`}},
		{"disjoint repository rules",
			`[{"repo": "^prod-"}, {"repo": "^prod$"}, {"repo": "^dev"}, {"repo": ".+"}]`,
			nil},
		{"tagged rule after an unconstrained one",
			`[{"repo": "^app$", "tagged": [{"keep": false}, {"tag": "^release-", "keep": true}]}]`,
			[]string{`rule 1: tagged rule 2 never applies: tagged rule 1 before it matches every tagged manifest`}},
		{"tagged rules after a .+ catch-all",
			`[{"repo": "^app$", "tagged": [{"tag": "^v", "keep": true}, {"tag": ".+", "keep": false}, {"tag": "^release-"}, {"digest": "^sha256:"}]}]`,
			[]string{`rule 1: tagged rules 3-4 never apply: tagged rule 2 before them matches every tagged manifest`}},
		{"untagged rule after an unconstrained one",
			`[{"repo": "^a$"}, {"description": "app", "repo": "^app$", "untagged": [{"keep": false}, {"digest": "^sha256:aa", "keep": true}]}]`,
			[]string{`rule 2 ("app"): untagged rule 2 never applies: untagged rule 1 before it matches every untagged manifest`}},
		{"constrained rules before a catch-all",
			`[{"repo": "^app$",
			   "tagged": [{"tag": "^v", "keep": true}, {"match_older": "30d", "keep": false}, {"newest": 3}, {"arch": "arm64"}, {"os": "linux"}, {"digest": "^sha256:"}, {"match_newer": "1h"}, {"keep": false}],
			   "untagged": [{"match_older": "24h", "keep": false}, {"keep": true}]}]`,
			nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			specs, err := ParseSpecs(strings.NewReader(tt.rules))
			if err != nil {
				t.Fatal(err)
			}
			ruleSet, err := Compile(specs)
			if err != nil {
				t.Fatal(err)
			}
			if got := Warnings(ruleSet); !slices.Equal(got, tt.want) {
				t.Errorf("warnings = %q\n    want %q", got, tt.want)
			}
		})
	}
}

func TestMatchesAnyName(t *testing.T) {
	for pattern, want := range map[string]bool{
		".+": true, ".*": true, "^.+$": true, "^.*$": true, `\A.+\z`: true, "(?s).*": true, ".{1,}": true, "(?m)^.+$": true,
		"^v.+": false, ".+-dev": false, ".": false, ".+^": false, "$.*": false, "^$": false, "a|.+": false, "[^/]+": false,
	} {
		if got := matchesAnyName(regexp.MustCompile(pattern)); got != want {
			t.Errorf("matchesAnyName(%q) = %v, want %v", pattern, got, want)
		}
	}
}
