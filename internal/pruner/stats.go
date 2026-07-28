package pruner

import (
	"context"
	"maps"
	"slices"
	"time"

	"github.com/JohanLindvall/acrprune/internal/registry"
	"github.com/JohanLindvall/acrprune/internal/rules"
)

// RepositoryStats summarizes one repository's manifests. Unique counts each
// blob once across the whole registry scan; Total counts every reference.
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
func CollectRegistryStats(ctx context.Context, reg *registry.Registry, runningRules []*rules.RepoRule, onUpdate func([]RepositoryStats) error) ([]RepositoryStats, error) {
	repositories, err := reg.ListRepositories(ctx)
	if err != nil {
		return nil, err
	}

	stats := []RepositoryStats{}
	seen := map[string]struct{}{}
	for _, repository := range repositories {
		manifests, found, err := reg.FetchRepositoryManifests(ctx, repository, true)
		if err != nil {
			return nil, err
		}
		if !found {
			continue
		}
		repoStats := calculateStatsSeen(repository, slices.Collect(maps.Values(manifests)), seen)
		repoStats.Running = countRunning(manifests, repository, runningRules)
		stats = append(stats, repoStats)
		if onUpdate != nil {
			if err := onUpdate(stats); err != nil {
				return nil, err
			}
		}
	}

	return stats, nil
}

// countRunning counts tagged manifests whose first matching tag rule keeps
// them, i.e. images that appear in the running set. Only the tag is consulted,
// which is all the rules generated from an image list constrain.
func countRunning(manifests map[string]*registry.Manifest, repository string, ruleSet []*rules.RepoRule) int {
	running := 0
	for _, m := range manifests {
		tags := m.Tags()
		if len(tags) == 0 {
			continue
		}
	match:
		for _, rule := range ruleSet {
			if !rule.Repo.MatchString(repository) {
				continue
			}
			for _, taggedRule := range rule.Tagged {
				if matchAny(taggedRule.Tag, tags) {
					if taggedRule.Keep {
						running++
					}
					break match
				}
			}
		}
	}
	return running
}

// calculateStats summarizes manifests without cross-repository blob
// deduplication.
func calculateStats(repository string, manifests []*registry.Manifest) RepositoryStats {
	return calculateStatsSeen(repository, manifests, map[string]struct{}{})
}

// calculateStatsSeen summarizes manifests, counting each blob — the manifest
// document itself, its config and its layers — towards Unique only the first
// time that digest is seen. Repeats still count towards Total, so Shared is
// the fraction of bytes a repository holds in common with what came before it.
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
		if len(m.Tags()) > 0 {
			tagged++
		} else {
			untagged++
		}
		if updated := m.LastUpdated(); m.HasTimestamp() {
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
