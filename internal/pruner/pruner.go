// Package pruner applies compiled rules to an Azure Container Registry,
// deleting manifests and repositories that no rule keeps.
package pruner

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/JohanLindvall/acrprune/internal/registry"
	"github.com/JohanLindvall/acrprune/internal/rules"
	"github.com/dustin/go-humanize"
)

type Pruner struct {
	Registry *registry.Registry
	Logger   *slog.Logger
	DryRun   bool
	// KeepYounger is a grace period: manifests updated within it are never
	// deleted, whatever the rules say.
	KeepYounger time.Duration
	// IncludeLocked unlocks manifests and tags whose delete/write attribute is
	// disabled before deleting them, instead of failing on them.
	IncludeLocked bool
}

// PruneStats accumulates counts over one or more repository prunes.
type PruneStats struct {
	Repositories, KeptRepositories                 int
	SeenManifests, KeptManifests, DeletedManifests int
	SeenBytes, KeptBytes, DeletedBytes             uint64
}

func (s *PruneStats) Add(o PruneStats) {
	s.Repositories += o.Repositories
	s.KeptRepositories += o.KeptRepositories
	s.SeenManifests += o.SeenManifests
	s.KeptManifests += o.KeptManifests
	s.DeletedManifests += o.DeletedManifests
	s.SeenBytes += o.SeenBytes
	s.KeptBytes += o.KeptBytes
	s.DeletedBytes += o.DeletedBytes
}

func (s PruneStats) LogValue() slog.Value {
	return slog.GroupValue(
		slog.Int("repositories", s.Repositories),
		slog.Int("kept_repos", s.KeptRepositories),
		slog.Int("deleted_repos", s.Repositories-s.KeptRepositories),
		slog.Int("seen_manifests", s.SeenManifests),
		slog.Int("kept_manifests", s.KeptManifests),
		slog.Int("deleted_manifests", s.DeletedManifests),
		slog.String("seen_bytes", humanize.Bytes(s.SeenBytes)),
		slog.String("kept_bytes", humanize.Bytes(s.KeptBytes)),
		slog.String("deleted_bytes", humanize.Bytes(s.DeletedBytes)),
	)
}

// Prune applies the first matching rule to every candidate repository.
func (p *Pruner) Prune(ctx context.Context, ruleSet []*rules.RepoRule) error {
	p.Logger.Debug("Starting prune", "dryRun", p.DryRun, "keepYounger", p.KeepYounger, "rules", len(ruleSet))

	repositories, err := p.candidateRepositories(ctx, ruleSet)
	if err != nil {
		return err
	}

	var total PruneStats
	var pruned, denied []string
	for i, repository := range repositories {
		for _, rule := range ruleSet {
			if !rule.Repo.MatchString(repository) {
				continue
			}
			stats, err := p.pruneRepository(ctx, repository, rule)
			if err != nil {
				// On an ABAC registry a broad rule can match repositories the
				// caller has no access to. Skip those (rather than aborting
				// the whole run) but report them and fail at the end.
				if registry.IsPermissionError(err) {
					denied = append(denied, repository)
					p.Logger.Warn("Insufficient permission to prune repository; skipping",
						"repository", repository, "pruned", len(pruned), "denied", len(denied),
						"remaining", len(repositories)-i-1, "err", err)
					break
				}
				return fmt.Errorf("failed to prune repository %s: %w", repository, err)
			}
			pruned = append(pruned, repository)
			total.Add(stats)
			p.Logger.Info("Processed", "totals", total)
			break
		}
	}

	if len(denied) > 0 {
		return fmt.Errorf("insufficient permission to prune %d of %d repositories (pruned %d): %s",
			len(denied), len(repositories), len(pruned), strings.Join(denied, ", "))
	}
	return nil
}

// candidateRepositories returns the repositories the rules can apply to: the
// literal names when every rule targets a plain ^name$ pattern, or the full
// registry listing as soon as one rule needs it to resolve what it matches.
func (p *Pruner) candidateRepositories(ctx context.Context, ruleSet []*rules.RepoRule) ([]string, error) {
	var literals []string
	for _, rule := range ruleSet {
		name, ok := rule.LiteralRepoName()
		if !ok {
			repositories, err := p.Registry.ListRepositories(ctx)
			if registry.IsPermissionError(err) {
				// On ABAC registries catalog listing needs the Catalog Lister
				// role. Point the user at the literal-name path that avoids it.
				return nil, fmt.Errorf("%w (listing the catalog requires the Container Registry Repository Catalog Lister role; on ABAC registries, use literal ^repo$ patterns to target specific repositories without listing)", err)
			}
			return repositories, err
		}
		if !slices.Contains(literals, name) {
			literals = append(literals, name)
		}
	}
	p.Logger.Debug("All rules target literal repositories; skipping catalog listing", "repositories", literals)
	return literals, nil
}

