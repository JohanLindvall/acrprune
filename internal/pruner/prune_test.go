package pruner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
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

// fakePruner returns a pruner over the in-memory backend, deleting for real.
func fakePruner(t *testing.T, backend registry.Backend) *Pruner {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	reg, err := registry.New(backend, logger, 4, nil)
	if err != nil {
		t.Fatal(err)
	}
	return &Pruner{Registry: reg, Logger: logger}
}

// ruleSet parses and compiles a JSON rule file.
func ruleSet(t *testing.T, doc string) []*rules.RepoRule {
	t.Helper()
	specs, err := rules.ParseSpecs(strings.NewReader(doc))
	if err != nil {
		t.Fatal(err)
	}
	ruleSet, err := rules.Compile(specs)
	if err != nil {
		t.Fatal(err)
	}
	return ruleSet
}

// captureLog makes the pruner log to the returned builder.
func captureLog(p *Pruner) *strings.Builder {
	var log strings.Builder
	p.Logger = slog.New(slog.NewTextHandler(&log, nil))
	return &log
}

// generated returns the rules generate writes for the images, which are
// references below myreg.azurecr.io.
func generated(t *testing.T, images ...string) []*rules.RepoRule {
	t.Helper()
	specs, _, err := rules.KeepRulesFromImageList(strings.NewReader(strings.Join(images, "\n")), "myreg.azurecr.io")
	if err != nil {
		t.Fatal(err)
	}
	ruleSet, err := rules.Compile(specs)
	if err != nil {
		t.Fatal(err)
	}
	return ruleSet
}

// schemeTag returns the tag naming a referrer of the manifest with the digest
// by the cosign or OCI referrers tag scheme: sha256-<hex> plus the suffix,
// such as ".sig".
func schemeTag(digest, suffix string) string {
	return strings.Replace(digest, ":", "-", 1) + suffix
}

// multiArch adds to the repository an image index with untagged amd64 and
// arm64 images, all updated at the given time. It returns the digests of the
// index and of its images.
func multiArch(fake *registrytest.Backend, repository, name string, updated time.Time, tags ...string) (index string, images []string) {
	amd := fake.Add(repository, registrytest.Image(name+"-amd64"), registry.Attributes{LastUpdated: updated})
	arm := fake.Add(repository, registrytest.Image(name+"-arm64"), registry.Attributes{LastUpdated: updated})
	index = fake.Add(repository, registrytest.Index(
		registrytest.Child(amd, "linux", "amd64"),
		registrytest.Child(arm, "linux", "arm64"),
	), registry.Attributes{Tags: tags, LastUpdated: updated})
	return index, []string{amd, arm}
}

func refs(repository string, digests ...string) []string {
	var result []string
	for _, digest := range digests {
		result = append(result, repository+"@"+digest)
	}
	slices.Sort(result)
	return result
}

const featureCleanup = `[{
	"repo": ".+",
	"untagged": [{"match_older": "24h", "keep": false}],
	"tagged": [{"tag": "^feature-", "match_older": "14d", "keep": false}]
}]`

// buildApp stores a typical repository: a multi-platform release whose
// platform images are untagged, an old feature image, a fresh feature image
// and an old untagged leftover.
func buildApp(fake *registrytest.Backend, now time.Time) (release, amd, arm, oldFeature, newFeature, leftover string) {
	old := now.Add(-30 * 24 * time.Hour)
	amd = fake.Add("app", registrytest.Image("amd"), registry.Attributes{LastUpdated: old})
	arm = fake.Add("app", registrytest.Image("arm"), registry.Attributes{LastUpdated: old})
	release = fake.Add("app", registrytest.Index(
		registrytest.Child(amd, "linux", "amd64"),
		registrytest.Child(arm, "linux", "arm64"),
	), registry.Attributes{Tags: []string{"1.0"}, LastUpdated: old})
	oldFeature = fake.Add("app", registrytest.Image("old-feature"), registry.Attributes{Tags: []string{"feature-old"}, LastUpdated: old})
	newFeature = fake.Add("app", registrytest.Image("new-feature"), registry.Attributes{Tags: []string{"feature-new"}, LastUpdated: now.Add(-time.Hour)})
	leftover = fake.Add("app", registrytest.Image("leftover"), registry.Attributes{LastUpdated: old})
	return
}

func TestPrune(t *testing.T) {
	fake := registrytest.New()
	release, amd, arm, oldFeature, newFeature, leftover := buildApp(fake, time.Now())

	if err := fakePruner(t, fake).Prune(context.Background(), ruleSet(t, featureCleanup)); err != nil {
		t.Fatal(err)
	}
	// The release's untagged platform images survive as its dependencies.
	if got, want := fake.Deleted(), refs("app", oldFeature, leftover); !slices.Equal(got, want) {
		t.Errorf("deleted = %v, want %v", got, want)
	}
	if got := fake.Digests("app"); !slices.Equal(got, sorted(release, amd, arm, newFeature)) {
		t.Errorf("remaining = %v", got)
	}
	if len(fake.DeletedRepositories()) != 0 {
		t.Errorf("repositories deleted: %v", fake.DeletedRepositories())
	}
}

