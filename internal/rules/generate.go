package rules

import (
	"bufio"
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
)

// KeepRulesFromImageList reads `registry.azurecr.io/repo:tag` image references
// (one per line, e.g. from a pod image dump) and produces rules that keep
// exactly those tags, deleting everything else in the referenced repositories.
// Lines that do not name a tagged image in the given registry are ignored.
//
// A repository yields exactly one rule however often it appears in the input.
// Pruning applies only the first rule matching a repository, so a second rule
// for the same repository would be unreachable and its images would instead be
// deleted by the first rule's catch-all — that is, an unsorted image list would
// delete running images.
func KeepRulesFromImageList(r io.Reader, registry string) ([]*RepoRuleSpec, error) {
	prefix := registry + "/"
	if !strings.Contains(registry, ".") {
		prefix = registry + ".azurecr.io/"
	}

	var order []string                   // repositories in first-seen order
	tags := map[string][]string{}        // repository -> tags to keep
	seen := map[string]map[string]bool{} // repository -> tags already recorded

	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		repository, tag, ok := splitImageRef(strings.TrimSpace(scanner.Text()), prefix)
		if !ok {
			continue
		}
		if seen[repository] == nil {
			seen[repository] = map[string]bool{}
			order = append(order, repository)
		}
		if seen[repository][tag] {
			continue
		}
		seen[repository][tag] = true
		tags[repository] = append(tags[repository], tag)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("failed to read image list: %w", err)
	}

	specs := make([]*RepoRuleSpec, 0, len(order))
	for _, repository := range order {
		spec := &RepoRuleSpec{
			RepoRegex:               anchored(repository),
			IgnoreMissingManifests:  to.Ptr(true),
			DeleteOrphanedManifests: to.Ptr(false),
			Untagged:                []*UntaggedRuleSpec{{CommonRuleSpec: CommonRuleSpec{Keep: to.Ptr(false)}}},
		}
		for _, tag := range tags[repository] {
			spec.Tagged = append(spec.Tagged, &TaggedRuleSpec{
				TagRegex:       to.Ptr(anchored(tag)),
				CommonRuleSpec: CommonRuleSpec{Keep: to.Ptr(true)},
			})
		}
		// Everything not explicitly kept above is deleted.
		spec.Tagged = append(spec.Tagged, &TaggedRuleSpec{
			TagRegex:       to.Ptr(".+"),
			CommonRuleSpec: CommonRuleSpec{Keep: to.Ptr(false)},
		})
		specs = append(specs, spec)
	}
	return specs, nil
}

// splitImageRef extracts the repository and tag from an image reference such
// as `myreg.azurecr.io/team/app:1.2.3`, given the registry prefix to strip.
func splitImageRef(line, prefix string) (repository, tag string, ok bool) {
	name, ok := strings.CutPrefix(line, prefix)
	if !ok {
		return "", "", false
	}
	// Drop any pinned digest: `repo@sha256:…` names no tag at all, and in
	// `repo:tag@sha256:…` the tag already identifies what to keep. Without
	// this the digest's own colon would be read as the tag separator.
	name, _, _ = strings.Cut(name, "@")
	// Repository names cannot contain a colon, so the first one starts the tag.
	repository, tag, ok = strings.Cut(name, ":")
	if !ok || repository == "" || tag == "" {
		return "", "", false
	}
	return repository, tag, true
}

// anchored turns a literal name into a regex matching exactly that name.
func anchored(literal string) string {
	return fmt.Sprintf("^%s$", regexp.QuoteMeta(literal))
}
