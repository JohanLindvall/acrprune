// Package rules defines the declarative JSON rule format used to decide
// which manifests to keep or delete, and its compiled, regex-checked form.
package rules

import (
	"encoding/json"
	"errors"
	"io"
)

// CommonRuleSpec holds the match criteria shared by tagged and untagged rules.
type CommonRuleSpec struct {
	ArchitectureRegex *string   `json:"arch,omitempty"`
	MatchNewest       *int      `json:"newest,omitempty"`
	MatchNewerThan    *Duration `json:"match_newer,omitempty"`
	MatchOlderThan    *Duration `json:"match_older,omitempty"`
	Keep              *bool     `json:"keep,omitempty"`
}

type UntaggedRuleSpec struct {
	CommonRuleSpec
}

type TaggedRuleSpec struct {
	TagRegex *string `json:"tag,omitempty"`
	CommonRuleSpec
}

type RepoRuleSpec struct {
	Description             *string             `json:"description,omitempty"`
	RepoRegex               string              `json:"repo,omitempty"`
	IgnoreMissingManifests  *bool               `json:"ignore_missing_manifests,omitempty"`
	DeleteOrphanedManifests *bool               `json:"delete_orphaned_manifests,omitempty"`
	MustDeleteEverything    *bool               `json:"must_delete_everything,omitempty"`
	Untagged                []*UntaggedRuleSpec `json:"untagged,omitempty"`
	Tagged                  []*TaggedRuleSpec   `json:"tagged,omitempty"`
}

// ParseSpecs decodes a JSON array of repository rule specs, rejecting unknown
// fields and trailing content so a malformed rule file cannot be silently
// truncated into a rule set that deletes more than intended.
func ParseSpecs(r io.Reader) ([]*RepoRuleSpec, error) {
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	var specs []*RepoRuleSpec
	if err := dec.Decode(&specs); err != nil {
		return nil, err
	}
	if dec.More() {
		return nil, errors.New("unexpected trailing content after the rule array")
	}
	return specs, nil
}