func sorted(values ...string) []string {
	slices.Sort(values)
	return values
}

// TestPruneDryRun: a dry run deletes nothing, and its log lists every
// manifest it would delete, those of a repository it would delete outright
// included, which it used to name by the repository alone.
func TestPruneDryRun(t *testing.T) {
	fake := registrytest.New()
	_, _, _, oldFeature, _, leftover := buildApp(fake, time.Now())
	stale := fake.Add("stale", registrytest.Image("stale"), registry.Attributes{LastUpdated: time.Now().Add(-1000 * time.Hour)})

	p := fakePruner(t, fake)
	p.DryRun = true
	log := captureLog(p)
	if err := p.Prune(context.Background(), ruleSet(t, featureCleanup)); err != nil {
		t.Fatal(err)
	}
	if fake.Calls("DeleteManifest") != 0 || fake.Calls("DeleteRepository") != 0 || len(fake.Deleted()) != 0 {
		t.Error("a dry run must not delete anything")
	}
	for _, want := range []string{
		`msg="Dry-run: deleting manifest" manifest.ref=app@` + oldFeature,
		`msg="Dry-run: deleting manifest" manifest.ref=app@` + leftover,
		`msg="Dry-run: deleting repository" repository=stale`,
		`msg="Dry-run: deleting manifest with the repository" manifest.ref=stale@` + stale,
	} {
		if !strings.Contains(log.String(), want) {
			t.Errorf("the plan lacks %s:\n%s", want, log)
		}
	}
}

// TestPruneDeletesEmptiedRepository: a repository losing every manifest is
// deleted outright instead of manifest by manifest.
func TestPruneDeletesEmptiedRepository(t *testing.T) {
	fake := registrytest.New()
	old := time.Now().Add(-1000 * 24 * time.Hour)
	fake.Add("ancient", registrytest.Image("a"), registry.Attributes{Tags: []string{"v1"}, LastUpdated: old})
	fake.Add("ancient", registrytest.Image("b"), registry.Attributes{LastUpdated: old})
	fresh := fake.Add("current", registrytest.Image("c"), registry.Attributes{Tags: []string{"v1"}, LastUpdated: time.Now()})

	oldRepos := `[{"repo": ".+", "must_delete_everything": true,
		"untagged": [{"match_older": "730d", "keep": false}],
		"tagged": [{"match_older": "730d", "keep": false}]}]`
	if err := fakePruner(t, fake).Prune(context.Background(), ruleSet(t, oldRepos)); err != nil {
		t.Fatal(err)
	}
	if got := fake.DeletedRepositories(); !slices.Equal(got, []string{"ancient"}) {
		t.Errorf("deleted repositories = %v, want [ancient]", got)
	}
	if fake.Calls("DeleteManifest") != 0 {
		t.Error("an emptied repository should not be deleted manifest by manifest")
	}
	if got := fake.Digests("current"); !slices.Equal(got, []string{fresh}) {
		t.Errorf("current = %v", got)
	}
}

// TestPruneLiteralRepositories: rules naming repositories literally address
// them directly, without the catalog listing ABAC registries may deny.
func TestPruneLiteralRepositories(t *testing.T) {
	fake := registrytest.New()
	_, _, _, oldFeature, _, leftover := buildApp(fake, time.Now())
	other := fake.Add("other", registrytest.Image("x"), registry.Attributes{LastUpdated: time.Now().Add(-48 * time.Hour)})
	fake.Fail = func(op, _ string) error {
		if op == "ListRepositories" {
			return registrytest.Forbidden("catalog")
		}
		return nil
	}

	literal := `[
		{"repo": "^app$", "untagged": [{"keep": false}], "tagged": [{"tag": "^feature-old$", "keep": false}]},
		{"repo": "^missing$", "untagged": [{"keep": false}]}
	]`
	if err := fakePruner(t, fake).Prune(context.Background(), ruleSet(t, literal)); err != nil {
		t.Fatal(err)
	}
	if got, want := fake.Deleted(), refs("app", oldFeature, leftover); !slices.Equal(got, want) {
		t.Errorf("deleted = %v, want %v", got, want)
	}
	if got := fake.Digests("other"); !slices.Equal(got, []string{other}) {
		t.Errorf("a repository no rule names was touched: %v", got)
	}
}

// TestPruneListingDenied: a pattern that needs the catalog fails with advice
// when listing is denied.
func TestPruneListingDenied(t *testing.T) {
	fake := registrytest.New()
	fake.Fail = func(op, _ string) error {
		if op == "ListRepositories" {
			return registrytest.Forbidden("catalog")
		}
		return nil
	}
	err := fakePruner(t, fake).Prune(context.Background(), ruleSet(t, featureCleanup))
	if !registry.IsPermissionError(err) || !strings.Contains(err.Error(), "literal ^repo$") {
		t.Errorf("error = %v, want a permission error advising literal patterns", err)
	}
}

