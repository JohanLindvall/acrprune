package pruner

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/JohanLindvall/acrprune/internal/registry"
	"github.com/JohanLindvall/acrprune/internal/rules"
)

// keepsWithNewest builds a rule matching untagged manifests with the given
// `newest` constraint and reports which of them it keeps.
func keepsWithNewest(t *testing.T, manifests []*registry.Manifest, newest int, now time.Time) []string {
	t.Helper()
	rule := compileRule(t, &rules.RepoRuleSpec{
		RepoRegex: ".+",
		Untagged: []*rules.UntaggedRuleSpec{
			{CommonRuleSpec: rules.CommonRuleSpec{MatchNewest: to.Ptr(newest), Keep: to.Ptr(false)}},
		},
	})
	e := newEvaluator(rule, manifests, now)
	var kept []string
	for _, m := range manifests {
		if e.keep(m) {
			kept = append(kept, m.Digest)
		}
	}
	slices.Sort(kept)
	return kept
}

func TestMatchNewest(t *testing.T) {
	now := time.Now()
	a := testManifest("a", now)
	b := testManifest("b", now.Add(-time.Hour))
	c := testManifest("c", now.Add(-2*time.Hour))
	all := []*registry.Manifest{a, b, c}

	tests := []struct {
		name       string
		newest     int
		wantKeptIs string // digests the delete rule did NOT match
	}{
		{"delete the 2 newest", 2, "c"},
		{"newest larger than the set deletes everything", 5, ""},
		{"delete all but the newest", -1, "a"},
		{"excluding more than the set deletes nothing", -5, "a,b,c"},
		{"zero imposes no constraint", 0, ""},
	}
	for _, tt := range tests {
		got := keepsWithNewest(t, all, tt.newest, now)
		if fmt.Sprint(got) != fmt.Sprint(splitList(tt.wantKeptIs)) {
			t.Errorf("%s: kept %v, want %v", tt.name, got, splitList(tt.wantKeptIs))
		}
	}
}

// TestNewestRanksWithinRuleMatches covers the point of the constraint: "keep
// the 2 newest release images" must rank the releases against each other, not
// against every manifest in the repository. Ranking repository-wide let a
// burst of feature-branch pushes push every release out of the top N and into
// the catch-all delete rule.
func TestNewestRanksWithinRuleMatches(t *testing.T) {
	now := time.Now()

	var features []*registry.Manifest
	for i := range 5 {
		features = append(features, testManifest(fmt.Sprintf("feature%d", i), now.Add(-time.Duration(i)*time.Hour), fmt.Sprintf("feature-%d", i)))
	}
	releases := []*registry.Manifest{
		testManifest("release3", now.Add(-30*24*time.Hour), "release-1.2"),
		testManifest("release2", now.Add(-31*24*time.Hour), "release-1.1"),
		testManifest("release1", now.Add(-32*24*time.Hour), "release-1.0"),
	}
	all := append(slices.Clone(features), releases...)

	rule := compileRule(t, &rules.RepoRuleSpec{
		RepoRegex: ".+",
		Tagged: []*rules.TaggedRuleSpec{
			{TagRegex: to.Ptr("^release-"), CommonRuleSpec: rules.CommonRuleSpec{MatchNewest: to.Ptr(2), Keep: to.Ptr(true)}},
			{TagRegex: to.Ptr(".+"), CommonRuleSpec: rules.CommonRuleSpec{Keep: to.Ptr(false)}},
		},
	})
	e := newEvaluator(rule, all, now)

	if !e.keep(releases[0]) || !e.keep(releases[1]) {
		t.Error("the 2 newest releases should be kept despite 5 newer feature images")
	}
	if e.keep(releases[2]) {
		t.Error("the 3rd newest release should fall through to the catch-all delete rule")
	}
	for _, m := range features {
		if e.keep(m) {
			t.Errorf("feature image %s should be deleted", m.Digest)
		}
	}
}

