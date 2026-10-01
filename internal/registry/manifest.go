package registry

import (
	"iter"
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
	// TagSubject is the digest of the manifest this one is a referrer of by
	// naming convention rather than by an OCI subject: cosign tags the
	// signatures, attestations and SBOMs of <alg>:<hex> as <alg>-<hex>.sig,
	// .att and .sbom, and the OCI referrers tag schema tags the index of its
	// referrers <alg>-<hex>. FetchRepositoryManifests sets it only when every
	// tag names the same manifest listed in the repository; it is empty
	// otherwise, and always when Subject is set. See SubjectDigest.
	TagSubject string
	// LockedTags are the manifest's tags that are locked against deletion or
	// overwriting, as loaded by Registry.LoadTagLocks: nil until loaded, and
	// empty but not nil once loaded when none is.
	LockedTags []string
	Size       uint64 // size of the manifest document itself
	Orphaned   bool   // manifest is missing or has a broken dependency chain
	HasOwner   bool   // referenced by an index manifest
	// platforms is the resolved set of OS/architecture pairs, including
	// those inherited through nested indexes. nil means not resolved yet.
	platforms []v1.Platform
}

// Ref returns the repository@digest reference identifying the manifest. It is
// for display and ordering; API calls address Repository and Digest directly.
func (m *Manifest) Ref() string {
	return m.Repository + "@" + m.Digest
}

// SubjectDigest returns the digest of the manifest this one refers to, when it
// is a referrer such as a signature, attestation or SBOM: its OCI subject, or
// else the subject its tags name (TagSubject). It returns "" for a manifest
// that refers to none.
func (m *Manifest) SubjectDigest() string {
	if m.Subject != nil {
		return string(m.Subject.Digest)
	}
	return m.TagSubject
}

// IsLocked reports whether deleting the manifest is prevented by a lock on the
// manifest itself or on one of its tags. Tag locks are known only once
// Registry.LoadTagLocks has loaded them.
func (m *Manifest) IsLocked() bool {
	return m.Locked || len(m.LockedTags) > 0
}

// HasTimestamp reports whether the registry told us when the manifest was last
// updated. Every age-based rule is meaningless without it.
func (m *Manifest) HasTimestamp() bool {
	return !m.LastUpdated.IsZero()
}

// Platforms yields the manifest's platform and its index entries' platforms,
// including platforms resolved from children whose descriptors omitted them.
func (m *Manifest) Platforms() iter.Seq[v1.Platform] {
	return func(yield func(v1.Platform) bool) {
		if m.platforms != nil {
			for _, platform := range m.platforms {
				if !yield(platform) {
					return
				}
			}
			return
		}
		if !yield(v1.Platform{Architecture: m.Architecture, OS: m.OS}) {
			return
		}
		for _, child := range m.Manifests {
			if child.Platform != nil && !yield(*child.Platform) {
				return
			}
		}
	}
}

// Architectures returns the distinct known architectures of the manifest and,
// for an index, of the manifests it references.
func (m *Manifest) Architectures() []string {
	var result []string
	add := addKnown(&result)
	for platform := range m.Platforms() {
		add(platform.Architecture)
	}
	return result
}

// OperatingSystems returns the distinct known operating systems of the
// manifest and, for an index, of the manifests it references.
func (m *Manifest) OperatingSystems() []string {
	var result []string
	add := addKnown(&result)
	for platform := range m.Platforms() {
		add(platform.OS)
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
	if m.IsLocked() {
		attrs = append(attrs, slog.Bool("locked", true))
	}
	if len(m.Tags) > 0 {
		attrs = append(attrs, slog.String("tags", strings.Join(m.Tags, ",")))
	}
	return slog.GroupValue(attrs...)
}