// TestPruneSkipsDeniedRepositories: a broad rule matching repositories the
// caller may not touch prunes the rest and reports the denied ones.
func TestPruneSkipsDeniedRepositories(t *testing.T) {
	fake := registrytest.New()
	_, _, _, oldFeature, _, leftover := buildApp(fake, time.Now())
	fake.AddRepository("secret")
	fake.Fail = func(op, repository string) error {
		if repository == "secret" {
			return registrytest.Forbidden(repository)
		}
		return nil
	}

	err := fakePruner(t, fake).Prune(context.Background(), ruleSet(t, featureCleanup))
	if err == nil || !strings.Contains(err.Error(), "insufficient permission to prune 1 of 2 repositories (pruned 1)") || !strings.HasSuffix(err.Error(), "secret") {
		t.Errorf("error = %v, want the denied repository reported", err)
	}
	if strings.Contains(err.Error(), "every repository was denied") {
		t.Errorf("partial denial is normal scoping: %v", err)
	}
	if got, want := fake.Deleted(), refs("app", oldFeature, leftover); !slices.Equal(got, want) {
		t.Errorf("the accessible repository should still be pruned: deleted %v, want %v", got, want)
	}

	// Denied everywhere points at the credential.
	fake.Fail = func(op, repository string) error {
		if repository != "" {
			return registrytest.Forbidden(repository)
		}
		return nil
	}
	err = fakePruner(t, fake).Prune(context.Background(), ruleSet(t, featureCleanup))
	if err == nil || !strings.Contains(err.Error(), "every repository was denied") {
		t.Errorf("error = %v, want the credential hint", err)
	}
}

func TestPruneAbortsOnOtherErrors(t *testing.T) {
	fake := registrytest.New()
	buildApp(fake, time.Now())
	boom := errors.New("boom")
	fake.Fail = func(op, _ string) error {
		if op == "DeleteManifest" {
			return boom
		}
		return nil
	}
	err := fakePruner(t, fake).Prune(context.Background(), ruleSet(t, featureCleanup))
	if !errors.Is(err, boom) || !strings.Contains(err.Error(), "failed to prune repository app") {
		t.Errorf("error = %v, want the failure wrapped with the repository", err)
	}
}

// TestPruneKeepsLastTag: where the registry refuses to delete a repository's
// last tagged manifest (GHCR), rules keeping only untagged manifests keep the
// newest tagged image as well — with its platform images and signature —
// rather than fail on it.
func TestPruneKeepsLastTag(t *testing.T) {
	now := time.Now()
	old := now.Add(-48 * time.Hour)
	build := func(protect bool) (*registrytest.Backend, map[string]string) {
		fake := registrytest.New()
		fake.ProtectLastTag = protect
		d := map[string]string{}
		d["pinned"] = fake.Add("app", registrytest.Image("pinned"), registry.Attributes{LastUpdated: old})
		d["v1"] = fake.Add("app", registrytest.Image("v1"), registry.Attributes{Tags: []string{"v1"}, LastUpdated: old.Add(-time.Hour)})
		d["child"] = fake.Add("app", registrytest.Image("child"), registry.Attributes{LastUpdated: old})
		d["v2"] = fake.Add("app", registrytest.Index(registrytest.Child(d["child"], "linux", "amd64")), registry.Attributes{Tags: []string{"v2"}, LastUpdated: old})
		signature := registrytest.Image("signature")
		signature.Subject = &v1.Descriptor{MediaType: v1.MediaTypeImageIndex, Digest: godigest.Digest(d["v2"])}
		d["sig"] = fake.Add("app", signature, registry.Attributes{Tags: []string{"sha256-v2.sig"}, LastUpdated: now.Add(-time.Minute)})
		return fake, d
	}
	// Keep the pinned image by digest; delete everything else.
	rule := func(pinned string) []*rules.RepoRule {
		return ruleSet(t, `[{"repo": "^app$",
			"untagged": [{"digest": "^`+pinned+`$", "keep": true}, {"keep": false}],
			"tagged": [{"keep": false}]}]`)
	}

	fake, d := build(true)
	if err := fakePruner(t, fake).Prune(context.Background(), rule(d["pinned"])); err != nil {
		t.Fatalf("pruning should not trip over the protected last tag: %v", err)
	}
	if got := fake.Digests("app"); !slices.Equal(got, sorted(d["pinned"], d["v2"], d["child"], d["sig"])) {
		t.Errorf("remaining = %v, want the pinned image plus v2 with its child and signature", got)
	}
	if got := fake.Deleted(); !slices.Equal(got, refs("app", d["v1"])) {
		t.Errorf("deleted = %v, want only v1", got)
	}

	// Registries without the restriction delete every tagged manifest.
	fake, d = build(false)
	if err := fakePruner(t, fake).Prune(context.Background(), rule(d["pinned"])); err != nil {
		t.Fatal(err)
	}
	if got := fake.Digests("app"); !slices.Equal(got, []string{d["pinned"]}) {
		t.Errorf("remaining = %v, want only the pinned image", got)
	}

	// Nothing is protected when the whole repository goes.
	fake, _ = build(true)
	deleteAll := ruleSet(t, `[{"repo": "^app$", "untagged": [{"keep": false}], "tagged": [{"keep": false}]}]`)
	if err := fakePruner(t, fake).Prune(context.Background(), deleteAll); err != nil {
		t.Fatal(err)
	}
	if got := fake.DeletedRepositories(); !slices.Equal(got, []string{"app"}) {
		t.Errorf("deleted repositories = %v, want [app]", got)
	}
}

