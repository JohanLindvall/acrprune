package pruner

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	godigest "github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/JohanLindvall/crprune/internal/registry"
	"github.com/JohanLindvall/crprune/internal/registry/registrytest"
	"github.com/JohanLindvall/crprune/internal/rules"
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
			{TagRegex: new("^release-"), CommonRuleSpec: rules.CommonRuleSpec{Keep: new(true)}},
			{TagRegex: new(".+"), CommonRuleSpec: rules.CommonRuleSpec{Keep: new(false)}},
		},
	})

	got := decideDigests(t, p, byDigest(image, sig), rule)
	if strings.Join(got, ",") != "img,sig" {
		t.Errorf("kept = %v, want the image and its signature", got)
	}

	// Deleting the subject deletes the signature in the same run.
	deleteAll := compileRule(t, &rules.RepoRuleSpec{
		RepoRegex: ".+",
		Tagged:    []*rules.TaggedRuleSpec{{TagRegex: new(".+"), CommonRuleSpec: rules.CommonRuleSpec{Keep: new(false)}}},
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
			{TagRegex: new("^keep$"), CommonRuleSpec: rules.CommonRuleSpec{Keep: new(true)}},
			{TagRegex: new(".+"), CommonRuleSpec: rules.CommonRuleSpec{Keep: new(false)}},
		},
		Untagged: []*rules.UntaggedRuleSpec{{CommonRuleSpec: rules.CommonRuleSpec{Keep: new(false)}}},
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
		MustDeleteEverything: new(true),
		Untagged:             []*rules.UntaggedRuleSpec{{CommonRuleSpec: rules.CommonRuleSpec{MatchOlderThan: olderThan, Keep: new(false)}}},
		Tagged:               []*rules.TaggedRuleSpec{{CommonRuleSpec: rules.CommonRuleSpec{MatchOlderThan: olderThan, Keep: new(false)}}},
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
		MustDeleteEverything: new(true),
		Untagged:             []*rules.UntaggedRuleSpec{{CommonRuleSpec: rules.CommonRuleSpec{Keep: new(false)}}},
		Tagged:               []*rules.TaggedRuleSpec{{CommonRuleSpec: rules.CommonRuleSpec{Keep: new(false)}}},
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
	rule := compileRule(t, &rules.RepoRuleSpec{RepoRegex: ".+", DeleteOrphanedManifests: new(true)})
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
		DeleteOrphanedManifests: new(true),
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

// TestPruneTagSchemeSignatures: cosign's default scheme stores the
// signatures, attestations and SBOMs of an image as ordinary manifests tagged
// sha256-<digest hex>.sig, .att and .sbom, with no OCI subject. They follow
// their image like any referrer: kept with a running image — the catch-all of
// generate's rules used to delete them, and admission control then rejected
// the image — and deleted with an image the rules delete.
func TestPruneTagSchemeSignatures(t *testing.T) {
	fake := registrytest.New()
	old := time.Now().Add(-30 * 24 * time.Hour)
	running := fake.Add("app", registrytest.Image("running"), registry.Attributes{Tags: []string{"v1"}, LastUpdated: old})
	stale := fake.Add("app", registrytest.Image("stale"), registry.Attributes{Tags: []string{"v0"}, LastUpdated: old})
	var kept []string
	for _, suffix := range []string{".sig", ".att", ".sbom"} {
		kept = append(kept, fake.Add("app", registrytest.Image("running"+suffix), registry.Attributes{Tags: []string{schemeTag(running, suffix)}, LastUpdated: old}))
	}
	staleSignature := fake.Add("app", registrytest.Image("stale.sig"), registry.Attributes{Tags: []string{schemeTag(stale, ".sig")}, LastUpdated: old})

	if err := fakePruner(t, fake).Prune(context.Background(), generated(t, "myreg.azurecr.io/app:v1")); err != nil {
		t.Fatal(err)
	}
	if got, want := fake.Deleted(), refs("app", stale, staleSignature); !slices.Equal(got, want) {
		t.Errorf("deleted = %v, want the stale image with its signature: %v", got, want)
	}
	if got, want := fake.Digests("app"), sorted(append(kept, running)...); !slices.Equal(got, want) {
		t.Errorf("remaining = %v, want the running image with its signature, attestation and SBOM: %v", got, want)
	}
}

// TestPruneReferrersTagSchema: on registries without the OCI referrers API
// (GHCR), OCI 1.1 tools list an image's referrers in an index tagged
// sha256-<digest hex>. That index follows the image: kept with a running
// image, which generate's catch-all used to break signature discovery for, and
// deleted with a deleted one, taking along the referrers it lists instead of
// keeping them alive as its dependencies.
func TestPruneReferrersTagSchema(t *testing.T) {
	old := time.Now().Add(-30 * 24 * time.Hour)
	build := func(tag string) (*registrytest.Backend, map[string]string) {
		fake := registrytest.New()
		d := map[string]string{}
		d["image"] = fake.Add("app", registrytest.Image("image"), registry.Attributes{Tags: []string{tag}, LastUpdated: old})
		signature := registrytest.Image("signature")
		signature.Subject = &v1.Descriptor{MediaType: v1.MediaTypeImageManifest, Digest: godigest.Digest(d["image"])}
		d["signature"] = fake.Add("app", signature, registry.Attributes{LastUpdated: old})
		d["referrers"] = fake.Add("app", registrytest.Index(v1.Descriptor{MediaType: v1.MediaTypeImageManifest, Digest: godigest.Digest(d["signature"])}),
			registry.Attributes{Tags: []string{schemeTag(d["image"], "")}, LastUpdated: old})
		d["release"] = fake.Add("app", registrytest.Image("release"), registry.Attributes{Tags: []string{"1.0"}, LastUpdated: old})
		return fake, d
	}

	fake, d := build("v1")
	if err := fakePruner(t, fake).Prune(context.Background(), generated(t, "myreg.azurecr.io/app:v1")); err != nil {
		t.Fatal(err)
	}
	if got, want := fake.Deleted(), refs("app", d["release"]); !slices.Equal(got, want) {
		t.Errorf("running image: deleted = %v, want only the release that is not running: %v", got, want)
	}

	fake, d = build("feature-x")
	if err := fakePruner(t, fake).Prune(context.Background(), ruleSet(t, featureCleanup)); err != nil {
		t.Fatal(err)
	}
	if got, want := fake.Deleted(), refs("app", d["image"], d["signature"], d["referrers"]); !slices.Equal(got, want) {
		t.Errorf("deleted image: deleted = %v, want the image with its referrers and their index: %v", got, want)
	}
}

// TestPruneNewestWithTagSchemeSignatures runs the README's "keep the three
// newest releases" rules over signed releases, and the same without a tag
// pattern: signatures, although newer than their releases, take no ranking
// slot, and follow their release rather than the catch-all.
func TestPruneNewestWithTagSchemeSignatures(t *testing.T) {
	for _, doc := range []string{
		`[{"repo": "^app$", "tagged": [{"tag": "^release-", "newest": 3, "keep": true}, {"tag": ".+", "keep": false}]}]`,
		`[{"repo": "^app$", "tagged": [{"newest": 3, "keep": true}, {"keep": false}]}]`,
	} {
		fake := registrytest.New()
		now := time.Now()
		var kept, deleted []string
		for i := range 4 {
			pushed := now.Add(-time.Duration(40-i) * 24 * time.Hour)
			release := fake.Add("app", registrytest.Image(fmt.Sprint("release", i)), registry.Attributes{Tags: []string{fmt.Sprint("release-", i)}, LastUpdated: pushed})
			signature := fake.Add("app", registrytest.Image(fmt.Sprint("signature", i)), registry.Attributes{Tags: []string{schemeTag(release, ".sig")}, LastUpdated: pushed.Add(time.Hour)})
			if i == 0 {
				deleted = append(deleted, release, signature)
			} else {
				kept = append(kept, release, signature)
			}
		}
		if err := fakePruner(t, fake).Prune(context.Background(), ruleSet(t, doc)); err != nil {
			t.Fatal(err)
		}
		if got, want := fake.Deleted(), refs("app", deleted...); !slices.Equal(got, want) {
			t.Errorf("%s: deleted = %v, want the oldest release with its signature: %v", doc, got, want)
		}
		if got, want := fake.Digests("app"), sorted(kept...); !slices.Equal(got, want) {
			t.Errorf("%s: remaining = %v, want the 3 newest releases with their signatures: %v", doc, got, want)
		}
	}
}

// TestPruneSignatureOfAbsentImage: signatures kept apart from their images, in
// a COSIGN_REPOSITORY, name an image their repository does not hold. There
// they are ordinary tagged manifests the rules judge, not referrers whose
// subject is gone, which are deleted.
func TestPruneSignatureOfAbsentImage(t *testing.T) {
	fake := registrytest.New()
	old := time.Now().Add(-30 * 24 * time.Hour)
	elsewhere := godigest.FromString("an image in another repository").String()
	signature := fake.Add("signatures", registrytest.Image("signature"), registry.Attributes{Tags: []string{schemeTag(elsewhere, ".sig")}, LastUpdated: old})
	leftover := fake.Add("signatures", registrytest.Image("leftover"), registry.Attributes{LastUpdated: old})

	deleteUntagged := ruleSet(t, `[{"repo": "^signatures$", "untagged": [{"keep": false}]}]`)
	if err := fakePruner(t, fake).Prune(context.Background(), deleteUntagged); err != nil {
		t.Fatal(err)
	}
	if got, want := fake.Deleted(), refs("signatures", leftover); !slices.Equal(got, want) {
		t.Errorf("deleted = %v, want only the untagged leftover: %v", got, want)
	}
	if got := fake.Digests("signatures"); !slices.Equal(got, []string{signature}) {
		t.Errorf("remaining = %v, want the signature, which no rule deletes", got)
	}
}

// TestPruneDanglingTagSchemeSignature pins what becomes of a cosign signature
// whose deletion fails after its image's: images go before their signatures,
// so that an image that cannot be deleted keeps them, and the signature left
// behind names an image the repository no longer holds. It is then an
// ordinary tagged manifest, which the next run keeps unless a rule matching
// its tag deletes it, as the README's rule for such signatures does.
func TestPruneDanglingTagSchemeSignature(t *testing.T) {
	fake := registrytest.New()
	old := time.Now().Add(-30 * 24 * time.Hour)
	image := fake.Add("app", registrytest.Image("feature"), registry.Attributes{Tags: []string{"feature-x"}, LastUpdated: old})
	signature := fake.Add("app", registrytest.Image("signature"), registry.Attributes{Tags: []string{schemeTag(image, ".sig")}, LastUpdated: old})
	release := fake.Add("app", registrytest.Image("release"), registry.Attributes{Tags: []string{"1.0"}, LastUpdated: old})
	boom := errors.New("boom")
	fake.Fail = func(op, _ string) error {
		if op == "DeleteManifest" && fake.Calls(op) == 2 {
			return boom // the signature's deletion, after the image's
		}
		return nil
	}
	if err := fakePruner(t, fake).Prune(context.Background(), ruleSet(t, featureCleanup)); !errors.Is(err, boom) {
		t.Fatalf("error = %v, want the signature's failed deletion", err)
	}
	if got, want := fake.Deleted(), refs("app", image); !slices.Equal(got, want) {
		t.Fatalf("deleted = %v, want the image first: %v", got, want)
	}

	fake.Fail = nil
	if err := fakePruner(t, fake).Prune(context.Background(), ruleSet(t, featureCleanup)); err != nil {
		t.Fatal(err)
	}
	if got, want := fake.Digests("app"), sorted(release, signature); !slices.Equal(got, want) {
		t.Errorf("remaining = %v, want the dangling signature kept, as no rule matches its tag: %v", got, want)
	}

	readme := `[{"repo": ".+", "tagged": [{"tag": "^sha(256|512)-[0-9a-f]+(\\.(sig|att|sbom))?$", "match_older": "7d", "keep": false}]}]`
	if err := fakePruner(t, fake).Prune(context.Background(), ruleSet(t, readme)); err != nil {
		t.Fatal(err)
	}
	if got := fake.Digests("app"); !slices.Equal(got, []string{release}) {
		t.Errorf("remaining = %v, want the dangling signature deleted by the rule matching its tag", got)
	}
}
