package rules

import (
	"bufio"
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
)

// KeepRulesFromImageList reads `registry.azurecr.io/repo:tag` image
// references (one per line, e.g. from a pod image dump) and produces rules
// that keep exactly those tags, deleting everything else in the referenced
// repositories.
func KeepRulesFromImageList(r io.Reader, registry string) []*RepoRuleSpec {
	scanner := bufio.NewScanner(r)
	specs := []*RepoRuleSpec{}
	prefix := registry + "/"
	if !strings.Contains(registry, ".") {
		prefix = registry + ".azurecr.io/"
	}

	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		repository, tag, ok := strings.Cut(line[len(prefix):], ":")
		if !ok {
			continue
		}
		repoRegex := fmt.Sprintf("^%s$", regexp.QuoteMeta(repository))
		if len(specs) == 0 || specs[len(specs)-1].RepoRegex != repoRegex {
			specs = append(specs, &RepoRuleSpec{
				RepoRegex:               repoRegex,
				IgnoreMissingManifests:  to.Ptr(true),
				DeleteOrphanedManifests: to.Ptr(false),
				Untagged:                []*UntaggedRuleSpec{{CommonRuleSpec: CommonRuleSpec{Keep: to.Ptr(false)}}},
			})
		}
		last := specs[len(specs)-1]
		last.Tagged = append(last.Tagged, &TaggedRuleSpec{
			TagRegex:       to.Ptr(fmt.Sprintf("^%s$", regexp.QuoteMeta(tag))),
			CommonRuleSpec: CommonRuleSpec{Keep: to.Ptr(true)},
		})
	}

	// Everything not explicitly kept above is deleted.
	for _, spec := range specs {
		spec.Tagged = append(spec.Tagged, &TaggedRuleSpec{
			TagRegex:       to.Ptr(".+"),
			CommonRuleSpec: CommonRuleSpec{Keep: to.Ptr(false)},
		})
	}

	return specs
}
