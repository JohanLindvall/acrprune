package pruner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	v1 "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/JohanLindvall/crprune/internal/progress"
	"github.com/JohanLindvall/crprune/internal/registry"
	"github.com/JohanLindvall/crprune/internal/registry/registrytest"
)

func TestWrappedDeletionFailuresAreNotMisclassified(t *testing.T) {
	denied := registrytest.Forbidden("app")
	for _, err := range []error{
		fmt.Errorf("deletions: %w", errors.Join(denied, errors.New("network failure"))),
		fmt.Errorf("deletions: %w", errors.Join(denied, context.Canceled)),
	} {
		if isDenied(err) {
			t.Errorf("mixed failures were classified as only permission errors: %v", err)
		}
	}
	if !isDenied(fmt.Errorf("deletions: %w", errors.Join(denied, denied))) {
		t.Error("wrapped permission failures were not recognized")
	}
}

// OCI indexes may omit platform descriptors and may point to nested indexes.
// Retention must still see the platforms of the images reachable through them.
func TestPlatformRetentionThroughIndexes(t *testing.T) {
	for _, nested := range []bool{false, true} {
		fake := registrytest.New()
		old := time.Now().Add(-48 * time.Hour)
		image := registrytest.Image("image")
		config := fake.PutConfig("amd64", "linux")
		image.Config = &config
		leaf := fake.Add("app", image, registry.Attributes{LastUpdated: old})
		child := registrytest.Child(leaf, "", "")
		child.Platform = nil
		if nested {
			inner := fake.Add("app", registrytest.Index(child), registry.Attributes{LastUpdated: old})
			child = registrytest.Child(inner, "", "")
			child.MediaType, child.Platform = v1.MediaTypeImageIndex, nil
		}
		fake.Add("app", registrytest.Index(child), registry.Attributes{Tags: []string{"v1"}, LastUpdated: old})
		want := fake.Digests("app")
		stale := fake.Add("app", registrytest.Image("stale"), registry.Attributes{LastUpdated: old, Architecture: "arm64", OS: "linux"})
		p := fakePruner(t, fake)
		if err := p.Prune(t.Context(), ruleSet(t, `[{"repo":"^app$","tagged":[{"arch":"^amd64$","os":"^linux$","keep":true},{"keep":false}],"untagged":[{"keep":false}]}]`)); err != nil {
			t.Fatal(err)
		}
		if got := fake.Digests("app"); !slices.Equal(got, want) {
			t.Errorf("nested %v: remaining = %v, want the amd64 index and its dependencies %v", nested, got, want)
		}
		if got := fake.Deleted(); !slices.Equal(got, refs("app", stale)) {
			t.Errorf("nested %v: deleted = %v, want only the unreferenced arm64 image", nested, got)
		}
	}
}

// The tests in this file cover what keeps pruning from deleting more than the
// rules ask for, or from deleting on the strength of a stale inspection.

