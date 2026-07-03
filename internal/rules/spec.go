// Package rules defines the declarative JSON rule format used to decide
// which manifests to keep or delete, and its compiled, regex-checked form.
package rules

import (
	"encoding/json"
	"errors"
	"io"
	"time"

	str2duration "github.com/xhit/go-str2duration/v2"
)

// Duration unmarshals from Go duration syntax ("24h"), extended syntax with
// days and weeks ("30d", "2w") or a plain number of nanoseconds.
type Duration struct {
	time.Duration
}

func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(d.String())
}

func (d *Duration) UnmarshalJSON(b []byte) error {
	var v interface{}
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	switch value := v.(type) {
	case float64:
		d.Duration = time.Duration(value)
		return nil
	case string:
		var err error
		d.Duration, err = str2duration.ParseDuration(value)
		if err != nil {
			d.Duration, err = time.ParseDuration(value)
		}
		return err
	default:
		return errors.New("invalid duration")
	}
}

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

// ParseSpecs decodes a JSON array of repository rule specs, rejecting
// unknown fields.
func ParseSpecs(r io.Reader) ([]*RepoRuleSpec, error) {
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	var specs []*RepoRuleSpec
	if err := dec.Decode(&specs); err != nil {
		return nil, err
	}
	return specs, nil
}
