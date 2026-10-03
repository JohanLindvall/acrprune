// Package pruner applies compiled rules to a container registry, deleting
// manifests and repositories that no rule keeps.
package pruner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/JohanLindvall/crprune/internal/progress"
	"github.com/JohanLindvall/crprune/internal/registry"
	"github.com/JohanLindvall/crprune/internal/rules"
	"github.com/dustin/go-humanize"
)

// Pruner applies rules to the repositories of a registry. A manifest the rules
// delete is kept anyway when it is protected: updated within the grace
// period, of unknown age, running, or locked. Keeping a manifest keeps what it
// references and its referrers, and keeps its repository from being deleted
// outright.
type Pruner struct {
	Registry *registry.Registry
	Logger   *slog.Logger
	// DryRun only logs what would be deleted.
	DryRun bool
	// KeepYounger is a grace period: manifests updated within it are never
	// deleted, whatever the rules say.
	KeepYounger time.Duration
	// IncludeLocked deletes manifests whose delete/write attribute is
	// disabled, on the manifest or on one of its tags, unlocking them just
	// before deleting them. Without it they are kept.
	IncludeLocked bool
	// Protect are keep rules for the running images, as generated from an
	// image list: a manifest they keep is never deleted, whatever the rules
	// say.
	Protect []*rules.RepoRule
	// preview captures the exact, protected decision for interactive review.
	// Only Prepare sets it, on a private dry-run copy of the pruner.
	preview *DeletionPlan
	// requested and selection bound a manual manifest preview to its selected
	// digests and their dependency/referrer closure. Only PrepareManifests sets
	// requested; each repository inspection resolves selection once.
	requested []string
	selection map[string]bool
}

// PruneStats accumulates counts over one or more repository prunes. The
// deleted counts are confirmed deletions, including partial successes when a
// repository fails. In a dry run they are what would be deleted. Bytes remain
// estimates: registries do not report when garbage collection reclaims them.
type PruneStats struct {
	// DryRun logs the deleted counts as would_delete_*.
	DryRun                                         bool
	SkippedRepositories                            int
	Repositories, KeptRepositories                 int
	SeenManifests, KeptManifests, DeletedManifests int
	SeenBytes, KeptBytes, DeletedBytes             uint64
}

// Add adds the counts of o to s. DryRun stays as it is.
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

// LogValue makes the counts log as an attribute group. It has a value
// receiver so that PruneStats values, not just pointers, are slog.LogValuers.
func (s PruneStats) LogValue() slog.Value {
	deleted := "deleted_"
	if s.DryRun {
		deleted = "would_delete_"
	}
	return slog.GroupValue(
		slog.Int("repositories", s.Repositories),
		slog.Int("kept_repos", s.KeptRepositories),
		slog.Int("skipped_repos", s.SkippedRepositories),
		slog.Int(deleted+"repos", s.Repositories-s.KeptRepositories),
		slog.Int("seen_manifests", s.SeenManifests),
		slog.Int("kept_manifests", s.KeptManifests),
		slog.Int(deleted+"manifests", s.DeletedManifests),
		slog.String("seen_bytes", humanize.Bytes(s.SeenBytes)),
		slog.String("kept_bytes", humanize.Bytes(s.KeptBytes)),
		slog.String(deleted+"bytes", humanize.Bytes(s.DeletedBytes)),
	)
}