// TestPruneDeletesOnlyWhenEveryTagIsCondemned: deleting a manifest deletes
// every tag on it. A build tagged by its feature branch, which
// rules/cleanup_feature_branches.json deletes, and later promoted by adding a
// release tag, which no rule deletes, is kept — and so is its repository when
// it is the only manifest there. A manifest whose every tag is condemned still
// goes.
func TestPruneDeletesOnlyWhenEveryTagIsCondemned(t *testing.T) {
	raw, err := os.ReadFile("../../rules/cleanup_feature_branches.json")
	if err != nil {
		t.Fatal(err)
	}
	fake := registrytest.New()
	old := time.Now().Add(-30 * 24 * time.Hour)
	promoted := fake.Add("app", registrytest.Image("promoted"), registry.Attributes{Tags: []string{"20260101.1-br.feature", "v2.3.0"}, LastUpdated: old})
	feature := fake.Add("app", registrytest.Image("feature"), registry.Attributes{Tags: []string{"20260102.1-br.feature", "20260102.1-pr.feature"}, LastUpdated: old})
	only := fake.Add("svc", registrytest.Image("only"), registry.Attributes{Tags: []string{"20260101.1-br.feature", "v2.3.0"}, LastUpdated: old})

	p := fakePruner(t, fake)
	log := captureLog(p)
	if err := p.Prune(context.Background(), ruleSet(t, string(raw))); err != nil {
		t.Fatal(err)
	}
	if got, want := fake.Deleted(), refs("app", feature); !slices.Equal(got, want) {
		t.Errorf("deleted = %v, want only the manifest all of whose tags are feature tags: %v", got, want)
	}
	if got := fake.DeletedRepositories(); len(got) != 0 {
		t.Errorf("deleted repositories %v along with a release tag", got)
	}
	if got := fake.Digests("app"); !slices.Equal(got, []string{promoted}) {
		t.Errorf("app = %v, want the promoted build", got)
	}
	if got := fake.Digests("svc"); !slices.Equal(got, []string{only}) {
		t.Errorf("svc = %v, want the promoted build", got)
	}
	for _, want := range []string{"Keeping manifest: deleting it would also delete a tag the rules keep", "tag=v2.3.0"} {
		if !strings.Contains(log.String(), want) {
			t.Errorf("log does not say why the promoted build is kept: missing %q in\n%s", want, log)
		}
	}

	// Rules generated from running images keep a running tag's manifest,
	// whatever other tags it carries.
	fake = registrytest.New()
	running := fake.Add("app", registrytest.Image("running"), registry.Attributes{Tags: []string{"running", "stale"}, LastUpdated: old})
	stale := fake.Add("app", registrytest.Image("stale"), registry.Attributes{Tags: []string{"stale2"}, LastUpdated: old})
	if err := fakePruner(t, fake).Prune(context.Background(), generated(t, "myreg.azurecr.io/app:running")); err != nil {
		t.Fatal(err)
	}
	if got, want := fake.Deleted(), refs("app", stale); !slices.Equal(got, want) {
		t.Errorf("deleted = %v, want only the manifest that is not running: %v", got, want)
	}
	if got := fake.Digests("app"); !slices.Equal(got, []string{running}) {
		t.Errorf("remaining = %v, want the running image", got)
	}
}

// buildLocked stores a repository whose index is locked itself, with an
// untagged platform image and a signature, and whose image v2 is locked
// through its tag, besides an unlocked image v3. The backend refuses, as ACR
// does, to delete anything locked.
func buildLocked(old time.Time) (*registrytest.Backend, map[string]string) {
	fake := registrytest.New()
	fake.EnforceLocks = true
	d := map[string]string{}
	d["child"] = fake.Add("app", registrytest.Image("child"), registry.Attributes{LastUpdated: old})
	d["index"] = fake.Add("app", registrytest.Index(registrytest.Child(d["child"], "linux", "amd64")), registry.Attributes{Tags: []string{"v1"}, LastUpdated: old, Locked: true})
	d["signature"] = fake.Add("app", registrytest.Image("signature"), registry.Attributes{Tags: []string{schemeTag(d["index"], ".sig")}, LastUpdated: old})
	d["tagLocked"] = fake.Add("app", registrytest.Image("tag-locked"), registry.Attributes{Tags: []string{"v2"}, LastUpdated: old})
	fake.LockTag("app", "v2")
	d["plain"] = fake.Add("app", registrytest.Image("plain"), registry.Attributes{Tags: []string{"v3"}, LastUpdated: old})
	return fake, d
}

