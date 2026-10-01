package registry

import (
	"context"
	_ "crypto/sha256" // digest algorithms go-digest verifies with
	_ "crypto/sha512"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"regexp"
	"slices"
	"strings"
	"sync"

	godigest "github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/JohanLindvall/acrprune/internal/imageref"
	"github.com/JohanLindvall/acrprune/internal/progress"
)

// ManifestMediaTypes is the Accept header for manifest downloads, listing the
// manifest and index formats parseManifest can decode.
const ManifestMediaTypes = v1.MediaTypeImageIndex + "," +
	v1.MediaTypeImageManifest + "," +
	"application/vnd.docker.distribution.manifest.list.v2+json," +
	"application/vnd.docker.distribution.manifest.v2+json"

// imageConfigTypes are the config media types that record an image's
// platform.
var imageConfigTypes = []string{
	v1.MediaTypeImageConfig,
	"application/vnd.docker.container.image.v1+json",
}

// tagSchemePattern matches the tags naming the referrers of the manifest with
// digest <alg>:<hex>: cosign's <alg>-<hex>.sig, .att and .sbom for signatures,
// attestations and SBOMs, and the OCI referrers tag schema's <alg>-<hex> for
// the index listing its referrers on registries without the referrers API.
var tagSchemePattern = regexp.MustCompile(`^(sha256-[0-9a-f]{64}|sha512-[0-9a-f]{128})(?:\.(?:sig|att|sbom))?$`)

// MaxDocumentSize bounds the manifests and image configs read into memory.
// Far beyond anything a registry accepts, it only guards against a runaway
// response.
const MaxDocumentSize = 16 << 20

// ReadDocument reads a manifest or config document to the end, failing when
// it exceeds MaxDocumentSize.
func ReadDocument(r io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, MaxDocumentSize+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxDocumentSize {
		return nil, fmt.Errorf("document exceeds %d bytes", MaxDocumentSize)
	}
	return data, nil
}

// FetchOptions tunes FetchRepositoryManifests.
type FetchOptions struct {
	// IgnoreMissing tolerates a missing repository or manifest instead of
	// failing on it.
	IgnoreMissing bool
	// Platforms asks for the platform of every image manifest. It is read
	// from the image config when neither the registry's listing nor an index
	// referencing the manifest reports it — which on GHCR, whose listings
	// carry no platforms, costs a download per single-platform image.
	Platforms bool
}

// Contents is what FetchRepositoryManifests found in a repository.
type Contents struct {
	// Manifests are the downloaded manifests, keyed by digest.
	Manifests map[string]*Manifest
	// Missing are the manifests the registry listed but could not serve,
	// ignored as FetchOptions.IgnoreMissing asks, sorted by digest. Nothing
	// is known about what they are, so they must be left alone — and with
	// them the repository.
	Missing []Attributes
}

