package rules

import (
	"encoding/json"
	"errors"
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
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	switch value := v.(type) {
	case float64:
		d.Duration = time.Duration(value)
		return nil
	case string:
		// str2duration accepts everything time.ParseDuration does plus the
		// day and week units.
		parsed, err := str2duration.ParseDuration(value)
		if err != nil {
			return err
		}
		d.Duration = parsed
		return nil
	default:
		return errors.New("invalid duration")
	}
}