// TestPruneKeepsLockedManifests: a manifest locked against deletion, by its
// own lock or its tag's, is kept like one in the grace period — with the
// manifests it references and its signatures, and with its repository, which
// is not deleted outright — unless --include-locked is given. Deleting it
// would fail and stop the run. Dry runs show the same.
func TestPruneKeepsLockedManifests(t *testing.T) {
	old := time.Now().Add(-48 * time.Hour)
	deleteAll := ruleSet(t, `[{"repo": "^app$", "untagged": [{"keep": false}], "tagged": [{"keep": false}]}]`)

	for _, dryRun := range []bool{true, false} {
		fake, d := buildLocked(old)
		p := fakePruner(t, fake)
		p.DryRun = dryRun
		log := captureLog(p)
		if err := p.Prune(context.Background(), deleteAll); err != nil {
			t.Fatalf("dry run %v: %v", dryRun, err)
		}
		// Listed to decide, and in a live run once more to recheck before
		// deleting.
		if got, want := fake.Calls("LockedTags"), map[bool]int{true: 1, false: 2}[dryRun]; got != want {
			t.Errorf("dry run %v: tag locks listed %d times, want %d", dryRun, got, want)
		}
		if got := fake.Unlocked(); len(got) != 0 {
			t.Errorf("dry run %v: unlocked %v", dryRun, got)
		}
		if got := strings.Count(log.String(), "Keeping locked manifest; use --include-locked to delete it"); got != 2 {
			t.Errorf("dry run %v: %d locked manifests logged, want the index and v2:\n%s", dryRun, got, log)
		}
		if dryRun {
			if !strings.Contains(log.String(), `msg="Dry-run: deleting manifest" manifest.ref=app@`+d["plain"]) || strings.Contains(log.String(), "Dry-run: deleting repository") {
				t.Errorf("dry run: want only v3 previewed for deletion:\n%s", log)
			}
			continue
		}
		if got, want := fake.Deleted(), refs("app", d["plain"]); !slices.Equal(got, want) {
			t.Errorf("deleted = %v, want only the unlocked image: %v", got, want)
		}
		if got := fake.DeletedRepositories(); len(got) != 0 {
			t.Errorf("deleted repositories %v holding locked manifests", got)
		}
	}

	// --include-locked unlocks them, tag locks included, and deletes them.
	fake, d := buildLocked(old)
	p := fakePruner(t, fake)
	p.IncludeLocked = true
	if err := p.Prune(context.Background(), deleteAll); err != nil {
		t.Fatal(err)
	}
	if got := fake.DeletedRepositories(); !slices.Equal(got, []string{"app"}) {
		t.Errorf("deleted repositories = %v, want [app]", got)
	}
	if got, want := fake.Unlocked(), sorted("app:v2", "app@"+d["index"]); !slices.Equal(got, want) {
		t.Errorf("unlocked = %v, want %v", got, want)
	}
}

// TestPruneMustDeleteEverythingKeepsLockedRepositories: a locked manifest
// counts as kept for must_delete_everything, so the repository stays whole.
func TestPruneMustDeleteEverythingKeepsLockedRepositories(t *testing.T) {
	fake, _ := buildLocked(time.Now().Add(-48 * time.Hour))
	want := fake.Digests("app")
	deleteAll := ruleSet(t, `[{"repo": "^app$", "must_delete_everything": true, "untagged": [{"keep": false}], "tagged": [{"keep": false}]}]`)
	if err := fakePruner(t, fake).Prune(context.Background(), deleteAll); err != nil {
		t.Fatal(err)
	}
	if got := fake.Digests("app"); !slices.Equal(got, want) {
		t.Errorf("remaining = %v, want the repository untouched: %v", got, want)
	}
}

func TestBulkPruneChecksLocksOfFreshReferrers(t *testing.T) {
	for _, dryRun := range []bool{false, true} {
		fake := registrytest.New()
		old := time.Now().Add(-48 * time.Hour)
		image := fake.Add("app", registrytest.Image("image"), registry.Attributes{LastUpdated: old})
		tag := schemeTag(image, ".sig")
		fake.Add("app", registrytest.Image("signature"), registry.Attributes{Tags: []string{tag}, LastUpdated: time.Now()})
		fake.LockTag("app", tag)
		want := fake.Digests("app")
		p := fakePruner(t, fake)
		p.DryRun, p.KeepYounger = dryRun, 24*time.Hour
		log := captureLog(p)
		if err := p.Prune(t.Context(), ruleSet(t, `[{"repo":"^app$","must_delete_everything":true,"untagged":[{"keep":false}],"tagged":[{"keep":false}]}]`)); err != nil {
			t.Fatal(err)
		}
		if got := fake.Digests("app"); !slices.Equal(got, want) {
			t.Errorf("dry run %v: remaining = %v, want %v", dryRun, got, want)
		}
		if strings.Contains(log.String(), "Dry-run: deleting") {
			t.Errorf("locked referrer's repository selected for deletion: %s", log)
		}
		if fake.Calls("LockedTags") != 1 {
			t.Error("the fresh referrer's tag lock was not checked")
		}
	}
}

