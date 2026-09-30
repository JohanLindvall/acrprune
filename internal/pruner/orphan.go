package pruner

import (
	"encoding/json"
	"fmt"

	"github.com/JohanLindvall/acrprune/internal/registry"
	"github.com/JohanLindvall/acrprune/internal/rules"
)

// markOrphans flags manifests whose dependency chain is broken, and records
// which manifests an index references.
//
// A manifest is orphaned when a manifest it references is missing or itself
// orphaned. The flag also propagates downwards — the debris of a broken index
// is itself unusable — but only to children *every* referencing index of which
// is orphaned: a child shared with a healthy index is still reachable through
// it and must survive.
func markOrphans(manifests map[string]*registry.Manifest) {
	owners := map[string]int{}
	brokenOwners := map[string]int{}
	parents := map[string][]*registry.Manifest{}
	var queue []*registry.Manifest
	mark := func(m *registry.Manifest) {
		if !m.Orphaned {
			m.Orphaned = true
			queue = append(queue, m)
		}
	}
	for _, m := range manifests {
		m.Orphaned, m.HasOwner = false, false
	}
	for _, m := range manifests {
		for _, child := range m.Manifests {
			digest := string(child.Digest)
			if dep, ok := manifests[digest]; ok {
				dep.HasOwner = true
				owners[digest]++
				parents[digest] = append(parents[digest], m)
			} else {
				mark(m)
			}
		}
		if m.Subject != nil {
			digest := string(m.Subject.Digest)
			if manifests[digest] != nil {
				parents[digest] = append(parents[digest], m)
			} else {
				mark(m)
			}
		}
	}
	// Each manifest is queued once and each edge visited a bounded number of
	// times, including deeply nested indexes and subject chains.
	for i := 0; i < len(queue); i++ {
		m := queue[i]
		for _, parent := range parents[m.Digest] {
			mark(parent)
		}
		for _, child := range m.Manifests {
			digest := string(child.Digest)
			brokenOwners[digest]++
			if dep := manifests[digest]; dep != nil && len(dep.Tags) == 0 && brokenOwners[digest] == owners[digest] {
				mark(dep)
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
		p.Logger.Warn("Orphaned manifest", "manifest", m.Ref(), "content", string(content))
	}
	if orphaned > 0 {
		return fmt.Errorf("%d manifest(s) orphaned", orphaned)
	}
	return nil
}
