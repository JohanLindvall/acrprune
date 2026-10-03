// Package imageref validates container repository names and image references.
package imageref

import (
	_ "crypto/sha256" // algorithms accepted by go-digest
	_ "crypto/sha512"
	"fmt"
	"regexp"
	"strings"

	"github.com/opencontainers/go-digest"
)

// Repository names and tags follow the OCI Distribution reference grammar.
const component = `[a-z0-9]+(?:(?:\.|_|__|-+)[a-z0-9]+)*`

var (
	repositoryPattern = regexp.MustCompile(`^` + component + `(?:/` + component + `)*$`)
	tagPattern        = regexp.MustCompile(`^[a-zA-Z0-9_][a-zA-Z0-9_.-]{0,127}$`)
)

// ValidRepository reports whether name can safely address a repository.
func ValidRepository(name string) bool {
	return len(name) <= 255 && repositoryPattern.MatchString(name)
}

// ValidTag reports whether tag is a complete OCI Distribution tag name.
func ValidTag(tag string) bool {
	return tagPattern.MatchString(tag)
}

// ValidOwner reports whether name can be a single namespace component.
func ValidOwner(name string) bool {
	return ValidRepository(name) && !strings.Contains(name, "/")
}

// Split parses a repository reference after its registry prefix has been
// removed. A name without a tag or digest refers to the latest tag.
func Split(ref string) (repository, tag, pinned string, err error) {
	name, pinned, hasDigest := strings.Cut(ref, "@")
	repository, tag, hasTag := strings.Cut(name, ":")
	if !ValidRepository(repository) {
		return "", "", "", fmt.Errorf("invalid repository name %q", repository)
	}
	if hasTag && !ValidTag(tag) {
		return "", "", "", fmt.Errorf("invalid image tag %q", tag)
	}
	if hasDigest {
		if _, err := digest.Parse(pinned); err != nil {
			return "", "", "", fmt.Errorf("invalid image digest %q: %w", pinned, err)
		}
	}
	if !hasTag && !hasDigest {
		tag = "latest"
	}
	return repository, tag, pinned, nil
}
