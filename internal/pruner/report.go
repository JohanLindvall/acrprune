package pruner

import (
	"cmp"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"text/tabwriter"

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
func ReadStats(r io.Reader) ([]RepositoryStats, error) {
	var stats []RepositoryStats
	if err := json.NewDecoder(r).Decode(&stats); err != nil {
		return nil, fmt.Errorf("failed to parse statistics JSON: %w", err)
	}
	return stats, nil
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
	fmt.Fprintln(tw, "NAME\tUNIQUE\tTOTAL\tSHARED\tTAGGED\tUNTAGGED\tCOUNT\tRUNNING\tNEWEST\tOLDEST")
	for _, s := range stats {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%.1f%%\t%d\t%d\t%d\t%d\t%s\t%s\n",
			s.Name,
			humanize.Bytes(s.Unique),
			humanize.Bytes(s.Total),
			s.Shared*100,
			s.Tagged,
			s.Untagged,
			s.Count,
			s.Running,
			s.Newest.Format("2006-01-02"),
			s.Oldest.Format("2006-01-02"),
		)
	}
	return tw.Flush()
}
