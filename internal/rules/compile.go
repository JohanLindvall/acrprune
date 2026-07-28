package rules

import (
	"errors"
	"fmt"
	"regexp"
	"time"
)

// CommonRule is the compiled form of CommonRuleSpec. A nil regexp matches
// anything.
type CommonRule struct {
	Architecture *regexp.Regexp
	// MatchNewest ranks a manifest against the other manifests this same rule
	// matches: >0 selects the N newest of them, <0 selects all but the N
	// newest, 0 imposes no constraint.
	MatchNewest    int
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
	// An empty pattern is a valid regex that matches every repository, which
	// is never what someone means to write in a file that deletes things.
	if s.RepoRegex == "" {
		return nil, errors.New("missing repo pattern (use \".+\" to match every repository)")
	}
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
	for i, u := range s.Untagged {
		common, err := u.compile()
		if err != nil {
			return nil, fmt.Errorf("untagged rule %d: %w", i, err)
		}
		result.Untagged = append(result.Untagged, UntaggedRule{CommonRule: common})
	}
	for i, t := range s.Tagged {
		common, err := t.compile()
		if err != nil {
			return nil, fmt.Errorf("tagged rule %d: %w", i, err)
		}
		rule := TaggedRule{CommonRule: common}
		if t.TagRegex != nil && *t.TagRegex != "" {
			if rule.Tag, err = regexp.Compile(*t.TagRegex); err != nil {
				return nil, fmt.Errorf("tagged rule %d: invalid tag regex %q: %w", i, *t.TagRegex, err)
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
