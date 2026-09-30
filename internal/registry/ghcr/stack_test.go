package ghcr

import (
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/JohanLindvall/acrprune/internal/pruner"
	"github.com/JohanLindvall/acrprune/internal/registry"
	"github.com/JohanLindvall/acrprune/internal/rules"
)

// The tests in this file run the pruner against the fake GitHub, end to end.

func stackPruner(t *testing.T, b *Backend, dryRun bool) *pruner.Pruner {
	t.Helper()
	logger := slog.New(slog.DiscardHandler)
	b.logger = logger
	reg, err := registry.New(b, logger, 4, registry.NewCache(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	return &pruner.Pruner{Registry: reg, Logger: logger, DryRun: dryRun, KeepYounger: 24 * time.Hour}
}

func compile(t *testing.T, doc string) []*rules.RepoRule {
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

func imageDoc(name, config string) map[string]any {
	return map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.manifest.v1+json",
		"config":        map[string]any{"mediaType": "application/vnd.oci.image.config.v1+json", "digest": config, "size": 50},
		"layers": []any{
			map[string]any{"mediaType": "application/vnd.oci.image.layer.v1.tar+gzip", "digest": "sha256:" + strings.Repeat("1", 64), "size": 1000},
		},
		"annotations": map[string]string{"name": name},
	}
}

func indexDoc(children ...map[string]any) map[string]any {
	return map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.index.v1+json",
		"manifests":     children,
	}
}

func child(digest, os, arch string) map[string]any {
	return map[string]any{
		"mediaType": "application/vnd.oci.image.manifest.v1+json",
		"digest":    digest,
		"size":      500,
		"platform":  map[string]string{"os": os, "architecture": arch},
	}
}

// TestPruneGHCR: GHCR lists the platform images and attestations of a
// multi-platform push as untagged package versions. Cleaning up untagged
// versions must not break the image, which is what naive untagged cleanup on
// GHCR does.
func TestPruneGHCR(t *testing.T) {
	f, b := newFakeGitHub(t, true)
	now := time.Now()
	old := now.Add(-30 * 24 * time.Hour)
	config := f.pushBlob([]byte(`{"architecture":"amd64","os":"linux"}`))

	amd := f.push("app", imageDoc("amd", config), old)
	arm := f.push("app", imageDoc("arm", config), old)
	attestation := f.push("app", imageDoc("attestation", config), old)
	release := f.push("app", indexDoc(
		child(amd, "linux", "amd64"),
		child(arm, "linux", "arm64"),
		child(attestation, "unknown", "unknown"),
	), old, "1.0", "latest")
	oldFeature := f.push("app", imageDoc("old-feature", config), old, "feature-old")
	newFeature := f.push("app", imageDoc("new-feature", config), now.Add(-time.Hour), "feature-new")
	leftover := f.push("app", imageDoc("leftover", config), old)
	deleted := []string{f.versionID("app", oldFeature), f.versionID("app", leftover)}
	ruleSet := compile(t, `[{
		"repo": ".+",
		"untagged": [{"match_older": "24h", "keep": false}],
		"tagged": [{"tag": "^feature-", "match_older": "14d", "keep": false}]
	}]`)

	if err := stackPruner(t, b, true).Prune(ctx, ruleSet); err != nil {
		t.Fatal(err)
	}
	if got := f.requested("DELETE"); len(got) != 0 {
		t.Fatalf("a dry run deleted %v", got)
	}

	if err := stackPruner(t, b, false).Prune(ctx, ruleSet); err != nil {
		t.Fatal(err)
	}
	want := []string{release, amd, arm, attestation, newFeature}
	slices.Sort(want)
	if got := f.remaining("app"); !slices.Equal(got, want) {
		t.Errorf("remaining = %v, want the release with its platform images and attestation, and the fresh feature image", got)
	}
	var wantDeletes []string
	for _, id := range deleted {
		wantDeletes = append(wantDeletes, "DELETE /orgs/"+testOwner+"/packages/container/app/versions/"+id)
	}
	slices.Sort(wantDeletes)
	got := f.requested("DELETE")
	slices.Sort(got)
	if !slices.Equal(got, wantDeletes) {
		t.Errorf("deletes = %v, want the old feature image and the leftover: %v", got, wantDeletes)
	}
}

// TestPruneGHCRKeepsLastTag: rules keeping only an untagged, digest-pinned
// image would have GHCR refuse to delete the last tagged version; the newest
// tagged image is kept instead, and the run succeeds.
func TestPruneGHCRKeepsLastTag(t *testing.T) {
	f, b := newFakeGitHub(t, false)
	old := time.Now().Add(-10 * 24 * time.Hour)
	config := f.pushBlob([]byte(`{"architecture":"amd64","os":"linux"}`))
	pinned := f.push("app", imageDoc("pinned", config), old.Add(-time.Hour))
	f.push("app", imageDoc("v1", config), old.Add(-time.Hour), "v1")
	v2 := f.push("app", imageDoc("v2", config), old, "v2")

	ruleSet := compile(t, `[{"repo": "^app$",
		"untagged": [{"digest": "^`+pinned+`$", "keep": true}, {"keep": false}],
		"tagged": [{"keep": false}]}]`)
	if err := stackPruner(t, b, false).Prune(ctx, ruleSet); err != nil {
		t.Fatalf("prune failed: %v", err)
	}
	want := []string{pinned, v2}
	slices.Sort(want)
	if got := f.remaining("app"); !slices.Equal(got, want) {
		t.Errorf("remaining = %v, want the pinned image and the newest tagged one", got)
	}
}

// TestPruneGHCRLeavesUnservedVersions: the REST API and ghcr.io are separate
// services. A version the one lists but the other cannot serve was never
// judged by the rules, so the package is not deleted around it even though
// the rules keep nothing else.
func TestPruneGHCRLeavesUnservedVersions(t *testing.T) {
	f, b := newFakeGitHub(t, true)
	old := time.Now().Add(-10 * 24 * time.Hour)
	config := f.pushBlob([]byte(`{"architecture":"amd64","os":"linux"}`))
	f.pushUnserved("app", old, "latest")
	f.push("app", imageDoc("a", config), old)
	f.push("app", imageDoc("b", config), old)
	want := f.remaining("app")

	ruleSet := compile(t, `[{"repo": "^app$", "untagged": [{"keep": false}]}]`)
	if err := stackPruner(t, b, false).Prune(ctx, ruleSet); err != nil {
		t.Fatal(err)
	}
	if !f.hasPackage("app") {
		t.Fatal("the package was deleted with a version nobody judged")
	}
	if got := f.remaining("app"); !slices.Equal(got, want) {
		t.Errorf("remaining = %v, want the unserved version and its possible dependencies", got)
	}
}

// TestPruneGHCRByPlatform: GHCR's listings carry no platform, so a rule on
// architecture reads single-platform images' configs, which ghcr.io serves
// through a redirect to blob storage; a repository keeping nothing is deleted
// as a whole package.
func TestPruneGHCRByPlatform(t *testing.T) {
	f, b := newFakeGitHub(t, true)
	old := time.Now().Add(-10 * 24 * time.Hour)
	amdConfig := f.pushBlob([]byte(`{"architecture":"amd64","os":"linux"}`))
	armConfig := f.pushBlob([]byte(`{"architecture":"arm64","os":"linux"}`))
	f.push("amd-only", imageDoc("amd", amdConfig), old, "v1")
	f.push("amd-only", imageDoc("amd2", amdConfig), old, "v2")
	arm := f.push("team/arm", imageDoc("arm", armConfig), old, "v1")

	ruleSet := compile(t, `[{"repo": ".+", "must_delete_everything": true,
		"untagged": [{"match_older": "24h", "keep": false}],
		"tagged": [{"arch": "arm64", "keep": true}, {"arch": "amd64", "keep": false}]}]`)
	if err := stackPruner(t, b, false).Prune(ctx, ruleSet); err != nil {
		t.Fatal(err)
	}
	if f.hasPackage("amd-only") {
		t.Error("the amd64-only package should be deleted")
	}
	if got := f.requested("DELETE /orgs/" + testOwner + "/packages/container/amd-only"); len(got) != 1 {
		t.Errorf("package deletes = %v, want the whole package deleted once", f.requested("DELETE"))
	}
	if got := f.remaining("team/arm"); !slices.Equal(got, []string{arm}) {
		t.Errorf("team/arm = %v, want it kept", got)
	}
	// Both amd64 images share one config: two configs downloaded in all.
	if got := f.requested("GET /cdn/"); len(got) != 2 {
		t.Errorf("config downloads = %v, want one per distinct config", got)
	}
}
