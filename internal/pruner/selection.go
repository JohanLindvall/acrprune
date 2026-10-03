package pruner

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"

	"github.com/JohanLindvall/crprune/internal/imageref"
	"github.com/JohanLindvall/crprune/internal/registry"
	"github.com/JohanLindvall/crprune/internal/rules"
	"github.com/opencontainers/go-digest"
)

// PrepareManifests previews manual deletion of digests in one repository,
// including their index children and referrers. Everything outside that scope
// is kept. The usual age, running-image, lock and dependency protections apply:
// selecting a signature alone cannot remove it from an image that is kept.
// Selection and retention use the same inspection, without a second download.
func (p *Pruner) PrepareManifests(ctx context.Context, repository string, digests []string) (*DeletionPlan, error) {
	if !imageref.ValidRepository(repository) {
		return nil, fmt.Errorf("invalid repository name %q", repository)
	}
	if len(digests) == 0 {
		return nil, errors.New("no manifests selected")
	}
	for _, value := range digests {
		if _, err := digest.Parse(value); err != nil {
			return nil, fmt.Errorf("invalid manifest digest %q: %w", value, err)
		}
	}
	local := *p
	local.requested = slices.Clone(digests)
	return local.Prepare(ctx, []*rules.RepoRule{{
		Repo: regexp.MustCompile("^" + regexp.QuoteMeta(repository) + "$"),
		// Unrelated broken dependencies do not invalidate a manual choice.
		// Missing documents still invalidate the entire preview in Prepare.
		IgnoreMissingManifests: true,
		Tagged:                 []rules.TaggedRule{{CommonRule: rules.CommonRule{Keep: false}}},
		Untagged:               []rules.UntaggedRule{{CommonRule: rules.CommonRule{Keep: false}}},
	}})
}

// expandSelection follows each selected edge once, including referrer chains
// and index children of artifacts. An absent selected document or dependency
// invalidates the preview, rather than silently reducing the requested scope.
func expandSelection(ctx context.Context, repository string, manifests map[string]*registry.Manifest, digests []string) (map[string]bool, error) {
	referrers := map[string][]string{}
	for _, m := range manifests {
		if subject := m.SubjectDigest(); subject != "" {
			referrers[subject] = append(referrers[subject], m.Digest)
		}
	}
	selected := map[string]bool{}
	pending := slices.Clone(digests)
	for len(pending) > 0 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		value := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if selected[value] {
			continue
		}
		m := manifests[value]
		if m == nil {
			return nil, fmt.Errorf("manifest %s@%s is unavailable; reload the repository", repository, value)
		}
		selected[value] = true
		for _, child := range m.Manifests {
			pending = append(pending, string(child.Digest))
		}
		pending = append(pending, referrers[value]...)
	}
	return selected, nil
}
