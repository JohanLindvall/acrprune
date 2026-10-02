package explore

import (
	"context"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/JohanLindvall/crprune/internal/registry"
	"github.com/JohanLindvall/crprune/internal/registry/registrytest"
	"github.com/JohanLindvall/crprune/internal/rules"
)

func testClient(b *registrytest.Backend) *Client {
	return &Client{Registry: "ghcr.io/test", KeepYounger: 24 * time.Hour, Connect: func(_ context.Context, l *slog.Logger, _ bool) (*registry.Registry, error) {
		return registry.New(b, l, 4, nil)
	}}
}
func compileRules(t *testing.T, doc string) []*rules.RepoRule {
	t.Helper()
	specs, err := rules.ParseSpecs(strings.NewReader(doc))
	if err != nil {
		t.Fatal(err)
	}
	r, err := rules.Compile(specs)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func quiet() *slog.Logger { return slog.New(slog.DiscardHandler) }

func TestScopedRulesPreservePatternsAndFirstMatch(t *testing.T) {
	r := compileRules(t, `[{"repo":"^app$","tagged":[{"keep":true}]},{"repo":"^app","tagged":[{"keep":false}]}]`)
	s := scopedRules(r, []string{"app.extra", "other", "app", "app"})
	if len(s) != 2 || !s[0].Tagged[0].Keep || s[1].Tagged[0].Keep {
		t.Fatalf("rules=%+v", s)
	}
	if s[1].Repo.MatchString("appXextra") || s[1].Repo.MatchString("app.extra/sub") {
		t.Fatal("scope regex escaped literal name")
	}
	if r[1].Repo.String() != "^app" {
		t.Fatal("original rule mutated")
	}
}

func TestRulePreviewCannotReachOutsideSnapshot(t *testing.T) {
	b := registrytest.New()
	for _, name := range []string{"one", "two", "outside"} {
		b.Add(name, registrytest.Image(name), registry.Attributes{LastUpdated: time.Now().Add(-48 * time.Hour)})
	}
	c := testClient(b)
	c.Rules = compileRules(t, `[{"repo":".+","untagged":[{"keep":false}]}]`)
	plan, err := c.prepare(t.Context(), quiet(), request{kind: "rules", repositories: []string{"one", "two"}})
	if err != nil {
		t.Fatal(err)
	}
	if b.Calls("ListRepositories") != 0 {
		t.Fatal("snapshot scope listed live registry")
	}
	if _, err := plan.Execute(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(b.DeletedRepositories(), []string{"one", "two"}) {
		t.Fatal(b.DeletedRepositories())
	}
}

func TestImageSelectionIncludesChildrenButRetainsSharedDependency(t *testing.T) {
	b := registrytest.New()
	old := time.Now().Add(-48 * time.Hour)
	attrs := registry.Attributes{LastUpdated: old}
	shared := b.Add("app", registrytest.Image("shared"), attrs)
	only := b.Add("app", registrytest.Image("only"), attrs)
	remove := b.Add("app", registrytest.Index(registrytest.Child(shared, "linux", "amd64"), registrytest.Child(only, "linux", "arm64")), registry.Attributes{LastUpdated: old, Tags: []string{"old", "alias"}})
	b.Add("app", registrytest.Index(registrytest.Child(shared, "linux", "amd64")), registry.Attributes{LastUpdated: old, Tags: []string{"keep"}})
	c := testClient(b)
	plan, err := c.prepare(t.Context(), quiet(), request{kind: "manifests", repositories: []string{"app"}, digests: []string{remove}})
	if err != nil {
		t.Fatal(err)
	}
	var digests []string
	for _, v := range plan.Targets() {
		digests = append(digests, v.Digest)
	}
	if !slices.Contains(digests, remove) || !slices.Contains(digests, only) || slices.Contains(digests, shared) {
		t.Fatal(digests)
	}
	if _, err := plan.Execute(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(b.Digests("app")) != 2 {
		t.Fatal(b.Digests("app"))
	}
}
