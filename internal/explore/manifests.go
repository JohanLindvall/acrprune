package explore

import (
	"fmt"
	"slices"
	"strings"

	"github.com/JohanLindvall/crprune/internal/registry"
	"github.com/dustin/go-humanize"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

func manifestKind(m *registry.Manifest) string {
	if m.SubjectDigest() != "" {
		return "referrer"
	}
	if len(m.Manifests) > 0 || m.MediaType == v1.MediaTypeImageIndex || m.MediaType == "application/vnd.docker.distribution.manifest.list.v2+json" {
		if len(m.Architectures()) > 1 {
			return "multiarch"
		}
		return "index"
	}
	if m.ArtifactType != "" {
		return "artifact"
	}
	return "image"
}

func platformName(p v1.Platform) string {
	if (p.OS == "" || p.OS == "unknown") && (p.Architecture == "" || p.Architecture == "unknown") {
		return ""
	}
	if p.OS == "" {
		p.OS = "unknown"
	}
	if p.Architecture == "" {
		p.Architecture = "unknown"
	}
	name := p.OS + "/" + p.Architecture
	if p.Variant != "" {
		name += "/" + p.Variant
	}
	return name
}

func manifestPlatforms(m *registry.Manifest) string {
	var platforms []string
	for p := range m.Platforms() {
		if name := platformName(p); name != "" {
			platforms = append(platforms, name)
		}
	}
	slices.Sort(platforms)
	platforms = slices.Compact(platforms)
	if len(platforms) == 0 {
		return "unknown platform"
	}
	return strings.Join(platforms, ", ")
}

func (a *app) manifestSummary(m *registry.Manifest) string {
	state := "tagged"
	if len(m.Tags) == 0 {
		state = "untagged"
	}
	parts := []string{state, manifestPlatforms(m)}
	if len(a.parents[m.Digest]) > 0 {
		parts = append(parts, "index child")
	}
	if m.IsLocked() {
		parts = append(parts, "locked")
	}
	return strings.Join(parts, " · ")
}

func (a *app) manifestDetails(m *registry.Manifest) []string {
	size := manifestBytes(m)
	lines := []string{a.repository + "@" + m.Digest, "Tags: " + tags(m.Tags), "Type: " + manifestKind(m),
		"Platforms: " + manifestPlatforms(m), "Updated: " + timestamp(m.LastUpdated),
		fmt.Sprintf("Own referenced size: %s (%d bytes)", humanize.Bytes(size), size),
		fmt.Sprintf("Manifest document: %d bytes; layers: %d", m.Size, len(m.Layers)),
		"Child manifests and their blobs are counted in their own rows.",
		"Media type: " + m.MediaType, fmt.Sprintf("Locked: %t", m.IsLocked())}
	if len(m.LockedTags) > 0 {
		lines = append(lines, "Locked tags: "+strings.Join(m.LockedTags, ", "))
	}
	if m.ArtifactType != "" {
		lines = append(lines, "Artifact type: "+m.ArtifactType)
	}
	if m.Config != nil {
		lines = append(lines, "Config: "+string(m.Config.Digest))
	}
	if subject := m.SubjectDigest(); subject != "" {
		lines = append(lines, "Subject: "+subject)
	}
	if len(m.Manifests) > 0 {
		lines = append(lines, "", fmt.Sprintf("INDEX CHILDREN (%d)", len(m.Manifests)))
		for _, child := range m.Manifests {
			digest := string(child.Digest)
			lines = append(lines, digest)
			if child.Platform != nil {
				if platform := platformName(*child.Platform); platform != "" {
					lines = append(lines, "  Descriptor platform: "+platform)
				}
			}
			if image := a.byDigest[digest]; image != nil {
				lines = append(lines, "  "+manifestKind(image)+" · "+a.manifestSummary(image), "  Tags: "+tags(image.Tags))
			} else {
				lines = append(lines, "  Not available in the repository listing")
			}
		}
	}
	if parents := a.parents[m.Digest]; len(parents) > 0 {
		lines = append(lines, "", fmt.Sprintf("PARENT INDEXES (%d)", len(parents)))
		for _, parent := range parents {
			lines = append(lines, parent.Digest, "  Tags: "+tags(parent.Tags)+" · "+manifestPlatforms(parent))
		}
	}
	return append(lines, "", "Deleting this manifest removes every tag above. Image indexes also select their children; a kept image's dependencies remain protected.")
}
