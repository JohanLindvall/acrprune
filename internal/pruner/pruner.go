// Package pruner applies compiled rules to an Azure Container Registry,
// deleting manifests and repositories that no rule keeps.
package pruner

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

	azerrors "github.com/Azure/azure-sdk-for-go-extensions/pkg/errors"
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

// Stats accumulates counts over one or more repository prunes.
type Stats struct {
	Repositories, KeptRepositories                 int
	SeenManifests, KeptManifests, DeletedManifests int
	SeenBytes, KeptBytes, DeletedBytes             uint64
}

func (s *Stats) Add(o Stats) {
	s.Repositories += o.Repositories
	s.KeptRepositories += o.KeptRepositories
	s.SeenManifests += o.SeenManifests
	s.KeptManifests += o.KeptManifests
	s.DeletedManifests += o.DeletedManifests
	s.SeenBytes += o.SeenBytes
	s.KeptBytes += o.KeptBytes
	s.DeletedBytes += o.DeletedBytes
}

func (s Stats) LogValue() slog.Value {
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

	var total Stats
	var purged, denied []string
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
				if isPermissionError(err) {
					denied = append(denied, repository)
					p.Logger.Warn("Insufficient permission to prune repository; skipping",
						"repository", repository, "purged", len(purged), "denied", len(denied),
						"remaining", len(repositories)-i-1, "err", err)
					break
				}
				return fmt.Errorf("failed to prune repository %s: %w", repository, err)
			}
			purged = append(purged, repository)
			total.Add(stats)
			p.Logger.Info("Processed", "totals", total)
			break
		}
	}

	if len(denied) > 0 {
		return fmt.Errorf("insufficient permission to prune %d of %d repositories (purged %d): %s",
			len(denied), len(repositories), len(purged), strings.Join(denied, ", "))
	}
	return nil
}

// isPermissionError reports whether err is an ACR 401/403, i.e. the caller
// lacks permission on the resource (typical on ABAC registries scoped to a
// subset of repositories).
func isPermissionError(err error) bool {
	if re := azerrors.IsResponseError(err); re != nil {
		return re.StatusCode == http.StatusForbidden || re.StatusCode == http.StatusUnauthorized
	}
	return false
}

