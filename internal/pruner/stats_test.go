package pruner

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	v1 "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/JohanLindvall/crprune/internal/registry"
	"github.com/JohanLindvall/crprune/internal/registry/registrytest"
	"github.com/JohanLindvall/crprune/internal/rules"
)

func TestRunningMatchUsesOnlyFirstRepositoryRule(t *testing.T) {
	rules := ruleSet(t, `[{"repo":"^app$","tagged":[{"tag":"^v2$","keep":true}]},{"repo":".+","tagged":[{"keep":true}],"untagged":[{"keep":true}]}]`)
	for _, m := range []*registry.Manifest{testManifest("tagged", time.Now(), "v1"), testManifest("untagged", time.Now())} {
		if runningMatch(m, "app", rules) {
			t.Error("a later repository rule marked an image as running")
		}
	}
}

func BenchmarkCountRunning(b *testing.B) {
	const repositories = 1000
	ruleset := make([]*rules.RepoRule, repositories)
	for i := range ruleset {
		ruleset[i] = &rules.RepoRule{Repo: regexp.MustCompile(fmt.Sprintf("^repo%d$", i)), Tagged: []rules.TaggedRule{{CommonRule: rules.CommonRule{Keep: true}}}}
	}
	manifests := make(map[string]*registry.Manifest, 1000)
	for i := range 1000 {
		m := testManifest(fmt.Sprint(i), time.Now(), "running")
		manifests[m.Digest] = m
	}
	b.ResetTimer()
	for b.Loop() {
		if countRunning(manifests, "repo999", ruleset) != len(manifests) {
			b.Fatal("incorrect running count")
		}
	}
}

func TestStatsRetainsMetadataOfUnavailableManifests(t *testing.T) {
	fake := registrytest.New()
	now := time.Now().UTC()
	old := now.Add(-48 * time.Hour)
	fake.Add("app", registrytest.Image("image"), registry.Attributes{LastUpdated: now.Add(-time.Hour)})
	fake.AddMissing("app", registry.Attributes{Digest: "sha256:" + strings.Repeat("a", 64), LastUpdated: now, Tags: []string{"running"}})
	fake.AddMissing("app", registry.Attributes{Digest: "sha256:" + strings.Repeat("b", 64), LastUpdated: old})
	stats, err := CollectRegistryStats(t.Context(), fakePruner(t, fake).Registry, generated(t, "myreg.azurecr.io/app:running"), nil)
	if err != nil {
		t.Fatal(err)
	}
	s := stats[0]
	if s.Count != 3 || s.Tagged != 1 || s.Untagged != 2 || s.Running != 1 || !s.Newest.Equal(now) || !s.Oldest.Equal(old) {
		t.Errorf("missing manifests lost their known metadata: %+v", s)
	}
}