func TestKeepLastTagPrefersImages(t *testing.T) {
	p := testPruner()
	now := time.Now()
	image := testManifest("image", now.Add(-time.Hour), "v1")
	sig := testManifest("sig", now, "sha256-image.sig") // newer, but a referrer
	sig.Subject = &v1.Descriptor{Digest: "image"}
	untagged := testManifest("untagged", now)
	all := []*registry.Manifest{sig, untagged, image}
	manifests := byDigest(all...)
	rule := compileRule(t, &rules.RepoRuleSpec{RepoRegex: ".+"})

	kept := map[string]struct{}{"untagged": {}}
	if err := p.keepLastTag(all, manifests, kept, rule, now); err != nil {
		t.Fatal(err)
	}
	if _, ok := kept["image"]; !ok {
		t.Errorf("kept = %v, want the tagged image rather than its signature", kept)
	}
	if _, ok := kept["sig"]; !ok {
		t.Errorf("kept = %v, want the kept image's signature to follow it", kept)
	}

	// Nothing changes when a tagged manifest is kept anyway, or when
	// nothing is tagged.
	kept = map[string]struct{}{"image": {}}
	if err := p.keepLastTag(all, manifests, kept, rule, now); err != nil || len(kept) != 1 {
		t.Errorf("kept = %v, %v; want it unchanged", kept, err)
	}
	kept = map[string]struct{}{"untagged": {}}
	if err := p.keepLastTag([]*registry.Manifest{untagged}, byDigest(untagged), kept, rule, now); err != nil || len(kept) != 1 {
		t.Errorf("kept = %v, %v; want it unchanged without tagged manifests", kept, err)
	}

	// Keeping the last tagged index fails like any keep when a platform
	// image it needs is missing and the rule does not ignore that.
	index := testManifest("index", now, "v1")
	index.Manifests = []v1.Descriptor{{Digest: "missing"}}
	strict := compileRule(t, &rules.RepoRuleSpec{RepoRegex: ".+", IgnoreMissingManifests: new(false)})
	kept = map[string]struct{}{"untagged": {}}
	if err := p.keepLastTag([]*registry.Manifest{index, untagged}, byDigest(index, untagged), kept, strict, now); err == nil {
		t.Error("a missing dependency of the kept tag should fail a strict rule")
	}
}

// TestKeepLastTagPrefersHealthyImages: a broken index cannot be kept whole —
// with missing manifests not ignored, trying fails the run — so a healthy
// tagged image is kept instead, however much older.
func TestKeepLastTagPrefersHealthyImages(t *testing.T) {
	p := testPruner()
	now := time.Now()
	broken := testManifest("broken", now, "v2")
	broken.Manifests = []v1.Descriptor{{Digest: "missing"}}
	healthy := testManifest("healthy", now.Add(-time.Hour), "v1")
	untagged := testManifest("untagged", now)
	manifests := byDigest(broken, healthy, untagged)
	markOrphans(manifests)
	strict := compileRule(t, &rules.RepoRuleSpec{RepoRegex: ".+", IgnoreMissingManifests: new(false), DeleteOrphanedManifests: new(true)})

	kept := map[string]struct{}{"untagged": {}}
	if err := p.keepLastTag([]*registry.Manifest{broken, untagged, healthy}, manifests, kept, strict, now); err != nil {
		t.Fatalf("keepLastTag should keep the healthy image instead of failing on the broken one: %v", err)
	}
	if _, ok := kept["healthy"]; !ok || len(kept) != 2 {
		t.Errorf("kept = %v, want the healthy image added", kept)
	}
}

