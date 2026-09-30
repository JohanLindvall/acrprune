package pruner

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/JohanLindvall/acrprune/internal/imageref"
	"github.com/JohanLindvall/acrprune/internal/jsonpos"
	"github.com/dustin/go-humanize"
)

// statCompare orders repositories per sort key so the most interesting entry
// comes first: sizes and counts descending, newest most-recent-first, oldest
// oldest-first, and names alphabetically.
var statCompare = map[string]func(a, b RepositoryStats) int{
	"name":     func(a, b RepositoryStats) int { return strings.Compare(a.Name, b.Name) },
	"unique":   func(a, b RepositoryStats) int { return cmp.Compare(b.Unique, a.Unique) },
	"total":    func(a, b RepositoryStats) int { return cmp.Compare(b.Total, a.Total) },
	"shared":   func(a, b RepositoryStats) int { return cmp.Compare(b.Shared, a.Shared) },
	"tagged":   func(a, b RepositoryStats) int { return cmp.Compare(b.Tagged, a.Tagged) },
	"untagged": func(a, b RepositoryStats) int { return cmp.Compare(b.Untagged, a.Untagged) },
	"count":    func(a, b RepositoryStats) int { return cmp.Compare(b.Count, a.Count) },
	"running":  func(a, b RepositoryStats) int { return cmp.Compare(b.Running, a.Running) },
	"newest":   func(a, b RepositoryStats) int { return b.Newest.Compare(a.Newest) },
	"oldest":   func(a, b RepositoryStats) int { return a.Oldest.Compare(b.Oldest) },
}

// StatSortKeys returns the sort keys accepted by SortStatsBy.
func StatSortKeys() []string {
	return slices.Sorted(maps.Keys(statCompare))
}

// ReadStats parses statistics JSON as written by the statistics command.
// Malformed JSON is reported with its line and column where encoding/json
// gives its offset, a mistyped value at the value's start. ReadStats rejects
// entries whose name is no valid repository name, which a statistics file
// from elsewhere could use to smuggle terminal escape sequences into the
// table.
func ReadStats(r io.Reader) ([]RepositoryStats, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("failed to read statistics: %w", err)
	}
	var stats []RepositoryStats
	// The offsets of type errors count from the first byte of the value.
	value := bytes.TrimLeft(data, " \t\r\n")
	base := int64(len(data) - len(value))
	dec := json.NewDecoder(bytes.NewReader(value))
	if err := dec.Decode(&stats); err != nil {
		return nil, jsonError(data, base, "failed to parse statistics JSON", err)
	}
	if stats == nil {
		return nil, errors.New("statistics must be a JSON array, not null")
	}
	if err := dec.Decode(new(json.RawMessage)); err != io.EOF {
		if err == nil {
			return nil, errors.New("unexpected trailing content after statistics JSON")
		}
		return nil, jsonError(data, base, "after statistics JSON", err)
	}
	for i, s := range stats {
		if !imageref.ValidRepository(s.Name) {
			return nil, fmt.Errorf("statistics entry %d: invalid repository name %q", i+1, s.Name)
		}
	}
	return stats, nil
}

// jsonError prefixes err, from decoding data[base:], with the line and column
// it points at, or with what failed when it points nowhere.
func jsonError(data []byte, base int64, what string, err error) error {
	if offset, ok := jsonpos.Offset(data, base, err); ok {
		return jsonpos.At(data, offset, err)
	}
	return fmt.Errorf("%s: %w", what, err)
}

// SortStatsBy sorts stats in place by the given key.
func SortStatsBy(stats []RepositoryStats, key string) error {
	compare, ok := statCompare[key]
	if !ok {
		return fmt.Errorf("unknown sort key %q (valid: %s)", key, strings.Join(StatSortKeys(), ", "))
	}
	slices.SortStableFunc(stats, compare)
	return nil
}

// WriteStatsTable writes the first top rows of stats (all rows if top <= 0)
// as an aligned table with human-readable sizes and percentages.
func WriteStatsTable(w io.Writer, stats []RepositoryStats, top int) error {
	if top > 0 && top < len(stats) {
		stats = stats[:top]
	}
	tw := tabwriter.NewWriter(w, 0, 8, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "NAME\tUNIQUE\tTOTAL\tSHARED\tTAGGED\tUNTAGGED\tCOUNT\tRUNNING\tNEWEST\tOLDEST"); err != nil {
		return err
	}
	for _, s := range stats {
		if _, err := fmt.Fprintf(tw, "%s\t%s\t%s\t%.1f%%\t%d\t%d\t%d\t%d\t%s\t%s\n",
			s.Name,
			humanize.Bytes(s.Unique),
			humanize.Bytes(s.Total),
			s.Shared*100,
			s.Tagged,
			s.Untagged,
			s.Count,
			s.Running,
			statDate(s.Newest),
			statDate(s.Oldest),
		); err != nil {
			return err
		}
	}
	return tw.Flush()
}

func statDate(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Format("2006-01-02")
}