// TestPruneDryRunShowsTagLocks: a tag lock shows only in a listing of the
// repository's tags. A dry run with --include-locked lists them too, and marks
// the manifests it would unlock.
func TestPruneDryRunShowsTagLocks(t *testing.T) {
	fake := registrytest.New()
	locked := fake.Add("app", registrytest.Image("locked"), registry.Attributes{Tags: []string{"v1"}, LastUpdated: time.Now().Add(-48 * time.Hour)})
	fake.Add("app", registrytest.Image("current"), registry.Attributes{Tags: []string{"v2"}, LastUpdated: time.Now()})
	fake.LockTag("app", "v1")

	p := fakePruner(t, fake)
	p.DryRun = true
	p.IncludeLocked = true
	log := captureLog(p)
	if err := p.Prune(context.Background(), ruleSet(t, `[{"repo": "^app$", "tagged": [{"tag": "^v1$", "keep": false}]}]`)); err != nil {
		t.Fatal(err)
	}
	if want := `msg="Dry-run: deleting manifest" manifest.ref=app@` + locked; !strings.Contains(log.String(), want) || !strings.Contains(log.String(), "manifest.locked=true") {
		t.Errorf("log does not mark the tag-locked manifest locked:\n%s", log)
	}
	if got := fake.Unlocked(); len(got) != 0 {
		t.Errorf("a dry run unlocked %v", got)
	}
}

// TestPruneStopsWhenTagLocksCannotBeListed: without the tag locks, what is
// locked is unknown, and nothing is deleted.
func TestPruneStopsWhenTagLocksCannotBeListed(t *testing.T) {
	fake := registrytest.New()
	fake.Add("app", registrytest.Image("old"), registry.Attributes{Tags: []string{"v1"}, LastUpdated: time.Now().Add(-48 * time.Hour)})
	boom := errors.New("boom")
	fake.Fail = func(op, _ string) error {
		if op == "LockedTags" {
			return boom
		}
		return nil
	}
	err := fakePruner(t, fake).Prune(context.Background(), ruleSet(t, `[{"repo": "^app$", "tagged": [{"keep": false}]}]`))
	if !errors.Is(err, boom) {
		t.Errorf("error = %v, want the listing failure", err)
	}
	if fake.Calls("DeleteManifest") != 0 || fake.Calls("DeleteRepository") != 0 {
		t.Error("deleted without knowing the tag locks")
	}
}