// Prune applies the first matching rule to every candidate repository.
//
// Repositories the credential may not prune, and repositories that cannot be
// pruned safely (see skippable), are skipped: the others are still pruned,
// and the skipped ones are reported in the returned error. Any other failure
// stops the run.
func (p *Pruner) Prune(ctx context.Context, ruleSet []*rules.RepoRule) error {
	p.Logger.Debug("Starting prune", "dryRun", p.DryRun, "keepYounger", p.KeepYounger, "rules", len(ruleSet))

	progress.Report(ctx, progress.Event{Kind: progress.Phase, Name: "Listing repositories"})
	matcher := rules.NewRepositoryMatcher(ruleSet)
	protect := rules.NewRepositoryMatcher(p.Protect)
	repositories, err := p.candidateRepositories(ctx, ruleSet, matcher)
	if err != nil {
		return err
	}
	progress.Report(ctx, progress.Event{Kind: progress.Candidates, Total: len(repositories)})

	total := PruneStats{DryRun: p.DryRun}
	var pruned, denied []string
	var skipped []error
	for i, repository := range repositories {
		if err := ctx.Err(); err != nil {
			return err
		}
		rule := matcher.Match(repository)
		if rule == nil {
			continue
		}
		progress.Report(ctx, progress.Event{Kind: progress.Repository, Name: repository})
		stats, err := p.pruneRepository(ctx, repository, rule, protect.Match(repository))
		if err != nil && stats.Repositories == 0 {
			stats = untouched // failure before the repository could be inspected
		}
		outcome := progress.OK
		var fatal error
		remaining := len(repositories) - i - 1
		switch {
		case err == nil:
			pruned = append(pruned, repository)
			if stats.SkippedRepositories > 0 {
				outcome = progress.Skipped
				if p.preview != nil {
					// A batch run can leave an unavailable repository alone,
					// but a reviewed plan must not silently omit part of the
					// requested scope and remain executable.
					fatal = fmt.Errorf("%s: repository could not be fully inspected; prepare a new preview", repository)
				}
			}
		case isDenied(err):
			// On an ABAC registry a broad rule can match repositories the
			// caller has no access to. Skip those (rather than aborting the
			// whole run) but report them and fail at the end.
			denied = append(denied, repository)
			outcome = progress.Denied
			p.Logger.Warn("Insufficient permission to prune repository; skipping",
				"repository", repository, "pruned", len(pruned), "denied", len(denied),
				"remaining", remaining, "deleted_manifests", stats.DeletedManifests, "err", err)
		case skippable(err):
			skipped = append(skipped, fmt.Errorf("%s: %w", repository, err))
			outcome = progress.Skipped
			p.Logger.Warn("Skipping repository that cannot be pruned safely",
				"repository", repository, "pruned", len(pruned), "skipped", len(skipped),
				"remaining", remaining, "err", err)
		default:
			outcome = progress.Skipped
			fatal = fmt.Errorf("failed to prune repository %s: %w", repository, err)
		}
		total.Add(stats)
		progress.Report(ctx, progress.Event{Kind: progress.Finished, Outcome: outcome})
		p.Logger.Info("Processed", "totals", total)
		if fatal != nil {
			return fatal
		}
	}

	var errs []error
	if len(denied) > 0 {
		hint := ""
		if len(pruned) == 0 && len(skipped) == 0 {
			// Partial denial is normal ABAC scoping; blanket denial usually
			// means the credential itself is wrong for this registry.
			hint = " — every repository was denied; check that the credential is valid for this registry"
		}
		errs = append(errs, fmt.Errorf("insufficient permission to prune %d of %d repositories (pruned %d)%s: %s",
			len(denied), len(repositories), len(pruned), hint, strings.Join(denied, ", ")))
	}
	if len(skipped) > 0 {
		errs = append(errs, fmt.Errorf("skipped %d of %d repositories that could not be pruned safely (pruned %d): %w",
			len(skipped), len(repositories), len(pruned), errors.Join(skipped...)))
	}
	return errors.Join(append(errs, ctx.Err())...)
}

// untouched counts a repository skipped with nothing deleted, whose manifests
// were not all inspected.
var untouched = PruneStats{Repositories: 1, KeptRepositories: 1, SkippedRepositories: 1}

// isDenied reports whether err is a permission error, and nothing but: when it
// joins several failures, as DeleteManifests does, every one of them must be.
// A denial hiding another failure would skip a repository that should stop
// the run.
func isDenied(err error) bool {
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		errs := joined.Unwrap()
		return len(errs) > 0 && !slices.ContainsFunc(errs, func(err error) bool { return !isDenied(err) })
	}
	if _, response := err.(*registry.ResponseError); !response {
		if cause := errors.Unwrap(err); cause != nil {
			return isDenied(cause)
		}
	}
	return registry.IsPermissionError(err)
}

