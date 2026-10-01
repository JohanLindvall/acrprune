package registry

import (
	"maps"
	"slices"

	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

// resolveIndexPlatforms carries platform pairs up through indexes. OCI permits
// both nested indexes and omitted platform descriptors. Resolving only down
// from descriptors makes a valid index invisible to platform retention rules.
// Each distinct pair travels each edge at most once; the queue also terminates
// on cycles without recursion. Descriptor constraints remain authoritative.
func resolveIndexPlatforms(manifests map[string]*Manifest) {
	type platformKey struct {
		manifest *Manifest
		arch, os string
	}
	type parent struct {
		manifest *Manifest
		platform *v1.Platform
	}
	type resolved struct {
		manifest *Manifest
		platform v1.Platform
	}
	parents := make(map[string][]parent)
	seen := make(map[platformKey]bool, len(manifests))
	var queue []resolved
	add := func(m *Manifest, p v1.Platform) {
		if p.Architecture == "unknown" {
			p.Architecture = ""
		}
		if p.OS == "unknown" {
			p.OS = ""
		}
		key := platformKey{m, p.Architecture, p.OS}
		if p.Architecture == "" && p.OS == "" || seen[key] {
			return
		}
		seen[key] = true
		// Only these fields participate in retention; discard optional
		// features and variants so the derived set stays small.
		p = v1.Platform{Architecture: p.Architecture, OS: p.OS}
		m.platforms = append(m.platforms, p)
		queue = append(queue, resolved{m, p})
	}
	for _, digest := range slices.Sorted(maps.Keys(manifests)) {
		m := manifests[digest]
		m.platforms = []v1.Platform{}
		add(m, v1.Platform{Architecture: m.Architecture, OS: m.OS})
		for _, child := range m.Manifests {
			if child.Platform != nil {
				add(m, *child.Platform)
			}
			parents[string(child.Digest)] = append(parents[string(child.Digest)], parent{m, child.Platform})
		}
	}
	for i := 0; i < len(queue); i++ {
		item := queue[i]
		for _, owner := range parents[item.manifest.Digest] {
			p := item.platform
			if constraint := owner.platform; constraint != nil {
				// Merge only compatible pairs, never an OS from one
				// platform and an architecture from a conflicting one.
				if constraint.Architecture != "" && p.Architecture != "" && constraint.Architecture != p.Architecture ||
					constraint.OS != "" && p.OS != "" && constraint.OS != p.OS {
					continue
				}
				if constraint.Architecture != "" {
					p.Architecture = constraint.Architecture
				}
				if constraint.OS != "" {
					p.OS = constraint.OS
				}
			}
			add(owner.manifest, p)
		}
	}
}
