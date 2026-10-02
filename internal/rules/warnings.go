package rules

import (
	"fmt"
	"regexp"
	"regexp/syntax"
)

// Warnings describes the rules of a compiled rule set that can never apply,
// in rule order. Pruning uses only the first repository rule matching a
// repository and, within it, the first matching tagged or untagged rule, so
// the rules after a catch-all are dead: most likely rules the author expected
// to protect something. Such rule sets are valid; the caller should report
// the warnings.
func Warnings(ruleSet []*RepoRule) []string {
	var warnings []string
	shadows := repositoryShadows(ruleSet)
	for i, rule := range ruleSet {
		if warning := shadows[i]; warning != "" {
			// What its lists would do is moot.
			warnings = append(warnings, warning)
			continue
		}
		name := ruleName(i, rule.Description)
		if warning := unreachable(name, "tagged", len(rule.Tagged), func(j int) bool {
			t := rule.Tagged[j]
			return t.unconstrained() && (t.Tag == nil || matchesAnyName(t.Tag))
		}); warning != "" {
			warnings = append(warnings, warning)
		}
		if warning := unreachable(name, "untagged", len(rule.Untagged), func(j int) bool {
			return rule.Untagged[j].unconstrained()
		}); warning != "" {
			warnings = append(warnings, warning)
		}
	}
	return warnings
}

// repositoryShadows names the earliest rule shadowing each unreachable rule.
// Index literal names and parse each catch-all pattern only once: generated
// inventories contain one literal rule per repository, and checking every
// pair used quadratic time and memory before any registry request was made.
func repositoryShadows(ruleSet []*RepoRule) map[int]string {
	warnings := map[int]string{}
	literals := map[string]int{}
	var patterns []int
	catchAll := -1
	for i, rule := range ruleSet {
		literal, isLiteral := rule.LiteralRepoName()
		shadow := catchAll
		if isLiteral {
			if j, ok := literals[literal]; ok && (shadow < 0 || j < shadow) {
				shadow = j
			}
			for _, j := range patterns {
				if shadow >= 0 && j >= shadow {
					break
				}
				if ruleSet[j].Repo.MatchString(literal) {
					shadow = j
					break
				}
			}
		}
		if shadow >= 0 {
			earlier := ruleSet[shadow]
			by := fmt.Sprintf("rule %d (repo %q)", shadow+1, earlier.Repo)
			if earlier.Description != "" {
				by = fmt.Sprintf("rule %d (%q, repo %q)", shadow+1, earlier.Description, earlier.Repo)
			}
			why := "matches every repository"
			if shadow != catchAll {
				why = fmt.Sprintf("also matches repository %q", literal)
			}
			warnings[i] = fmt.Sprintf("%s never applies: %s before it %s, and only the first matching rule applies to a repository",
				ruleName(i, rule.Description), by, why)
		}
		if catchAll >= 0 {
			continue // later rules can never be the earliest match
		}
		if isLiteral {
			if _, seen := literals[literal]; !seen {
				literals[literal] = i
			}
		} else if matchesAnyName(rule.Repo) {
			catchAll = i
		} else {
			patterns = append(patterns, i)
		}
	}
	return warnings
}

// unreachable describes the entries of a tagged or untagged list that follow
// the first catch-all entry, or returns "" if there are none.
func unreachable(rule, list string, n int, catchAll func(int) bool) string {
	for j := range n {
		if !catchAll(j) {
			continue
		}
		switch after := n - j - 1; after {
		case 0:
			return ""
		case 1:
			return fmt.Sprintf("%s: %s rule %d never applies: %s rule %d before it matches every %s manifest",
				rule, list, j+2, list, j+1, list)
		default:
			return fmt.Sprintf("%s: %s rules %d-%d never apply: %s rule %d before them matches every %s manifest",
				rule, list, j+2, n, list, j+1, list)
		}
	}
	return ""
}

// unconstrained reports whether the rule's criteria, other than a tagged
// rule's tag, match every manifest.
func (c CommonRule) unconstrained() bool {
	return c.Architecture == nil && c.OS == nil && c.Digest == nil &&
		c.MatchNewest == 0 && c.MatchNewerThan == 0 && c.MatchOlderThan == 0
}

// matchesAnyName reports whether re is one of the usual spellings of "any
// name": .+ or .*, possibly anchored. Tags and repository names are never
// empty and never contain a newline, so these match every one of them.
func matchesAnyName(re *regexp.Regexp) bool {
	parsed, err := syntax.Parse(re.String(), syntax.Perl)
	if err != nil {
		return false
	}
	parsed = parsed.Simplify()
	parts := []*syntax.Regexp{parsed}
	if parsed.Op == syntax.OpConcat {
		parts = parsed.Sub
	}
	// Anchors at the start and end do not narrow a repetition of any
	// character.
	for len(parts) > 0 && (parts[0].Op == syntax.OpBeginText || parts[0].Op == syntax.OpBeginLine) {
		parts = parts[1:]
	}
	for len(parts) > 0 && (parts[len(parts)-1].Op == syntax.OpEndText || parts[len(parts)-1].Op == syntax.OpEndLine) {
		parts = parts[:len(parts)-1]
	}
	if len(parts) != 1 {
		return false
	}
	repeat := parts[0]
	if repeat.Op != syntax.OpStar && repeat.Op != syntax.OpPlus {
		return false
	}
	anyChar := repeat.Sub[0].Op
	return anyChar == syntax.OpAnyChar || anyChar == syntax.OpAnyCharNotNL
}
