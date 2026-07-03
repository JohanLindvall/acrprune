package pruner

import (
	"regexp"
	"slices"
	"time"

	"github.com/JohanLindvall/acrprune/internal/registry"
	"github.com/JohanLindvall/acrprune/internal/rules"
)

// commonRuleMatches reports whether the manifest satisfies all of the rule's
// criteria. ordered must be sorted newest first and contain the manifests
// the newest-N constraint ranks against.
func commonRuleMatches(rule rules.CommonRule, m *registry.Manifest, ordered []*registry.Manifest, now time.Time) bool {
	if !matchAny(rule.Architecture, m.Architectures()) {
		return false
	}
	if rule.MatchNewest != 0 && !matchesNewest(m, ordered, rule.MatchNewest) {
		return false
	}
	if rule.MatchNewerThan != 0 && !m.Azure.LastUpdatedOn.Add(rule.MatchNewerThan).After(now) {
		return false
	}
	if rule.MatchOlderThan != 0 && !m.Azure.LastUpdatedOn.Add(rule.MatchOlderThan).Before(now) {
		return false
	}
	return true
}

// matchAny reports whether any value matches the regexp; a nil regexp
// matches unconditionally.
func matchAny(re *regexp.Regexp, values []string) bool {
	if re == nil {
		return true
	}
	return slices.ContainsFunc(values, re.MatchString)
}

// matchesNewest reports whether m is within the newest slots of ordered
// (sorted newest first) when newest is positive, or outside the -newest
// newest slots when negative.
func matchesNewest(m *registry.Manifest, ordered []*registry.Manifest, newest int) bool {
	if newest > 0 {
		return slices.Contains(ordered[:min(newest, len(ordered))], m)
	}
	if newest < 0 {
		return slices.Contains(ordered[min(-newest, len(ordered)):], m)
	}
	return false
}