// TestPruneSkipsRepositoriesThatChanged: a listing is no snapshot. Before
// deleting anything, manifest by manifest or the whole repository, the
// repository is listed again; if a push, retag or deletion changed it since it
// was inspected, it is skipped with nothing deleted, the other repositories
// are still pruned, and the run fails at the end naming it.
func TestPruneSkipsRepositoriesThatChanged(t *testing.T) {
	old := time.Now().Add(-48 * time.Hour)
	for _, whole := range []bool{false, true} {
		fake := registrytest.New()
		fake.Add("app", registrytest.Image("v1"), registry.Attributes{Tags: []string{"v1"}, LastUpdated: old})
		if !whole {
			fake.Add("app", registrytest.Image("v2"), registry.Attributes{Tags: []string{"v2"}, LastUpdated: old})
		}
		fake.Add("next", registrytest.Image("next"), registry.Attributes{Tags: []string{"v1"}, LastUpdated: old})
		want := fake.Digests("app")
		var listings atomic.Int32
		fake.Fail = func(op, repository string) error {
			if op == "ListManifests" && repository == "app" && listings.Add(1) == 2 {
				fake.Add("app", registrytest.Image("pushed"), registry.Attributes{Tags: []string{"v3"}, LastUpdated: time.Now()})
			}
			return nil
		}

		err := fakePruner(t, fake).Prune(context.Background(), ruleSet(t, `[{"repo": ".+", "tagged": [{"tag": "^v1$", "keep": false}]}]`))
		if !errors.Is(err, registry.ErrRepositoryChanged) || !strings.Contains(err.Error(), "skipped 1 of 2 repositories that could not be pruned safely (pruned 1): app: ") {
			t.Errorf("whole %v: error = %v, want app reported as changed", whole, err)
		}
		if got := fake.Digests("app"); len(got) != len(want)+1 || fake.Calls("DeleteManifest") != 0 {
			t.Errorf("whole %v: app = %v, want nothing deleted from it", whole, got)
		}
		if got := fake.DeletedRepositories(); !slices.Equal(got, []string{"next"}) {
			t.Errorf("whole %v: deleted repositories = %v, want the next repository still pruned", whole, got)
		}
	}
}

// TestPruneSkipsRepositoriesWithUnsupportedManifests: a manifest in a format
// that cannot be decoded, a Docker schema 1 image say, hides what it
// references. Its repository is skipped, the others are pruned, and the run
// fails at the end naming it.
func TestPruneSkipsRepositoriesWithUnsupportedManifests(t *testing.T) {
	fake := registrytest.New()
	old := time.Now().Add(-48 * time.Hour)
	fake.Put("legacy", []byte(`{"schemaVersion": 1, "name": "legacy", "tag": "v1"}`), registry.Attributes{Tags: []string{"v1"}, LastUpdated: old})
	fake.Add("legacy", registrytest.Image("leftover"), registry.Attributes{LastUpdated: old})
	fake.Add("next", registrytest.Image("leftover"), registry.Attributes{LastUpdated: old})
	want := fake.Digests("legacy")

	err := fakePruner(t, fake).Prune(context.Background(), ruleSet(t, `[{"repo": ".+", "untagged": [{"keep": false}]}]`))
	if !errors.Is(err, registry.ErrUnsupportedManifest) || !strings.Contains(err.Error(), "skipped 1 of 2 repositories") {
		t.Errorf("error = %v, want legacy reported as unsupported", err)
	}
	if got := fake.Digests("legacy"); !slices.Equal(got, want) {
		t.Errorf("legacy = %v, want it untouched", got)
	}
	if got := fake.DeletedRepositories(); !slices.Equal(got, []string{"next"}) {
		t.Errorf("deleted repositories = %v, want the next repository still pruned", got)
	}
}

// TestPruneSkipsRepositoryGoneBeforeDeletion: a repository deleted by
// someone else after it was inspected leaves nothing to delete.
func TestPruneSkipsRepositoryGoneBeforeDeletion(t *testing.T) {
	fake := registrytest.New()
	old := time.Now().Add(-48 * time.Hour)
	fake.Add("app", registrytest.Image("v1"), registry.Attributes{Tags: []string{"v1"}, LastUpdated: old})
	fake.Add("app", registrytest.Image("v2"), registry.Attributes{Tags: []string{"v2"}, LastUpdated: old})
	var listings atomic.Int32
	fake.Fail = func(op, repository string) error {
		if op == "ListManifests" && listings.Add(1) == 2 {
			return registrytest.NotFound("repository " + repository)
		}
		return nil
	}
	p := fakePruner(t, fake)
	log := captureLog(p)
	if err := p.Prune(context.Background(), ruleSet(t, `[{"repo": "^app$", "tagged": [{"tag": "^v1$", "keep": false}]}]`)); err != nil {
		t.Fatal(err)
	}
	if fake.Calls("DeleteManifest") != 0 || fake.Calls("DeleteRepository") != 0 {
		t.Error("deleted from a repository that is gone")
	}
	if !strings.Contains(log.String(), "Repository deleted during inspection; skipping") || !strings.Contains(log.String(), "skipped_repos=1") {
		t.Errorf("log does not report the repository skipped:\n%s", log)
	}
}

