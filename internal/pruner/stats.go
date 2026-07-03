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
// them, i.e. images that appear in the running set.
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

// calculateStatsSeen summarizes manifests; blobs whose digest is already in
// seen count towards Total but not Unique.
func calculateStatsSeen(repository string, manifests []*registry.Manifest, seen map[string]struct{}) RepositoryStats {
	seen[""] = struct{}{}
	var unique, total uint64
	var tagged, untagged int
	var newest, oldest time.Time
	for _, m := range manifests {
		if len(m.Azure.Tags) > 0 {
			tagged++
		} else {
			untagged++
		}
		if newest.IsZero() || m.Azure.LastUpdatedOn.After(newest) {
			newest = *m.Azure.LastUpdatedOn
		}
		if oldest.IsZero() || m.Azure.LastUpdatedOn.Before(oldest) {
			oldest = *m.Azure.LastUpdatedOn
		}
		unique += m.Size
		total += m.Size
		if m.Config != nil {
			dig := string(m.Config.Digest)
			if _, ok := seen[dig]; !ok {
				seen[dig] = struct{}{}
				unique += uint64(m.Config.Size)
			}
			total += uint64(m.Config.Size)
		}
		for _, layer := range m.Layers {
			dig := string(layer.Digest)
			if _, ok := seen[dig]; !ok {
				seen[dig] = struct{}{}
				unique += uint64(layer.Size)
			}
			total += uint64(layer.Size)
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