// FetchRepositoryManifests downloads every manifest in the repository. found
// is false when the repository itself does not exist and missing manifests
// are ignored. A manifest listed twice fails with ErrRepositoryChanged, and a
// manifest in a format that cannot be decoded with ErrUnsupportedManifest.
//
// Manifests without an OCI subject whose tags all name another manifest of the
// repository by the cosign or OCI referrers tag scheme get it as TagSubject.
func (r *Registry) FetchRepositoryManifests(ctx context.Context, repository string, opts FetchOptions) (contents Contents, found bool, err error) {
	if err := ctx.Err(); err != nil {
		return Contents{}, false, err
	}
	if !imageref.ValidRepository(repository) {
		return Contents{}, false, fmt.Errorf("invalid repository name %q", repository)
	}
	fetchCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	group, groupCtx := r.group(fetchCtx)

	// List on this goroutine, downloading each manifest document in the
	// background as its digest becomes known. Listing uses groupCtx so a
	// failed download stops it too.
	var mu sync.Mutex
	manifests := map[string]*Manifest{}
	var missing []Attributes
	listed := 0
	seen := map[string]struct{}{}
	listErr := r.backend.ListManifests(groupCtx, repository, func(attrs Attributes) error {
		if err := groupCtx.Err(); err != nil {
			return err
		}
		if listed%100 == 0 {
			r.logger.Debug("Fetching manifest attributes", "repository", repository, "listed", listed)
		}
		listed++
		progress.Report(ctx, progress.Event{Kind: progress.Listed, Count: 1})
		if _, err := godigest.Parse(attrs.Digest); err != nil {
			return fmt.Errorf("listing returned an invalid manifest digest %q: %w", attrs.Digest, err)
		}
		if _, duplicate := seen[attrs.Digest]; duplicate {
			return fmt.Errorf("%w: manifest %s@%s was listed more than once, as happens when the repository changes during pagination; rerun the command", ErrRepositoryChanged, repository, attrs.Digest)
		}
		seen[attrs.Digest] = struct{}{}
		attrs.Tags = slices.Clone(attrs.Tags)
		group.Go(func() error {
			m, err := r.fetchManifest(groupCtx, repository, attrs.Digest, opts.IgnoreMissing)
			if err != nil {
				return err
			}
			progress.Report(ctx, progress.Event{Kind: progress.Fetched, Count: 1})
			mu.Lock()
			defer mu.Unlock()
			if m == nil {
				missing = append(missing, attrs)
				return nil
			}
			m.Attributes = attrs
			manifests[attrs.Digest] = m
			return nil
		})
		return nil
	})
	if listErr != nil {
		// A download failure cancels groupCtx and so surfaces from listing as
		// well; report the original error from Wait in that case.
		cancel()
		if waitErr := group.Wait(); waitErr != nil && errors.Is(listErr, context.Canceled) {
			return Contents{}, false, waitErr
		}
		if opts.IgnoreMissing && IsNotFound(listErr) {
			r.logger.Warn("Repository missing", "repository", repository, "err", listErr)
			return Contents{}, false, nil
		}
		return Contents{}, false, fmt.Errorf("failed to list manifests for %s: %w", repository, listErr)
	}
	if err := group.Wait(); err != nil {
		return Contents{}, false, err
	}

	r.deriveTagSubjects(manifests, seen)
	if opts.Platforms {
		progress.Report(ctx, progress.Event{Kind: progress.Phase, Name: "Resolving image platforms"})
	}
	if err := r.resolvePlatforms(ctx, repository, manifests, opts.Platforms); err != nil {
		return Contents{}, false, err
	}
	slices.SortFunc(missing, func(a, b Attributes) int { return strings.Compare(a.Digest, b.Digest) })
	return Contents{Manifests: manifests, Missing: missing}, true, nil
}

// deriveTagSubjects sets the TagSubject of the manifests without an OCI
// subject whose every tag names, by the cosign or OCI referrers tag scheme, one
// and the same other manifest listed in the repository. A manifest naming a
// manifest that is not listed — its signatures kept in a separate
// COSIGN_REPOSITORY, or already dangling — stays an ordinary tagged manifest.
func (r *Registry) deriveTagSubjects(manifests map[string]*Manifest, listed map[string]struct{}) {
	for _, m := range manifests {
		if m.Subject != nil {
			continue
		}
		subject := tagSchemeSubject(m.Tags)
		if subject == "" || subject == m.Digest {
			continue
		}
		if _, ok := listed[subject]; !ok {
			r.logger.Debug("Tag-scheme subject not in repository; treating as an ordinary tagged manifest", "manifest", m, "subject", subject)
			continue
		}
		r.logger.Debug("Treating tag-scheme manifest as a referrer of its subject", "manifest", m, "subject", subject)
		m.TagSubject = subject
	}
}

// tagSchemeSubject returns the digest every one of tags names by the cosign or
// OCI referrers tag scheme, or "" when there is no such digest.
func tagSchemeSubject(tags []string) string {
	subject := ""
	for _, tag := range tags {
		match := tagSchemePattern.FindStringSubmatch(tag)
		if match == nil {
			return ""
		}
		digest := strings.Replace(match[1], "-", ":", 1)
		if subject != "" && digest != subject {
			return ""
		}
		subject = digest
	}
	return subject
}

