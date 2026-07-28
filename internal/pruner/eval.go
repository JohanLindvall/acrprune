package pruner

import (
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/JohanLindvall/acrprune/internal/registry"
	"github.com/JohanLindvall/acrprune/internal/rules"
)

// evaluator decides, for a single repository under a single repository rule,
// which manifests a rule matches.
//
// It exists because of `newest`: that constraint ranks a manifest against the
// other manifests the *same rule* matches, so the candidate set of every
// tagged and untagged rule has to be resolved before any single manifest can
// be judged.
type evaluator struct {
	rule *rules.RepoRule
	now  time.Time
	// tagged[i] and untagged[i] give the position, newest first, of every
	// manifest matching rule i's non-rank criteria. Absent means no match.
	tagged   []map[*registry.Manifest]int
	untagged []map[*registry.Manifest]int
}

func newEvaluator(rule *rules.RepoRule, manifests []*registry.Manifest, now time.Time) *evaluator {
	e := &evaluator{rule: rule, now: now}

	var tagged, untagged []*registry.Manifest
	for _, m := range manifests {
		if len(m.Tags()) > 0 {
			tagged = append(tagged, m)
		} else {
			untagged = append(untagged, m)
		}
	}
	slices.SortFunc(tagged, byNewest)
	slices.SortFunc(untagged, byNewest)

	e.tagged = make([]map[*registry.Manifest]int, len(rule.Tagged))
	for i, r := range rule.Tagged {
		e.tagged[i] = e.rank(tagged, func(m *registry.Manifest) bool {
			return matchAny(r.Tag, m.Tags()) && e.matchesCriteria(r.CommonRule, m)
		})
	}
	e.untagged = make([]map[*registry.Manifest]int, len(rule.Untagged))
	for i, r := range rule.Untagged {
		e.untagged[i] = e.rank(untagged, func(m *registry.Manifest) bool {
			return e.matchesCriteria(r.CommonRule, m)
		})
	}
	return e
}

// keep reports whether the first rule matching the manifest keeps it. A
// manifest no rule matches is kept.
func (e *evaluator) keep(m *registry.Manifest) bool {
	if len(m.Tags()) == 0 {
		for i, r := range e.rule.Untagged {
			if e.matches(e.untagged[i], r.CommonRule, m) {
				return r.Keep
			}
		}
	} else {
		for i, r := range e.rule.Tagged {
			if e.matches(e.tagged[i], r.CommonRule, m) {
				return r.Keep
			}
		}
	}
	return true
}

// rank numbers the manifests satisfying pred by their position in ordered,
// which must already be sorted newest first.
func (e *evaluator) rank(ordered []*registry.Manifest, pred func(*registry.Manifest) bool) map[*registry.Manifest]int {
	positions := map[*registry.Manifest]int{}
	for _, m := range ordered {
		if pred(m) {
			positions[m] = len(positions)
		}
	}
	return positions
}

// matches reports whether a rule applies to the manifest, given the positions
// of everything else that rule matched.
func (e *evaluator) matches(positions map[*registry.Manifest]int, rule rules.CommonRule, m *registry.Manifest) bool {
	position, ok := positions[m]
	if !ok {
		return false
	}
	switch {
	case rule.MatchNewest > 0:
		return position < rule.MatchNewest
	case rule.MatchNewest < 0:
		return position >= -rule.MatchNewest
	default:
		return true
	}
}

// matchesCriteria reports whether the manifest satisfies every criterion of
// the rule except `newest`, which can only be resolved once the rest of the
// candidate set is known.
func (e *evaluator) matchesCriteria(rule rules.CommonRule, m *registry.Manifest) bool {
	if !matchAny(rule.Architecture, m.Architectures()) {
		return false
	}
	if rule.MatchNewerThan != 0 && !m.LastUpdated().Add(rule.MatchNewerThan).After(e.now) {
		return false
	}
	if rule.MatchOlderThan != 0 && !m.LastUpdated().Add(rule.MatchOlderThan).Before(e.now) {
		return false
	}
	return true
}

// matchAny reports whether any value matches the regexp; a nil regexp matches
// unconditionally.
func matchAny(re *regexp.Regexp, values []string) bool {
	if re == nil {
		return true
	}
	return slices.ContainsFunc(values, re.MatchString)
}

// byNewest orders manifests newest first, breaking ties on the reference so
// that repeated runs over an unchanged repository decide identically.
func byNewest(a, b *registry.Manifest) int {
	if c := b.LastUpdated().Compare(a.LastUpdated()); c != 0 {
		return c
	}
	return strings.Compare(a.Ref(), b.Ref())
}