// TestPruneLeavesEmptyRepositories: ACR and GHCR keep no empty repositories,
// so one listing nothing is an anomaly, and deleting it would free nothing.
func TestPruneLeavesEmptyRepositories(t *testing.T) {
	for _, doc := range []string{
		`[{"repo": "^empty$"}]`,
		`[{"repo": "^empty$", "untagged": [{"keep": false}], "tagged": [{"keep": false}]}]`,
	} {
		for _, dryRun := range []bool{true, false} {
			fake := registrytest.New()
			fake.AddRepository("empty")
			p := fakePruner(t, fake)
			p.DryRun = dryRun
			log := captureLog(p)
			if err := p.Prune(context.Background(), ruleSet(t, doc)); err != nil {
				t.Fatal(err)
			}
			if got := fake.DeletedRepositories(); len(got) != 0 {
				t.Errorf("%s: deleted repositories %v", doc, got)
			}
			noneDeleted := "totals.deleted_repos=0"
			if dryRun {
				noneDeleted = "totals.would_delete_repos=0"
			}
			if strings.Contains(log.String(), "Dry-run: deleting repository") || !strings.Contains(log.String(), noneDeleted) {
				t.Errorf("%s, dry run %v: the repository is planned for deletion:\n%s", doc, dryRun, log)
			}
		}
	}
}

// TestPruneReportsDenialOnlyWhenEveryDeletionIsDenied: a repository whose
// deletions are all refused for lack of permission is skipped as denied. When
// another deletion failed for another reason, that failure stops the run
// instead of hiding behind the denial.
func TestPruneReportsDenialOnlyWhenEveryDeletionIsDenied(t *testing.T) {
	boom := errors.New("boom")
	for _, mixed := range []bool{false, true} {
		fake := registrytest.New()
		old := time.Now().Add(-48 * time.Hour)
		fake.Add("app", registrytest.Image("a"), registry.Attributes{LastUpdated: old})
		fake.Add("app", registrytest.Image("b"), registry.Attributes{LastUpdated: old})
		fake.Add("app", registrytest.Image("kept"), registry.Attributes{Tags: []string{"v1"}, LastUpdated: old})
		var deletes atomic.Int32
		fake.Fail = func(op, repository string) error {
			if op != "DeleteManifest" {
				return nil
			}
			if mixed && deletes.Add(1) == 2 {
				return boom
			}
			return registrytest.Forbidden(repository)
		}

		err := fakePruner(t, fake).Prune(context.Background(), ruleSet(t, `[{"repo": "^app$", "untagged": [{"keep": false}]}]`))
		denied := err != nil && strings.Contains(err.Error(), "insufficient permission to prune 1 of 1 repositories")
		if denied == mixed || mixed && !errors.Is(err, boom) {
			t.Errorf("mixed %v: error = %v", mixed, err)
		}
	}
}

func TestPruneReportsCompletedDeletionsOnFailure(t *testing.T) {
	for _, failure := range []error{registrytest.Forbidden("app"), errors.New("connection failed"), context.Canceled} {
		t.Run(failure.Error(), func(t *testing.T) {
			fake := registrytest.New()
			old := time.Now().Add(-48 * time.Hour)
			fake.Add("app", registrytest.Image("a"), registry.Attributes{LastUpdated: old})
			fake.Add("app", registrytest.Image("b"), registry.Attributes{LastUpdated: old})
			fake.Add("app", registrytest.Image("kept"), registry.Attributes{Tags: []string{"v1"}, LastUpdated: old})
			var calls atomic.Int32
			fake.Fail = func(op, repo string) error {
				if op == "DeleteManifest" && calls.Add(1) > 1 {
					return failure
				}
				return nil
			}
			p := fakePruner(t, fake)
			log := captureLog(p)
			err := p.Prune(t.Context(), ruleSet(t, `[{"repo":"^app$","untagged":[{"keep":false}]}]`))
			if err == nil || len(fake.Deleted()) != 1 {
				t.Fatalf("error = %v, deleted = %v", err, fake.Deleted())
			}
			for _, want := range []string{"totals.deleted_manifests=1", "totals.kept_manifests=2", "totals.deleted_repos=0"} {
				if !strings.Contains(log.String(), want) {
					t.Errorf("missing %q in summary: %s", want, log)
				}
			}
		})
	}
}