// fetchManifest downloads (or loads from cache) a single manifest document.
// It returns nil without error when the manifest is missing and ignoreMissing
// is set.
func (r *Registry) fetchManifest(ctx context.Context, repository, digest string, ignoreMissing bool) (*Manifest, error) {
	ref := repository + "@" + digest
	raw, err := r.fetchDocument(ctx, digest, func() ([]byte, error) {
		r.logger.Debug("Downloading manifest", "manifest", ref)
		return r.backend.GetManifest(ctx, repository, digest)
	}, func(raw []byte) error {
		return checkFormat(ref, raw)
	})
	if err != nil {
		if errors.Is(err, ErrUnsupportedManifest) {
			return nil, err
		}
		if ignoreMissing && IsNotFound(err) {
			r.logger.Warn("Manifest missing", "manifest", ref)
			return nil, nil
		}
		return nil, fmt.Errorf("failed to get manifest %s: %w", ref, err)
	}
	return parseManifest(repository, digest, raw)
}

// fetchDocument returns the cached document for digest, or downloads it,
// verifies it and caches it. A cached document that fails verification is
// downloaded afresh. check, when set, vets a download before verification.
func (r *Registry) fetchDocument(ctx context.Context, digest string, download func() ([]byte, error), check func([]byte) error) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if raw := r.cache.Get(digest); raw != nil && verify(digest, raw) == nil {
		progress.Report(ctx, progress.Event{Kind: progress.Cached, Count: 1})
		return raw, nil
	}
	raw, err := download()
	if err != nil {
		return nil, err
	}
	if len(raw) > MaxDocumentSize {
		return nil, fmt.Errorf("document exceeds %d bytes", MaxDocumentSize)
	}
	if check != nil {
		if err := check(raw); err != nil {
			return nil, err
		}
	}
	// Verify against the digest that was asked for, whatever the server
	// claims: the content is cached under that digest.
	if err := verify(digest, raw); err != nil {
		return nil, err
	}
	if err := r.cache.Put(digest, raw); err != nil {
		r.cacheWarning.Do(func() {
			r.logger.Warn("Failed to write to the cache; further failures are not reported", "err", err)
		})
	}
	return raw, nil
}

// verify checks that content hashes to digest.
func verify(digest string, content []byte) error {
	d, err := godigest.Parse(digest)
	if err != nil {
		return err
	}
	if actual := d.Algorithm().FromBytes(content); actual != d {
		return fmt.Errorf("registry returned content with digest %s", actual)
	}
	return nil
}

// checkFormat rejects a manifest document in a format parseManifest cannot
// decode, with ErrUnsupportedManifest. It runs before digest verification: the
// digest of a signed Docker schema 1 manifest covers its payload without the
// signatures, so such a manifest would otherwise read as tampered content.
// Anything that is not JSON is left to verification and decoding to report.
func checkFormat(ref string, raw []byte) error {
	var head struct {
		SchemaVersion int    `json:"schemaVersion"`
		MediaType     string `json:"mediaType"`
	}
	if json.Unmarshal(raw, &head) != nil {
		return nil
	}
	return formatError(ref, head.SchemaVersion, head.MediaType)
}

// formatError returns an ErrUnsupportedManifest for a manifest of an
// undecodable schema version or media type, and nil for a supported one.
func formatError(ref string, schemaVersion int, mediaType string) error {
	switch {
	case schemaVersion == 1:
		return fmt.Errorf("manifest %s: %w: schema version 1 (Docker image manifest schema 1)", ref, ErrUnsupportedManifest)
	case schemaVersion != 2:
		return fmt.Errorf("manifest %s: %w: schema version %d", ref, ErrUnsupportedManifest, schemaVersion)
	case mediaType != "" && !slices.Contains(strings.Split(ManifestMediaTypes, ","), mediaType):
		return fmt.Errorf("manifest %s: %w: media type %q", ref, ErrUnsupportedManifest, mediaType)
	}
	return nil
}

