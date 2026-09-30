// Package pruner applies compiled rules to a container registry, deleting
// manifests and repositories that no rule keeps.
package pruner

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/JohanLindvall/acrprune/internal/progress"
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
	SkippedRepositories                            int
	Repositories, KeptRepositories                 int
	SeenManifests, KeptManifests, DeletedManifests int
	SeenBytes, KeptBytes, DeletedBytes             uint64
}

func (s *PruneStats) Add(o PruneStats) {
	s.SkippedRepositories += o.SkippedRepositories
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
		slog.Int("skipped_repos", s.SkippedRepositories),
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

	progress.Report(ctx, progress.Event{Kind: progress.Phase, Name: "Listing repositories"})
	repositories, err := p.candidateRepositories(ctx, ruleSet)
	if err != nil {
		return err
	}
	progress.Report(ctx, progress.Event{Kind: progress.Candidates, Total: len(repositories)})

	var total PruneStats
	var pruned, denied []string
	for i, repository := range repositories {
		if err := ctx.Err(); err != nil {
			return err
		}
		for _, rule := range ruleSet {
			if !rule.Repo.MatchString(repository) {
				continue
			}
			progress.Report(ctx, progress.Event{Kind: progress.Repository, Name: repository})
			stats, err := p.pruneRepository(ctx, repository, rule)
			if err != nil {
				// On an ABAC registry a broad rule can match repositories the
				// caller has no access to. Skip those (rather than aborting
				// the whole run) but report them and fail at the end.
				if registry.IsPermissionError(err) {
					denied = append(denied, repository)
					progress.Report(ctx, progress.Event{Kind: progress.Finished, Name: "denied"})
					p.Logger.Warn("Insufficient permission to prune repository; skipping",
						"repository", repository, "pruned", len(pruned), "denied", len(denied),
						"remaining", len(repositories)-i-1, "err", err)
					break
				}
				return fmt.Errorf("failed to prune repository %s: %w", repository, err)
			}
			pruned = append(pruned, repository)
			total.Add(stats)
			status := ""
			if stats.SkippedRepositories > 0 || stats.Repositories == 0 {
				status = "skipped"
			}
			progress.Report(ctx, progress.Event{Kind: progress.Finished, Name: status})
			p.Logger.Info("Processed", "totals", total)
			break
		}
	}

	if len(denied) > 0 {
		hint := ""
		if len(pruned) == 0 {
			// Partial denial is normal ABAC scoping; blanket denial usually
			// means the credential itself is wrong for this registry.
			hint = " — every repository was denied; check that the credential is valid for this registry"
		}
		return fmt.Errorf("insufficient permission to prune %d of %d repositories (pruned %d)%s: %s",
			len(denied), len(repositories), len(pruned), hint, strings.Join(denied, ", "))
	}
	return nil
}

// candidateRepositories returns the repositories the rules can apply to: the
// literal names when every rule targets a plain ^name$ pattern, or the full
// registry listing as soon as one rule needs it to resolve what it matches.
func (p *Pruner) candidateRepositories(ctx context.Context, ruleSet []*rules.RepoRule) ([]string, error) {
	var literals []string
	seen := map[string]struct{}{}
	for _, rule := range ruleSet {
		name, ok := rule.LiteralRepoName()
		if !ok {
			repositories, err := p.Registry.ListRepositories(ctx)
			if registry.IsPermissionError(err) {
				// Point the user at the literal-name path, which avoids
				// listing altogether.
				return nil, fmt.Errorf("repository pattern %q needs every repository listed; use literal ^repo$ patterns to target specific repositories without listing: %w", rule.Repo, err)
			}
			if err != nil {
				return nil, err
			}
			return slices.DeleteFunc(repositories, func(repository string) bool {
				return !slices.ContainsFunc(ruleSet, func(rule *rules.RepoRule) bool { return rule.Repo.MatchString(repository) })
			}), nil
		}
		if _, ok := seen[name]; !ok {
			literals = append(literals, name)
			seen[name] = struct{}{}
		}
	}
	p.Logger.Debug("All rules target literal repositories; skipping catalog listing", "repositories", literals)
	return literals, nil
}