// candidateRepositories returns the repositories the rules can apply to: the
// literal names when every rule targets a plain ^name$ pattern, or the full
// registry listing otherwise.
func (p *Pruner) candidateRepositories(ctx context.Context, ruleSet []*rules.RepoRule) ([]string, error) {
	var literals []string
	for _, rule := range ruleSet {
		name, ok := rule.LiteralRepoName()
		if !ok {
			repositories, err := p.Registry.ListRepositories(ctx)
			if err != nil && isPermissionError(err) {
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

func (p *Pruner) pruneRepository(ctx context.Context, repository string, rule *rules.RepoRule) (Stats, error) {
	manifests, found, err := p.Registry.FetchRepositoryManifests(ctx, repository, rule.IgnoreMissingManifests)
	if err != nil {
		return Stats{}, err
	}
	if !found {
		return Stats{}, nil // repository disappeared; nothing to do
	}

	markOrphans(manifests, repository)

	if err := p.reportOrphans(manifests, rule); err != nil {
		return Stats{}, err
	}

	kept, err := p.decide(manifests, repository, rule)
	if err != nil {
		return Stats{}, err
	}

	toKeep := make([]*registry.Manifest, 0, len(kept))
	var toDelete []*registry.Manifest
	for ref, m := range manifests {
		if _, ok := kept[ref]; ok {
			toKeep = append(toKeep, m)
			if !m.HasOwner {
				p.Logger.Debug("Keeping root manifest", "manifest", m)
			}
		} else {
			toDelete = append(toDelete, m)
			if !m.HasOwner {
				p.Logger.Debug("Deleting root manifest", "manifest", m)
			}
		}
	}

	totalBytes := calculateStats("", slices.Collect(maps.Values(manifests))).Unique
	keptBytes := calculateStats("", toKeep).Unique

	p.Logger.Info("Processed manifests", "repository", repository, "seen", humanize.Bytes(totalBytes), "kept", humanize.Bytes(keptBytes), "deleted", humanize.Bytes(totalBytes-keptBytes))

	slices.SortFunc(toDelete, func(a, b *registry.Manifest) int {
		return strings.Compare(a.Ref, b.Ref)
	})

	if !p.DryRun && p.IncludeLocked {
		toUnlock := toDelete
		if len(kept) == 0 {
			toUnlock = slices.Collect(maps.Values(manifests))
		}
		lockedTags, tagErr := p.Registry.ListTagLocks(ctx, repository)
		if tagErr != nil {
			return Stats{}, tagErr
		}
		p.Registry.UnlockManifests(ctx, toUnlock, lockedTags)
	}

	if p.DryRun {
		if len(kept) == 0 {
			p.Logger.Info("Dry-run: deleting repository", "repository", repository)
		} else {
			for _, m := range toDelete {
				p.Logger.Info("Dry-run: deleting manifest", "manifest", m)
			}
		}
	} else if len(kept) == 0 {
		p.Logger.Info("Deleting repository", "repository", repository)
		err = p.Registry.DeleteRepository(ctx, repository)
	} else {
		err = p.Registry.DeleteManifests(ctx, toDelete)
	}

	stats := Stats{
		Repositories:     1,
		SeenManifests:    len(manifests),
		KeptManifests:    len(kept),
		DeletedManifests: len(toDelete),
		SeenBytes:        totalBytes,
		KeptBytes:        keptBytes,
		DeletedBytes:     totalBytes - keptBytes,
	}
	if len(kept) > 0 {
		stats.KeptRepositories = 1
	}
	return stats, err
}

// markOrphans propagates the Orphaned flag until a fixpoint: a manifest is
// orphaned when a manifest it references is missing or itself orphaned, and
// every manifest referenced by an orphaned index is orphaned too. It also
// marks referenced manifests as owned.
func markOrphans(manifests map[string]*registry.Manifest, repository string) {
	for changed := true; changed; {
		changed = false
		for _, m := range manifests {
			for _, child := range m.Manifests {
				dep, ok := manifests[registry.MakeRef(repository, string(child.Digest))]
				if ok {
					dep.HasOwner = true
				}
				if (!ok || dep.Orphaned) && !m.Orphaned {
					changed = true
					m.Orphaned = true
				} else if ok && m.Orphaned && !dep.Orphaned {
					changed = true
					dep.Orphaned = true
				}
			}
		}
	}
}

// reportOrphans fails on orphaned manifests unless the rule either ignores
// missing manifests or deletes orphans.
func (p *Pruner) reportOrphans(manifests map[string]*registry.Manifest, rule *rules.RepoRule) error {
	if rule.IgnoreMissingManifests || rule.DeleteOrphanedManifests {
		return nil
	}
	orphaned := 0
	for _, m := range manifests {
		if !m.Orphaned {
			continue
		}
		orphaned++
		content, _ := json.Marshal(m.OCIManifest)
		p.Logger.Warn("Orphaned manifest", "manifest", m.Ref, "content", string(content))
	}
	if orphaned > 0 {
		return fmt.Errorf("%d manifest(s) orphaned", orphaned)
	}
	return nil
}

// decide returns the set of manifest refs to keep, including the transitive
// dependencies of every kept manifest.
func (p *Pruner) decide(manifests map[string]*registry.Manifest, repository string, rule *rules.RepoRule) (map[string]struct{}, error) {
	now := time.Now()

	ordered := slices.Collect(maps.Values(manifests))
	slices.SortFunc(ordered, func(a, b *registry.Manifest) int { // newest first
		return b.Azure.LastUpdatedOn.Compare(*a.Azure.LastUpdatedOn)
	})
	var tagged, untagged []*registry.Manifest
	for _, m := range ordered {
		if m.Azure != nil && len(m.Azure.Tags) > 0 {
			tagged = append(tagged, m)
		} else {
			untagged = append(untagged, m)
		}
	}

	kept := map[string]struct{}{}
	for ref, m := range manifests {
		if !p.shouldKeep(m, rule, tagged, untagged, now) {
			continue
		}
		if err := p.keepWithDependencies(ref, manifests, kept, repository, rule); err != nil {
			return nil, err
		}
	}

	// A must-delete-everything rule is all-or-nothing: keeping one manifest
	// keeps the whole repository.
	if rule.MustDeleteEverything && len(kept) > 0 {
		for ref := range manifests {
			kept[ref] = struct{}{}
		}
	}

	return kept, nil
}

// shouldKeep applies the first matching tagged/untagged rule, then the
// overrides: orphan deletion, subject retention and the grace period.
func (p *Pruner) shouldKeep(m *registry.Manifest, rule *rules.RepoRule, tagged, untagged []*registry.Manifest, now time.Time) bool {
	keep := true
	if tags := m.Tags(); len(tags) == 0 {
		for _, r := range rule.Untagged {
			if commonRuleMatches(r.CommonRule, m, untagged, now) {
				keep = r.Keep
				break
			}
		}
	} else {
		for _, r := range rule.Tagged {
			if matchAny(r.Tag, tags) && commonRuleMatches(r.CommonRule, m, tagged, now) {
				keep = r.Keep
				break
			}
		}
	}

	if rule.DeleteOrphanedManifests && m.Orphaned {
		keep = false
	}

	if m.Subject != nil {
		keep = true
		p.Logger.Info("Keeping manifest with subject", "manifest", m)
	}

	if p.KeepYounger != 0 && !keep {
		keep = m.Azure.LastUpdatedOn.Add(p.KeepYounger).After(now)
	}

	return keep
}

// keepWithDependencies marks ref and every manifest it transitively
// references as kept.
func (p *Pruner) keepWithDependencies(ref string, manifests map[string]*registry.Manifest, kept map[string]struct{}, repository string, rule *rules.RepoRule) error {
	if _, ok := kept[ref]; ok {
		return nil
	}
	kept[ref] = struct{}{}

	m := manifests[ref]
	if m == nil {
		if rule.IgnoreMissingManifests {
			p.Logger.Warn("Manifest missing", "manifest", ref)
			return nil
		}
		return fmt.Errorf("manifest missing %s", ref)
	}
	for _, child := range m.Manifests {
		if err := p.keepWithDependencies(registry.MakeRef(repository, string(child.Digest)), manifests, kept, repository, rule); err != nil {
			return err
		}
	}
	return nil
}
