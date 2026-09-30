package pruner

import (
	"fmt"
	"math"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	v1 "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/JohanLindvall/acrprune/internal/registry"
	"github.com/JohanLindvall/acrprune/internal/rules"
)

// keepsWithNewest builds a rule deleting untagged manifests with the given
// `newest` constraint and reports which of them it keeps. The rule is built
// directly rather than compiled, since rule files may not say `newest: 0`.
func keepsWithNewest(t *testing.T, manifests []*registry.Manifest, newest int, now time.Time) []string {
	t.Helper()
	rule := &rules.RepoRule{
		Repo:     regexp.MustCompile(".+"),
		Untagged: []rules.UntaggedRule{{CommonRule: rules.CommonRule{MatchNewest: newest, Keep: false}}},
	}
	e := newEvaluator(rule, manifests, now)
	var kept []string
	for _, m := range manifests {
		if keeps(e, m) {
			kept = append(kept, m.Digest)
		}
	}
	slices.Sort(kept)
	return kept
}

// keeps reports whether the evaluator keeps the manifest.
func keeps(e *evaluator, m *registry.Manifest) bool {
	keep, _ := e.keep(m)
	return keep
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
		{"minimum int excludes everything", math.MinInt, "a,b,c"},
		{"zero imposes no constraint", 0, ""},
	}
	for _, tt := range tests {
		got := keepsWithNewest(t, all, tt.newest, now)
		if fmt.Sprint(got) != fmt.Sprint(splitList(tt.wantKeptIs)) {
			t.Errorf("%s: kept %v, want %v", tt.name, got, splitList(tt.wantKeptIs))
		}
	}
}