// TestPruneLeavesRepositoryWithMissingManifests: a manifest the registry
// lists but cannot serve was never judged by the rules, so the repository is
// not deleted outright even when the rules keep nothing else — deleting it
// would take that manifest along. Its unknown dependencies must survive too.
func TestPruneLeavesRepositoryWithMissingManifests(t *testing.T) {
	for _, protect := range []bool{false, true} {
		fake := registrytest.New()
		fake.ProtectLastTag = protect
		old := time.Now().Add(-48 * time.Hour)
		missing := "sha256:" + strings.Repeat("0", 64)
		fake.AddMissing("app", registry.Attributes{Digest: missing, Tags: []string{"latest"}, LastUpdated: old})
		a := fake.Add("app", registrytest.Image("a"), registry.Attributes{LastUpdated: old})
		b := fake.Add("app", registrytest.Image("b"), registry.Attributes{LastUpdated: old})

		deleteUntagged := ruleSet(t, `[{"repo": "^app$", "untagged": [{"keep": false}]}]`)
		if err := fakePruner(t, fake).Prune(context.Background(), deleteUntagged); err != nil {
			t.Fatalf("protect=%v: %v", protect, err)
		}
		if got := fake.DeletedRepositories(); len(got) != 0 {
			t.Errorf("protect=%v: deleted repositories %v; the missing manifest would have gone with them", protect, got)
		}
		if got := fake.Deleted(); len(got) != 0 {
			t.Errorf("protect=%v: deleted possible dependencies: %v", protect, got)
		}
		if got := fake.Digests("app"); !slices.Equal(got, slices.Sorted(slices.Values([]string{missing, a, b}))) {
			t.Errorf("protect=%v: remaining = %v, want the whole repository left alone", protect, got)
		}
	}
}

func TestPruneIncludeLocked(t *testing.T) {
	fake := registrytest.New()
	old := time.Now().Add(-48 * time.Hour)
	locked := fake.Add("app", registrytest.Image("locked"), registry.Attributes{Tags: []string{"old"}, LastUpdated: old, Locked: true})
	fake.Add("app", registrytest.Image("current"), registry.Attributes{Tags: []string{"current"}, LastUpdated: time.Now()})
	fake.LockTag("app", "old")
	deleteOld := ruleSet(t, `[{"repo": "^app$", "tagged": [{"tag": "^old$", "keep": false}]}]`)

	p := fakePruner(t, fake)
	p.IncludeLocked = true
	p.DryRun = true
	if err := p.Prune(context.Background(), deleteOld); err != nil {
		t.Fatal(err)
	}
	if len(fake.Unlocked()) != 0 {
		t.Errorf("a dry run unlocked %v", fake.Unlocked())
	}

	p.DryRun = false
	if err := p.Prune(context.Background(), deleteOld); err != nil {
		t.Fatal(err)
	}
	if got := fake.Unlocked(); !slices.Equal(got, []string{"app:old", "app@" + locked}) {
		t.Errorf("unlocked = %v", got)
	}
	if got := fake.Deleted(); !slices.Equal(got, refs("app", locked)) {
		t.Errorf("deleted = %v", got)
	}
}

// TestPruneNewestUntaggedRanksImages: a multi-platform build stores its
// platform images as untagged manifests. They follow their index and take no
// `newest` slot, so keeping the 3 newest untagged images keeps 3 rollback
// builds — not 1 plus the platform images of the tagged latest build — and the
// platform images of a deleted build go with it rather than stay dangling.
func TestPruneNewestUntaggedRanksImages(t *testing.T) {
	for _, doc := range []string{
		`[{"repo": "^app$", "untagged": [{"newest": 3, "keep": true}, {"keep": false}]}]`,
		`[{"repo": "^app$", "untagged": [{"newest": -3, "keep": false}]}]`,
	} {
		fake := registrytest.New()
		now := time.Now()
		latest, images := multiArch(fake, "app", "latest", now.Add(-10*24*time.Hour), "latest")
		kept := append(images, latest)
		var deleted []string
		for i := 1; i <= 4; i++ {
			rollback, images := multiArch(fake, "app", fmt.Sprint("rollback", i), now.Add(-time.Duration(10+i)*24*time.Hour))
			if i <= 3 {
				kept = append(kept, append(images, rollback)...)
			} else {
				deleted = append(deleted, append(images, rollback)...)
			}
		}
		if err := fakePruner(t, fake).Prune(context.Background(), ruleSet(t, doc)); err != nil {
			t.Fatal(err)
		}
		if got, want := fake.Deleted(), refs("app", deleted...); !slices.Equal(got, want) {
			t.Errorf("%s: deleted = %v, want the oldest rollback build: %v", doc, got, want)
		}
		if got, want := fake.Digests("app"), sorted(kept...); !slices.Equal(got, want) {
			t.Errorf("%s: remaining = %v, want latest and 3 rollback builds, whole: %v", doc, got, want)
		}
	}
}

