package pruner

import (
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/containers/azcontainerregistry"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/JohanLindvall/acrprune/internal/registry"
	"github.com/JohanLindvall/acrprune/internal/rules"
)

func testManifest(ref string, updated time.Time, tags ...string) *registry.Manifest {
	m := &registry.Manifest{
		Ref: ref,
		Azure: &azcontainerregistry.ManifestAttributes{
			LastUpdatedOn: &updated,
		},
	}
	for _, tag := range tags {
		m.Azure.Tags = append(m.Azure.Tags, to.Ptr(tag))
	}
	return m
}

func testPruner() *Pruner {
	return &Pruner{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

func compileRule(t *testing.T, spec *rules.RepoRuleSpec) *rules.RepoRule {
	t.Helper()
	rule, err := spec.Compile()
	if err != nil {
		t.Fatal(err)
	}
	return rule
}

func TestMatchesNewest(t *testing.T) {
	now := time.Now()
	a := testManifest("r@a", now)
	b := testManifest("r@b", now.Add(-time.Hour))
	c := testManifest("r@c", now.Add(-2*time.Hour))
	all := []*registry.Manifest{a, b, c}

	tests := []struct {
		name   string
		in     *registry.Manifest
		newest int
		want   bool
	}{
		{"first of 2 newest", a, 2, true},
		{"second of 2 newest", b, 2, true},
		{"third not in 2 newest", c, 2, false},
		{"newest larger than len", c, 5, true},
		{"exclude 1 newest, a excluded", a, -1, false},
		{"exclude 1 newest, b included", b, -1, true},
		{"exclude more than len", a, -5, false},
		{"zero never matches", a, 0, false},
	}
	for _, tt := range tests {
		if got := matchesNewest(tt.in, all, tt.newest); got != tt.want {
			t.Errorf("%s: matchesNewest = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestCommonRuleMatches(t *testing.T) {
	now := time.Now()
	old := testManifest("r@old", now.Add(-100*24*time.Hour))
	fresh := testManifest("r@fresh", now.Add(-time.Hour))
	ordered := []*registry.Manifest{fresh, old}

	compile := func(spec rules.CommonRuleSpec) rules.CommonRule {
		rule := compileRule(t, &rules.RepoRuleSpec{Untagged: []*rules.UntaggedRuleSpec{{CommonRuleSpec: spec}}})
		return rule.Untagged[0].CommonRule
	}
	duration := func(d time.Duration) *rules.Duration {
		return &rules.Duration{Duration: d}
	}

	// Empty rule matches everything.
	if !commonRuleMatches(compile(rules.CommonRuleSpec{}), old, ordered, now) {
		t.Error("empty rule should match")
	}

	older := compile(rules.CommonRuleSpec{MatchOlderThan: duration(30 * 24 * time.Hour)})
	if !commonRuleMatches(older, old, ordered, now) {
		t.Error("old manifest should match match_older=30d")
	}
	if commonRuleMatches(older, fresh, ordered, now) {
		t.Error("fresh manifest should not match match_older=30d")
	}

	newer := compile(rules.CommonRuleSpec{MatchNewerThan: duration(24 * time.Hour)})
	if !commonRuleMatches(newer, fresh, ordered, now) {
		t.Error("fresh manifest should match match_newer=24h")
	}
	if commonRuleMatches(newer, old, ordered, now) {
		t.Error("old manifest should not match match_newer=24h")
	}

	newest := compile(rules.CommonRuleSpec{MatchNewest: to.Ptr(1)})
	if !commonRuleMatches(newest, fresh, ordered, now) {
		t.Error("fresh should be in the 1 newest")
	}
	if commonRuleMatches(newest, old, ordered, now) {
		t.Error("old should not be in the 1 newest")
	}

	arm := testManifest("r@arm", now)
	arm.Azure.Architecture = to.Ptr(azcontainerregistry.ArtifactArchitectureArm64)
	if !commonRuleMatches(compile(rules.CommonRuleSpec{ArchitectureRegex: to.Ptr("arm64")}), arm, ordered, now) {
		t.Error("arm64 manifest should match arch regex arm64")
	}
	if commonRuleMatches(compile(rules.CommonRuleSpec{ArchitectureRegex: to.Ptr("amd64")}), arm, ordered, now) {
		t.Error("arm64 manifest should not match arch regex amd64")
	}
}

func TestShouldKeep(t *testing.T) {
	now := time.Now()
	p := testPruner()

	rule := compileRule(t, &rules.RepoRuleSpec{
		RepoRegex: ".+",
		Untagged:  []*rules.UntaggedRuleSpec{{CommonRuleSpec: rules.CommonRuleSpec{Keep: to.Ptr(false)}}},
		Tagged: []*rules.TaggedRuleSpec{
			{TagRegex: to.Ptr("^release-"), CommonRuleSpec: rules.CommonRuleSpec{Keep: to.Ptr(true)}},
			{TagRegex: to.Ptr(".+"), CommonRuleSpec: rules.CommonRuleSpec{Keep: to.Ptr(false)}},
		},
	})

	old := now.Add(-48 * time.Hour)
	release := testManifest("r@1", old, "release-1.0")
	feature := testManifest("r@2", old, "feature-x")
	untagged := testManifest("r@3", old)
	unmatchedTag := testManifest("r@4", old, "v1")

	if !p.shouldKeep(release, rule, nil, nil, now) {
		t.Error("release tag should be kept")
	}
	if p.shouldKeep(feature, rule, nil, nil, now) {
		t.Error("feature tag should be deleted")
	}
	if p.shouldKeep(untagged, rule, nil, nil, now) {
		t.Error("untagged should be deleted")
	}
	if p.shouldKeep(unmatchedTag, rule, nil, nil, now) {
		t.Error("catch-all tag rule should delete v1")
	}

	// No matching rule at all keeps the manifest.
	empty := compileRule(t, &rules.RepoRuleSpec{RepoRegex: ".+"})
	if !p.shouldKeep(feature, empty, nil, nil, now) {
		t.Error("manifest with no matching rule should be kept")
	}

	// Manifests with a subject are always kept.
	signature := testManifest("r@5", old, "feature-x")
	signature.Subject = &v1.Descriptor{Digest: "sha256:parent"}
	if !p.shouldKeep(signature, rule, nil, nil, now) {
		t.Error("manifest with subject should always be kept")
	}

	// The grace period overrides deletion.
	p.KeepYounger = 24 * time.Hour
	young := testManifest("r@6", now.Add(-time.Hour), "feature-x")
	if !p.shouldKeep(young, rule, nil, nil, now) {
		t.Error("young manifest should be kept by grace period")
	}
	if p.shouldKeep(feature, rule, nil, nil, now) {
		t.Error("old manifest should still be deleted with grace period set")
	}
	p.KeepYounger = 0

	// Orphan deletion overrides a keep decision.
	orphanRule := compileRule(t, &rules.RepoRuleSpec{RepoRegex: ".+", DeleteOrphanedManifests: to.Ptr(true)})
	orphan := testManifest("r@7", old, "release-1.0")
	orphan.Orphaned = true
	if p.shouldKeep(orphan, orphanRule, nil, nil, now) {
		t.Error("orphaned manifest should be deleted when delete_orphaned_manifests is set")
	}
}

func TestMarkOrphans(t *testing.T) {
	index := testManifest("r@sha256:idx", time.Now(), "latest")
	index.Manifests = []v1.Descriptor{{Digest: "sha256:child"}, {Digest: "sha256:missing"}}
	child := testManifest("r@sha256:child", time.Now())
	grandIndex := testManifest("r@sha256:top", time.Now())
	grandIndex.Manifests = []v1.Descriptor{{Digest: "sha256:idx"}}
	standalone := testManifest("r@sha256:alone", time.Now())

	manifests := map[string]*registry.Manifest{
		index.Ref:      index,
		child.Ref:      child,
		grandIndex.Ref: grandIndex,
		standalone.Ref: standalone,
	}
	markOrphans(manifests, "r")

	if !index.Orphaned {
		t.Error("index with a missing child should be orphaned")
	}
	if !grandIndex.Orphaned {
		t.Error("orphaned state should propagate up to the referencing index")
	}
	if !child.Orphaned {
		t.Error("children of an orphaned index should be orphaned")
	}
	if standalone.Orphaned {
		t.Error("standalone manifest should not be orphaned")
	}
	if !child.HasOwner || !index.HasOwner {
		t.Error("referenced manifests should be marked as owned")
	}
	if grandIndex.HasOwner || standalone.HasOwner {
		t.Error("unreferenced manifests should not be marked as owned")
	}
}

func TestKeepWithDependencies(t *testing.T) {
	p := testPruner()
	index := testManifest("r@sha256:idx", time.Now(), "latest")
	index.Manifests = []v1.Descriptor{{Digest: "sha256:a"}, {Digest: "sha256:b"}}
	a := testManifest("r@sha256:a", time.Now())
	b := testManifest("r@sha256:b", time.Now())
	manifests := map[string]*registry.Manifest{index.Ref: index, a.Ref: a, b.Ref: b}

	rule := compileRule(t, &rules.RepoRuleSpec{RepoRegex: ".+"})
	kept := map[string]struct{}{}
	if err := p.keepWithDependencies(index.Ref, manifests, kept, "r", rule); err != nil {
		t.Fatal(err)
	}
	if len(kept) != 3 {
		t.Errorf("kept = %v, want all 3", kept)
	}

	// A missing dependency fails unless ignore_missing_manifests is set.
	index.Manifests = append(index.Manifests, v1.Descriptor{Digest: "sha256:gone"})
	strict := compileRule(t, &rules.RepoRuleSpec{RepoRegex: ".+", IgnoreMissingManifests: to.Ptr(false)})
	if err := p.keepWithDependencies(index.Ref, manifests, map[string]struct{}{}, "r", strict); err == nil {
		t.Error("missing dependency should fail in strict mode")
	}
	if err := p.keepWithDependencies(index.Ref, manifests, map[string]struct{}{}, "r", rule); err != nil {
		t.Errorf("missing dependency should be tolerated by default: %v", err)
	}
}

func TestDecideMustDeleteEverything(t *testing.T) {
	p := testPruner()
	now := time.Now()
	keep := testManifest("r@sha256:keep", now, "arm64-app")
	keep.Azure.Architecture = to.Ptr(azcontainerregistry.ArtifactArchitectureArm64)
	deletable := testManifest("r@sha256:del", now.Add(-time.Hour), "amd64-app")
	deletable.Azure.Architecture = to.Ptr(azcontainerregistry.ArtifactArchitectureAmd64)
	manifests := map[string]*registry.Manifest{keep.Ref: keep, deletable.Ref: deletable}

	rule := compileRule(t, &rules.RepoRuleSpec{
		RepoRegex:            ".+",
		MustDeleteEverything: to.Ptr(true),
		Tagged: []*rules.TaggedRuleSpec{
			{CommonRuleSpec: rules.CommonRuleSpec{ArchitectureRegex: to.Ptr("arm64"), Keep: to.Ptr(true)}},
			{CommonRuleSpec: rules.CommonRuleSpec{ArchitectureRegex: to.Ptr("amd64"), Keep: to.Ptr(false)}},
		},
	})

	kept, err := p.decide(manifests, "r", rule)
	if err != nil {
		t.Fatal(err)
	}
	if len(kept) != 2 {
		t.Errorf("must_delete_everything with one kept manifest should keep all, kept = %v", kept)
	}

	// Without the arm64 manifest everything goes.
	delete(manifests, keep.Ref)
	kept, err = p.decide(manifests, "r", rule)
	if err != nil {
		t.Fatal(err)
	}
	if len(kept) != 0 {
		t.Errorf("amd64-only repo should be fully deleted, kept = %v", kept)
	}
}

func TestCountRunning(t *testing.T) {
	now := time.Now()
	running := testManifest("r@1", now, "v1")
	stopped := testManifest("r@2", now, "v9")
	untagged := testManifest("r@3", now)
	manifests := map[string]*registry.Manifest{running.Ref: running, stopped.Ref: stopped, untagged.Ref: untagged}

	specs := rules.KeepRulesFromImageList(strings.NewReader("myreg.azurecr.io/app:v1\n"), "myreg")
	ruleSet, err := rules.Compile(specs)
	if err != nil {
		t.Fatal(err)
	}

	if got := countRunning(manifests, "app", ruleSet); got != 1 {
		t.Errorf("countRunning = %d, want 1 (only v1 is running; the catch-all delete rule must not count)", got)
	}
	if got := countRunning(manifests, "other", ruleSet); got != 0 {
		t.Errorf("countRunning for unmatched repo = %d, want 0", got)
	}
}

func TestCalculateStats(t *testing.T) {
	now := time.Now()
	older := now.Add(-time.Hour)

	m1 := testManifest("r@1", now, "latest")
	m1.Size = 100
	m1.Config = &v1.Descriptor{Digest: "sha256:cfg", Size: 10}
	m1.Layers = []v1.Descriptor{
		{Digest: "sha256:l1", Size: 1000},
		{Digest: "sha256:l2", Size: 2000},
	}

	m2 := testManifest("r@2", older)
	m2.Size = 50
	m2.Config = &v1.Descriptor{Digest: "sha256:cfg2", Size: 20}
	m2.Layers = []v1.Descriptor{
		{Digest: "sha256:l1", Size: 1000}, // shared with m1
	}

	stats := calculateStats("myrepo", []*registry.Manifest{m1, m2})
	if stats.Name != "myrepo" {
		t.Errorf("Name = %q", stats.Name)
	}
	if stats.Count != 2 || stats.Tagged != 1 || stats.Untagged != 1 {
		t.Errorf("Count/Tagged/Untagged = %d/%d/%d, want 2/1/1", stats.Count, stats.Tagged, stats.Untagged)
	}
	wantTotal := uint64(100 + 10 + 1000 + 2000 + 50 + 20 + 1000)
	wantUnique := uint64(100 + 10 + 1000 + 2000 + 50 + 20) // second l1 not counted
	if stats.Total != wantTotal || stats.Unique != wantUnique {
		t.Errorf("Total/Unique = %d/%d, want %d/%d", stats.Total, stats.Unique, wantTotal, wantUnique)
	}
	if !stats.Newest.Equal(now) || !stats.Oldest.Equal(older) {
		t.Errorf("Newest/Oldest = %v/%v", stats.Newest, stats.Oldest)
	}

	if empty := calculateStats("empty", nil); empty.Shared != 0 {
		t.Errorf("empty repository Shared = %v, want 0", empty.Shared)
	}
}
