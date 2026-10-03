package pruner

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/JohanLindvall/crprune/internal/registry"
	"github.com/JohanLindvall/crprune/internal/registry/registrytest"
	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestPrepareManifestsRejectsIncompleteSelection(t *testing.T) {
	for _, scenario := range []string{"empty", "invalid repository", "invalid digest", "missing selection", "missing dependency", "missing document", "canceled"} {
		t.Run(scenario, func(t *testing.T) {
			b := registrytest.New()
			attrs := registry.Attributes{LastUpdated: time.Now().Add(-48 * time.Hour)}
			selected := b.Add("app", registrytest.Image("selected"), attrs)
			absent := "sha256:" + strings.Repeat("a", 64)
			repo, values := "app", []string{selected}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			switch scenario {
			case "empty":
				values = nil
			case "invalid repository":
				repo = "../app"
			case "invalid digest":
				values = []string{"not a digest"}
			case "missing selection":
				values = []string{absent}
			case "missing dependency":
				values = []string{b.Add("app", registrytest.Index(registrytest.Child(absent, "linux", "amd64")), attrs)}
			case "missing document":
				attrs.Digest = absent
				b.AddMissing("app", attrs)
			case "canceled":
				cancel()
			}
			plan, err := fakePruner(t, b).PrepareManifests(ctx, repo, values)
			if err == nil || plan != nil {
				t.Fatalf("incomplete selection produced a plan: %v, %v", plan, err)
			}
			if b.Calls("DeleteManifest")+b.Calls("DeleteRepository") != 0 {
				t.Fatal("preview mutated the registry")
			}
		})
	}
}

func TestPrepareManifestsIncludesReferrerIndexChildren(t *testing.T) {
	b := registrytest.New()
	attrs := registry.Attributes{LastUpdated: time.Now().Add(-48 * time.Hour)}
	root := b.Add("app", registrytest.Image("root"), attrs)
	child := b.Add("app", registrytest.Image("signature-child"), attrs)
	index := registrytest.Index(registrytest.Child(child, "unknown", "unknown"))
	index.Subject = &v1.Descriptor{Digest: digest.Digest(root)}
	referrer := b.Add("app", index, attrs)
	signature := registrytest.Image("nested-signature")
	signature.Subject = &v1.Descriptor{Digest: digest.Digest(referrer)}
	nested := b.Add("app", signature, attrs)
	keep := b.Add("app", registrytest.Image("unrelated"), attrs)
	p := fakePruner(t, b)
	plan, err := p.PrepareManifests(t.Context(), "app", []string{root, root})
	if err != nil {
		t.Fatal(err)
	}
	if targets := plan.Targets(); len(targets) != 4 {
		t.Fatalf("incomplete or duplicate targets: %+v", targets)
	}
	if _, err := plan.Execute(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got, want := b.Deleted(), refs("app", root, child, referrer, nested); !slices.Equal(got, want) {
		t.Fatalf("deleted %v, want %v", got, want)
	}
	// The caller's pruner must remain reusable without the manual scope.
	plan, err = p.Prepare(t.Context(), ruleSet(t, `[{"repo":"^app$","untagged":[{"keep":false}]}]`))
	if err != nil || len(plan.Targets()) != 1 || plan.Targets()[0].Digest != keep {
		t.Fatalf("selection leaked into a subsequent preview: %v, %v", plan, err)
	}
}

func TestPrepareManifestsHonorsProtections(t *testing.T) {
	for _, protection := range []string{"recent", "unknown age", "manifest lock", "tag lock", "running", "kept subject"} {
		t.Run(protection, func(t *testing.T) {
			b := registrytest.New()
			p := fakePruner(t, b)
			p.KeepYounger = 24 * time.Hour
			attrs := registry.Attributes{LastUpdated: time.Now().Add(-48 * time.Hour), Tags: []string{"v1"}}
			switch protection {
			case "recent":
				attrs.LastUpdated = time.Now()
			case "unknown age":
				attrs.LastUpdated = time.Time{}
			case "manifest lock":
				attrs.Locked = true
			case "tag lock":
				b.LockTag("app", "v1")
			case "running":
				p.Protect = generated(t, "myreg.azurecr.io/app:v1")
			}
			selected := b.Add("app", registrytest.Image("selected"), attrs)
			if protection == "kept subject" {
				sig := registrytest.Image("signature")
				sig.Subject = &v1.Descriptor{Digest: digest.Digest(selected)}
				attrs.Tags = nil
				selected = b.Add("app", sig, attrs)
			}
			plan, err := p.PrepareManifests(t.Context(), "app", []string{selected})
			if err != nil || len(plan.Targets()) != 0 {
				t.Fatalf("protected selection produced targets: %v, %v", plan, err)
			}
		})
	}
}
