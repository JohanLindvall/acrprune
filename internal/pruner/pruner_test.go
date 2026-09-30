package pruner

import (
	"io"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/JohanLindvall/acrprune/internal/registry"
	"github.com/JohanLindvall/acrprune/internal/rules"
)

// testManifest builds a manifest in repository "r" with the given digest,
// last-updated time (the zero time for none) and tags.
func testManifest(digest string, updated time.Time, tags ...string) *registry.Manifest {
	return &registry.Manifest{
		Repository: "r",
		Attributes: registry.Attributes{Digest: digest, LastUpdated: updated, Tags: tags},
	}
}

// byDigest keys manifests the way a repository fetch does.
func byDigest(manifests ...*registry.Manifest) map[string]*registry.Manifest {
	result := map[string]*registry.Manifest{}
	for _, m := range manifests {
		result[m.Digest] = m
	}
	return result
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

// decide runs the pruner's decision over a manifest map, returning the kept
// digests sorted for comparison.
func decideDigests(t *testing.T, p *Pruner, manifests map[string]*registry.Manifest, rule *rules.RepoRule) []string {
	t.Helper()
	all := slices.SortedFunc(maps.Values(manifests), byNewest)
	kept, err := p.decide(all, manifests, rule)
	if err != nil {
		t.Fatal(err)
	}
	return slices.Sorted(maps.Keys(kept))
}

func splitList(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
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
	release := testManifest("1", old, "release-1.0")
	feature := testManifest("2", old, "feature-x")
	untagged := testManifest("3", old)
	unmatchedTag := testManifest("4", old, "v1")
	all := []*registry.Manifest{release, feature, untagged, unmatchedTag}

	shouldKeep := func(m *registry.Manifest, rule *rules.RepoRule, manifests []*registry.Manifest) bool {
		return p.shouldKeep(m, newEvaluator(rule, manifests, now), rule)
	}

	if !shouldKeep(release, rule, all) {
		t.Error("release tag should be kept")
	}
	if shouldKeep(feature, rule, all) {
		t.Error("feature tag should be deleted")
	}
	if shouldKeep(untagged, rule, all) {
		t.Error("untagged should be deleted")
	}
	if shouldKeep(unmatchedTag, rule, all) {
		t.Error("catch-all tag rule should delete v1")
	}

	// No matching rule at all keeps the manifest.
	empty := compileRule(t, &rules.RepoRuleSpec{RepoRegex: ".+"})
	if !shouldKeep(feature, empty, all) {
		t.Error("manifest with no matching rule should be kept")
	}

	// Manifests with a subject are always kept.
	signature := testManifest("5", old, "feature-x")
	signature.Subject = &v1.Descriptor{Digest: "sha256:parent"}
	if !shouldKeep(signature, rule, append(slices.Clone(all), signature)) {
		t.Error("manifest with subject should always be kept")
	}

	// The grace period overrides deletion.
	p.KeepYounger = 24 * time.Hour
	young := testManifest("6", now.Add(-time.Hour), "feature-x")
	if !shouldKeep(young, rule, append(slices.Clone(all), young)) {
		t.Error("young manifest should be kept by grace period")
	}
	if shouldKeep(feature, rule, all) {
		t.Error("old manifest should still be deleted with grace period set")
	}
	p.KeepYounger = 0

	// Orphan deletion overrides a keep decision.
	orphanRule := compileRule(t, &rules.RepoRuleSpec{RepoRegex: ".+", DeleteOrphanedManifests: to.Ptr(true)})
	orphan := testManifest("7", old, "release-1.0")
	orphan.Orphaned = true
	if shouldKeep(orphan, orphanRule, []*registry.Manifest{orphan}) {
		t.Error("orphaned manifest should be deleted when delete_orphaned_manifests is set")
	}
}

// TestShouldKeepWithoutTimestamp guards against deleting a manifest on the
// strength of an age rule the registry gave us no age for: the zero time would
// otherwise read as infinitely old.
func TestShouldKeepWithoutTimestamp(t *testing.T) {
	now := time.Now()
	p := testPruner()
	rule := compileRule(t, &rules.RepoRuleSpec{
		RepoRegex: ".+",
		Untagged: []*rules.UntaggedRuleSpec{
			{CommonRuleSpec: rules.CommonRuleSpec{MatchOlderThan: &rules.Duration{Duration: time.Hour}, Keep: to.Ptr(false)}},
		},
	})

	undated := testManifest("undated", time.Time{})
	dated := testManifest("dated", now.Add(-48*time.Hour))
	all := []*registry.Manifest{undated, dated}
	e := newEvaluator(rule, all, now)

	if !p.shouldKeep(undated, e, rule) {
		t.Error("a manifest with no last-updated timestamp must not be deleted by an age rule")
	}
	if p.shouldKeep(dated, e, rule) {
		t.Error("a manifest older than match_older should be deleted")
	}
}

func TestKeepWithDependencies(t *testing.T) {
	p := testPruner()
	index := testManifest("idx", time.Now(), "latest")
	index.Manifests = []v1.Descriptor{{Digest: "a"}, {Digest: "b"}}
	a := testManifest("a", time.Now())
	b := testManifest("b", time.Now())
	manifests := byDigest(index, a, b)

	rule := compileRule(t, &rules.RepoRuleSpec{RepoRegex: ".+"})
	kept := map[string]struct{}{}
	if _, err := p.keepWithDependencies(index.Digest, manifests, kept, rule); err != nil {
		t.Fatal(err)
	}
	if len(kept) != 3 {
		t.Errorf("kept = %v, want all 3", kept)
	}

	// A missing dependency fails unless ignore_missing_manifests is set.
	index.Manifests = append(index.Manifests, v1.Descriptor{Digest: "gone"})
	strict := compileRule(t, &rules.RepoRuleSpec{RepoRegex: ".+", IgnoreMissingManifests: to.Ptr(false)})
	if _, err := p.keepWithDependencies(index.Digest, manifests, map[string]struct{}{}, strict); err == nil {
		t.Error("missing dependency should fail in strict mode")
	}

	kept = map[string]struct{}{}
	if _, err := p.keepWithDependencies(index.Digest, manifests, kept, rule); err != nil {
		t.Errorf("missing dependency should be tolerated by default: %v", err)
	}
	// The missing digest must not be recorded as kept: an empty repository is
	// recognized by nothing being left in the kept set.
	if _, ok := kept["gone"]; ok {
		t.Errorf("a missing dependency should not count as kept: %v", slices.Sorted(maps.Keys(kept)))
	}
	if len(kept) != 3 {
		t.Errorf("kept = %v, want the 3 manifests that exist", slices.Sorted(maps.Keys(kept)))
	}
}

// TestKeepWithDependenciesCycle checks that a manifest cycle terminates.
func TestKeepWithDependenciesCycle(t *testing.T) {
	p := testPruner()
	a := testManifest("a", time.Now(), "latest")
	b := testManifest("b", time.Now())
	a.Manifests = []v1.Descriptor{{Digest: "b"}}
	b.Manifests = []v1.Descriptor{{Digest: "a"}}

	kept := map[string]struct{}{}
	rule := compileRule(t, &rules.RepoRuleSpec{RepoRegex: ".+"})
	if _, err := p.keepWithDependencies("a", byDigest(a, b), kept, rule); err != nil {
		t.Fatal(err)
	}
	if len(kept) != 2 {
		t.Errorf("kept = %v, want both", slices.Sorted(maps.Keys(kept)))
	}
}

func TestDecideKeepsDependencies(t *testing.T) {
	now := time.Now()
	p := testPruner()

	index := testManifest("idx", now, "latest")
	index.Manifests = []v1.Descriptor{{Digest: "child"}}
	child := testManifest("child", now) // untagged, and the untagged rule deletes

	rule := compileRule(t, &rules.RepoRuleSpec{
		RepoRegex: ".+",
		Untagged:  []*rules.UntaggedRuleSpec{{CommonRuleSpec: rules.CommonRuleSpec{Keep: to.Ptr(false)}}},
	})

	got := decideDigests(t, p, byDigest(index, child), rule)
	if strings.Join(got, ",") != "child,idx" {
		t.Errorf("kept = %v, want the index and the child it references", got)
	}
}

func TestDecideMustDeleteEverything(t *testing.T) {
	p := testPruner()
	now := time.Now()
	keep := testManifest("keep", now, "arm64-app")
	keep.Architecture = "arm64"
	deletable := testManifest("del", now.Add(-time.Hour), "amd64-app")
	deletable.Architecture = "amd64"

	rule := compileRule(t, &rules.RepoRuleSpec{
		RepoRegex:            ".+",
		MustDeleteEverything: to.Ptr(true),
		Tagged: []*rules.TaggedRuleSpec{
			{CommonRuleSpec: rules.CommonRuleSpec{ArchitectureRegex: to.Ptr("arm64"), Keep: to.Ptr(true)}},
			{CommonRuleSpec: rules.CommonRuleSpec{ArchitectureRegex: to.Ptr("amd64"), Keep: to.Ptr(false)}},
		},
	})

	if got := decideDigests(t, p, byDigest(keep, deletable), rule); len(got) != 2 {
		t.Errorf("must_delete_everything with one kept manifest should keep all, kept = %v", got)
	}

	// Without the arm64 manifest everything goes.
	if got := decideDigests(t, p, byDigest(deletable), rule); len(got) != 0 {
		t.Errorf("amd64-only repo should be fully deleted, kept = %v", got)
	}
}
