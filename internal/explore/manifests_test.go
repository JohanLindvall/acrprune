package explore

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/JohanLindvall/crprune/internal/registry"
	"github.com/JohanLindvall/crprune/internal/registry/registrytest"
	"github.com/gdamore/tcell/v2"
)

func mixedImages() (*registrytest.Backend, map[string]string) {
	b := registrytest.New()
	ids := map[string]string{}
	old := time.Now().Add(-48 * time.Hour)
	config := b.PutConfig("amd64", "linux")
	for _, name := range []string{"tagged", "untagged"} {
		doc := registrytest.Image(name)
		doc.Config = &config
		attrs := registry.Attributes{LastUpdated: old}
		if name == "tagged" {
			attrs.Tags = []string{"standalone", "stable"}
		}
		ids[name] = b.Add("one", doc, attrs)
	}
	for _, arch := range []string{"amd64", "arm64"} {
		ids[arch] = b.Add("one", registrytest.Image(arch), registry.Attributes{LastUpdated: old})
	}
	arm := registrytest.Child(ids["arm64"], "linux", "arm64")
	arm.Platform.Variant = "v8"
	ids["index"] = b.Add("one", registrytest.Index(registrytest.Child(ids["amd64"], "linux", "amd64"), arm), registry.Attributes{Tags: []string{"latest", "v2"}, LastUpdated: old})
	b.LockTag("one", "latest")
	referrer := registrytest.Image("attestation")
	referrer.Config = nil
	referrer.ArtifactType = "application/vnd.in-toto+json"
	subject := registrytest.Child(ids["index"], "", "")
	referrer.Subject = &subject
	ids["referrer"] = b.Add("one", referrer, registry.Attributes{LastUpdated: old})
	return b, ids
}

func TestManifestBrowserLoadsAllImagesAndPlatformsWithReadAccess(t *testing.T) {
	b, ids := mixedImages()
	c := testClient(b)
	c.Connect = func(_ context.Context, logger *slog.Logger, write bool) (*registry.Registry, error) {
		if write {
			t.Fatal("browsing requested deletion credentials")
		}
		return registry.New(b, logger, 4, nil)
	}
	images, err := c.manifests(t.Context(), quiet(), "one")
	if err != nil || len(images) != len(ids) {
		t.Fatal(images, err)
	}
	for _, m := range images {
		if m.Digest != ids["referrer"] && !strings.Contains(manifestPlatforms(m), "linux/") {
			t.Fatal("image platform not loaded", m)
		}
		if m.Digest == ids["index"] && (manifestKind(m) != "multiarch" || !m.IsLocked()) {
			t.Fatal("lock hid the multiarch type or was not loaded", m)
		}
	}
	if b.Calls("GetBlob") != 1 {
		t.Fatal("standalone images did not resolve their shared config once", b.Calls("GetBlob"))
	}
	if len(b.Deleted()) != 0 || len(b.DeletedRepositories()) != 0 {
		t.Fatal("browsing deleted images")
	}
}