func (p *Pruner) pruneRepository(ctx context.Context, repository string, rule *rules.RepoRule) (PruneStats, error) {
	manifests, found, err := p.Registry.FetchRepositoryManifests(ctx, repository, rule.IgnoreMissingManifests)
	if err != nil {
		return PruneStats{}, err
	}
	if !found {
		return PruneStats{}, nil // repository disappeared; nothing to do
	}

	markOrphans(manifests)

	if err := p.reportOrphans(manifests, rule); err != nil {
		return PruneStats{}, err
	}

	all := slices.SortedFunc(maps.Values(manifests), byNewest)
	kept, err := p.decide(all, manifests, rule)
	if err != nil {
		return PruneStats{}, err
	}

	toKeep := make([]*registry.Manifest, 0, len(kept))
	toDelete := make([]*registry.Manifest, 0, len(all))
	for _, m := range all {
		_, keep := kept[m.Digest]
		if keep {
			toKeep = append(toKeep, m)
		} else {
			toDelete = append(toDelete, m)
		}
		if !m.HasOwner {
			p.Logger.Debug("Root manifest", "manifest", m, "kept", keep)
		}
	}

	seenBytes := calculateStats("", all).Unique
	keptBytes := calculateStats("", toKeep).Unique

	p.Logger.Info("Processed manifests", "repository", repository,
		"seen", humanize.Bytes(seenBytes), "kept", humanize.Bytes(keptBytes), "deleted", humanize.Bytes(seenBytes-keptBytes))

	// An empty repository is deleted outright rather than manifest by
	// manifest; ACR keeps no repository without manifests anyway.
	deleteWholeRepository := len(kept) == 0

	if p.DryRun {
		if deleteWholeRepository {
			p.Logger.Info("Dry-run: deleting repository", "repository", repository)
		} else {
			for _, m := range toDelete {
				p.Logger.Info("Dry-run: deleting manifest", "manifest", m)
			}
		}
	} else {
		if p.IncludeLocked {
			if err := p.unlock(ctx, repository, toDelete); err != nil {
				return PruneStats{}, err
			}
		}
		if deleteWholeRepository {
			p.Logger.Info("Deleting repository", "repository", repository)
			err = p.Registry.DeleteRepository(ctx, repository)
		} else {
			err = p.Registry.DeleteManifests(ctx, toDelete)
		}
		if err != nil {
			return PruneStats{}, err
		}
	}

	stats := PruneStats{
		Repositories:     1,
		SeenManifests:    len(all),
		KeptManifests:    len(kept),
		DeletedManifests: len(toDelete),
		SeenBytes:        seenBytes,
		KeptBytes:        keptBytes,
		DeletedBytes:     seenBytes - keptBytes,
	}
	if !deleteWholeRepository {
		stats.KeptRepositories = 1
	}
	return stats, nil
}

// unlock re-enables delete and write on the manifests about to be deleted, and
// on any of their tags that are locked.
func (p *Pruner) unlock(ctx context.Context, repository string, toDelete []*registry.Manifest) error {
	if len(toDelete) == 0 {
		return nil
	}
	lockedTags, err := p.Registry.ListTagLocks(ctx, repository)
	if err != nil {
		return err
	}
	p.Registry.UnlockManifests(ctx, toDelete, lockedTags)
	return nil
}

// decide returns the digests of the manifests to keep, including the
// transitive dependencies of every kept manifest. all must hold the values of
// manifests in a deterministic order.
func (p *Pruner) decide(all []*registry.Manifest, manifests map[string]*registry.Manifest, rule *rules.RepoRule) (map[string]struct{}, error) {
	evaluator := newEvaluator(rule, all, time.Now())

	kept := map[string]struct{}{}
	for _, m := range all {
		if !p.shouldKeep(m, evaluator, rule) {
			continue
		}
		if err := p.keepWithDependencies(m.Digest, manifests, kept, rule); err != nil {
			return nil, err
		}
	}

	// A must-delete-everything rule is all-or-nothing: keeping one manifest
	// keeps the whole repository.
	if rule.MustDeleteEverything && len(kept) > 0 {
		for digest := range manifests {
			kept[digest] = struct{}{}
		}
	}

	return kept, nil
}

// shouldKeep applies the first matching tagged/untagged rule, then the
// overrides that can only save a manifest: orphan deletion, subject retention,
// the grace period and a missing timestamp.
func (p *Pruner) shouldKeep(m *registry.Manifest, evaluator *evaluator, rule *rules.RepoRule) bool {
	keep := evaluator.keep(m)

	if rule.DeleteOrphanedManifests && m.Orphaned {
		keep = false
	}

	if m.Subject != nil {
		// Signatures and attestations are deleted along with their subject,
		// not on their own account.
		keep = true
		p.Logger.Info("Keeping manifest with subject", "manifest", m)
	}

	if !keep && !m.HasTimestamp() {
		// Every age-based rule silently treats an unknown timestamp as the
		// zero time, i.e. as infinitely old. Refuse to delete on that basis.
		keep = true
		p.Logger.Warn("Keeping manifest with no last-updated timestamp", "manifest", m)
	}

	if !keep && p.KeepYounger != 0 {
		keep = m.LastUpdated().Add(p.KeepYounger).After(evaluator.now)
	}

	return keep
}

// keepWithDependencies marks the manifest and every manifest it transitively
// references as kept.
func (p *Pruner) keepWithDependencies(digest string, manifests map[string]*registry.Manifest, kept map[string]struct{}, rule *rules.RepoRule) error {
	if _, ok := kept[digest]; ok {
		return nil
	}

	m := manifests[digest]
	if m == nil {
		// Do not record a missing dependency as kept: len(kept) decides
		// whether anything of the repository survives.
		if rule.IgnoreMissingManifests {
			p.Logger.Warn("Kept manifest depends on a missing manifest", "digest", digest)
			return nil
		}
		return fmt.Errorf("manifest missing %s", digest)
	}
	kept[digest] = struct{}{}

	for _, child := range m.Manifests {
		if err := p.keepWithDependencies(string(child.Digest), manifests, kept, rule); err != nil {
			return err
		}
	}
	return nil
}
