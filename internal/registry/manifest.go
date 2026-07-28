package registry

import (
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/containers/azcontainerregistry"
	"github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

// OCIManifest holds the fields of an OCI image manifest or index document.
type OCIManifest struct {
	specs.Versioned
	MediaType    string            `json:"mediaType,omitempty"`
	ArtifactType string            `json:"artifactType,omitempty"`
	Config       *v1.Descriptor    `json:"config,omitempty"`
	Manifests    []v1.Descriptor   `json:"manifests,omitempty"`
	Layers       []v1.Descriptor   `json:"layers,omitempty"`
	Subject      *v1.Descriptor    `json:"subject,omitempty"`
	Annotations  map[string]string `json:"annotations,omitempty"`
}

// Manifest pairs a downloaded OCI manifest with its registry attributes and
// the pruner's bookkeeping. Repository and Digest address it in the registry;
// the config and layer blobs it points at are not included in Size.
type Manifest struct {
	OCIManifest
	Repository string
	Digest     string
	Size       uint64 // size of the manifest document itself
	Azure      *azcontainerregistry.ManifestAttributes
	Orphaned   bool // manifest is missing or has a broken dependency chain
	HasOwner   bool // referenced by an index manifest
}

// Ref returns the repository@digest reference identifying the manifest. It is
// for display and ordering; API calls address Repository and Digest directly.
func (m *Manifest) Ref() string {
	return m.Repository + "@" + m.Digest
}

// Locked reports whether the manifest is protected from deletion, i.e. its
// delete or write attribute has been disabled.
func (m *Manifest) Locked() bool {
	if m.Azure == nil || m.Azure.ChangeableAttributes == nil {
		return false
	}
	return locked(m.Azure.ChangeableAttributes.CanDelete, m.Azure.ChangeableAttributes.CanWrite)
}

// locked reports whether a set of changeable attributes protects an artifact
// from deletion. Manifests and tags carry the same flags in distinct types.
func locked(canDelete, canWrite *bool) bool {
	return canDelete != nil && !*canDelete || canWrite != nil && !*canWrite
}

// Tags returns the manifest's registry tags.
func (m *Manifest) Tags() []string {
	if m.Azure == nil {
		return nil
	}
	result := make([]string, 0, len(m.Azure.Tags))
	for _, tag := range m.Azure.Tags {
		if tag != nil {
			result = append(result, *tag)
		}
	}
	return result
}

// LastUpdated returns when the registry last updated the manifest. The result
// is the zero time when the registry reported no timestamp; callers deciding
// what to delete must treat that as "age unknown" rather than "infinitely
// old" — see Manifest.HasTimestamp.
func (m *Manifest) LastUpdated() time.Time {
	if m.Azure == nil || m.Azure.LastUpdatedOn == nil {
		return time.Time{}
	}
	return *m.Azure.LastUpdatedOn
}

// HasTimestamp reports whether the registry told us when the manifest was last
// updated. Every age-based rule is meaningless without it.
func (m *Manifest) HasTimestamp() bool {
	return m.Azure != nil && m.Azure.LastUpdatedOn != nil && !m.Azure.LastUpdatedOn.IsZero()
}

// Architectures returns the distinct known architectures of the manifest and,
// for an index, of the manifests it references.
func (m *Manifest) Architectures() []string {
	var result []string
	add := func(arch string) {
		if arch != "" && arch != "unknown" && !slices.Contains(result, arch) {
			result = append(result, arch)
		}
	}
	if m.Azure != nil && m.Azure.Architecture != nil {
		add(string(*m.Azure.Architecture))
	}
	for _, child := range m.Manifests {
		if child.Platform != nil {
			add(child.Platform.Architecture)
		}
	}
	return result
}

// LogValue makes manifests log as a compact attribute group.
func (m *Manifest) LogValue() slog.Value {
	attrs := []slog.Attr{slog.String("ref", m.Ref())}
	if m.HasTimestamp() {
		attrs = append(attrs, slog.Time("updated", m.LastUpdated()))
	}
	if archs := m.Architectures(); len(archs) > 0 {
		attrs = append(attrs, slog.String("archs", strings.Join(archs, ",")))
	}
	if m.Orphaned {
		attrs = append(attrs, slog.Bool("orphaned", true))
	}
	if m.Locked() {
		attrs = append(attrs, slog.Bool("locked", true))
	}
	if tags := m.Tags(); len(tags) > 0 {
		attrs = append(attrs, slog.String("tags", strings.Join(tags, ",")))
	}
	return slog.GroupValue(attrs...)
}
