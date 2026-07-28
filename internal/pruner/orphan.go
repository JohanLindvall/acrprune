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
	owners := map[string][]*registry.Manifest{} // digest -> indexes referencing it
	for _, m := range manifests {
		for _, child := range m.Manifests {
			digest := string(child.Digest)
			if dep, ok := manifests[digest]; ok {
				dep.HasOwner = true
				owners[digest] = append(owners[digest], m)
			}
		}
	}

	// Both rules only ever turn the flag on, so this reaches a fixpoint.
	for changed := true; changed; {
		changed = false
		for digest, m := range manifests {
			if m.Orphaned {
				continue
			}
			if hasBrokenChild(m, manifests) || onlyOrphanedOwners(owners[digest]) {
				m.Orphaned = true
				changed = true
			}
		}
	}
}

// hasBrokenChild reports whether the manifest references one that is missing
// from the repository or already known to be orphaned.
func hasBrokenChild(m *registry.Manifest, manifests map[string]*registry.Manifest) bool {
	for _, child := range m.Manifests {
		dep, ok := manifests[string(child.Digest)]
		if !ok || dep.Orphaned {
			return true
		}
	}
	return false
}

// onlyOrphanedOwners reports whether a manifest is reachable solely through
// orphaned indexes. An unreferenced manifest is a root in its own right and is
// never orphaned by this rule.
func onlyOrphanedOwners(owners []*registry.Manifest) bool {
	if len(owners) == 0 {
		return false
	}
	for _, owner := range owners {
		if !owner.Orphaned {
			return false
		}
	}
	return true
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