// TestPruneNewestTaggedRanksImages: builds pushed with a tag per architecture
// and a manifest list over them (docker manifest create app:release-N
// app:release-N-amd64 app:release-N-arm64) filled 3 `newest` slots each, so
// the README's rules keeping the three most recent release images kept one
// build. Platform images take no slot, tagged or not.
func TestPruneNewestTaggedRanksImages(t *testing.T) {
	readme := `[{"repo": "^app$", "tagged": [{"tag": "^release-", "newest": 3, "keep": true}, {"tag": ".+", "keep": false}]}]`
	fake := registrytest.New()
	now := time.Now()
	var kept, deleted []string
	for i := 1; i <= 4; i++ {
		updated := now.Add(-time.Duration(10-i) * 24 * time.Hour)
		tag := fmt.Sprint("release-", i)
		amd := fake.Add("app", registrytest.Image(tag+"-amd64"), registry.Attributes{Tags: []string{tag + "-amd64"}, LastUpdated: updated})
		arm := fake.Add("app", registrytest.Image(tag+"-arm64"), registry.Attributes{Tags: []string{tag + "-arm64"}, LastUpdated: updated})
		index := fake.Add("app", registrytest.Index(registrytest.Child(amd, "linux", "amd64"), registrytest.Child(arm, "linux", "arm64")), registry.Attributes{Tags: []string{tag}, LastUpdated: updated})
		if i == 1 {
			deleted = append(deleted, index, amd, arm)
		} else {
			kept = append(kept, index, amd, arm)
		}
	}
	if err := fakePruner(t, fake).Prune(context.Background(), ruleSet(t, readme)); err != nil {
		t.Fatal(err)
	}
	if got, want := fake.Digests("app"), sorted(kept...); !slices.Equal(got, want) {
		t.Errorf("remaining %v, want the three newest builds, whole: %v", got, want)
	}
	if got, want := sorted(fake.Deleted()...), refs("app", deleted...); !slices.Equal(got, want) {
		t.Errorf("deleted %v, want the oldest build, whole: %v", got, want)
	}
}

// TestPruneTaggedPlatformImageFollowsItsIndex: a platform image carrying a
// tag of its own, retagged more recently than its index, took the first
// `newest` slot, pushing out a whole build, and could be kept on its own while
// its index was deleted. It follows its index instead: a ranked rule its tag
// reaches leaves it to the index rather than to the rules after it.
func TestPruneTaggedPlatformImageFollowsItsIndex(t *testing.T) {
	for _, doc := range []string{
		`[{"repo": "^app$", "tagged": [{"tag": "^release-", "newest": 2, "keep": true}, {"tag": ".+", "keep": false}], "untagged": [{"keep": false}]}]`,
		`[{"repo": "^app$", "tagged": [{"tag": "^release-", "newest": -2, "keep": false}], "untagged": [{"keep": false}]}]`,
	} {
		fake := registrytest.New()
		now := time.Now()
		old := now.Add(-30 * 24 * time.Hour)
		build := func(name string, updated time.Time) []string {
			index, images := multiArch(fake, "app", name, updated, name)
			return append(images, index)
		}
		kept := append(build("release-3", old.Add(2*time.Hour)), build("release-2", old.Add(time.Hour))...)
		amd := fake.Add("app", registrytest.Image("release-1-amd64"), registry.Attributes{Tags: []string{"release-1-amd64"}, LastUpdated: now.Add(-48 * time.Hour)})
		arm := fake.Add("app", registrytest.Image("release-1-arm64"), registry.Attributes{LastUpdated: old})
		index := fake.Add("app", registrytest.Index(registrytest.Child(amd, "linux", "amd64"), registrytest.Child(arm, "linux", "arm64")), registry.Attributes{Tags: []string{"release-1"}, LastUpdated: old})

		if err := fakePruner(t, fake).Prune(context.Background(), ruleSet(t, doc)); err != nil {
			t.Fatal(err)
		}
		if got, want := fake.Digests("app"), sorted(kept...); !slices.Equal(got, want) {
			t.Errorf("%s: remaining %v, want the two newest builds, whole: %v", doc, got, want)
		}
		if got, want := sorted(fake.Deleted()...), refs("app", index, amd, arm); !slices.Equal(got, want) {
			t.Errorf("%s: deleted %v, want the oldest build with its tagged platform image: %v", doc, got, want)
		}
	}
}

// TestPruneLogsPlannedDeletions: a dry run's log lines say what would be
// deleted, never that it was.
func TestPruneLogsPlannedDeletions(t *testing.T) {
	for _, dryRun := range []bool{true, false} {
		fake := registrytest.New()
		buildApp(fake, time.Now())
		p := fakePruner(t, fake)
		p.DryRun = dryRun
		log := captureLog(p)
		if err := p.Prune(context.Background(), ruleSet(t, featureCleanup)); err != nil {
			t.Fatal(err)
		}
		planned := strings.Contains(log.String(), "totals.would_delete_manifests=2") && strings.Contains(log.String(), " would_delete=")
		done := strings.Contains(log.String(), "totals.deleted_manifests=2") && strings.Contains(log.String(), " deleted=")
		if planned != dryRun || done == dryRun {
			t.Errorf("dry run %v: log labels deletions wrongly:\n%s", dryRun, log)
		}
	}
}