func TestCommonRuleMatches(t *testing.T) {
	now := time.Now()
	old := testManifest("old", now.Add(-100*24*time.Hour))
	fresh := testManifest("fresh", now.Add(-time.Hour))
	all := []*registry.Manifest{fresh, old}

	// matches reports whether the single untagged rule built from spec picks
	// out the manifest.
	matches := func(spec rules.CommonRuleSpec, m *registry.Manifest) bool {
		rule := compileRule(t, &rules.RepoRuleSpec{
			RepoRegex: ".+",
			Untagged:  []*rules.UntaggedRuleSpec{{CommonRuleSpec: spec}},
		})
		e := newEvaluator(rule, all, now)
		return e.matchesCriteria(rule.Untagged[0].CommonRule, m)
	}
	duration := func(d time.Duration) *rules.Duration { return &rules.Duration{Duration: d} }

	if !matches(rules.CommonRuleSpec{}, old) {
		t.Error("empty rule should match")
	}

	older := rules.CommonRuleSpec{MatchOlderThan: duration(30 * 24 * time.Hour)}
	if !matches(older, old) {
		t.Error("old manifest should match match_older=30d")
	}
	if matches(older, fresh) {
		t.Error("fresh manifest should not match match_older=30d")
	}

	newer := rules.CommonRuleSpec{MatchNewerThan: duration(24 * time.Hour)}
	if !matches(newer, fresh) {
		t.Error("fresh manifest should match match_newer=24h")
	}
	if matches(newer, old) {
		t.Error("old manifest should not match match_newer=24h")
	}

	arm := testManifest("arm", now)
	arm.Architecture = "arm64"
	if !matches(rules.CommonRuleSpec{ArchitectureRegex: to.Ptr("arm64")}, arm) {
		t.Error("arm64 manifest should match arch regex arm64")
	}
	if matches(rules.CommonRuleSpec{ArchitectureRegex: to.Ptr("amd64")}, arm) {
		t.Error("arm64 manifest should not match arch regex amd64")
	}

	windows := testManifest("win", now)
	windows.OS = "windows"
	if !matches(rules.CommonRuleSpec{OSRegex: to.Ptr("windows")}, windows) {
		t.Error("windows manifest should match os regex windows")
	}
	if matches(rules.CommonRuleSpec{OSRegex: to.Ptr("linux")}, windows) {
		t.Error("windows manifest should not match os regex linux")
	}

	pinned := testManifest("sha256:pinned", now)
	if !matches(rules.CommonRuleSpec{DigestRegex: to.Ptr("^sha256:pinned$")}, pinned) {
		t.Error("manifest should match its own digest")
	}
	if matches(rules.CommonRuleSpec{DigestRegex: to.Ptr("^sha256:other$")}, pinned) {
		t.Error("manifest should not match a different digest")
	}
}

// TestReferrersDontConsumeNewestSlots: signatures are decided by their
// subject, so their tags must not displace images from a `newest` window.
func TestReferrersDontConsumeNewestSlots(t *testing.T) {
	now := time.Now()
	sig := testManifest("sig", now, "sha256-a.sig") // newest of all
	sig.Subject = &v1.Descriptor{Digest: "a"}
	a := testManifest("a", now.Add(-time.Hour), "v2")
	b := testManifest("b", now.Add(-2*time.Hour), "v1")

	rule := compileRule(t, &rules.RepoRuleSpec{
		RepoRegex: ".+",
		Tagged: []*rules.TaggedRuleSpec{
			{CommonRuleSpec: rules.CommonRuleSpec{MatchNewest: to.Ptr(2), Keep: to.Ptr(true)}},
			{TagRegex: to.Ptr(".+"), CommonRuleSpec: rules.CommonRuleSpec{Keep: to.Ptr(false)}},
		},
	})
	e := newEvaluator(rule, []*registry.Manifest{sig, a, b}, now)
	if !e.keep(a) || !e.keep(b) {
		t.Error("both images should occupy the 2 newest slots; the signature must not consume one")
	}
}

// TestByNewestIsDeterministic pins the tiebreak: manifests pushed in the same
// second must not swap places between runs, or `newest` would delete a
// different one each time.
func TestByNewestIsDeterministic(t *testing.T) {
	now := time.Now()
	a := testManifest("aaa", now)
	b := testManifest("bbb", now)
	c := testManifest("ccc", now.Add(-time.Hour))

	for _, order := range [][]*registry.Manifest{{a, b, c}, {c, b, a}, {b, c, a}} {
		sorted := slices.Clone(order)
		slices.SortFunc(sorted, byNewest)
		if sorted[0] != a || sorted[1] != b || sorted[2] != c {
			t.Errorf("byNewest is not a total order: %v", []string{sorted[0].Digest, sorted[1].Digest, sorted[2].Digest})
		}
	}
}

func TestMatchAny(t *testing.T) {
	if !matchAny(nil, nil) {
		t.Error("a nil regexp should match unconditionally")
	}
	rule := compileRule(t, &rules.RepoRuleSpec{
		RepoRegex: ".+",
		Tagged:    []*rules.TaggedRuleSpec{{TagRegex: to.Ptr("^v")}},
	})
	if !matchAny(rule.Tagged[0].Tag, []string{"latest", "v1"}) {
		t.Error("should match when any value matches")
	}
	if matchAny(rule.Tagged[0].Tag, []string{"latest"}) {
		t.Error("should not match when no value matches")
	}
	if matchAny(rule.Tagged[0].Tag, nil) {
		t.Error("should not match an empty value list")
	}
}
