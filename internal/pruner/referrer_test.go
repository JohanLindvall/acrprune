package pruner

import (
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/JohanLindvall/acrprune/internal/registry"
	"github.com/JohanLindvall/acrprune/internal/rules"
)

func TestReferrerFollowsSubject(t *testing.T) {
	p := testPruner()
	old := time.Now().Add(-48 * time.Hour)

	image := testManifest("img", old, "release-1.0")
	sig := testManifest("sig", old, "sha256-img.sig")
	sig.Subject = &v1.Descriptor{Digest: "img"}

	// The tag rules would delete the signature's own tag; the subject decides
	// regardless.
	rule := compileRule(t, &rules.RepoRuleSpec{
		RepoRegex: ".+",
		Tagged: []*rules.TaggedRuleSpec{
			{TagRegex: to.Ptr("^release-"), CommonRuleSpec: rules.CommonRuleSpec{Keep: to.Ptr(true)}},
			{TagRegex: to.Ptr(".+"), CommonRuleSpec: rules.CommonRuleSpec{Keep: to.Ptr(false)}},
		},
	})

	got := decideDigests(t, p, byDigest(image, sig), rule)
	if strings.Join(got, ",") != "img,sig" {
		t.Errorf("kept = %v, want the image and its signature", got)
	}

	// Deleting the subject deletes the signature in the same run.
	deleteAll := compileRule(t, &rules.RepoRuleSpec{
		RepoRegex: ".+",
		Tagged:    []*rules.TaggedRuleSpec{{TagRegex: to.Ptr(".+"), CommonRuleSpec: rules.CommonRuleSpec{Keep: to.Ptr(false)}}},
	})
	if got := decideDigests(t, p, byDigest(image, sig), deleteAll); len(got) != 0 {
		t.Errorf("kept = %v, want the signature deleted along with its subject", got)
	}
}

// TestDanglingReferrerDeleted covers pre-existing garbage: a signature whose
// subject is already gone used to be kept forever ("always keep manifests
// with a subject"); now it follows its absent subject.
func TestDanglingReferrerDeleted(t *testing.T) {
	p := testPruner()
	old := time.Now().Add(-48 * time.Hour)

	sig := testManifest("sig", old, "sha256-gone.sig")
	sig.Subject = &v1.Descriptor{Digest: "gone"}
	keepEverything := compileRule(t, &rules.RepoRuleSpec{RepoRegex: ".+"})

	if got := decideDigests(t, p, byDigest(sig), keepEverything); len(got) != 0 {
		t.Errorf("kept = %v, want the dangling signature deleted", got)
	}
}

// TestReferrerChain follows a signature on an attestation on an image through
// both fates.
func TestReferrerChain(t *testing.T) {
	p := testPruner()
	old := time.Now().Add(-48 * time.Hour)

	build := func(tag string) map[string]*registry.Manifest {
		image := testManifest("img", old, tag)
		att := testManifest("att", old)
		att.Subject = &v1.Descriptor{Digest: "img"}
		sig := testManifest("sig", old)
		sig.Subject = &v1.Descriptor{Digest: "att"}
		return byDigest(image, att, sig)
	}

	rule := compileRule(t, &rules.RepoRuleSpec{
		RepoRegex: ".+",
		Tagged: []*rules.TaggedRuleSpec{
			{TagRegex: to.Ptr("^keep$"), CommonRuleSpec: rules.CommonRuleSpec{Keep: to.Ptr(true)}},
			{TagRegex: to.Ptr(".+"), CommonRuleSpec: rules.CommonRuleSpec{Keep: to.Ptr(false)}},
		},
		Untagged: []*rules.UntaggedRuleSpec{{CommonRuleSpec: rules.CommonRuleSpec{Keep: to.Ptr(false)}}},
	})

	if got := decideDigests(t, p, build("keep"), rule); strings.Join(got, ",") != "att,img,sig" {
		t.Errorf("kept = %v, want the whole chain kept with the image", got)
	}
	if got := decideDigests(t, p, build("drop"), rule); len(got) != 0 {
		t.Errorf("kept = %v, want the whole chain deleted with the image", got)
	}
}

