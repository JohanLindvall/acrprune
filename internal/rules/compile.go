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
	OS           *regexp.Regexp
	Digest       *regexp.Regexp
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
	var err error
	if rule.Architecture, err = compileOptional("arch", s.ArchitectureRegex); err != nil {
		return rule, err
	}
	if rule.OS, err = compileOptional("os", s.OSRegex); err != nil {
		return rule, err
	}
	if rule.Digest, err = compileOptional("digest", s.DigestRegex); err != nil {
		return rule, err
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

// compileOptional compiles an optional regex field; nil or empty means no
// constraint and compiles to a nil (match-all) regexp.
func compileOptional(field string, pattern *string) (*regexp.Regexp, error) {
	if pattern == nil || *pattern == "" {
		return nil, nil
	}
	re, err := regexp.Compile(*pattern)
	if err != nil {
		return nil, fmt.Errorf("invalid %s regex %q: %w", field, *pattern, err)
	}
	return re, nil
}
