package pruner

import (
	"os"
	"slices"
	"testing"
	"time"

	v1 "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/JohanLindvall/crprune/internal/registry"
	"github.com/JohanLindvall/crprune/internal/registry/registrytest"
)

// Exercise the shipped rule against complete repositories: timestamps pick
// the latest build, while its tags, dependencies and referrers survive whole.
func TestPruneDeleteAllButLatestRule(t *testing.T) {
	raw, err := os.ReadFile("../../rules/delete_all_but_latest.json")
	if err != nil {
		t.Fatal(err)
	}
	rules := ruleSet(t, string(raw))
	for _, kind := range []string{"single image", "multi-arch index", "nested index"} {
		t.Run(kind, func(t *testing.T) {
			fake := registrytest.New()
			old := time.Now().Add(-7 * 24 * time.Hour)
			shared := fake.Add("app", registrytest.Image("shared-amd64"), registry.Attributes{
				Tags: []string{"shared-amd64"}, LastUpdated: old,
			})
			// A platform image and signature updated after the latest build
			// must still follow their old index rather than displace it.
			oldArm := fake.Add("app", registrytest.Image("old-arm64"), registry.Attributes{
				Tags: []string{"v9-arm64"}, LastUpdated: old.Add(4 * time.Hour),
			})
			oldIndex := fake.Add("app", registrytest.Index(
				registrytest.Child(shared, "linux", "amd64"),
				registrytest.Child(oldArm, "linux", "arm64"),
			), registry.Attributes{Tags: []string{"latest", "v9", "old-alias"}, LastUpdated: old})
			fake.Add("app", registrytest.Image("old-signature"), registry.Attributes{
				Tags: []string{schemeTag(oldIndex, ".sig")}, LastUpdated: old.Add(5 * time.Hour),
			})
			fake.Add("app", registrytest.Image("old-single"), registry.Attributes{
				Tags: []string{"v8", "old-single-alias"}, LastUpdated: old.Add(-time.Hour),
			})

			var kept []string
			latestDoc := registrytest.Image("latest")
			if kind != "single image" {
				arm := fake.Add("app", registrytest.Image("latest-arm64"), registry.Attributes{LastUpdated: old})
				latestDoc = registrytest.Index(
					registrytest.Child(shared, "linux", "amd64"),
					registrytest.Child(arm, "linux", "arm64"),
				)
				kept = append(kept, shared, arm)
				if kind == "nested index" {
					inner := fake.Add("app", latestDoc, registry.Attributes{
						Tags: []string{"v2-platforms"}, LastUpdated: old.Add(4 * time.Hour),
					})
					child := registrytest.Child(inner, "", "")
					child.MediaType, child.Platform = v1.MediaTypeImageIndex, nil
					latestDoc = registrytest.Index(child)
					kept = append(kept, inner)
				}
			}
			latestTags := []string{"v2", "stable", "sha-build"}
			latest := fake.Add("app", latestDoc, registry.Attributes{Tags: latestTags, LastUpdated: old.Add(time.Hour)})
			signature := fake.Add("app", registrytest.Image("latest-signature"), registry.Attributes{
				Tags: []string{schemeTag(latest, ".sig")}, LastUpdated: old.Add(2 * time.Hour),
			})
			kept = append(kept, latest, signature)

			// Newer untagged builds must not consume a slot or survive alone.
			fake.Add("app", registrytest.Image("untagged"), registry.Attributes{LastUpdated: old.Add(6 * time.Hour)})
			multiArch(fake, "app", "untagged-index", old.Add(7*time.Hour))
			// Ranking is per repository, even when all its images are newer.
			fake.Add("other", registrytest.Image("older"), registry.Attributes{
				Tags: []string{"v1"}, LastUpdated: old.Add(8 * time.Hour),
			})
			otherLatest := fake.Add("other", registrytest.Image("newer"), registry.Attributes{
				Tags: []string{"v2"}, LastUpdated: old.Add(9 * time.Hour),
			})

			if err := fakePruner(t, fake).Prune(t.Context(), rules); err != nil {
				t.Fatal(err)
			}
			if got, want := fake.Digests("app"), sorted(kept...); !slices.Equal(got, want) {
				t.Errorf("app = %v, want the latest build with its dependencies and signature: %v", got, want)
			}
			if got := fake.Digests("other"); !slices.Equal(got, []string{otherLatest}) {
				t.Errorf("other = %v, want its own latest image %s", got, otherLatest)
			}
			if err := fake.ListManifests(t.Context(), "app", func(attrs registry.Attributes) error {
				if attrs.Digest == latest && !slices.Equal(attrs.Tags, latestTags) {
					t.Errorf("latest tags = %v, want all aliases %v", attrs.Tags, latestTags)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}

	t.Run("no tagged image", func(t *testing.T) {
		fake := registrytest.New()
		old := time.Now().Add(-7 * 24 * time.Hour)
		multiArch(fake, "app", "untagged", old)
		fake.Add("app", registrytest.Image("untagged-single"), registry.Attributes{LastUpdated: old})
		if err := fakePruner(t, fake).Prune(t.Context(), rules); err != nil {
			t.Fatal(err)
		}
		if got := fake.DeletedRepositories(); !slices.Equal(got, []string{"app"}) {
			t.Errorf("deleted repositories = %v, want app", got)
		}
	})
}
