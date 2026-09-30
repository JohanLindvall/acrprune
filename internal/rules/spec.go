// Package rules defines the declarative JSON rule format used to decide
// which manifests to keep or delete, and its compiled, regex-checked form.
package rules

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// CommonRuleSpec holds the match criteria shared by tagged and untagged rules.
// A nil field imposes no constraint.
type CommonRuleSpec struct {
	ArchitectureRegex *string   `json:"arch,omitempty"`
	OSRegex           *string   `json:"os,omitempty"`
	DigestRegex       *string   `json:"digest,omitempty"`
	MatchNewest       *int      `json:"newest,omitempty"`
	MatchNewerThan    *Duration `json:"match_newer,omitempty"`
	MatchOlderThan    *Duration `json:"match_older,omitempty"`
	Keep              *bool     `json:"keep,omitempty"` // defaults to true
}

// UntaggedRuleSpec is an entry of a repository rule's "untagged" list.
type UntaggedRuleSpec struct {
	CommonRuleSpec
}

// TaggedRuleSpec is an entry of a repository rule's "tagged" list. TagRegex
// matches tags; nil matches every tag. See RepoRule for how a manifest with
// several tags is decided.
type TaggedRuleSpec struct {
	TagRegex *string `json:"tag,omitempty"`
	CommonRuleSpec
}

// RepoRuleSpec is one entry of a rule file: the repositories it applies to
// and how to treat their manifests. See RepoRule for how rules are applied.
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
// fields, duplicate keys and trailing content so a malformed rule file cannot
// be silently read as a rule set that deletes more than intended. Errors give
// the line and column of the offending value.
func ParseSpecs(r io.Reader) ([]*RepoRuleSpec, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("failed to read rules: %w", err)
	}
	if len(bytes.Trim(data, jsonSpace)) == 0 {
		return nil, errors.New("no rules: expected a JSON array of rules ([] for none)")
	}
	doc, err := scanDocument(data)
	if err != nil {
		return nil, err
	}
	// Offsets in decoding errors count from the first byte of the value.
	value := bytes.TrimLeft(data, jsonSpace)
	dec := json.NewDecoder(bytes.NewReader(value))
	dec.DisallowUnknownFields()
	var specs []*RepoRuleSpec
	if err := dec.Decode(&specs); err != nil {
		return nil, doc.decodeError(err, int64(len(data)-len(value)))
	}
	if specs == nil {
		return nil, errors.New("rules must be a JSON array, not null")
	}
	return specs, nil
}