// TestPruneProtectsRunningImages: with --running, images in use are never
// deleted, whatever the rules say. An old base image still serving traffic
// keeps rules/delete_old_repos.json from deleting its repository, and a
// running image keeps its platform images and its signature.
func TestPruneProtectsRunningImages(t *testing.T) {
	raw, err := os.ReadFile("../../rules/delete_old_repos.json")
	if err != nil {
		t.Fatal(err)
	}
	fake := registrytest.New()
	ancient := time.Now().Add(-800 * 24 * time.Hour)
	proxy := fake.Add("legacy-proxy", registrytest.Image("proxy"), registry.Attributes{Tags: []string{"v1"}, LastUpdated: ancient})
	previous := fake.Add("legacy-proxy", registrytest.Image("previous"), registry.Attributes{Tags: []string{"v0"}, LastUpdated: ancient})
	fake.Add("unused", registrytest.Image("unused"), registry.Attributes{Tags: []string{"v1"}, LastUpdated: ancient})

	p := fakePruner(t, fake)
	p.Protect = generated(t, "myreg.azurecr.io/legacy-proxy:v1")
	if err := p.Prune(context.Background(), ruleSet(t, string(raw))); err != nil {
		t.Fatal(err)
	}
	if got := fake.DeletedRepositories(); !slices.Equal(got, []string{"unused"}) {
		t.Errorf("deleted repositories = %v, want only the unused one", got)
	}
	if got := fake.Digests("legacy-proxy"); !slices.Equal(got, sorted(proxy, previous)) {
		t.Errorf("legacy-proxy = %v, want it whole", got)
	}

	fake = registrytest.New()
	old := time.Now().Add(-30 * 24 * time.Hour)
	running, images := multiArch(fake, "app", "running", old, "feature-running")
	signature := fake.Add("app", registrytest.Image("signature"), registry.Attributes{Tags: []string{schemeTag(running, ".sig")}, LastUpdated: old})
	stale := fake.Add("app", registrytest.Image("stale"), registry.Attributes{Tags: []string{"feature-stale"}, LastUpdated: old})
	p = fakePruner(t, fake)
	p.Protect = generated(t, "myreg.azurecr.io/app:feature-running")
	log := captureLog(p)
	if err := p.Prune(context.Background(), ruleSet(t, featureCleanup)); err != nil {
		t.Fatal(err)
	}
	if got, want := fake.Deleted(), refs("app", stale); !slices.Equal(got, want) {
		t.Errorf("deleted = %v, want only the image that is not running: %v", got, want)
	}
	if got, want := fake.Digests("app"), sorted(append(images, running, signature)...); !slices.Equal(got, want) {
		t.Errorf("remaining = %v, want the running image with its platform images and signature: %v", got, want)
	}
	if !strings.Contains(log.String(), "Keeping running image") {
		t.Errorf("log does not say why the running image is kept:\n%s", log)
	}
}

