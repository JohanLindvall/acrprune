package registry

import (
	"fmt"
	"log/slog"
	"strings"

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
// the pruner's bookkeeping.
type Manifest struct {
	OCIManifest
	Ref      string // repository@digest
	Size     uint64 // manifest document + config blob
	Azure    *azcontainerregistry.ManifestAttributes
	Orphaned bool // manifest is missing or has a broken dependency chain
	HasOwner bool // referenced by an index manifest
}

// MakeRef builds a repository@digest manifest reference.
func MakeRef(repository, digest string) string {
	return fmt.Sprintf("%s@%s", repository, digest)
}

// ParseRef splits a repository@digest manifest reference. Both results are
// empty when the reference is malformed.
func ParseRef(ref string) (repository, digest string) {
	if repo, dig, ok := strings.Cut(ref, "@"); ok {
		repository, digest = repo, dig
	}
	return
}

// Locked reports whether the manifest is protected from deletion, i.e. its
// delete or write attribute has been disabled.
func (m *Manifest) Locked() bool {
	if m.Azure == nil || m.Azure.ChangeableAttributes == nil {
		return false
	}
	c := m.Azure.ChangeableAttributes
	return (c.CanDelete != nil && !*c.CanDelete) || (c.CanWrite != nil && !*c.CanWrite)
}

// Tags returns the manifest's registry tags.
func (m *Manifest) Tags() []string {
	if m.Azure == nil {
		return nil
	}
	result := make([]string, 0, len(m.Azure.Tags))
	for _, tag := range m.Azure.Tags {
		result = append(result, *tag)
	}
	return result
}

// Architectures returns the known architectures of the manifest and, for an
// index, of the manifests it references.
func (m *Manifest) Architectures() []string {
	var result []string
	if m.Azure != nil && m.Azure.Architecture != nil && *m.Azure.Architecture != "unknown" {
		result = append(result, string(*m.Azure.Architecture))
	}
	for _, child := range m.Manifests {
		if child.Platform != nil && child.Platform.Architecture != "" && child.Platform.Architecture != "unknown" {
			result = append(result, child.Platform.Architecture)
		}
	}
	return result
}

// LogValue makes manifests log as a compact attribute group.
func (m *Manifest) LogValue() slog.Value {
	attrs := []slog.Attr{slog.String("ref", m.Ref)}
	if m.Azure != nil && m.Azure.LastUpdatedOn != nil {
		attrs = append(attrs, slog.Time("updated", *m.Azure.LastUpdatedOn))
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
