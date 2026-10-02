package pruner

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/JohanLindvall/crprune/internal/registry"
	"github.com/JohanLindvall/crprune/internal/registry/registrytest"
)

func TestPreparedPlanIsReadOnlyDetachedAndSingleUse(t *testing.T) {
	b := registrytest.New()
	old := time.Now().Add(-48 * time.Hour)
	keep := b.Add("app", registrytest.Image("keep"), registry.Attributes{Tags: []string{"keep"}, LastUpdated: old})
	remove := b.Add("app", registrytest.Image("remove"), registry.Attributes{Tags: []string{"old", "alias"}, LastUpdated: old})
	p := fakePruner(t, b)
	r := ruleSet(t, `[{"repo":"^app$","tagged":[{"tag":"^keep$","keep":true},{"keep":false}]}]`)
	plan, err := p.Prepare(t.Context(), r)
	if err != nil {
		t.Fatal(err)
	}
	if b.Calls("DeleteManifest")+b.Calls("DeleteRepository")+b.Calls("UnlockManifest") != 0 {
		t.Fatal("preview changed registry")
	}
	targets := plan.Targets()
	if len(targets) != 1 || targets[0].Digest != remove || targets[0].WholeRepository || len(targets[0].Tags) != 2 {
		t.Fatalf("targets: %+v", targets)
	}
	targets[0].Tags[0] = "tampered"
	targets[0].Digest = keep
	// Mutating the original rules or pruner cannot widen the reviewed plan.
	r[0].Tagged = nil
	p.IncludeLocked = true
	s, err := plan.Execute(t.Context())
	if err != nil || s.DeletedManifests != 1 || s.KeptManifests != 1 {
		t.Fatalf("result=%+v err=%v", s, err)
	}
	if got := b.Deleted(); !slices.Equal(got, refs("app", remove)) {
		t.Fatalf("deleted %v", got)
	}
	if _, err := plan.Execute(t.Context()); err == nil {
		t.Fatal("plan executed twice")
	}
}

func TestPlanRechecksEveryRepositoryBeforeAnyMutation(t *testing.T) {
	for _, change := range []string{"new image", "retag", "updated", "manifest lock", "tag lock"} {
		t.Run(change, func(t *testing.T) {
			b := registrytest.New()
			old := time.Now().Add(-48 * time.Hour)
			for _, repo := range []string{"a", "b"} {
				b.Add(repo, registrytest.Image(repo), registry.Attributes{Tags: []string{"v1"}, LastUpdated: old})
			}
			p := fakePruner(t, b)
			plan, err := p.Prepare(t.Context(), ruleSet(t, `[{"repo":".+","tagged":[{"keep":false}]}]`))
			if err != nil {
				t.Fatal(err)
			}
			attrs := registry.Attributes{Tags: []string{"v1"}, LastUpdated: old}
			switch change {
			case "new image":
				b.Add("b", registrytest.Image("new"), attrs)
			case "retag":
				attrs.Tags = []string{"promoted"}
				b.Add("b", registrytest.Image("b"), attrs)
			case "updated":
				attrs.LastUpdated = time.Now()
				b.Add("b", registrytest.Image("b"), attrs)
			case "manifest lock":
				attrs.Locked = true
				b.Add("b", registrytest.Image("b"), attrs)
			case "tag lock":
				b.LockTag("b", "v1")
			}
			_, err = plan.Execute(t.Context())
			if !errors.Is(err, registry.ErrRepositoryChanged) {
				t.Fatalf("error=%v", err)
			}
			if b.Calls("DeleteManifest")+b.Calls("DeleteRepository") != 0 {
				t.Fatal("deleted before validating all previews")
			}
		})
	}
}

func TestPlanHonorsProtectionsAndReportsPartialSuccess(t *testing.T) {
	b := registrytest.New()
	old := time.Now().Add(-48 * time.Hour)
	b.Add("recent", registrytest.Image("recent"), registry.Attributes{LastUpdated: time.Now()})
	b.Add("locked", registrytest.Image("locked"), registry.Attributes{LastUpdated: old, Locked: true})
	for _, repo := range []string{"a", "b"} {
		b.Add(repo, registrytest.Image(repo), registry.Attributes{LastUpdated: old})
	}
	p := fakePruner(t, b)
	p.KeepYounger = 24 * time.Hour
	plan, err := p.Prepare(t.Context(), ruleSet(t, `[{"repo":".+","untagged":[{"keep":false}]}]`))
	if err != nil {
		t.Fatal(err)
	}
	if s := plan.Summary(); s.DeletedManifests != 2 || s.Repositories != 2 {
		t.Fatalf("summary=%+v", s)
	}
	b.Fail = func(op, repo string) error {
		if op == "DeleteRepository" && repo == "b" {
			return registrytest.Forbidden(repo)
		}
		return nil
	}
	s, err := plan.Execute(t.Context())
	if err == nil || s.DeletedManifests != 1 || s.Repositories-s.KeptRepositories != 1 {
		t.Fatalf("partial=%+v err=%v", s, err)
	}
	if !slices.Equal(b.DeletedRepositories(), []string{"a"}) {
		t.Fatal(b.DeletedRepositories())
	}
}

func TestPlanCanceledBeforeExecutionMakesNoChanges(t *testing.T) {
	b := registrytest.New()
	b.Add("app", registrytest.Image("a"), registry.Attributes{LastUpdated: time.Now().Add(-48 * time.Hour)})
	p := fakePruner(t, b)
	plan, err := p.Prepare(t.Context(), ruleSet(t, `[{"repo":".+","untagged":[{"keep":false}]}]`))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err = plan.Execute(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if b.Calls("DeleteManifest")+b.Calls("DeleteRepository") != 0 {
		t.Fatal("canceled plan mutated registry")
	}
}
