package rules

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/JohanLindvall/acrprune/internal/imageref"
)

// repoKeeps accumulates what one repository's rule must keep.
type repoKeeps struct {
	tags, digests []string
	seen          map[string]bool // "t:"+tag / "d:"+digest already recorded
}

// ImageListCounts says how much of an image list KeepRulesFromImageList used.
type ImageListCounts struct {
	// Lines is the number of non-empty lines read.
	Lines int
	// Matched is the number of those lines naming an image of the registry;
	// the others were ignored.
	Matched int
}

// KeepRulesFromImageList reads image references (one per line, e.g. from a pod
// image dump) and produces rules that keep exactly those images, deleting
// everything else in the referenced repositories. Both tag references
// (`myreg.azurecr.io/repo:tag`) and digest-pinned references
// (`myreg.azurecr.io/repo@sha256:…`, with or without a tag) are kept; lines
// not naming an image below location are ignored. location is what the
// registry's image references start with: a login server such as
// myreg.azurecr.io, or ghcr.io/<owner> on GHCR. It is compared
// case-insensitively, as host names are, so `MyReg.azurecr.io/app:v1` is one
// of myreg.azurecr.io's images. The counts say how many lines were read and
// how many of them named an image below location.
//
// A repository yields exactly one rule however often it appears in the input.
// Pruning applies only the first rule matching a repository, so a second rule
// for the same repository would be unreachable and its images would instead be
// deleted by the first rule's catch-all — that is, an unsorted image list would
// delete running images.
func KeepRulesFromImageList(r io.Reader, location string) ([]*RepoRuleSpec, ImageListCounts, error) {
	var counts ImageListCounts
	if location == "" {
		return nil, counts, errors.New("no registry location to match image references against")
	}
	prefix := strings.TrimSuffix(location, "/") + "/"

	var order []string // repositories in first-seen order
	keeps := map[string]*repoKeeps{}

	scanner := bufio.NewScanner(r)
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		counts.Lines++
		// Host names are case-insensitive. Registry locations are ASCII, so
		// comparing the first len(prefix) bytes finds the host (and owner)
		// in any case.
		if len(line) < len(prefix) || !strings.EqualFold(line[:len(prefix)], prefix) {
			continue
		}
		counts.Matched++
		repository, tag, digest, err := imageref.Split(line[len(prefix):])
		if err != nil {
			return nil, ImageListCounts{}, fmt.Errorf("image list line %d: %w", lineNumber, err)
		}
		k := keeps[repository]
		if k == nil {
			k = &repoKeeps{seen: map[string]bool{}}
			keeps[repository] = k
			order = append(order, repository)
		}
		// A reference carrying both keeps both: the digest pins what is
		// actually running even if the tag has since been moved.
		if tag != "" && !k.seen["t:"+tag] {
			k.seen["t:"+tag] = true
			k.tags = append(k.tags, tag)
		}
		if digest != "" && !k.seen["d:"+digest] {
			k.seen["d:"+digest] = true
			k.digests = append(k.digests, digest)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, ImageListCounts{}, fmt.Errorf("failed to read image list: %w", err)
	}

	specs := make([]*RepoRuleSpec, 0, len(order))
	for _, repository := range order {
		k := keeps[repository]
		spec := &RepoRuleSpec{
			RepoRegex:               anchored(repository),
			IgnoreMissingManifests:  new(true),
			DeleteOrphanedManifests: new(false),
		}
		// Digest-pinned images may or may not be tagged in the registry, so
		// digest keeps go in both lists, ahead of the catch-alls.
		for _, digest := range k.digests {
			keep := CommonRuleSpec{DigestRegex: new(anchored(digest)), Keep: new(true)}
			spec.Tagged = append(spec.Tagged, &TaggedRuleSpec{CommonRuleSpec: keep})
			spec.Untagged = append(spec.Untagged, &UntaggedRuleSpec{CommonRuleSpec: keep})
		}
		for _, tag := range k.tags {
			spec.Tagged = append(spec.Tagged, &TaggedRuleSpec{
				TagRegex:       new(anchored(tag)),
				CommonRuleSpec: CommonRuleSpec{Keep: new(true)},
			})
		}
		// Everything not explicitly kept above is deleted.
		spec.Tagged = append(spec.Tagged, &TaggedRuleSpec{
			TagRegex:       new(".+"),
			CommonRuleSpec: CommonRuleSpec{Keep: new(false)},
		})
		spec.Untagged = append(spec.Untagged, &UntaggedRuleSpec{CommonRuleSpec: CommonRuleSpec{Keep: new(false)}})
		specs = append(specs, spec)
	}
	return specs, counts, nil
}

// anchored turns a literal name into a regex matching exactly that name.
func anchored(literal string) string {
	return fmt.Sprintf("^%s$", regexp.QuoteMeta(literal))
}