func TestEnterOpensRepositoryImagesAndDetails(t *testing.T) {
	b, ids := mixedImages()
	a := newApp(sampleStats(), Options{Client: testClient(b), Filter: "one"})
	a.repos.marked["one"] = true
	press(t, a, tcell.KeyEnter, 0)
	if r := finishJob(t, a); r.err != nil {
		t.Fatal(r.err)
	}
	if a.repository != "one" || a.panel != "" || len(a.manifests.data) != len(ids) {
		t.Fatal("Enter did not open all repository images", a.repository, a.panel, a.manifests.data)
	}
	// Search platforms, image kinds, and tag status without dropping index
	// children or artifacts from the full repository view.
	for query, want := range map[string][]string{
		"multiarch": {ids["index"]},
		"arm64":     {ids["index"], ids["arm64"]},
		"untagged":  {ids["untagged"], ids["amd64"], ids["arm64"], ids["referrer"]},
		"referrer":  {ids["referrer"]},
		"stable":    {ids["tagged"]},
	} {
		a.manifests.query = query
		a.manifests.refilter("")
		var got []string
		for _, row := range a.manifests.visible {
			got = append(got, row.Name)
		}
		slices.Sort(got)
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Fatalf("search %q: got %v want %v", query, got, want)
		}
	}
	a.manifests.query = ""
	for _, tt := range []struct {
		image string
		parts []string
	}{
		{"index", []string{"Type: multiarch", "Tags: latest, v2", "linux/amd64", "linux/arm64", "linux/arm64/v8", "Locked: true", "Locked tags: latest", "INDEX CHILDREN (2)", ids["amd64"], ids["arm64"], "Own referenced size:"}},
		{"arm64", []string{"Tags: (untagged)", "PARENT INDEXES (1)", ids["index"], "Tags: latest, v2"}},
		{"referrer", []string{"Type: referrer", "Subject: " + ids["index"], "Artifact type: application/vnd.in-toto+json"}},
	} {
		a.manifests.refilter(ids[tt.image])
		press(t, a, tcell.KeyEnter, 0)
		message := strings.Join(a.message, "\n")
		for _, part := range tt.parts {
			if !strings.Contains(message, part) {
				t.Fatalf("%s details missing %q:\n%s", tt.image, part, message)
			}
		}
		press(t, a, tcell.KeyEsc, 0)
		if a.repository != "one" || a.manifests.current() != ids[tt.image] {
			t.Fatal("closing details lost the image selection")
		}
	}
	press(t, a, tcell.KeyRune, 'i')
	if a.panel != "repository-details" || !strings.Contains(strings.Join(a.message, " "), "Exact bytes:") {
		t.Fatal("repository statistics are unavailable")
	}
	press(t, a, tcell.KeyEsc, 0)
	press(t, a, tcell.KeyEsc, 0)
	if a.repository != "" || a.repos.query != "one" || !a.repos.marked["one"] || a.repos.current() != "one" {
		t.Fatal("returning to repositories lost the view or selection")
	}
	press(t, a, tcell.KeyRune, 'i')
	press(t, a, tcell.KeyRune, 'm')
	if r := finishJob(t, a); r.err != nil || a.repository != "one" || a.panel != "" {
		t.Fatal("could not open images from repository statistics", r.err)
	}
}

func TestLocalRepositoryDetailsExplainRegistryRequirement(t *testing.T) {
	a := newApp(sampleStats(), Options{})
	press(t, a, tcell.KeyEnter, 0)
	if a.job != nil || a.panel != "repository-details" || !strings.Contains(strings.Join(a.message, " "), "--registry") {
		t.Fatal("local details require network access or do not explain how to browse images")
	}
}

func TestIndexDetailsHandleRepeatedAndUnavailableChildren(t *testing.T) {
	child := &registry.Manifest{Attributes: registry.Attributes{Digest: "child", Architecture: "amd64", OS: "linux"}}
	index := &registry.Manifest{Attributes: registry.Attributes{Digest: "index", Locked: true}, OCIManifest: registrytest.Index(
		registrytest.Child("child", "linux", "amd64"),
		registrytest.Child("child", "linux", "amd64"),
		registrytest.Child("missing-attestation", "unknown", "unknown"),
	)}
	a := newApp(sampleStats(), Options{})
	a.finish(result{kind: "manifests", repository: "one", manifests: []*registry.Manifest{index, child}})
	if len(a.parents["child"]) != 1 || manifestKind(index) != "index" {
		t.Fatal("repeated children or unknown platforms changed the index classification")
	}
	if details := strings.Join(a.manifestDetails(index), "\n"); !strings.Contains(details, "Not available in the repository listing") {
		t.Fatal("unavailable child was not identified", details)
	}
	if kind := manifestKind(&registry.Manifest{OCIManifest: registrytest.Index()}); kind != "index" {
		t.Fatal("empty index treated as an image", kind)
	}
}

func TestRepositoryImageViewDrawsAtDifferentSizes(t *testing.T) {
	b, ids := mixedImages()
	images, err := testClient(b).manifests(t.Context(), quiet(), "one")
	if err != nil {
		t.Fatal(err)
	}
	for _, size := range [][2]int{{140, 34}, {80, 24}, {60, 18}, {40, 10}, {1, 1}, {0, 0}} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			s := screenFor(t, size[0], size[1])
			a := newApp(sampleStats(), Options{})
			a.finish(result{kind: "manifests", repository: "one", manifests: images})
			a.manifests.refilter(ids["index"])
			a.draw(s, time.Now())
			if size[0] >= 80 {
				text := screenText(s)
				for _, part := range []string{"REPOSITORY IMAGES", "TAGGED / UNTAGGED", "2 / 4", "multiarch", "index child", "linux/arm64", "referrer", "untagged", "locked"} {
					if !strings.Contains(text, part) {
						t.Fatalf("missing %q:\n%s", part, text)
					}
				}
				if size[0] == 80 {
					t.Log("\n" + text)
				}
			}
			press(t, a, tcell.KeyEnter, 0)
			a.draw(s, time.Now())
			press(t, a, tcell.KeyEnd, 0)
			a.draw(s, time.Now())
		})
	}
}