// TestMustDeleteEverythingWithPinnedReferrer: a locked or running signature
// was kept on its own while rules/delete_old_repos.json deleted its image
// manifest by manifest, leaving the signature dangling for good. A lock or a
// running image never lapses, so it keeps the whole repository, as a pinned
// image does; a signature kept by the grace period alone does not (see
// TestMustDeleteEverythingWithGraceKeptReferrer).
func TestMustDeleteEverythingWithPinnedReferrer(t *testing.T) {
	raw, err := os.ReadFile("../../rules/delete_old_repos.json")
	if err != nil {
		t.Fatal(err)
	}
	ancient := time.Now().Add(-800 * 24 * time.Hour)
	for _, tt := range []struct {
		name   string
		pin    func(fake *registrytest.Backend, p *Pruner, signature, tag string)
		locked bool
	}{
		{name: "locked", locked: true},
		{name: "locked tag", pin: func(fake *registrytest.Backend, _ *Pruner, _, tag string) { fake.LockTag("app", tag) }},
		{name: "running", pin: func(_ *registrytest.Backend, p *Pruner, signature, _ string) {
			p.Protect = generated(t, "myreg.azurecr.io/app@"+signature)
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fake := registrytest.New()
			fake.EnforceLocks = true
			image := fake.Add("app", registrytest.Image("img"), registry.Attributes{Tags: []string{"v1"}, LastUpdated: ancient})
			tag := schemeTag(image, ".sig")
			signature := fake.Add("app", registrytest.Image("sig"), registry.Attributes{Tags: []string{tag}, LastUpdated: ancient, Locked: tt.locked})
			p := fakePruner(t, fake)
			if tt.pin != nil {
				tt.pin(fake, p, signature, tag)
			}
			if err := p.Prune(context.Background(), ruleSet(t, string(raw))); err != nil {
				t.Fatal(err)
			}
			if got, want := fake.Digests("app"), sorted(image, signature); !slices.Equal(got, want) {
				t.Errorf("remaining %v, want the whole repository %v; deleted %v", got, want, fake.Deleted())
			}
		})
	}
}

// TestPruneCountsSkippedRepositories: a repository that changed before the
// recheck, that the credential may not prune, or that disappeared, was left
// out of the logged totals, and the dashboard counted the changed one's
// selection as selected although nothing of it was attempted.
func TestPruneCountsSkippedRepositories(t *testing.T) {
	old := time.Now().Add(-48 * time.Hour)
	fake := registrytest.New()
	for _, repository := range []string{"app", "denied", "next"} {
		fake.Add(repository, registrytest.Image(repository+"-v1"), registry.Attributes{Tags: []string{"v1"}, LastUpdated: old})
	}
	fake.Add("app", registrytest.Image("app-v2"), registry.Attributes{Tags: []string{"v2"}, LastUpdated: old})
	fake.AddRepository("gone")
	var listings atomic.Int32
	fake.Fail = func(op, repository string) error {
		switch {
		case op == "ListManifests" && repository == "gone":
			return registrytest.NotFound(repository)
		case op == "ListManifests" && repository == "denied":
			return registrytest.Forbidden(repository)
		case op == "ListManifests" && repository == "app" && listings.Add(1) == 2:
			fake.Add("app", registrytest.Image("pushed"), registry.Attributes{Tags: []string{"v3"}, LastUpdated: time.Now()})
		}
		return nil
	}
	p := fakePruner(t, fake)
	log := captureLog(p)
	tracker := progress.NewTracker()
	err := p.Prune(tracker.Context(context.Background()), ruleSet(t, `[{"repo": ".+", "tagged": [{"tag": "^v1$", "keep": false}]}]`))
	if err == nil || !strings.Contains(err.Error(), "skipped 1 of 4") || !strings.Contains(err.Error(), "insufficient permission to prune 1 of 4") {
		t.Fatalf("error = %v, want the changed and the denied repository reported", err)
	}
	lines := strings.Split(strings.TrimSpace(log.String()), "\n")
	totals := lines[len(lines)-1]
	for _, want := range []string{"totals.repositories=4", "totals.kept_repos=3", "totals.skipped_repos=3", "totals.deleted_repos=1"} {
		if !strings.Contains(totals, want) {
			t.Errorf("final totals %q lack %s", totals, want)
		}
	}
	if s := tracker.Snapshot(); s.Completed != 4 || s.Skipped != 2 || s.Denied != 1 || s.Selected != 1 || s.Deleted != 1 {
		t.Errorf("dashboard completed %d, skipped %d, denied %d, selected %d, deleted %d; want 4, 2, 1, 1, 1",
			s.Completed, s.Skipped, s.Denied, s.Selected, s.Deleted)
	}
}