func (p *Pruner) pruneRepository(ctx context.Context, repository string, rule *rules.RepoRule) (PruneStats, error) {
	contents, found, err := p.Registry.FetchRepositoryManifests(ctx, repository, registry.FetchOptions{
		IgnoreMissing: rule.IgnoreMissingManifests,
		Platforms:     rule.UsesPlatform(),
	})
	if err != nil {
		return PruneStats{}, err
	}
	if !found {
		return PruneStats{}, nil // repository disappeared; nothing to do
	}
	manifests := contents.Manifests
	if len(contents.Missing) > 0 {
		// An unavailable index can reference any of the downloaded manifests.
		// Without its document there is no safe way to prune its dependencies.
		p.Logger.Warn("Skipping repository with unavailable manifest documents; dependencies cannot be determined",
			"repository", repository, "missing", len(contents.Missing))
		known := calculateStats(repository, slices.Collect(maps.Values(manifests)))
		count := len(manifests) + len(contents.Missing)
		return PruneStats{Repositories: 1, KeptRepositories: 1, SkippedRepositories: 1, SeenManifests: count,
			KeptManifests: count, SeenBytes: known.Unique, KeptBytes: known.Unique}, nil
	}

	progress.Report(ctx, progress.Event{Kind: progress.Phase, Name: "Evaluating rules and dependencies"})
	markOrphans(manifests)

	if err := p.reportOrphans(manifests, rule); err != nil {
		return PruneStats{}, err
	}

	all := slices.SortedFunc(maps.Values(manifests), byNewest)
	kept, err := p.decide(all, manifests, rule)
	if err != nil {
		return PruneStats{}, err
	}

	// A repository keeping nothing can be deleted outright.
	deleteWholeRepository := len(kept) == 0
	if !deleteWholeRepository && p.Registry.ProtectsLastTag() {
		if err := p.keepLastTag(all, manifests, contents.Missing, kept, rule); err != nil {
			return PruneStats{}, err
		}
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
	progress.Report(ctx, progress.Event{Kind: progress.Plan, Kept: len(toKeep), Count: len(toDelete), Bytes: seenBytes - keptBytes})

	p.Logger.Info("Processed manifests", "repository", repository,
		"seen", humanize.Bytes(seenBytes), "kept", humanize.Bytes(keptBytes), "deleted", humanize.Bytes(seenBytes-keptBytes))

	if p.DryRun {
		progress.Report(ctx, progress.Event{Kind: progress.Phase, Name: "Previewing deletions"})
		if deleteWholeRepository {
			p.Logger.Info("Dry-run: deleting repository", "repository", repository)
		} else {
			for _, m := range toDelete {
				p.Logger.Info("Dry-run: deleting manifest", "manifest", m)
			}
		}
	} else {
		if deleteWholeRepository {
			progress.Report(ctx, progress.Event{Kind: progress.Phase, Name: "Rechecking repository before deletion"})
			if err := p.Registry.VerifyRepositorySnapshot(ctx, repository, all); err != nil {
				return PruneStats{}, err
			}
		}
		if p.IncludeLocked {
			if err := p.Registry.UnlockManifests(ctx, repository, toDelete); err != nil {
				return PruneStats{}, err
			}
		}
		if deleteWholeRepository {
			progress.Report(ctx, progress.Event{Kind: progress.Phase, Name: "Deleting repository"})
			p.Logger.Info("Deleting repository", "repository", repository)
			err = p.Registry.DeleteRepository(ctx, repository, toDelete...)
			if err == nil {
				progress.Report(ctx, progress.Event{Kind: progress.Deleted, Count: len(toDelete)})
			}
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

// decide returns the digests of the manifests to keep, including the
// transitive dependencies of every kept manifest. all must hold the values of
// manifests in a deterministic order.
func (p *Pruner) decide(all []*registry.Manifest, manifests map[string]*registry.Manifest, rule *rules.RepoRule) (map[string]struct{}, error) {
	evaluator := newEvaluator(rule, all, time.Now())

	kept := map[string]struct{}{}
	for _, m := range all {
		if m.Subject != nil {
			continue // referrers follow their subject; decided below
		}
		if !p.shouldKeep(m, evaluator, rule) {
			continue
		}
		if _, err := p.keepWithDependencies(m.Digest, manifests, kept, rule); err != nil {
			return nil, err
		}
	}

	// A must-delete-everything rule is all-or-nothing: keeping one manifest
	// keeps the whole repository. Referrers do not count as keeps here — they
	// only ever live on their subject's account.
	if rule.MustDeleteEverything && len(kept) > 0 {
		for digest := range manifests {
			kept[digest] = struct{}{}
		}
		return kept, nil
	}

	if err := p.keepReferrers(all, manifests, kept, rule, evaluator.now); err != nil {
		return nil, err
	}

	return kept, nil
}

// keepLastTag keeps the newest tagged manifest of a repository that is not
// deleted outright but would otherwise keep no tagged manifest, on registries
// that refuse to delete a repository's last tagged manifest (GHCR). Deleting
// it would fail, and deleting the whole repository instead would take the
// untagged manifests the rules keep — digest-pinned running images, say —
// along with it. missing are the manifests the registry listed but could not
// serve, which stay.
func (p *Pruner) keepLastTag(all []*registry.Manifest, manifests map[string]*registry.Manifest, missing []registry.Attributes, kept map[string]struct{}, rule *rules.RepoRule) error {
	if slices.ContainsFunc(missing, func(a registry.Attributes) bool { return len(a.Tags) > 0 }) {
		return nil // a tagged manifest survives anyway
	}
	// Prefer a healthy image: an orphaned manifest cannot be kept whole, and
	// a tagged referrer such as a signature is no image at all.
	rank := func(m *registry.Manifest) int {
		r := 0
		if m.Orphaned {
			r += 2
		}
		if m.Subject != nil {
			r++
		}
		return r
	}
	var last *registry.Manifest
	for _, m := range all { // newest first
		if len(m.Tags) == 0 {
			continue
		}
		if _, ok := kept[m.Digest]; ok {
			return nil // a tagged manifest survives anyway
		}
		if last == nil || rank(m) < rank(last) {
			last = m
		}
	}
	if last == nil {
		return nil // nothing is tagged
	}
	p.Logger.Warn("Keeping the last tagged manifest, which the registry refuses to delete without deleting the repository", "manifest", last)
	if _, err := p.keepWithDependencies(last.Digest, manifests, kept, rule); err != nil {
		return err
	}
	return p.keepReferrers(all, manifests, kept, rule, time.Now())
}

// keepReferrers settles the fate of subject-bearing manifests (signatures,
// attestations, SBOMs): a referrer is kept exactly when its subject is kept,
// and deleted along with it — including referrers whose subject is already
// gone, which are otherwise unreachable garbage. A queue follows subject
// chains (a signature on an attestation on an image) in linear time.
func (p *Pruner) keepReferrers(all []*registry.Manifest, manifests map[string]*registry.Manifest, kept map[string]struct{}, rule *rules.RepoRule, now time.Time) error {
	referrers := map[string][]*registry.Manifest{}
	var queue []*registry.Manifest
	for _, m := range all {
		if m.Subject == nil {
			continue
		}
		referrers[string(m.Subject.Digest)] = append(referrers[string(m.Subject.Digest)], m)
		if _, ok := kept[m.Digest]; !ok && p.protectedByAge(m, now) {
			added, err := p.keepWithDependencies(m.Digest, manifests, kept, rule)
			if err != nil {
				return err
			}
			queue = append(queue, added...)
		}
	}
	for _, m := range all {
		if _, ok := kept[m.Digest]; ok {
			queue = append(queue, m)
		}
	}
	for i := 0; i < len(queue); i++ {
		for _, m := range referrers[queue[i].Digest] {
			if _, ok := kept[m.Digest]; ok || rule.DeleteOrphanedManifests && m.Orphaned {
				continue
			}
			p.Logger.Debug("Keeping referrer", "manifest", m, "subject", string(m.Subject.Digest))
			added, err := p.keepWithDependencies(m.Digest, manifests, kept, rule)
			if err != nil {
				return err
			}
			queue = append(queue, added...)
		}
	}
	return nil
}

// shouldKeep applies the first matching tagged/untagged rule, then the
// overrides: orphan deletion, the grace period and a missing timestamp.
// Referrers never come through here — keepReferrers decides them.
func (p *Pruner) shouldKeep(m *registry.Manifest, evaluator *evaluator, rule *rules.RepoRule) bool {
	keep := evaluator.keep(m)

	if rule.DeleteOrphanedManifests && m.Orphaned {
		keep = false
	}

	return keep || p.protectedByAge(m, evaluator.now)
}

// protectedByAge applies the same timestamp protection to images and referrers.
func (p *Pruner) protectedByAge(m *registry.Manifest, now time.Time) bool {
	if !m.HasTimestamp() {
		// Every age-based rule silently treats an unknown timestamp as the
		// zero time, i.e. as infinitely old. Refuse to delete on that basis.
		p.Logger.Warn("Keeping manifest with no last-updated timestamp", "manifest", m)
		return true
	}
	return p.KeepYounger > 0 && m.LastUpdated.Add(p.KeepYounger).After(now)
}

// keepWithDependencies marks the manifest and every manifest it transitively
// references as kept.
func (p *Pruner) keepWithDependencies(digest string, manifests map[string]*registry.Manifest, kept map[string]struct{}, rule *rules.RepoRule) ([]*registry.Manifest, error) {
	pending := []string{digest}
	var added []*registry.Manifest
	for len(pending) > 0 {
		digest := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if _, ok := kept[digest]; ok {
			continue
		}
		m := manifests[digest]
		if m == nil {
			// Missing dependencies must not count as surviving manifests.
			if !rule.IgnoreMissingManifests {
				return nil, fmt.Errorf("manifest missing %s", digest)
			}
			p.Logger.Warn("Kept manifest depends on a missing manifest", "digest", digest)
			continue
		}
		kept[digest] = struct{}{}
		added = append(added, m)
		for _, child := range m.Manifests {
			pending = append(pending, string(child.Digest))
		}
	}
	return added, nil
}