func TestPlatformCriteriaMatchOnePlatform(t *testing.T) {
	m := testManifest("index", time.Now(), "v1")
	m.Manifests = []v1.Descriptor{
		{Platform: &v1.Platform{OS: "linux", Architecture: "arm64"}},
		{Platform: &v1.Platform{OS: "windows", Architecture: "amd64"}},
	}
	for _, arch := range []string{"amd64", "arm64"} {
		rule := compileRule(t, &rules.RepoRuleSpec{RepoRegex: ".+", Tagged: []*rules.TaggedRuleSpec{{
			CommonRuleSpec: rules.CommonRuleSpec{OSRegex: new("^linux$"), ArchitectureRegex: new("^" + arch + "$"), Keep: new(false)},
		}}})
		got := keeps(newEvaluator(rule, []*registry.Manifest{m}, time.Now()), m)
		if want := arch == "amd64"; got != want {
			t.Errorf("linux/%s rule keeps index = %v, want %v", arch, got, want)
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
			{TagRegex: new("^release-"), CommonRuleSpec: rules.CommonRuleSpec{MatchNewest: new(2), Keep: new(true)}},
			{TagRegex: new(".+"), CommonRuleSpec: rules.CommonRuleSpec{Keep: new(false)}},
		},
	})
	e := newEvaluator(rule, all, now)

	if !keeps(e, releases[0]) || !keeps(e, releases[1]) {
		t.Error("the 2 newest releases should be kept despite 5 newer feature images")
	}
	if keeps(e, releases[2]) {
		t.Error("the 3rd newest release should fall through to the catch-all delete rule")
	}
	for _, m := range features {
		if keeps(e, m) {
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
	if !matches(rules.CommonRuleSpec{ArchitectureRegex: new("arm64")}, arm) {
		t.Error("arm64 manifest should match arch regex arm64")
	}
	if matches(rules.CommonRuleSpec{ArchitectureRegex: new("amd64")}, arm) {
		t.Error("arm64 manifest should not match arch regex amd64")
	}

	windows := testManifest("win", now)
	windows.OS = "windows"
	if !matches(rules.CommonRuleSpec{OSRegex: new("windows")}, windows) {
		t.Error("windows manifest should match os regex windows")
	}
	if matches(rules.CommonRuleSpec{OSRegex: new("linux")}, windows) {
		t.Error("windows manifest should not match os regex linux")
	}

	pinned := testManifest("sha256:pinned", now)
	if !matches(rules.CommonRuleSpec{DigestRegex: new("^sha256:pinned$")}, pinned) {
		t.Error("manifest should match its own digest")
	}
	if matches(rules.CommonRuleSpec{DigestRegex: new("^sha256:other$")}, pinned) {
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
			{CommonRuleSpec: rules.CommonRuleSpec{MatchNewest: new(2), Keep: new(true)}},
			{TagRegex: new(".+"), CommonRuleSpec: rules.CommonRuleSpec{Keep: new(false)}},
		},
	})
	e := newEvaluator(rule, []*registry.Manifest{sig, a, b}, now)
	if !keeps(e, a) || !keeps(e, b) {
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
		Tagged:    []*rules.TaggedRuleSpec{{TagRegex: new("^v")}},
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

// TestKeepDecidesTagByTag: deleting a manifest deletes all of its tags, so a
// rule deleting one tag cannot delete the manifest while another tag is kept
// by a rule, or matched by none. The tag keeping it is reported.
func TestKeepDecidesTagByTag(t *testing.T) {
	now := time.Now()
	rule := ruleSet(t, `[{"repo": ".+", "tagged": [
		{"tag": "^release-", "keep": true},
		{"tag": "^feature-", "keep": false}
	]}]`)[0]
	tests := []struct {
		tags         []string
		want         bool
		wantSparedBy string
	}{
		{[]string{"feature-a"}, false, ""},
		{[]string{"feature-a", "feature-b"}, false, ""},
		{[]string{"feature-a", "release-1"}, true, "release-1"},
		{[]string{"feature-a", "sha-1234"}, true, "sha-1234"},
		{[]string{"release-1", "sha-1234"}, true, ""},
	}
	for _, tt := range tests {
		m := testManifest("m", now, tt.tags...)
		keep, sparedBy := newEvaluator(rule, []*registry.Manifest{m}, now).keep(m)
		if keep != tt.want || sparedBy != tt.wantSparedBy {
			t.Errorf("tags %v: keep = %v, %q; want %v, %q", tt.tags, keep, sparedBy, tt.want, tt.wantSparedBy)
		}
	}
}

// TestNewestRanksManifestsByAnyTag: a ranked rule ranks the manifests any of
// whose tags it matches, and then decides each tag it matches by the
// manifest's rank.
func TestNewestRanksManifestsByAnyTag(t *testing.T) {
	now := time.Now()
	newest := testManifest("newest", now, "v2", "latest")
	older := testManifest("older", now.Add(-time.Hour), "v1")
	rule := ruleSet(t, `[{"repo": ".+", "tagged": [
		{"tag": "^v", "newest": 1, "keep": true},
		{"tag": ".+", "keep": false}
	]}]`)[0]
	e := newEvaluator(rule, []*registry.Manifest{newest, older}, now)
	if keep, sparedBy := e.keep(newest); !keep || sparedBy != "v2" {
		t.Errorf("newest: keep = %v, %q; want it kept by v2 despite latest", keep, sparedBy)
	}
	if keeps(e, older) {
		t.Error("older: the second newest v tag should fall through to the catch-all")
	}
}

// TestNewestWithOtherCriteria: `newest` ranks only the manifests matching the
// rule's other criteria, so its window shifts neither with manifests those
// criteria exclude nor with rules earlier in the list.
func TestNewestWithOtherCriteria(t *testing.T) {
	now := time.Now()
	manifest := func(digest string, age time.Duration, arch string) *registry.Manifest {
		m := testManifest(digest, now.Add(-age))
		m.Architecture = arch
		return m
	}
	tests := []struct {
		name      string
		untagged  string // the untagged rules, as JSON
		manifests []*registry.Manifest
		wantKept  string
	}{
		{
			name:     "ranks only manifests old enough",
			untagged: `[{"newest": 2, "match_older": "1h", "keep": false}]`,
			manifests: []*registry.Manifest{
				manifest("fresh1", 10*time.Minute, ""), manifest("fresh2", 20*time.Minute, ""),
				manifest("old1", 2*time.Hour, ""), manifest("old2", 3*time.Hour, ""), manifest("old3", 4*time.Hour, ""),
			},
			wantKept: "fresh1,fresh2,old3",
		},
		{
			name:     "excludes the newest of the architecture",
			untagged: `[{"newest": -1, "arch": "^amd64$", "keep": false}]`,
			manifests: []*registry.Manifest{
				manifest("arm", 0, "arm64"),
				manifest("amd1", time.Hour, "amd64"), manifest("amd2", 2*time.Hour, "amd64"), manifest("amd3", 3*time.Hour, "amd64"),
			},
			wantKept: "amd1,arm",
		},
		{
			// The newest manifest, kept by the earlier rule, still takes
			// the first slot of the window.
			name:     "ranked and unranked rules mixed",
			untagged: `[{"digest": "^pinned$", "keep": true}, {"newest": 2, "keep": true}, {"keep": false}]`,
			manifests: []*registry.Manifest{
				manifest("pinned", 0, ""),
				manifest("new1", time.Hour, ""), manifest("new2", 2*time.Hour, ""), manifest("new3", 3*time.Hour, ""),
			},
			wantKept: "new1,pinned",
		},
		{
			name:     "equal ages rank by reference",
			untagged: `[{"newest": -1, "keep": false}]`,
			manifests: []*registry.Manifest{
				manifest("c", time.Hour, ""), manifest("a", time.Hour, ""), manifest("b", time.Hour, ""),
			},
			wantKept: "a",
		},
	}
	for _, tt := range tests {
		rule := ruleSet(t, `[{"repo": ".+", "untagged": `+tt.untagged+`}]`)[0]
		e := newEvaluator(rule, tt.manifests, now)
		var kept []string
		for _, m := range tt.manifests {
			if keeps(e, m) {
				kept = append(kept, m.Digest)
			}
		}
		slices.Sort(kept)
		if got := strings.Join(kept, ","); got != tt.wantKept {
			t.Errorf("%s: kept %s, want %s", tt.name, got, tt.wantKept)
		}
	}
}
