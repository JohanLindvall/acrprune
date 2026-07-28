package registry

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/containers/azcontainerregistry"
)

// acceptedManifestTypes are the manifest media types we can decode.
var acceptedManifestTypes = "application/vnd.oci.image.index.v1+json," +
	string(azcontainerregistry.ContentTypeApplicationVndDockerDistributionManifestV2JSON) + "," +
	string(azcontainerregistry.ContentTypeApplicationVndOciImageManifestV1JSON)

// FetchRepositoryManifests downloads every manifest in the repository, keyed
// by digest. found is false when the repository itself does not exist and
// ignoreMissing is set.
func (r *Registry) FetchRepositoryManifests(ctx context.Context, repository string, ignoreMissing bool) (manifests map[string]*Manifest, found bool, err error) {
	group, groupCtx := r.group(ctx)

	// Page through the manifest attributes on this goroutine, downloading each
	// manifest document in the background as its digest becomes known. Paging
	// uses groupCtx so a failed download stops the listing too.
	pager := r.client.NewListManifestsPager(repository, &azcontainerregistry.ClientListManifestsOptions{
		OrderBy: to.Ptr(azcontainerregistry.ArtifactManifestOrderByNone),
		MaxNum:  to.Ptr(r.pageSize),
	})

	var mu sync.Mutex
	manifests = map[string]*Manifest{}
	attributes := map[string]*azcontainerregistry.ManifestAttributes{}

	for page := 0; pager.More(); page++ {
		r.logger.Debug("Fetching manifest attributes", "page", page, "repository", repository)
		attributePage, pageErr := pager.NextPage(groupCtx)
		if pageErr != nil {
			// A download failure cancels groupCtx and so surfaces here as
			// well; report the original error from Wait in that case.
			if waitErr := group.Wait(); waitErr != nil {
				return nil, false, waitErr
			}
			if ignoreMissing && hasStatus(pageErr, http.StatusNotFound) {
				r.logger.Warn("Repository missing", "repository", repository)
				return nil, false, nil
			}
			return nil, false, fmt.Errorf("failed to list manifests for %s: %w", repository, pageErr)
		}
		for _, attrs := range attributePage.Attributes {
			if attrs == nil || attrs.Digest == nil {
				return nil, false, fmt.Errorf("registry returned a manifest without a digest for %s", repository)
			}
			digest := *attrs.Digest
			attributes[digest] = attrs
			group.Go(func() error {
				m, err := r.fetchManifest(groupCtx, repository, digest, ignoreMissing)
				if err != nil || m == nil {
					return err
				}
				mu.Lock()
				defer mu.Unlock()
				manifests[digest] = m
				return nil
			})
		}
	}
	if err := group.Wait(); err != nil {
		return nil, false, err
	}

	for digest, m := range manifests {
		m.Azure = attributes[digest]
	}

	return manifests, true, nil
}

// fetchManifest downloads (or loads from cache) a single manifest document.
// It returns nil without error when the manifest is missing and ignoreMissing
// is set.
func (r *Registry) fetchManifest(ctx context.Context, repository, digest string, ignoreMissing bool) (*Manifest, error) {
	ref := repository + "@" + digest
	raw := r.cache.Get(digest)

	if raw == nil {
		r.logger.Debug("Downloading manifest", "manifest", ref)
		res, err := r.client.GetManifest(ctx, repository, digest, &azcontainerregistry.ClientGetManifestOptions{Accept: &acceptedManifestTypes})
		if err != nil {
			if ignoreMissing && hasStatus(err, http.StatusNotFound) {
				r.logger.Warn("Manifest missing", "manifest", ref)
				return nil, nil
			}
			return nil, fmt.Errorf("failed to get manifest %s: %w", ref, err)
		}
		if res.DockerContentDigest == nil {
			return nil, fmt.Errorf("manifest %s: registry returned no content digest to validate against", ref)
		}
		reader, err := azcontainerregistry.NewDigestValidationReader(*res.DockerContentDigest, res.ManifestData)
		if err != nil {
			return nil, fmt.Errorf("failed to validate manifest %s: %w", ref, err)
		}
		if raw, err = io.ReadAll(reader); err != nil {
			return nil, fmt.Errorf("failed to read manifest %s: %w", ref, err)
		}
		r.cache.Put(digest, raw)
	}

	return parseManifest(repository, digest, raw)
}

// parseManifest decodes a downloaded manifest document.
//
// Size covers the document itself and nothing else: the config and layer blobs
// it points at are separate blobs, shared with other manifests, and are
// accounted for by whoever walks the descriptors. Folding the config size in
// here made every consumer that adds it double-count.
func parseManifest(repository, digest string, raw []byte) (*Manifest, error) {
	result := &Manifest{Repository: repository, Digest: digest, Size: uint64(len(raw))}
	if err := json.Unmarshal(raw, &result.OCIManifest); err != nil {
		return nil, fmt.Errorf("failed to decode manifest %s@%s: %w", repository, digest, err)
	}
	if result.SchemaVersion != 2 {
		return nil, fmt.Errorf("manifest %s@%s: unsupported schema version %d", repository, digest, result.SchemaVersion)
	}
	return result, nil
}