// TestPruneFailsOnOrphansUnlessTolerated: a rule neither ignoring missing
// manifests nor deleting orphans stops the run at a broken index, before
// anything is deleted.
func TestPruneFailsOnOrphansUnlessTolerated(t *testing.T) {
	fake := registrytest.New()
	old := time.Now().Add(-48 * time.Hour)
	missing := "sha256:" + strings.Repeat("0", 64)
	fake.Add("app", registrytest.Index(registrytest.Child(missing, "linux", "amd64")), registry.Attributes{Tags: []string{"v1"}, LastUpdated: old})
	fake.Add("app", registrytest.Image("leftover"), registry.Attributes{LastUpdated: old})

	strict := ruleSet(t, `[{"repo": "^app$", "ignore_missing_manifests": false, "untagged": [{"keep": false}]}]`)
	if err := fakePruner(t, fake).Prune(context.Background(), strict); err == nil || !strings.Contains(err.Error(), "orphaned") {
		t.Errorf("error = %v, want the orphan reported", err)
	}
	if fake.Calls("DeleteManifest") != 0 || fake.Calls("DeleteRepository") != 0 {
		t.Error("deleted despite the orphan")
	}
}

// TestPruneStopsWhenCanceled: an interrupted run starts no further
// repository.
func TestPruneStopsWhenCanceled(t *testing.T) {
	fake := registrytest.New()
	old := time.Now().Add(-48 * time.Hour)
	fake.Add("a", registrytest.Image("a"), registry.Attributes{Tags: []string{"v1"}, LastUpdated: old})
	next := fake.Add("b", registrytest.Image("b"), registry.Attributes{LastUpdated: old})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fake.Fail = func(op, repository string) error {
		if op == "GetManifest" && repository == "a" {
			cancel() // while the first repository is inspected
		}
		return nil
	}

	err := fakePruner(t, fake).Prune(ctx, ruleSet(t, `[{"repo": ".+", "untagged": [{"keep": false}]}]`))
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want the cancellation", err)
	}
	if got := fake.Digests("b"); !slices.Equal(got, []string{next}) || fake.Calls("DeleteRepository") != 0 {
		t.Errorf("b = %v, want the repository after the interruption untouched", got)
	}
}

// TestPrunePlatformsFromConfig covers registries whose listings report no
// platform (GHCR): a rule matching on architecture has it read from the image
// config, and a rule that does not never pays for the download.
func TestPrunePlatformsFromConfig(t *testing.T) {
	fake := registrytest.New()
	old := time.Now().Add(-48 * time.Hour)
	for repository, arch := range map[string]string{"amd-only": "amd64", "multi": "arm64"} {
		image := registrytest.Image(repository)
		config := fake.PutConfig(arch, "linux")
		image.Config = &config
		fake.Add(repository, image, registry.Attributes{Tags: []string{"v1"}, LastUpdated: old})
	}
	amd64Only := `[{"repo": ".+", "must_delete_everything": true,
		"untagged": [{"match_older": "24h", "keep": false}],
		"tagged": [{"arch": "arm64", "keep": true}, {"arch": "amd64", "keep": false}]}]`

	if err := fakePruner(t, fake).Prune(context.Background(), ruleSet(t, featureCleanup)); err != nil {
		t.Fatal(err)
	}
	if got := fake.Calls("GetBlob"); got != 0 {
		t.Errorf("configs downloaded %d times for rules not matching on platform", got)
	}

	if err := fakePruner(t, fake).Prune(context.Background(), ruleSet(t, amd64Only)); err != nil {
		t.Fatal(err)
	}
	if got := fake.DeletedRepositories(); !slices.Equal(got, []string{"amd-only"}) {
		t.Errorf("deleted repositories = %v, want [amd-only]", got)
	}
	if fake.Digests("multi") == nil {
		t.Error("the arm64 repository should survive")
	}
}

