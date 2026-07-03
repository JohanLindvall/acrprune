package rules

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// literalRepoName matches repository patterns that are plain names rather
// than regexes needing a full registry listing to resolve.
var literalRepoName = regexp.MustCompile(`^[a-zA-Z0-9._/-]+$`)

// CommonRule is the compiled form of CommonRuleSpec. A nil regexp matches
// anything.
type CommonRule struct {
	Architecture   *regexp.Regexp
	MatchNewest    int // >0: only the N newest; <0: all but the N newest; 0: no constraint
	MatchNewerThan time.Duration
	MatchOlderThan time.Duration
	Keep           bool
}

type UntaggedRule struct {
	CommonRule
}

type TaggedRule struct {
	Tag *regexp.Regexp // nil matches any tag
	CommonRule
}

type RepoRule struct {
	Repo                    *regexp.Regexp
	IgnoreMissingManifests  bool
	DeleteOrphanedManifests bool
	MustDeleteEverything    bool
	Untagged                []UntaggedRule
	Tagged                  []TaggedRule
}

// LiteralRepoName reports whether the rule's repository pattern is a plain
// anchored name (^name$) and returns that name, allowing callers to skip
// listing the whole registry.
func (r *RepoRule) LiteralRepoName() (string, bool) {
	pattern := r.Repo.String()
	if !strings.HasPrefix(pattern, "^") || !strings.HasSuffix(pattern, "$") {
		return "", false
	}
	name := pattern[1 : len(pattern)-1]
	if !literalRepoName.MatchString(name) {
		return "", false
	}
	return name, true
}

// Compile validates and compiles a slice of rule specs.
func Compile(specs []*RepoRuleSpec) ([]*RepoRule, error) {
	result := make([]*RepoRule, len(specs))
	for i, spec := range specs {
		rule, err := spec.Compile()
		if err != nil {
			return nil, fmt.Errorf("rule %d: %w", i, err)
		}
		result[i] = rule
	}
	return result, nil
}

// Compile validates the spec's regexes and applies defaults.
func (s *RepoRuleSpec) Compile() (*RepoRule, error) {
	repo, err := regexp.Compile(s.RepoRegex)
	if err != nil {
		return nil, fmt.Errorf("invalid repo regex %q: %w", s.RepoRegex, err)
	}
	result := &RepoRule{
		Repo:                   repo,
		IgnoreMissingManifests: true,
	}
	if s.IgnoreMissingManifests != nil {
		result.IgnoreMissingManifests = *s.IgnoreMissingManifests
	}
	if s.DeleteOrphanedManifests != nil {
		result.DeleteOrphanedManifests = *s.DeleteOrphanedManifests
	}
	if s.MustDeleteEverything != nil {
		result.MustDeleteEverything = *s.MustDeleteEverything
	}
	for _, u := range s.Untagged {
		common, err := u.CommonRuleSpec.compile()
		if err != nil {
			return nil, err
		}
		result.Untagged = append(result.Untagged, UntaggedRule{CommonRule: common})
	}
	for _, t := range s.Tagged {
		common, err := t.CommonRuleSpec.compile()
		if err != nil {
			return nil, err
		}
		rule := TaggedRule{CommonRule: common}
		if t.TagRegex != nil && *t.TagRegex != "" {
			if rule.Tag, err = regexp.Compile(*t.TagRegex); err != nil {
				return nil, fmt.Errorf("invalid tag regex %q: %w", *t.TagRegex, err)
			}
		}
		result.Tagged = append(result.Tagged, rule)
	}
	return result, nil
}

func (s *CommonRuleSpec) compile() (CommonRule, error) {
	rule := CommonRule{Keep: true}
	if s.ArchitectureRegex != nil && *s.ArchitectureRegex != "" {
		re, err := regexp.Compile(*s.ArchitectureRegex)
		if err != nil {
			return rule, fmt.Errorf("invalid arch regex %q: %w", *s.ArchitectureRegex, err)
		}
		rule.Architecture = re
	}
	if s.MatchNewest != nil {
		rule.MatchNewest = *s.MatchNewest
	}
	if s.MatchNewerThan != nil {
		rule.MatchNewerThan = s.MatchNewerThan.Duration
	}
	if s.MatchOlderThan != nil {
		rule.MatchOlderThan = s.MatchOlderThan.Duration
	}
	if s.Keep != nil {
		rule.Keep = *s.Keep
	}
	return rule, nil
}
