package pruner

import (
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/JohanLindvall/acrprune/internal/rules"
)

func TestMarkOrphans(t *testing.T) {
	now := time.Now()
	index := testManifest("idx", now, "latest")
	index.Manifests = []v1.Descriptor{{Digest: "child"}, {Digest: "missing"}}
	child := testManifest("child", now)
	grandIndex := testManifest("top", now)
	grandIndex.Manifests = []v1.Descriptor{{Digest: "idx"}}
	standalone := testManifest("alone", now)

	markOrphans(byDigest(index, child, grandIndex, standalone))

	if !index.Orphaned {
		t.Error("index with a missing child should be orphaned")
	}
	if !grandIndex.Orphaned {
		t.Error("orphaned state should propagate up to the referencing index")
	}
	if !child.Orphaned {
		t.Error("the sole child of an orphaned index should be orphaned")
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

// TestMarkOrphansKeepsSharedChildren covers a child reachable through both a
// broken and an intact index. Marking it orphaned because one owner is broken
// would spread back up to the healthy index and delete a complete image.
func TestMarkOrphansKeepsSharedChildren(t *testing.T) {
	now := time.Now()
	broken := testManifest("broken", now, "broken")
	broken.Manifests = []v1.Descriptor{{Digest: "shared"}, {Digest: "missing"}}
	healthy := testManifest("healthy", now, "healthy")
	healthy.Manifests = []v1.Descriptor{{Digest: "shared"}}
	shared := testManifest("shared", now)

	markOrphans(byDigest(broken, healthy, shared))

	if !broken.Orphaned {
		t.Error("the index with a missing child should be orphaned")
	}
	if shared.Orphaned {
		t.Error("a child still reachable through a healthy index must not be orphaned")
	}
	if healthy.Orphaned {
		t.Error("a complete index must not be orphaned by sharing a child with a broken one")
	}
}

func TestMarkOrphansKeepsIndependentlyTaggedChildren(t *testing.T) {
	index := testManifest("index", time.Now(), "broken")
	index.Manifests = []v1.Descriptor{{Digest: "child"}, {Digest: "missing"}}
	child := testManifest("child", time.Now(), "standalone")
	markOrphans(byDigest(index, child))
	if !index.Orphaned || child.Orphaned {
		t.Fatalf("orphaned index=%v, child=%v: a tag keeps the child independently reachable", index.Orphaned, child.Orphaned)
	}
}

// TestMarkOrphansFlagsDanglingSubject: a referrer whose subject is missing or
// orphaned is as broken as an index with a missing child.
func TestMarkOrphansFlagsDanglingSubject(t *testing.T) {
	now := time.Now()
	dangling := testManifest("dangling", now)
	dangling.Subject = &v1.Descriptor{Digest: "gone"}

	image := testManifest("img", now, "v1")
	attached := testManifest("attached", now)
	attached.Subject = &v1.Descriptor{Digest: "img"}

	manifests := byDigest(dangling, image, attached)
	markOrphans(manifests)

	if !dangling.Orphaned {
		t.Error("a referrer with a missing subject should be orphaned")
	}
	if attached.Orphaned {
		t.Error("a referrer with a present subject should not be orphaned")
	}
	if image.Orphaned {
		t.Error("the subject itself should not be orphaned")
	}
}

// TestMarkOrphansCycle checks the fixpoint terminates on a reference cycle.
func TestMarkOrphansCycle(t *testing.T) {
	now := time.Now()
	a := testManifest("a", now)
	b := testManifest("b", now)
	a.Manifests = []v1.Descriptor{{Digest: "b"}}
	b.Manifests = []v1.Descriptor{{Digest: "a"}}

	done := make(chan struct{})
	go func() {
		markOrphans(byDigest(a, b))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("markOrphans did not terminate on a reference cycle")
	}

	if a.Orphaned || b.Orphaned {
		t.Error("a closed cycle has no missing manifest and should not be orphaned")
	}
}

func TestReportOrphans(t *testing.T) {
	p := testPruner()
	orphan := testManifest("o", time.Now())
	orphan.Orphaned = true
	manifests := byDigest(orphan)

	strict := compileRule(t, &rules.RepoRuleSpec{RepoRegex: ".+", IgnoreMissingManifests: to.Ptr(false)})
	if err := p.reportOrphans(manifests, strict); err == nil {
		t.Error("orphans should be reported when neither ignored nor deleted")
	}

	tolerant := []*rules.RepoRuleSpec{
		{RepoRegex: ".+", IgnoreMissingManifests: to.Ptr(true)},
		{RepoRegex: ".+", IgnoreMissingManifests: to.Ptr(false), DeleteOrphanedManifests: to.Ptr(true)},
	}
	for _, spec := range tolerant {
		if err := p.reportOrphans(manifests, compileRule(t, spec)); err != nil {
			t.Errorf("%+v: unexpected error: %v", spec, err)
		}
	}

	// A repository without orphans never fails.
	if err := p.reportOrphans(byDigest(testManifest("ok", time.Now())), strict); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}
