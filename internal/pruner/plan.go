package pruner

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"sync/atomic"

	"github.com/JohanLindvall/crprune/internal/progress"
	"github.com/JohanLindvall/crprune/internal/registry"
	"github.com/JohanLindvall/crprune/internal/rules"
)

// DeletionPlan holds an immutable decision for review. It can be executed once,
// against the same registry, after rechecking every repository it will touch.
// Its manifests and deletion options are private so the review cannot drift.
type DeletionPlan struct {
	registry     *registry.Registry
	options      registry.DeleteOptions
	repositories []repositoryDeletion
	used         atomic.Bool
}

type repositoryDeletion struct {
	name                 string
	all, selected        []*registry.Manifest
	whole                bool
	seenBytes, keptBytes uint64
}

// DeletionTarget identifies a reviewed deletion. Tags includes every tag that
// deleting this manifest removes; WholeRepository also removes the repository.
type DeletionTarget struct {
	Repository, Digest string
	Tags               []string
	WholeRepository    bool
	Unlock             bool
}

// Targets returns a detached list, in repository and digest order.
func (p *DeletionPlan) Targets() []DeletionTarget {
	var targets []DeletionTarget
	for _, repo := range p.repositories {
		for _, m := range repo.selected {
			targets = append(targets, DeletionTarget{repo.name, m.Digest, slices.Clone(m.Tags), repo.whole, p.options.Unlock && m.IsLocked()})
		}
	}
	slices.SortFunc(targets, func(a, b DeletionTarget) int {
		if a.Repository != b.Repository {
			return cmp.Compare(a.Repository, b.Repository)
		}
		return cmp.Compare(a.Digest, b.Digest)
	})
	return targets
}

// Summary describes the plan, not confirmed deletions or reclaimed storage.
func (p *DeletionPlan) Summary() PruneStats {
	s := PruneStats{DryRun: true}
	for _, repo := range p.repositories {
		s.Repositories++
		if !repo.whole {
			s.KeptRepositories++
		}
		s.SeenManifests += len(repo.all)
		s.KeptManifests += len(repo.all) - len(repo.selected)
		s.DeletedManifests += len(repo.selected)
		s.SeenBytes += repo.seenBytes
		s.KeptBytes += repo.keptBytes
		s.DeletedBytes += repo.seenBytes - repo.keptBytes
	}
	return s
}

// Prepare evaluates the normal pruning rules and protections without mutating
// the registry. A failed scan yields no executable plan, including when other
// repositories were inspected successfully.
func (p *Pruner) Prepare(ctx context.Context, ruleSet []*rules.RepoRule) (*DeletionPlan, error) {
	preview := &DeletionPlan{registry: p.Registry, options: registry.DeleteOptions{Unlock: p.IncludeLocked}}
	local := *p
	local.DryRun, local.preview = true, preview
	if err := local.Prune(ctx, ruleSet); err != nil {
		return nil, err
	}
	return preview, nil
}

// Execute deletes only the reviewed targets. It first checks every affected
// repository, then checks each again immediately before its deletion. A changed
// snapshot requires a fresh preview and confirmation. Partial API successes are
// counted even on error. Requests and any lock restores finish before it returns.
func (p *DeletionPlan) Execute(ctx context.Context) (PruneStats, error) {
	var total PruneStats
	if !p.used.CompareAndSwap(false, true) {
		return total, errors.New("deletion plan already used; prepare and confirm a new preview")
	}
	progress.Report(ctx, progress.Event{Kind: progress.Candidates, Total: len(p.repositories)})
	for _, repo := range p.repositories {
		progress.Report(ctx, progress.Event{Kind: progress.Repository, Name: repo.name})
		progress.Report(ctx, progress.Event{Kind: progress.Phase, Name: "Verifying reviewed snapshot"})
		if err := p.registry.VerifyRepositorySnapshot(ctx, repo.name, repo.all); err != nil {
			return total, fmt.Errorf("preview is no longer valid; prepare a new preview: %w", err)
		}
	}
	for _, repo := range p.repositories {
		progress.Report(ctx, progress.Event{Kind: progress.Repository, Name: repo.name})
		progress.Report(ctx, progress.Event{Kind: progress.Phase, Name: "Rechecking repository before deletion"})
		if err := p.registry.VerifyRepositorySnapshot(ctx, repo.name, repo.all); err != nil {
			return total, err
		}
		progress.Report(ctx, progress.Event{Kind: progress.Plan, Kept: len(repo.all) - len(repo.selected), Count: len(repo.selected), Bytes: repo.seenBytes - repo.keptBytes})
		var confirmed []*registry.Manifest
		var err error
		if repo.whole {
			progress.Report(ctx, progress.Event{Kind: progress.Phase, Name: "Deleting repository"})
			err = p.registry.DeleteRepository(ctx, repo.name, repo.selected, p.options)
			if err == nil {
				confirmed = repo.selected
				progress.Report(ctx, progress.Event{Kind: progress.Deleted, Count: len(confirmed)})
			}
		} else {
			confirmed, err = p.registry.DeleteManifests(ctx, repo.selected, p.options)
		}
		removed := make(map[string]bool, len(confirmed))
		for _, m := range confirmed {
			removed[m.Digest] = true
		}
		remaining := slices.DeleteFunc(slices.Clone(repo.all), func(m *registry.Manifest) bool { return removed[m.Digest] })
		keptBytes := calculateStats("", remaining).Unique
		s := PruneStats{Repositories: 1, SeenManifests: len(repo.all), KeptManifests: len(remaining), DeletedManifests: len(confirmed),
			SeenBytes: repo.seenBytes, KeptBytes: keptBytes, DeletedBytes: repo.seenBytes - keptBytes}
		if !repo.whole || err != nil {
			s.KeptRepositories = 1
		}
		outcome := progress.OK
		if err != nil {
			s.SkippedRepositories, outcome = 1, progress.Skipped
		}
		total.Add(s)
		progress.Report(ctx, progress.Event{Kind: progress.Finished, Outcome: outcome})
		if err != nil {
			return total, err
		}
	}
	return total, ctx.Err()
}