func TestCollectRegistryStats(t *testing.T) {
	fake := registrytest.New()
	now := time.Now()
	fake.Add("app", registrytest.Image("running"), registry.Attributes{Tags: []string{"v1"}, LastUpdated: now})
	fake.Add("app", registrytest.Image("idle"), registry.Attributes{LastUpdated: now.Add(-time.Hour)})
	fake.Add("tools", registrytest.Image("tool"), registry.Attributes{Tags: []string{"v1"}, LastUpdated: now})
	fake.AddRepository("secret")
	fake.Fail = func(_, repository string) error {
		if repository == "secret" {
			return registrytest.Forbidden(repository)
		}
		return nil
	}
	reg, err := registry.New(fake, slog.New(slog.DiscardHandler), 4, nil)
	if err != nil {
		t.Fatal(err)
	}
	specs, _, err := rules.KeepRulesFromImageList(strings.NewReader("myreg.azurecr.io/app:v1\n"), "myreg.azurecr.io")
	if err != nil {
		t.Fatal(err)
	}
	runningRules, err := rules.Compile(specs)
	if err != nil {
		t.Fatal(err)
	}

	var updates []int
	stats, err := CollectRegistryStats(context.Background(), reg, runningRules, func(stats []RepositoryStats) error {
		updates = append(updates, len(stats))
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "insufficient permission to scan 1 of 3 repositories: secret") {
		t.Errorf("error = %v, want the denied repository reported", err)
	}
	if len(stats) != 2 || stats[0].Name != "app" || stats[1].Name != "tools" {
		t.Fatalf("stats = %+v, want app and tools", stats)
	}
	if stats[0].Count != 2 || stats[0].Tagged != 1 || stats[0].Running != 1 || !stats[0].Newest.Equal(now) {
		t.Errorf("app stats = %+v", stats[0])
	}
	if !slices.Equal(updates, []int{1, 2}) {
		t.Errorf("updates = %v, want one per scanned repository", updates)
	}

	// Listing failures and other errors abort.
	fake.Fail = func(op, _ string) error {
		if op == "ListRepositories" {
			return errors.New("boom")
		}
		return nil
	}
	if stats, err := CollectRegistryStats(context.Background(), reg, nil, nil); err == nil || stats != nil {
		t.Errorf("CollectRegistryStats = %v, %v; want the listing error", stats, err)
	}
	fake.Fail = func(op, _ string) error {
		if op == "GetManifest" {
			return errors.New("boom")
		}
		return nil
	}
	if stats, err := CollectRegistryStats(context.Background(), reg, nil, nil); err == nil || stats != nil {
		t.Errorf("CollectRegistryStats = %v, %v; want the download error", stats, err)
	}
	fake.Fail = nil
	boom := errors.New("write failed")
	if _, err := CollectRegistryStats(context.Background(), reg, nil, func([]RepositoryStats) error { return boom }); !errors.Is(err, boom) {
		t.Errorf("an update failure should abort, got %v", err)
	}
}

// TestCollectRegistryStatsSucceeds: a scan of repositories that can all be
// read succeeds. A repository gone between the catalog listing and its scan
// is left out, and manifests the registry lists but cannot serve are counted
// like the others, their bytes unknown.
func TestCollectRegistryStatsSucceeds(t *testing.T) {
	fake := registrytest.New()
	now := time.Now()
	fake.Add("app", registrytest.Image("app"), registry.Attributes{Tags: []string{"v1"}, LastUpdated: now})
	fake.AddMissing("app", registry.Attributes{Digest: "sha256:" + strings.Repeat("0", 64), Tags: []string{"v0"}, LastUpdated: now})
	fake.AddMissing("app", registry.Attributes{Digest: "sha256:" + strings.Repeat("1", 64), LastUpdated: now})
	fake.AddRepository("gone")
	fake.Fail = func(op, repository string) error {
		if op == "ListManifests" && repository == "gone" {
			return registrytest.NotFound("repository " + repository)
		}
		return nil
	}
	reg, err := registry.New(fake, slog.New(slog.DiscardHandler), 4, nil)
	if err != nil {
		t.Fatal(err)
	}

	stats, err := CollectRegistryStats(context.Background(), reg, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(stats) != 1 || stats[0].Name != "app" {
		t.Fatalf("stats = %+v, want app only", stats)
	}
	if got := stats[0]; got.Count != 3 || got.Tagged != 2 || got.Untagged != 1 {
		t.Errorf("app count/tagged/untagged = %d/%d/%d, want 3/2/1 with the missing manifests", got.Count, got.Tagged, got.Untagged)
	}
}

// TestCollectRegistryStatsSkipsUnreliableRepositories: a repository that
// changed during its scan, or holds a manifest in an unsupported format, is
// skipped; the others are scanned, and the error names the skipped ones.
func TestCollectRegistryStatsSkipsUnreliableRepositories(t *testing.T) {
	fake := registrytest.New()
	now := time.Now()
	fake.Put("legacy", []byte(`{"schemaVersion": 1, "name": "legacy", "tag": "v1"}`), registry.Attributes{Tags: []string{"v1"}, LastUpdated: now})
	fake.Add("tools", registrytest.Image("tool"), registry.Attributes{Tags: []string{"v1"}, LastUpdated: now})
	reg, err := registry.New(fake, slog.New(slog.DiscardHandler), 4, nil)
	if err != nil {
		t.Fatal(err)
	}

	stats, err := CollectRegistryStats(context.Background(), reg, nil, nil)
	if !errors.Is(err, registry.ErrUnsupportedManifest) || !strings.Contains(err.Error(), "skipped 1 of 2 repositories that could not be scanned reliably: legacy: ") {
		t.Errorf("error = %v, want legacy reported", err)
	}
	if len(stats) != 1 || stats[0].Name != "tools" {
		t.Errorf("stats = %+v, want tools scanned", stats)
	}
}

// TestCollectRegistryStatsReturnsPartialResultsWhenCanceled: an interrupted
// scan returns what it collected, with the cancellation.
func TestCollectRegistryStatsReturnsPartialResultsWhenCanceled(t *testing.T) {
	fake := registrytest.New()
	fake.Add("a", registrytest.Image("a"), registry.Attributes{LastUpdated: time.Now()})
	fake.Add("b", registrytest.Image("b"), registry.Attributes{LastUpdated: time.Now()})
	reg, err := registry.New(fake, slog.New(slog.DiscardHandler), 4, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stats, err := CollectRegistryStats(ctx, reg, nil, func([]RepositoryStats) error {
		cancel()
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want the cancellation", err)
	}
	if len(stats) != 1 || stats[0].Name != "a" {
		t.Errorf("stats = %+v, want the first repository's", stats)
	}
}
