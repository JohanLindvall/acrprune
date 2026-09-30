package rules

import (
	"encoding/json"
	"fmt"
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
	if len(b) > 0 && b[0] == '"' {
		var value string
		if err := json.Unmarshal(b, &value); err != nil {
			return err
		}
		// str2duration accepts everything time.ParseDuration does plus the
		// day and week units.
		parsed, err := str2duration.ParseDuration(value)
		if err != nil {
			return err
		}
		d.Duration = parsed
		return nil
	}
	// Decode integers directly: converting through float64 loses precision
	// and can turn a large positive duration into a negative one.
	var nanos int64
	if string(b) == "null" {
		return fmt.Errorf("duration must be a string or an integer number of nanoseconds")
	}
	if err := json.Unmarshal(b, &nanos); err != nil {
		return fmt.Errorf("duration must be a string or an integer number of nanoseconds: %w", err)
	}
	d.Duration = time.Duration(nanos)
	return nil
}
