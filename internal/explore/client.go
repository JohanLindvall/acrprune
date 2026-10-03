// Package explore implements the statistics explorer for snapshots and live
// registries. Registry changes always go through a reviewed pruning plan.
package explore

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/JohanLindvall/crprune/internal/imageref"
	"github.com/JohanLindvall/crprune/internal/progress"
	"github.com/JohanLindvall/crprune/internal/pruner"
	"github.com/JohanLindvall/crprune/internal/registry"
	"github.com/JohanLindvall/crprune/internal/rules"
)

// Client connects only when a live action is requested. write asks the
// connector to check deletion credentials, without itself making any changes.
type Client struct {
	Registry      string
	Connect       func(ctx context.Context, logger *slog.Logger, write bool) (*registry.Registry, error)
	Rules         []*rules.RepoRule
	RuleSource    string
	KeepYounger   time.Duration
	IncludeLocked bool
	Protect       []*rules.RepoRule
}

type request struct {
	kind         string // repositories, manifests, or rules
	repositories []string
	digests      []string
}

func (c *Client) statistics(ctx context.Context, logger *slog.Logger) ([]pruner.RepositoryStats, error) {
	reg, err := c.Connect(ctx, logger, false)
	if err != nil {
		return nil, err
	}
	return pruner.CollectRegistryStats(ctx, reg, c.Protect, nil)
}

func (c *Client) manifests(ctx context.Context, logger *slog.Logger, repository string) ([]*registry.Manifest, error) {
	reg, err := c.Connect(ctx, logger, false)
	if err != nil {
		return nil, err
	}
	progress.Report(ctx, progress.Event{Kind: progress.Repository, Name: repository})
	contents, _, err := reg.FetchRepositoryManifests(ctx, repository, registry.FetchOptions{Platforms: true})
	if err != nil {
		return nil, err
	}
	all := slices.Collect(maps.Values(contents.Manifests))
	if err := reg.LoadTagLocks(ctx, repository, all); err != nil {
		return nil, err
	}
	slices.SortFunc(all, func(a, b *registry.Manifest) int { return strings.Compare(a.Digest, b.Digest) })
	return all, nil
}

func (c *Client) prepare(ctx context.Context, logger *slog.Logger, req request) (*pruner.DeletionPlan, error) {
	if len(req.repositories) == 0 {
		return nil, fmt.Errorf("no repositories selected")
	}
	for _, name := range req.repositories {
		if !imageref.ValidRepository(name) {
			return nil, fmt.Errorf("invalid repository %q", name)
		}
	}
	// Validate actions before asking for credentials or making any requests.
	switch req.kind {
	case "rules":
		if len(c.Rules) == 0 && c.RuleSource == "" {
			return nil, fmt.Errorf("no rules selected; press l to choose rules, L to load a rule file, or pass --rules FILE")
		}
		for _, warning := range rules.Warnings(c.Rules) {
			logger.Warn("Rule never applies", "file", c.RuleSource, "detail", warning)
		}
	case "manifests":
		if len(req.repositories) != 1 || len(req.digests) == 0 {
			return nil, fmt.Errorf("select manifests in one repository")
		}
	case "repositories":
	default:
		return nil, fmt.Errorf("unknown action %q", req.kind)
	}
	reg, err := c.Connect(ctx, logger, true)
	if err != nil {
		return nil, err
	}
	p := &pruner.Pruner{Registry: reg, Logger: logger, KeepYounger: c.KeepYounger, IncludeLocked: c.IncludeLocked, Protect: c.Protect}
	var ruleSet []*rules.RepoRule
	switch req.kind {
	case "rules":
		ruleSet = scopedRules(c.Rules, req.repositories)
	case "repositories":
		for _, name := range req.repositories {
			ruleSet = append(ruleSet, deletionRule(name))
		}
	case "manifests":
		return p.PrepareManifests(ctx, req.repositories[0], req.digests)
	}
	return p.Prepare(ctx, ruleSet)
}

func deletionRule(name string) *rules.RepoRule {
	common := rules.CommonRule{Keep: false}
	return &rules.RepoRule{Repo: regexp.MustCompile("^" + regexp.QuoteMeta(name) + "$"),
		MustDeleteEverything: true,
		Tagged:               []rules.TaggedRule{{CommonRule: common}}, Untagged: []rules.UntaggedRule{{CommonRule: common}}}
}

// scopedRules preserves first-match semantics and the original repo patterns:
// a rule that does not match the explored repo never gets forced onto it.
// Literal clones prevent a broad pattern from reaching outside the snapshot.
func scopedRules(ruleSet []*rules.RepoRule, repositories []string) []*rules.RepoRule {
	var scoped []*rules.RepoRule
	matcher := rules.NewRepositoryMatcher(ruleSet)
	for _, name := range slices.Compact(slices.Sorted(slices.Values(repositories))) {
		if rule := matcher.Match(name); rule != nil {
			clone := *rule
			clone.Repo = regexp.MustCompile("^" + regexp.QuoteMeta(name) + "$")
			scoped = append(scoped, &clone)
		}
	}
	return scoped
}
