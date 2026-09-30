package pruner

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/JohanLindvall/acrprune/internal/progress"
	"github.com/JohanLindvall/acrprune/internal/registry"
	"github.com/JohanLindvall/acrprune/internal/rules"
)

// RepositoryStats summarizes one repository's manifests. Unique counts each
// blob once across the whole registry scan, charged to the first repository in
// alphabetical order that references it; Total counts every reference. The
// counts include the manifests the registry listed but could not serve, whose
// bytes are unknown.
type RepositoryStats struct {
	Name     string    `json:"name"`
	Unique   uint64    `json:"unique"`
	Total    uint64    `json:"total"`
	Shared   float64   `json:"shared"`
	Tagged   int       `json:"tagged"`
	Untagged int       `json:"untagged"`
	Count    int       `json:"count"`
	Newest   time.Time `json:"newest"`
	Oldest   time.Time `json:"oldest"`
	Running  int       `json:"running"`
}

// CollectRegistryStats gathers stats for every repository in the registry.
// runningRules, typically generated from a pod image list, mark manifests as
// running. onUpdate, if non-nil, receives the stats collected so far after
// each repository.
//
// Repositories the caller has no permission for (ABAC scoping), and ones that
// changed during inspection or hold a manifest in an unsupported format, are
// skipped; when any were, the collected stats are returned together with a
// non-nil error naming them, so partial results remain usable.
func CollectRegistryStats(ctx context.Context, reg *registry.Registry, runningRules []*rules.RepoRule, onUpdate func([]RepositoryStats) error) ([]RepositoryStats, error) {
	progress.Report(ctx, progress.Event{Kind: progress.Phase, Name: "Listing repositories"})
	repositories, err := reg.ListRepositories(ctx)
	if err != nil {
		return nil, err
	}
	progress.Report(ctx, progress.Event{Kind: progress.Candidates, Total: len(repositories)})

	logger := reg.Logger()
	stats := []RepositoryStats{}
	seen := map[string]struct{}{}
	var denied []string
	var skipped []error
	partial := func(err error) ([]RepositoryStats, error) {
		if len(stats) == 0 {
			return nil, err
		}
		return stats, err
	}
	for i, repository := range repositories {
		if err := ctx.Err(); err != nil {
			return partial(err)
		}
		progress.Report(ctx, progress.Event{Kind: progress.Repository, Name: repository})
		contents, found, err := reg.FetchRepositoryManifests(ctx, repository, registry.FetchOptions{IgnoreMissing: true})
		if err != nil {
			// Same tolerance as pruning: on an ABAC registry the catalog can
			// list repositories the caller cannot read. Skip them, keep the
			// partial statistics useful, and report at the end.
			if registry.IsPermissionError(err) {
				denied = append(denied, repository)
				progress.Report(ctx, progress.Event{Kind: progress.Finished, Outcome: progress.Denied})
				logger.Warn("Insufficient permission to scan repository; skipping",
					"repository", repository, "scanned", len(stats), "denied", len(denied),
					"remaining", len(repositories)-i-1, "err", err)
				continue
			}
			if skippable(err) {
				skipped = append(skipped, fmt.Errorf("%s: %w", repository, err))
				progress.Report(ctx, progress.Event{Kind: progress.Finished, Outcome: progress.Skipped})
				logger.Warn("Skipping repository that cannot be scanned reliably",
					"repository", repository, "scanned", len(stats), "skipped", len(skipped),
					"remaining", len(repositories)-i-1, "err", err)
				continue
			}
			return partial(fmt.Errorf("failed to scan repository %s: %w", repository, err))
		}
		if !found {
			progress.Report(ctx, progress.Event{Kind: progress.Finished, Outcome: progress.Skipped})
			continue
		}
		repoStats := calculateStatsSeen(repository, slices.Collect(maps.Values(contents.Manifests)), seen)
		countMissing(&repoStats, contents.Missing)
		repoStats.Running = countRunning(contents.Manifests, repository, runningRules)
		stats = append(stats, repoStats)
		progress.Report(ctx, progress.Event{Kind: progress.Plan, Kept: repoStats.Count, Bytes: repoStats.Unique})
		if onUpdate != nil {
			progress.Report(ctx, progress.Event{Kind: progress.Phase, Name: "Writing statistics snapshot"})
			if err := onUpdate(stats); err != nil {
				return nil, err
			}
		}
		progress.Report(ctx, progress.Event{Kind: progress.Finished})
	}

	var errs []error
	if len(denied) > 0 {
		errs = append(errs, fmt.Errorf("insufficient permission to scan %d of %d repositories: %s",
			len(denied), len(repositories), strings.Join(denied, ", ")))
	}
	if len(skipped) > 0 {
		errs = append(errs, fmt.Errorf("skipped %d of %d repositories that could not be scanned reliably: %w",
			len(skipped), len(repositories), errors.Join(skipped...)))
	}
	if len(errs) > 0 {
		return partial(errors.Join(errs...))
	}
	return stats, nil
}