// parseManifest decodes a downloaded manifest document.
//
// Size covers the document itself and nothing else: the config and layer blobs
// it points at are separate blobs, shared with other manifests, and are
// accounted for by whoever walks the descriptors. Folding the config size in
// here made every consumer that adds it double-count.
func parseManifest(repository, digest string, raw []byte) (*Manifest, error) {
	result := &Manifest{Repository: repository, Attributes: Attributes{Digest: digest}, Size: uint64(len(raw))}
	if err := json.Unmarshal(raw, &result.OCIManifest); err != nil {
		return nil, fmt.Errorf("failed to decode manifest %s@%s: %w", repository, digest, err)
	}
	if err := formatError(result.Ref(), result.SchemaVersion, result.MediaType); err != nil {
		return nil, err
	}
	check := func(desc v1.Descriptor) error {
		if err := desc.Digest.Validate(); err != nil {
			return fmt.Errorf("invalid descriptor digest %q: %w", desc.Digest, err)
		}
		if desc.Size < 0 {
			return fmt.Errorf("negative descriptor size for %s", desc.Digest)
		}
		return nil
	}
	for _, descriptors := range [][]v1.Descriptor{result.Manifests, result.Layers} {
		for _, desc := range descriptors {
			if err := check(desc); err != nil {
				return nil, fmt.Errorf("manifest %s@%s: %w", repository, digest, err)
			}
		}
	}
	for _, desc := range []*v1.Descriptor{result.Config, result.Subject} {
		if desc != nil {
			if err := check(*desc); err != nil {
				return nil, fmt.Errorf("manifest %s@%s: %w", repository, digest, err)
			}
		}
	}
	return result, nil
}

// resolvePlatforms fills in the platform of manifests the listing reported
// none for: from the descriptor of an index referencing them, which is free,
// and, when fromConfig is set and the backend can download blobs, from their
// image config.
func (r *Registry) resolvePlatforms(ctx context.Context, repository string, manifests map[string]*Manifest, fromConfig bool) error {
	// Sorted, so a child two indexes disagree about resolves the same way on
	// every run.
	for _, digest := range slices.Sorted(maps.Keys(manifests)) {
		for _, child := range manifests[digest].Manifests {
			m := manifests[string(child.Digest)]
			if m != nil && child.Platform != nil {
				fillPlatform(m, *child.Platform)
			}
		}
	}

	blobs, ok := r.backend.(BlobGetter)
	if !fromConfig || !ok {
		resolveIndexPlatforms(manifests)
		return nil
	}
	// Images built alike share a config; download each config once.
	waiting := map[string][]*Manifest{}
	for _, m := range manifests {
		if (m.Architecture == "" || m.OS == "") && m.Config != nil && slices.Contains(imageConfigTypes, m.Config.MediaType) {
			config := string(m.Config.Digest)
			waiting[config] = append(waiting[config], m)
		}
	}
	group, groupCtx := r.group(ctx)
	for config, images := range waiting {
		group.Go(func() error {
			platform, err := r.fetchPlatform(groupCtx, blobs, repository, config)
			if err != nil {
				return err
			}
			for _, m := range images {
				fillPlatform(m, platform)
			}
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		return err
	}
	resolveIndexPlatforms(manifests)
	return nil
}

// Fill only absent platform fields, and only from a compatible descriptor.
// Mixing conflicting reports can invent an OS/architecture combination.
func fillPlatform(m *Manifest, platform v1.Platform) {
	if m.Architecture != "" && platform.Architecture != "" && m.Architecture != platform.Architecture ||
		m.OS != "" && platform.OS != "" && m.OS != platform.OS {
		return
	}
	if m.Architecture == "" {
		m.Architecture = platform.Architecture
	}
	if m.OS == "" {
		m.OS = platform.OS
	}
}

// fetchPlatform reads an image's platform from its config blob. A missing
// config leaves the platform unknown rather than failing: rules then treat the
// image as matching no platform.
func (r *Registry) fetchPlatform(ctx context.Context, blobs BlobGetter, repository, digest string) (v1.Platform, error) {
	raw, err := r.fetchDocument(ctx, digest, func() ([]byte, error) {
		r.logger.Debug("Downloading image config", "repository", repository, "digest", digest)
		return blobs.GetBlob(ctx, repository, digest)
	}, nil)
	if err != nil {
		if IsNotFound(err) {
			r.logger.Warn("Image config missing; platform unknown", "repository", repository, "digest", digest)
			return v1.Platform{}, nil
		}
		return v1.Platform{}, fmt.Errorf("failed to get image config %s@%s: %w", repository, digest, err)
	}
	var config struct {
		Architecture string `json:"architecture"`
		OS           string `json:"os"`
	}
	if err := json.Unmarshal(raw, &config); err != nil {
		return v1.Platform{}, fmt.Errorf("failed to decode image config %s@%s: %w", repository, digest, err)
	}
	return v1.Platform{Architecture: config.Architecture, OS: config.OS}, nil
}
