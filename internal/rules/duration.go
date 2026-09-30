package rules

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	str2duration "github.com/xhit/go-str2duration/v2"
)

// durationExamples shows what a valid duration looks like in error messages.
const durationExamples = `e.g. "24h", "14d" or "2w"`

// ParseDuration parses Go duration syntax ("24h", "1h30m") extended with days
// and weeks ("30d", "2w"): the string form of durations in rule files. It
// accepts negative durations; callers that need a non-negative value check
// for themselves.
func ParseDuration(s string) (time.Duration, error) {
	// str2duration accepts everything time.ParseDuration does plus the day
	// and week units.
	d, err := str2duration.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q (use %s)", s, durationExamples)
	}
	return d, nil
}

// Duration is a rule file duration. It unmarshals from a string in the syntax
// ParseDuration accepts or from an integer number of nanoseconds, and
// marshals to a string.
type Duration struct {
	time.Duration
}

// MarshalJSON encodes the duration as a Go duration string such as "336h0m0s".
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(d.String())
}

// UnmarshalJSON decodes a duration string or an integer number of
// nanoseconds. An integer from 0 to just under one second is rejected: it is
// almost certainly a count of some other unit ("match_older": 30 meaning 30
// days), and would make an age constraint match nearly every manifest.
func (d *Duration) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '"' {
		var value string
		if err := json.Unmarshal(b, &value); err != nil {
			return err
		}
		parsed, err := ParseDuration(value)
		if err != nil {
			return err
		}
		d.Duration = parsed
		return nil
	}
	if len(b) == 0 || b[0] != '-' && (b[0] < '0' || b[0] > '9') {
		return errors.New("a duration must be a string (" + durationExamples + ") or an integer number of nanoseconds")
	}
	// Parse integers directly: converting through float64 loses precision
	// and can turn a large positive duration into a negative one.
	nanos, err := strconv.ParseInt(string(b), 10, 64)
	if err != nil {
		return fmt.Errorf("invalid duration %s: a number must be a whole number of nanoseconds that fits a Go duration; use a string instead (%s)", b, durationExamples)
	}
	// Negative values are left to Compile, which rejects them by field name.
	if nanos >= 0 && nanos < int64(time.Second) {
		return fmt.Errorf("integer duration %d counts nanoseconds; use a string instead (%s)", nanos, durationExamples)
	}
	d.Duration = time.Duration(nanos)
	return nil
}