// countMissing adds the manifests the registry listed but could not serve to
// the manifest counts. Their bytes are unknown.
func countMissing(stats *RepositoryStats, missing []registry.Attributes) {
	for _, attrs := range missing {
		stats.Count++
		if len(attrs.Tags) > 0 {
			stats.Tagged++
		} else {
			stats.Untagged++
		}
	}
}

// countRunning counts manifests whose first matching rule keeps them, i.e.
// images that appear in the running set. Only tags and digests are consulted —
// all that rules generated from an image list constrain.
func countRunning(manifests map[string]*registry.Manifest, repository string, ruleSet []*rules.RepoRule) int {
	running := 0
	for _, m := range manifests {
		if runningMatch(m, repository, ruleSet) {
			running++
		}
	}
	return running
}

// runningMatch reports whether the manifest's first matching rule keeps it.
// As in pruning, each tag of a tagged manifest gets the first rule matching
// it, and one tag kept is enough.
func runningMatch(m *registry.Manifest, repository string, ruleSet []*rules.RepoRule) bool {
	digest := []string{m.Digest}
	for _, rule := range ruleSet {
		if !rule.Repo.MatchString(repository) {
			continue
		}
		if len(m.Tags) > 0 {
			matched := false
			for _, tag := range m.Tags {
				for _, r := range rule.Tagged {
					if matchString(r.Tag, tag) && matchAny(r.Digest, digest) {
						if r.Keep {
							return true
						}
						matched = true
						break
					}
				}
			}
			if matched {
				return false
			}
		} else {
			for _, r := range rule.Untagged {
				if matchAny(r.Digest, digest) {
					return r.Keep
				}
			}
		}
	}
	return false
}

// calculateStats summarizes manifests without cross-repository blob
// deduplication.
func calculateStats(repository string, manifests []*registry.Manifest) RepositoryStats {
	return calculateStatsSeen(repository, manifests, map[string]struct{}{})
}

// calculateStatsSeen summarizes manifests, counting each blob — the manifest
// document itself, its config and its layers — towards Unique only the first
// time that digest is seen. Repeats still count towards Total, so Shared is
// the fraction of bytes a repository holds in common with what came before it:
// with repositories earlier in the scan, which is alphabetical, not with
// later ones.
func calculateStatsSeen(repository string, manifests []*registry.Manifest, seen map[string]struct{}) RepositoryStats {
	var unique, total uint64
	var tagged, untagged int
	var newest, oldest time.Time

	// count adds a blob, charging it to Unique unless its digest is a repeat.
	count := func(digest string, size uint64) {
		total += size
		if digest != "" {
			if _, repeat := seen[digest]; repeat {
				return
			}
			seen[digest] = struct{}{}
		}
		// A blob the manifest states no digest for cannot be shown to be a
		// duplicate, so it counts in full.
		unique += size
	}

	for _, m := range manifests {
		if len(m.Tags) > 0 {
			tagged++
		} else {
			untagged++
		}
		if updated := m.LastUpdated; m.HasTimestamp() {
			if newest.IsZero() || updated.After(newest) {
				newest = updated
			}
			if oldest.IsZero() || updated.Before(oldest) {
				oldest = updated
			}
		}

		count(m.Digest, m.Size)
		if m.Config != nil {
			count(string(m.Config.Digest), uint64(m.Config.Size))
		}
		for _, layer := range m.Layers {
			count(string(layer.Digest), uint64(layer.Size))
		}
	}

	shared := 0.0
	if total > 0 {
		shared = 1.0 - float64(unique)/float64(total)
	}

	return RepositoryStats{
		Name:     repository,
		Total:    total,
		Unique:   unique,
		Shared:   shared,
		Count:    len(manifests),
		Tagged:   tagged,
		Untagged: untagged,
		Newest:   newest,
		Oldest:   oldest,
	}
}