// skippable reports whether err says that a repository cannot be pruned
// safely, which is no reason to stop pruning the others: it changed while it
// was inspected, or holds a manifest in a format whose references cannot be
// read. Both are found before anything in the repository is deleted.
func skippable(err error) bool {
	return errors.Is(err, registry.ErrRepositoryChanged) || errors.Is(err, registry.ErrUnsupportedManifest)
}

// candidateRepositories returns the repositories the rules can apply to: the
// literal names when every rule targets a plain ^name$ pattern, or the full
// registry listing as soon as one rule needs it to resolve what it matches.
func (p *Pruner) candidateRepositories(ctx context.Context, ruleSet []*rules.RepoRule, matcher *rules.RepositoryMatcher) ([]string, error) {
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
				return matcher.Match(repository) == nil
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

// pruneRepository applies the rule to the repository. In a live run it lists
// the repository again before deleting anything, and fails with
// registry.ErrRepositoryChanged when anything changed since it was inspected.
func (p *Pruner) pruneRepository(ctx context.Context, repository string, rule, protect *rules.RepoRule) (PruneStats, error) {
	// A generated inventory has a rule per repository. Find this one's rule
	// once, rather than scanning the entire inventory for every manifest in
	// each of the decision passes. Keep the caller's Pruner unchanged.
	local := *p
	local.Protect = nil
	if protect != nil {
		local.Protect = []*rules.RepoRule{protect}
	}
	p = &local
	contents, found, err := p.Registry.FetchRepositoryManifests(ctx, repository, registry.FetchOptions{
		IgnoreMissing: rule.IgnoreMissingManifests,
		Platforms:     rule.UsesPlatform(),
	})
	if err != nil {
		return PruneStats{}, err
	}
	if !found {
		return untouched, nil // repository disappeared; nothing to do
	}
	manifests := contents.Manifests
	if len(contents.Missing) > 0 {
		// An unavailable index can reference any of the downloaded manifests.
		// Without its document there is no safe way to prune its dependencies.
		p.Logger.Warn("Skipping repository with unavailable manifest documents; dependencies cannot be determined",
			"repository", repository, "missing", len(contents.Missing))
		known := calculateStats(repository, slices.Collect(maps.Values(manifests)))
		return leftAlone(len(manifests)+len(contents.Missing), known.Unique), nil
	}
	if p.requested != nil {
		p.selection, err = expandSelection(ctx, repository, manifests, p.requested)
		if err != nil {
			return PruneStats{}, err
		}
	}
	if len(manifests) == 0 {
		// Neither ACR nor GHCR keeps empty repositories, so an existing one
		// listing nothing is an anomaly, and deleting it would gain nothing.
		p.Logger.Debug("Repository listed no manifests; leaving it alone", "repository", repository)
		return PruneStats{Repositories: 1, KeptRepositories: 1}, nil
	}

	progress.Report(ctx, progress.Event{Kind: progress.Phase, Name: "Evaluating rules and dependencies"})
	markOrphans(manifests)

	if err := p.reportOrphans(manifests, rule); err != nil {
		return PruneStats{}, err
	}

	all := slices.SortedFunc(maps.Values(manifests), byNewest)
	now := time.Now()
	// Tag locks are listed only for the manifests the rules would delete,
	// and only when one of them is tagged. Decide once, unlogged, to find
	// those; then decide again, knowing their tag locks too, and log that
	// decision. Dry runs load the locks as well, so that they preview what a
	// live run does.
	quiet := *p
	quiet.Logger = slog.New(slog.DiscardHandler)
	kept, err := quiet.decide(all, manifests, rule, now)
	if err != nil {
		return PruneStats{}, err
	}
	selected := slices.DeleteFunc(slices.Clone(all), func(m *registry.Manifest) bool { _, ok := kept[m.Digest]; return ok })
	if len(selected) > 0 {
		lockCandidates := selected
		if rule.MustDeleteEverything && !p.IncludeLocked {
			// A referrer kept for its age alone does not protect this
			// repository, but its tag lock does. Check it even though the
			// first decision did not select that referrer for deletion.
			lockCandidates = all
		}
		if err := p.Registry.LoadTagLocks(ctx, repository, lockCandidates); err != nil {
			return PruneStats{}, err
		}
	}
	if kept, err = p.decide(all, manifests, rule, now); err != nil {
		return PruneStats{}, err
	}

	// A repository keeping nothing can be deleted outright.
	deleteWholeRepository := len(kept) == 0
	if !deleteWholeRepository && p.Registry.ProtectsLastTag() {
		if err := p.keepLastTag(all, manifests, kept, rule, now); err != nil {
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
	if p.preview != nil && len(toDelete) > 0 {
		p.preview.repositories = append(p.preview.repositories, repositoryDeletion{
			name: repository, all: all, selected: toDelete, whole: deleteWholeRepository,
			seenBytes: seenBytes, keptBytes: keptBytes,
		})
	}
	// A live run reports its plan once the recheck confirmed it, so that a
	// repository skipped then counts no selected manifests.
	plan := progress.Event{Kind: progress.Plan, Kept: len(toKeep), Count: len(toDelete), Bytes: seenBytes - keptBytes}

	deleted := "deleted"
	if p.DryRun {
		deleted = "would_delete"
	}
	p.Logger.Info("Planned manifests", "repository", repository,
		"seen", humanize.Bytes(seenBytes), "kept", humanize.Bytes(keptBytes), "selected", humanize.Bytes(seenBytes-keptBytes))
	confirmed := toDelete // a dry run reports the plan

	switch {
	case p.DryRun:
		progress.Report(ctx, plan)
		progress.Report(ctx, progress.Event{Kind: progress.Phase, Name: "Previewing deletions"})
		if deleteWholeRepository {
			// Each manifest is listed all the same, so that the plan names
			// every image it would delete.
			p.Logger.Info("Dry-run: deleting repository", "repository", repository)
			for _, m := range toDelete {
				if m.IsLocked() {
					p.Logger.Info("Dry-run: unlocking manifest to delete it with the repository", "manifest", m)
				}
				p.Logger.Info("Dry-run: deleting manifest with the repository", "manifest", m)
			}
		} else {
			for _, m := range toDelete {
				p.Logger.Info("Dry-run: deleting manifest", "manifest", m)
			}
		}
	case len(toDelete) == 0:
		progress.Report(ctx, plan)
	default:
		// Listings are not atomic snapshots. A push, retag or deletion
		// during inspection can make the decision unsafe; on GHCR, whose
		// listing pages are numbered, a concurrent deletion even hides
		// another manifest from the listing.
		progress.Report(ctx, progress.Event{Kind: progress.Phase, Name: "Rechecking repository before deletion"})
		if err := p.Registry.VerifyRepositorySnapshot(ctx, repository, all); err != nil {
			if !errors.Is(err, registry.ErrRepositoryGone) {
				return PruneStats{}, err
			}
			p.Logger.Warn("Repository deleted during inspection; skipping", "repository", repository, "err", err)
			return leftAlone(len(all), seenBytes), nil
		}
		progress.Report(ctx, plan)
		opts := registry.DeleteOptions{Unlock: p.IncludeLocked}
		if deleteWholeRepository {
			progress.Report(ctx, progress.Event{Kind: progress.Phase, Name: "Deleting repository"})
			p.Logger.Info("Deleting repository", "repository", repository)
			err = p.Registry.DeleteRepository(ctx, repository, toDelete, opts)
			if err == nil {
				progress.Report(ctx, progress.Event{Kind: progress.Deleted, Count: len(toDelete)})
			} else {
				confirmed = nil
			}
		} else {
			confirmed, err = p.Registry.DeleteManifests(ctx, toDelete, opts)
		}
		if err != nil {
			// A failed request can follow successful, irreversible deletes.
			// Count only confirmed successes; unconfirmed manifests and any
			// blobs they reference still count as remaining.
			removed := make(map[string]bool, len(confirmed))
			for _, m := range confirmed {
				removed[m.Digest] = true
			}
			toKeep = slices.DeleteFunc(slices.Clone(all), func(m *registry.Manifest) bool { return removed[m.Digest] })
			keptBytes = calculateStats("", toKeep).Unique
			deleteWholeRepository = false
		}
	}

	stats := PruneStats{
		Repositories:     1,
		SeenManifests:    len(all),
		KeptManifests:    len(toKeep),
		DeletedManifests: len(confirmed),
		SeenBytes:        seenBytes,
		KeptBytes:        keptBytes,
		DeletedBytes:     seenBytes - keptBytes,
	}
	if !deleteWholeRepository {
		stats.KeptRepositories = 1
	}
	if err != nil {
		stats.SkippedRepositories = 1
	}
	p.Logger.Info("Processed manifests", "repository", repository,
		"seen", humanize.Bytes(seenBytes), "kept", humanize.Bytes(keptBytes), deleted, humanize.Bytes(stats.DeletedBytes))
	return stats, err
}

// leftAlone counts a skipped repository, keeping the manifests and bytes seen
// in it.
func leftAlone(manifests int, bytes uint64) PruneStats {
	return PruneStats{Repositories: 1, KeptRepositories: 1, SkippedRepositories: 1,
		SeenManifests: manifests, KeptManifests: manifests, SeenBytes: bytes, KeptBytes: bytes}
}

// decide returns the digests of the manifests to keep — those the rules keep
// or that are protected, and the referrers of kept manifests — including the
// transitive dependencies of every kept manifest. all must hold the values of
// manifests in a deterministic order.
func (p *Pruner) decide(all []*registry.Manifest, manifests map[string]*registry.Manifest, rule *rules.RepoRule, now time.Time) (map[string]struct{}, error) {
	evaluator := newEvaluator(rule, all, now)

	kept := map[string]struct{}{}
	for _, m := range all {
		if p.selection != nil && !p.selection[m.Digest] {
			// Manual cleanup never sweeps unrelated referrers, even those
			// whose subjects are gone or form a tag-scheme cycle.
			if _, err := p.keepWithDependencies(m.Digest, manifests, kept, rule); err != nil {
				return nil, err
			}
			continue
		}
		if m.SubjectDigest() != "" {
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
	// keeps the whole repository. Referrers count only when a lock or
	// --running protects them, which never lapses. Otherwise they live on
	// their subject's account, and one kept for its age alone outlives its
	// subject by at most the grace period, so that signed repositories can
	// still be deleted.
	if rule.MustDeleteEverything && (len(kept) > 0 || slices.ContainsFunc(all, func(m *registry.Manifest) bool {
		return m.SubjectDigest() != "" && p.pinned(m)
	})) {
		for digest := range manifests {
			kept[digest] = struct{}{}
		}
		return kept, nil
	}

	if err := p.keepReferrers(all, manifests, kept, rule, now); err != nil {
		return nil, err
	}

	return kept, nil
}

// keepLastTag keeps the newest tagged manifest of a repository that is not
// deleted outright but would otherwise keep no tagged manifest, on registries
// that refuse to delete a repository's last tagged manifest (GHCR). Deleting
// it would fail, and deleting the whole repository instead would take the
// untagged manifests the rules keep — digest-pinned running images, say —
// along with it.
func (p *Pruner) keepLastTag(all []*registry.Manifest, manifests map[string]*registry.Manifest, kept map[string]struct{}, rule *rules.RepoRule, now time.Time) error {
	// Prefer a healthy image: an orphaned manifest cannot be kept whole, and
	// a tagged referrer such as a signature is no image at all.
	rank := func(m *registry.Manifest) int {
		r := 0
		if m.Orphaned {
			r += 2
		}
		if m.SubjectDigest() != "" {
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
	return p.keepReferrers(all, manifests, kept, rule, now)
}

// keepReferrers settles the fate of referrers (signatures, attestations,
// SBOMs; see registry.Manifest.SubjectDigest): a referrer is kept exactly when
// its subject is kept, and deleted along with it — including referrers whose
// subject is already gone, which are otherwise unreachable garbage — unless it
// is protected itself. A queue follows subject chains (a signature on an
// attestation on an image) in linear time.
func (p *Pruner) keepReferrers(all []*registry.Manifest, manifests map[string]*registry.Manifest, kept map[string]struct{}, rule *rules.RepoRule, now time.Time) error {
	referrers := map[string][]*registry.Manifest{}
	var queue []*registry.Manifest
	for _, m := range all {
		if subject := m.SubjectDigest(); subject != "" {
			referrers[subject] = append(referrers[subject], m)
		}
		if _, ok := kept[m.Digest]; ok {
			queue = append(queue, m)
		}
	}
	// follow keeps the referrers of the manifests queued since it last ran,
	// and theirs in turn.
	next := 0
	follow := func() error {
		for ; next < len(queue); next++ {
			for _, m := range referrers[queue[next].Digest] {
				if _, ok := kept[m.Digest]; ok || rule.DeleteOrphanedManifests && m.Orphaned {
					continue
				}
				p.Logger.Debug("Keeping referrer", "manifest", m, "subject", m.SubjectDigest())
				added, err := p.keepWithDependencies(m.Digest, manifests, kept, rule)
				if err != nil {
					return err
				}
				queue = append(queue, added...)
			}
		}
		return nil
	}
	if err := follow(); err != nil {
		return err
	}
	// A referrer no kept subject keeps is deleted, unless it is protected
	// itself — freshly pushed, say.
	for _, m := range all {
		if _, ok := kept[m.Digest]; ok || m.SubjectDigest() == "" || !p.protected(m, now) {
			continue
		}
		added, err := p.keepWithDependencies(m.Digest, manifests, kept, rule)
		if err != nil {
			return err
		}
		queue = append(queue, added...)
		if err := follow(); err != nil {
			return err
		}
	}
	return nil
}

// shouldKeep applies the first matching tagged/untagged rule, then the
// overrides: orphan deletion, and the protections of protected. Referrers
// never come through here — keepReferrers decides them.
func (p *Pruner) shouldKeep(m *registry.Manifest, evaluator *evaluator, rule *rules.RepoRule) bool {
	keep, sparedBy := evaluator.keep(m)

	if rule.DeleteOrphanedManifests && m.Orphaned {
		keep = false
	} else if sparedBy != "" {
		// Tagging a build twice, by branch and by commit say, and deleting
		// by only one of the two keeps the build; say why.
		p.Logger.Info("Keeping manifest: deleting it would also delete a tag the rules keep", "manifest", m, "tag", sparedBy)
	}

	return keep || p.protected(m, evaluator.now)
}

// protected reports whether a manifest must be kept whatever the rules say:
// because of its age (see protectedByAge), or because it is pinned.
func (p *Pruner) protected(m *registry.Manifest, now time.Time) bool {
	return p.protectedByAge(m, now) || p.pinned(m)
}

// pinned reports whether a manifest must be kept whatever the rules say for as
// long as it is running (Protect), or locked while IncludeLocked is not set.
// The locks of its tags count once Registry.LoadTagLocks has loaded them.
func (p *Pruner) pinned(m *registry.Manifest) bool {
	switch {
	case runningMatch(m, m.Repository, p.Protect):
		p.Logger.Info("Keeping running image", "manifest", m)
		return true
	case !p.IncludeLocked && m.IsLocked():
		p.Logger.Info("Keeping locked manifest; use --include-locked to delete it", "manifest", m)
		return true
	}
	return false
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
