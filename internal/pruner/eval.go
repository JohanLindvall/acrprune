package pruner

import (
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/JohanLindvall/acrprune/internal/registry"
	"github.com/JohanLindvall/acrprune/internal/rules"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

// evaluator decides, for a single repository under a single repository rule,
// which manifests a rule matches.
//
// It exists because of `newest`: that constraint ranks a manifest against the
// other manifests the *same rule* matches. Only rules with that constraint
// need a precomputed candidate set; ordinary predicates run on demand.
type evaluator struct {
	rule *rules.RepoRule
	now  time.Time
	// tagged[i] and untagged[i] give the position, newest first, of every
	// manifest matching rule i's non-rank criteria. For ranked rules, an
	// absent manifest means no match; other rules have a nil map.
	tagged   []map[*registry.Manifest]int
	untagged []map[*registry.Manifest]int
}

func newEvaluator(rule *rules.RepoRule, manifests []*registry.Manifest, now time.Time) *evaluator {
	e := &evaluator{rule: rule, now: now}
	e.tagged = make([]map[*registry.Manifest]int, len(rule.Tagged))
	e.untagged = make([]map[*registry.Manifest]int, len(rule.Untagged))
	needTagged := slices.ContainsFunc(rule.Tagged, func(r rules.TaggedRule) bool { return r.MatchNewest != 0 })
	needUntagged := slices.ContainsFunc(rule.Untagged, func(r rules.UntaggedRule) bool { return r.MatchNewest != 0 })
	if !needTagged && !needUntagged {
		return e
	}

	var tagged, untagged []*registry.Manifest
	for _, m := range manifests {
		if m.SubjectDigest() != "" || m.HasOwner {
			// Referrers (signatures, attestations) follow their subject's
			// fate instead of matching rules, and the platform images and
			// attestations of an index, tagged or not, are kept whenever
			// the index is. Neither must consume `newest` ranking slots
			// meant for images: every signed or multi-platform build would
			// otherwise fill the window meant for rollbacks.
			continue
		}
		if len(m.Tags) > 0 {
			if needTagged {
				tagged = append(tagged, m)
			}
		} else if needUntagged {
			untagged = append(untagged, m)
		}
	}
	slices.SortFunc(tagged, byNewest)
	slices.SortFunc(untagged, byNewest)

	for i, r := range rule.Tagged {
		if r.MatchNewest == 0 {
			continue // ordinary predicates need no repository-sized rank map
		}
		e.tagged[i] = e.rank(tagged, func(m *registry.Manifest) bool {
			return matchAny(r.Tag, m.Tags) && e.matchesCriteria(r.CommonRule, m)
		})
	}
	for i, r := range rule.Untagged {
		if r.MatchNewest == 0 {
			continue
		}
		e.untagged[i] = e.rank(untagged, func(m *registry.Manifest) bool {
			return e.matchesCriteria(r.CommonRule, m)
		})
	}
	return e
}

// keep reports whether the rules keep the manifest. A manifest no rule
// matches is kept.
//
// Deleting a tagged manifest deletes every one of its tags, so a tagged
// manifest is decided tag by tag: each tag gets the first rule matching it and
// the manifest, and the manifest is deleted only when each of those rules
// deletes it. A tag no rule matches keeps the manifest. When some tag's rule
// deletes the manifest and another tag keeps it, sparedBy names the tag keeping
// it; it is empty otherwise.
func (e *evaluator) keep(m *registry.Manifest) (keep bool, sparedBy string) {
	if m.SubjectDigest() != "" {
		return true, "" // referrers are decided by keepReferrers
	}
	if len(m.Tags) == 0 {
		return e.keepUntagged(m), ""
	}
	condemned := false
	for _, tag := range m.Tags {
		if !e.keepTag(m, tag) {
			condemned = true
		} else if !keep {
			keep, sparedBy = true, tag
		}
	}
	if !condemned {
		sparedBy = "" // nothing needed sparing
	}
	return keep, sparedBy
}

// keepTag reports whether the first tagged rule matching the tag and the
// manifest keeps the manifest, or whether no rule matches. `newest` ranks the
// manifest among those the rule matches by any of their tags; the manifests
// of an index are left to it, as keepUntagged explains.
func (e *evaluator) keepTag(m *registry.Manifest, tag string) bool {
	for i, r := range e.rule.Tagged {
		if !matchString(r.Tag, tag) {
			continue
		}
		if r.MatchNewest != 0 && m.HasOwner {
			if e.matchesCriteria(r.CommonRule, m) {
				return false
			}
			continue
		}
		if e.matches(e.tagged[i], r.CommonRule, m) {
			return r.Keep
		}
	}
	return true
}

// keepUntagged reports whether the first untagged rule matching the manifest
// keeps it, or whether no rule matches.
func (e *evaluator) keepUntagged(m *registry.Manifest) bool {
	for i, r := range e.rule.Untagged {
		if r.MatchNewest != 0 && m.HasOwner {
			// An index's manifests are not ranked. One the rule matches
			// otherwise is left to the indexes referencing it, which keep
			// it whenever one of them is kept: kept on its own it could
			// outlive them, and passed on to a later rule it could match
			// none and outlive them all the same.
			if e.matchesCriteria(r.CommonRule, m) {
				return false
			}
			continue
		}
		if e.matches(e.untagged[i], r.CommonRule, m) {
			return r.Keep
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
	if rule.MatchNewest == 0 {
		return e.matchesCriteria(rule, m)
	}
	position, ok := positions[m]
	if !ok {
		return false
	}
	if rule.MatchNewest > 0 {
		return position < rule.MatchNewest
	}
	// Negate the non-negative position, not the possibly minimum int.
	return -position <= rule.MatchNewest
}

// matchesCriteria reports whether the manifest satisfies every criterion of
// the rule except `newest`, which can only be resolved once the rest of the
// candidate set is known.
func (e *evaluator) matchesCriteria(rule rules.CommonRule, m *registry.Manifest) bool {
	if !matchesPlatform(rule, m) {
		return false
	}
	if !matchAny(rule.Digest, []string{m.Digest}) {
		return false
	}
	if rule.MatchNewerThan != 0 && !m.LastUpdated.Add(rule.MatchNewerThan).After(e.now) {
		return false
	}
	if rule.MatchOlderThan != 0 && !m.LastUpdated.Add(rule.MatchOlderThan).Before(e.now) {
		return false
	}
	return true
}

// Both platform constraints must match the same platform. Matching the OS
// and architecture separately can invent a platform from two index entries.
func matchesPlatform(rule rules.CommonRule, m *registry.Manifest) bool {
	if rule.Architecture == nil && rule.OS == nil {
		return true
	}
	match := func(re *regexp.Regexp, value string) bool {
		return re == nil || value != "" && value != "unknown" && re.MatchString(value)
	}
	platformMatches := func(platform v1.Platform) bool {
		return match(rule.Architecture, platform.Architecture) && match(rule.OS, platform.OS)
	}
	if platformMatches(v1.Platform{Architecture: m.Architecture, OS: m.OS}) {
		return true
	}
	for _, child := range m.Manifests {
		if child.Platform != nil && platformMatches(*child.Platform) {
			return true
		}
	}
	return false
}

// matchAny reports whether any value matches the regexp; a nil regexp matches
// unconditionally.
func matchAny(re *regexp.Regexp, values []string) bool {
	if re == nil {
		return true
	}
	return slices.ContainsFunc(values, re.MatchString)
}

// matchString reports whether the value matches the regexp; a nil regexp
// matches unconditionally.
func matchString(re *regexp.Regexp, value string) bool {
	return re == nil || re.MatchString(value)
}

// byNewest orders manifests newest first, breaking ties on the reference so
// that repeated runs over an unchanged repository decide identically.
func byNewest(a, b *registry.Manifest) int {
	if c := b.LastUpdated.Compare(a.LastUpdated); c != 0 {
		return c
	}
	return strings.Compare(a.Ref(), b.Ref())
}
