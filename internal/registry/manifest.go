package registry

import (
	"log/slog"
	"slices"
	"strings"
	"time"

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

// Attributes are what a registry's listing reports about a manifest besides
// its content.
type Attributes struct {
	Digest string
	Tags   []string
	// LastUpdated is when the registry last updated the manifest, or the zero
	// time when it reported none: callers deciding what to delete must treat
	// that as "age unknown" rather than "infinitely old" — see
	// Manifest.HasTimestamp.
	LastUpdated time.Time
	// Architecture and OS are the platform of an image manifest, or empty
	// when unknown.
	Architecture string
	OS           string
	// Locked is set when deleting or overwriting the manifest is disabled
	// (ACR image locks; GHCR has none).
	Locked bool
	// ID is the backend's handle on the manifest where that is not the
	// digest: the package version id on GHCR.
	ID string
}

// Manifest pairs a downloaded OCI manifest with its registry attributes and
// the pruner's bookkeeping. Repository and Digest address it in the registry;
// the config and layer blobs it points at are not included in Size.
type Manifest struct {
	OCIManifest
	Attributes
	Repository string
	Size       uint64 // size of the manifest document itself
	Orphaned   bool   // manifest is missing or has a broken dependency chain
	HasOwner   bool   // referenced by an index manifest
}

// Ref returns the repository@digest reference identifying the manifest. It is
// for display and ordering; API calls address Repository and Digest directly.
func (m *Manifest) Ref() string {
	return m.Repository + "@" + m.Digest
}

// HasTimestamp reports whether the registry told us when the manifest was last
// updated. Every age-based rule is meaningless without it.
func (m *Manifest) HasTimestamp() bool {
	return !m.LastUpdated.IsZero()
}

// Architectures returns the distinct known architectures of the manifest and,
// for an index, of the manifests it references.
func (m *Manifest) Architectures() []string {
	var result []string
	add := addKnown(&result)
	add(m.Architecture)
	for _, child := range m.Manifests {
		if child.Platform != nil {
			add(child.Platform.Architecture)
		}
	}
	return result
}

// OperatingSystems returns the distinct known operating systems of the
// manifest and, for an index, of the manifests it references.
func (m *Manifest) OperatingSystems() []string {
	var result []string
	add := addKnown(&result)
	add(m.OS)
	for _, child := range m.Manifests {
		if child.Platform != nil {
			add(child.Platform.OS)
		}
	}
	return result
}

// addKnown appends platform values to *result, dropping empty and "unknown"
// entries (attestation manifests in an index report unknown/unknown) and
// duplicates.
func addKnown(result *[]string) func(string) {
	return func(value string) {
		if value != "" && value != "unknown" && !slices.Contains(*result, value) {
			*result = append(*result, value)
		}
	}
}

// LogValue makes manifests log as a compact attribute group.
func (m *Manifest) LogValue() slog.Value {
	attrs := []slog.Attr{slog.String("ref", m.Ref())}
	if m.HasTimestamp() {
		attrs = append(attrs, slog.Time("updated", m.LastUpdated))
	}
	if archs := m.Architectures(); len(archs) > 0 {
		attrs = append(attrs, slog.String("archs", strings.Join(archs, ",")))
	}
	if m.Orphaned {
		attrs = append(attrs, slog.Bool("orphaned", true))
	}
	if m.Locked {
		attrs = append(attrs, slog.Bool("locked", true))
	}
	if len(m.Tags) > 0 {
		attrs = append(attrs, slog.String("tags", strings.Join(m.Tags, ",")))
	}
	return slog.GroupValue(attrs...)
}