// TestMustDeleteEverythingIgnoresReferrers pins the fix for signed
// repositories being immune to bulk deletion: the signature's derived keep
// must not count as "something was kept" for must_delete_everything.
func TestMustDeleteEverythingIgnoresReferrers(t *testing.T) {
	p := testPruner()
	old := time.Now().Add(-1000 * 24 * time.Hour)
	olderThan := &rules.Duration{Duration: 730 * 24 * time.Hour}

	image := testManifest("img", old, "v1")
	sig := testManifest("sig", old, "sha256-img.sig")
	sig.Subject = &v1.Descriptor{Digest: "img"}

	// rules/delete_old_repos.json shape.
	rule := compileRule(t, &rules.RepoRuleSpec{
		RepoRegex:            ".+",
		MustDeleteEverything: to.Ptr(true),
		Untagged:             []*rules.UntaggedRuleSpec{{CommonRuleSpec: rules.CommonRuleSpec{MatchOlderThan: olderThan, Keep: to.Ptr(false)}}},
		Tagged:               []*rules.TaggedRuleSpec{{CommonRuleSpec: rules.CommonRuleSpec{MatchOlderThan: olderThan, Keep: to.Ptr(false)}}},
	})

	if got := decideDigests(t, p, byDigest(image, sig), rule); len(got) != 0 {
		t.Errorf("kept = %v, want the signed repository fully deleted", got)
	}

	// And when the image is young enough to stay, the signature stays too —
	// via must_delete_everything keeping the whole repository.
	fresh := testManifest("img", time.Now(), "v1")
	if got := decideDigests(t, p, byDigest(fresh, sig), rule); len(got) != 2 {
		t.Errorf("kept = %v, want everything kept when the image is kept", got)
	}
}

// TestMustDeleteEverythingWithGraceKeptReferrer pins the evaluation order:
// referrer fates are settled after the must_delete_everything check, so a
// doomed signature surviving only on the grace period cannot keep the whole
// repository alive — the images still go, and the signature follows once the
// grace period lapses.
func TestMustDeleteEverythingWithGraceKeptReferrer(t *testing.T) {
	p := testPruner()
	p.KeepYounger = 24 * time.Hour
	old := time.Now().Add(-1000 * 24 * time.Hour)

	image := testManifest("img", old, "v1")
	sig := testManifest("sig", time.Now().Add(-time.Hour)) // freshly pushed signature
	sig.Subject = &v1.Descriptor{Digest: "img"}

	rule := compileRule(t, &rules.RepoRuleSpec{
		RepoRegex:            ".+",
		MustDeleteEverything: to.Ptr(true),
		Untagged:             []*rules.UntaggedRuleSpec{{CommonRuleSpec: rules.CommonRuleSpec{Keep: to.Ptr(false)}}},
		Tagged:               []*rules.TaggedRuleSpec{{CommonRuleSpec: rules.CommonRuleSpec{Keep: to.Ptr(false)}}},
	})

	if got := decideDigests(t, p, byDigest(image, sig), rule); strings.Join(got, ",") != "sig" {
		t.Errorf("kept = %v, want only the grace-kept signature — not the whole repository", got)
	}
}

// TestReferrerGracePeriod: --keep-younger holds for referrers as well; a
// young dangling signature outlives its subject by at most the grace period.
func TestReferrerGracePeriod(t *testing.T) {
	p := testPruner()
	p.KeepYounger = 24 * time.Hour

	young := testManifest("young", time.Now().Add(-time.Hour))
	young.Subject = &v1.Descriptor{Digest: "gone"}
	old := testManifest("old", time.Now().Add(-48*time.Hour))
	old.Subject = &v1.Descriptor{Digest: "gone"}

	rule := compileRule(t, &rules.RepoRuleSpec{RepoRegex: ".+"})
	if got := decideDigests(t, p, byDigest(young, old), rule); strings.Join(got, ",") != "young" {
		t.Errorf("kept = %v, want only the young dangling referrer", got)
	}
}

func TestReferrerWithoutTimestampIsProtected(t *testing.T) {
	p := testPruner()
	sig := testManifest("sig", time.Time{})
	sig.Subject = &v1.Descriptor{Digest: "gone"}
	rule := compileRule(t, &rules.RepoRuleSpec{RepoRegex: ".+", DeleteOrphanedManifests: to.Ptr(true)})
	manifests := byDigest(sig)
	markOrphans(manifests)
	if got := decideDigests(t, p, manifests, rule); strings.Join(got, ",") != "sig" {
		t.Errorf("kept = %v, want unknown-age referrer protected", got)
	}
}

// TestOrphanedReferrerDeletedDespiteKeptSubject: delete_orphaned_manifests
// still wins over the subject linkage for a structurally broken referrer.
func TestOrphanedReferrerDeletedDespiteKeptSubject(t *testing.T) {
	p := testPruner()
	old := time.Now().Add(-48 * time.Hour)

	image := testManifest("img", old, "v1")
	sig := testManifest("sig", old)
	sig.Subject = &v1.Descriptor{Digest: "img"}
	sig.Manifests = []v1.Descriptor{{Digest: "missing-child"}} // broken index referrer

	rule := compileRule(t, &rules.RepoRuleSpec{
		RepoRegex:               ".+",
		DeleteOrphanedManifests: to.Ptr(true),
	})

	manifests := byDigest(image, sig)
	markOrphans(manifests)
	if !sig.Orphaned {
		t.Fatal("referrer with a missing child should be orphaned")
	}
	if got := decideDigests(t, p, manifests, rule); strings.Join(got, ",") != "img" {
		t.Errorf("kept = %v, want only the image", got)
	}
}
