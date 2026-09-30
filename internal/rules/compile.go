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
	// Architecture and OS match the image platform; when both are set they
	// must match the same platform.
	Architecture *regexp.Regexp
	OS           *regexp.Regexp
	// Digest matches the manifest digest.
	Digest *regexp.Regexp
	// MatchNewest ranks a manifest against the other manifests this same rule
	// matches: >0 selects the N newest of them, <0 selects all but the N
	// newest, 0 imposes no constraint.
	MatchNewest int
	// MatchNewerThan and MatchOlderThan match manifests last updated less,
	// respectively more, than this long ago; 0 imposes no constraint.
	MatchNewerThan time.Duration
	MatchOlderThan time.Duration
	// Keep decides the fate of the manifests the rule matches.
	Keep bool
}

// UntaggedRule decides the fate of untagged manifests.
type UntaggedRule struct {
	CommonRule
}

// TaggedRule decides the fate of tagged manifests, one tag at a time: it
// matches a tag of a manifest when the tag matches Tag and the manifest the
// common criteria. For `newest`, it ranks the manifests any of whose tags
// match Tag.
type TaggedRule struct {
	Tag *regexp.Regexp // nil matches any tag
	CommonRule
}

// RepoRule is the compiled form of RepoRuleSpec.
//
// Pruning applies only the first RepoRule whose Repo matches a repository;
// later rules matching the same repository are never consulted. Within that
// rule, the first UntaggedRule that matches an untagged manifest decides
// whether it is kept. A tagged manifest is decided tag by tag, since deleting
// it deletes all of its tags: it is deleted only when the first TaggedRule
// matching each of its tags deletes it. A manifest or tag no rule matches is
// kept.
type RepoRule struct {
	// Description is the rule's optional description, used in messages.
	Description string
	// Repo matches the repository names the rule applies to.
	Repo *regexp.Regexp
	// IgnoreMissingManifests tolerates listed manifests that no longer exist
	// (404s). It defaults to true.
	IgnoreMissingManifests bool
	// DeleteOrphanedManifests deletes manifests whose dependencies are
	// missing.
	DeleteOrphanedManifests bool
	// MustDeleteEverything keeps the whole repository if the rules keep any
	// of its manifests.
	MustDeleteEverything bool
	// Untagged and Tagged are tried in order; see above.
	Untagged []UntaggedRule
	Tagged   []TaggedRule
}

// UsesPlatform reports whether any tagged or untagged rule matches on
// architecture or operating system, which needs the platform of every image
// known.
func (r *RepoRule) UsesPlatform() bool {
	for _, t := range r.Tagged {
		if t.Architecture != nil || t.OS != nil {
			return true
		}
	}
	for _, u := range r.Untagged {
		if u.Architecture != nil || u.OS != nil {
			return true
		}
	}
	return false
}

// Compile validates and compiles a slice of rule specs. Errors name the rule
// by its 1-based position in the file and its description, if it has one.
func Compile(specs []*RepoRuleSpec) ([]*RepoRule, error) {
	result := make([]*RepoRule, len(specs))
	for i, spec := range specs {
		rule, err := spec.Compile()
		if err != nil {
			var description string
			if spec != nil && spec.Description != nil {
				description = *spec.Description
			}
			return nil, fmt.Errorf("%s: %w", ruleName(i, description), err)
		}
		result[i] = rule
	}
	return result, nil
}

// ruleName names the i-th (0-based) repository rule in messages.
func ruleName(i int, description string) string {
	if description == "" {
		return fmt.Sprintf("rule %d", i+1)
	}
	return fmt.Sprintf("rule %d (%q)", i+1, description)
}

// Compile validates the spec's regexes and applies defaults. Errors number
// tagged and untagged rules from 1.
func (s *RepoRuleSpec) Compile() (*RepoRule, error) {
	if s == nil {
		return nil, errors.New("repository rule must not be null")
	}
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
	if s.Description != nil {
		result.Description = *s.Description
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
		if u == nil {
			return nil, fmt.Errorf("untagged rule %d must not be null", i+1)
		}
		common, err := u.compile()
		if err != nil {
			return nil, fmt.Errorf("untagged rule %d: %w", i+1, err)
		}
		result.Untagged = append(result.Untagged, UntaggedRule{CommonRule: common})
	}
	for i, t := range s.Tagged {
		if t == nil {
			return nil, fmt.Errorf("tagged rule %d must not be null", i+1)
		}
		common, err := t.compile()
		if err != nil {
			return nil, fmt.Errorf("tagged rule %d: %w", i+1, err)
		}
		rule := TaggedRule{CommonRule: common}
		if rule.Tag, err = compileOptional("tag", t.TagRegex); err != nil {
			return nil, fmt.Errorf("tagged rule %d: %w", i+1, err)
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
		// The evaluator reads 0 as "no constraint", the opposite of what
		// "the 0 newest" says.
		if *s.MatchNewest == 0 {
			return rule, errors.New("newest must not be 0 (use N for the N newest, -N for all but the N newest, or omit the field)")
		}
		rule.MatchNewest = *s.MatchNewest
	}
	if s.MatchNewerThan != nil {
		if s.MatchNewerThan.Duration < 0 {
			return rule, errors.New("match_newer must not be negative")
		}
		// Likewise, "newer than 0" matches nothing, but 0 means no
		// constraint.
		if s.MatchNewerThan.Duration == 0 {
			return rule, errors.New("match_newer must be greater than 0 (omit the field to match any age)")
		}
		rule.MatchNewerThan = s.MatchNewerThan.Duration
	}
	if s.MatchOlderThan != nil {
		if s.MatchOlderThan.Duration < 0 {
			return rule, errors.New("match_older must not be negative")
		}
		rule.MatchOlderThan = s.MatchOlderThan.Duration
	}
	if s.Keep != nil {
		rule.Keep = *s.Keep
	}
	return rule, nil
}

// compileOptional compiles an optional regex field; nil means no constraint
// and compiles to a nil (match-all) regexp. An empty pattern would match
// everything too, but it is rejected like an empty repo pattern: in a file
// that deletes things it is far more likely an unset template variable than
// a deliberate catch-all.
func compileOptional(field string, pattern *string) (*regexp.Regexp, error) {
	if pattern == nil {
		return nil, nil
	}
	if *pattern == "" {
		return nil, fmt.Errorf("empty %s pattern (omit the field to match everything)", field)
	}
	re, err := regexp.Compile(*pattern)
	if err != nil {
		return nil, fmt.Errorf("invalid %s regex %q: %w", field, *pattern, err)
	}
	return re, nil
}