func TestCalculateStats(t *testing.T) {
	now := time.Now()
	older := now.Add(-time.Hour)

	m1 := testManifest("sha256:m1", now, "latest")
	m1.Size = 100
	m1.Config = &v1.Descriptor{Digest: "sha256:cfg", Size: 10}
	m1.Layers = []v1.Descriptor{
		{Digest: "sha256:l1", Size: 1000},
		{Digest: "sha256:l2", Size: 2000},
	}

	m2 := testManifest("sha256:m2", older)
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

// TestCalculateStatsCountsConfigOnce pins the contract between the registry
// and the accounting here: Manifest.Size covers the manifest document alone,
// and the config blob is added exactly once on top of it. Folding the config
// into Size as well inflated every repository's reported size.
func TestCalculateStatsCountsConfigOnce(t *testing.T) {
	m := testManifest("sha256:only", time.Now())
	m.Size = 100 // the manifest document
	m.Config = &v1.Descriptor{Digest: "sha256:cfg", Size: 10}

	stats := calculateStats("r", []*registry.Manifest{m})
	if stats.Total != 110 || stats.Unique != 110 {
		t.Errorf("Total/Unique = %d/%d, want 110/110 (document + config, counted once)", stats.Total, stats.Unique)
	}
	if stats.Shared != 0 {
		t.Errorf("Shared = %v, want 0: nothing is shared here", stats.Shared)
	}
}

// TestCalculateStatsSharesAcrossRepositories checks that a blob already seen
// in an earlier repository counts towards Total but not Unique — including the
// manifest document itself, which the same image tagged in two repositories
// shares.
func TestCalculateStatsSharesAcrossRepositories(t *testing.T) {
	now := time.Now()
	seen := map[string]struct{}{}

	build := func(repository string) *registry.Manifest {
		m := testManifest("sha256:shared", now, "latest")
		m.Repository = repository
		m.Size = 100
		m.Layers = []v1.Descriptor{{Digest: "sha256:l1", Size: 900}}
		return m
	}

	first := calculateStatsSeen("a", []*registry.Manifest{build("a")}, seen)
	second := calculateStatsSeen("b", []*registry.Manifest{build("b")}, seen)

	if first.Unique != 1000 || first.Total != 1000 {
		t.Errorf("first repository = %d unique / %d total, want 1000/1000", first.Unique, first.Total)
	}
	if second.Unique != 0 || second.Total != 1000 {
		t.Errorf("second repository = %d unique / %d total, want 0/1000", second.Unique, second.Total)
	}
	if second.Shared != 1 {
		t.Errorf("second repository Shared = %v, want 1", second.Shared)
	}
}

// TestCalculateStatsIgnoresMissingTimestamps keeps a manifest the registry
// reported no timestamp for from dragging Oldest back to the year 1.
func TestCalculateStatsIgnoresMissingTimestamps(t *testing.T) {
	now := time.Now()
	dated := testManifest("sha256:dated", now)
	undated := testManifest("sha256:undated", time.Time{})

	stats := calculateStats("r", []*registry.Manifest{dated, undated})
	if !stats.Oldest.Equal(now) || !stats.Newest.Equal(now) {
		t.Errorf("Oldest/Newest = %v/%v, want both %v", stats.Oldest, stats.Newest, now)
	}
}

func TestCountRunning(t *testing.T) {
	now := time.Now()
	running := testManifest("1", now, "v1")
	stopped := testManifest("2", now, "v9")
	untagged := testManifest("3", now)
	manifests := byDigest(running, stopped, untagged)

	specs, _, err := rules.KeepRulesFromImageList(strings.NewReader("myreg.azurecr.io/app:v1\n"), "myreg.azurecr.io")
	if err != nil {
		t.Fatal(err)
	}
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

// TestCountRunningDigestPinned covers digest-pinned pod images: the pinned
// manifest counts as running whether tagged or untagged, and — because digest
// keep rules constrain no tag — other manifests must not ride along.
func TestCountRunningDigestPinned(t *testing.T) {
	now := time.Now()
	pinnedTagged := testManifest("sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", now, "v1")
	pinnedUntagged := testManifest("sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", now)
	otherTagged := testManifest("sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc", now, "v9")
	otherUntagged := testManifest("sha256:ddd", now)
	manifests := byDigest(pinnedTagged, pinnedUntagged, otherTagged, otherUntagged)

	input := "myreg.azurecr.io/app@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\nmyreg.azurecr.io/app@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb\n"
	specs, _, err := rules.KeepRulesFromImageList(strings.NewReader(input), "myreg.azurecr.io")
	if err != nil {
		t.Fatal(err)
	}
	ruleSet, err := rules.Compile(specs)
	if err != nil {
		t.Fatal(err)
	}

	if got := countRunning(manifests, "app", ruleSet); got != 2 {
		t.Errorf("countRunning = %d, want 2 (both pinned manifests, nothing else)", got)
	}
}

// TestRunningMatchDecidesTagByTag: as in pruning, a manifest counts as running
// when the first rule matching any one of its tags keeps it, even if an
// earlier rule matches another of its tags.
func TestRunningMatchDecidesTagByTag(t *testing.T) {
	ruleSet := ruleSet(t, `[{"repo": "^app$", "tagged": [{"tag": "^stale$", "keep": false}, {"tag": "^running$", "keep": true}]}]`)
	now := time.Now()
	if !runningMatch(testManifest("both", now, "stale", "running"), "app", ruleSet) {
		t.Error("a manifest with a running tag should count as running")
	}
	if runningMatch(testManifest("stale", now, "stale"), "app", ruleSet) {
		t.Error("a manifest with only a stale tag should not count as running")
	}
}
