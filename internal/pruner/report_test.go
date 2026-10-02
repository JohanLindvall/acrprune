package pruner

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

const statsJSON = `[
  {
    "name": "alloy",
    "unique": 2816819627,
    "total": 3226969527,
    "shared": 0.12710064243504093,
    "tagged": 11,
    "untagged": 22,
    "count": 33,
    "newest": "2026-06-08T11:15:20.8706166Z",
    "oldest": "2025-10-13T08:56:53.8069537Z",
    "running": 0
  },
  {
    "name": "authservice",
    "unique": 14213771,
    "total": 28419387,
    "shared": 0.49985652399891667,
    "tagged": 1,
    "untagged": 2,
    "count": 3,
    "newest": "2025-11-24T18:20:13.5694688Z",
    "oldest": "2025-11-24T18:20:13.0489177Z",
    "running": 1
  },
  {
    "name": "binfmt",
    "unique": 152837093,
    "total": 152837093,
    "shared": 0,
    "tagged": 3,
    "untagged": 8,
    "count": 11,
    "newest": "2026-03-16T08:44:37.9277163Z",
    "oldest": "2025-06-11T08:08:27.1414224Z",
    "running": 0
  }
]`

func readTestStats(t *testing.T) []RepositoryStats {
	t.Helper()
	stats, err := ReadStats(strings.NewReader(statsJSON))
	if err != nil {
		t.Fatal(err)
	}
	return stats
}

func names(stats []RepositoryStats) []string {
	out := make([]string, len(stats))
	for i, s := range stats {
		out[i] = s.Name
	}
	return out
}

func TestReadStats(t *testing.T) {
	stats := readTestStats(t)
	if len(stats) != 3 {
		t.Fatalf("expected 3 repositories, got %d", len(stats))
	}
	if stats[0].Name != "alloy" || stats[0].Unique != 2816819627 {
		t.Fatalf("unexpected first entry: %+v", stats[0])
	}
	if stats[0].Newest.Year() != 2026 {
		t.Fatalf("newest not parsed: %v", stats[0].Newest)
	}
}

func TestReadStatsRejectsInvalidJSON(t *testing.T) {
	for _, input := range []string{"not json", "null", "[]]", "[]}", "[] []", "[] trailing"} {
		if _, err := ReadStats(strings.NewReader(input)); err == nil {
			t.Errorf("accepted invalid statistics %q", input)
		}
	}
}

func TestReadStatsRejectsAmbiguousOrInvalidValues(t *testing.T) {
	for _, test := range []struct{ input, want string }{
		{`[{"name":"app"},{"name":"app"}]`, `duplicate repository "app" (first at entry 1)`},
		{`[{"name":"app","count":-1}]`, "counts must not be negative"},
		{`[{"name":"app","tagged":-1}]`, "counts must not be negative"},
		{`[{"name":"app","untagged":-1}]`, "counts must not be negative"},
		{`[{"name":"app","running":-1}]`, "counts must not be negative"},
		{`[{"name":"app","shared":-0.1}]`, "shared must be a fraction"},
		{`[{"name":"app","shared":50}]`, "shared must be a fraction"},
	} {
		if _, err := ReadStats(strings.NewReader(test.input)); err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("ReadStats(%s) = %v; want %q", test.input, err, test.want)
		}
	}
}

// TestReadStatsReportsPositions: a hand-edited statistics file is hard to fix
// from encoding/json's message alone, which gives no position.
func TestReadStatsReportsPositions(t *testing.T) {
	for _, tt := range []struct{ input, want string }{
		{"[\n  {\"name\": \"a\",}\n]", "line 2, column 16: invalid character '}'"},
		{"[\n  {\"name\": \"a\"},\n  {\"name\": \"b\", \"count\": \"two\"}\n]", "line 3, column 26: json: cannot unmarshal string"},
		{"\n  [{\"name\": \"a\", \"tagged\": {}}]", "line 2, column 28: json: cannot unmarshal object"},
		{"[]\n\n  x", "line 3, column 3: invalid character 'x'"},
		{"[\n  {\"name\": \"a\"}", "failed to parse statistics JSON: unexpected EOF"},
	} {
		_, err := ReadStats(strings.NewReader(tt.input))
		if err == nil || !strings.HasPrefix(err.Error(), tt.want) {
			t.Errorf("%q: error = %v, want it to start with %q", tt.input, err, tt.want)
		}
	}
}

// TestReadStatsRejectsInvalidNames: top prints names to the terminal, so a
// statistics file from elsewhere must not carry escape sequences in them.
func TestReadStatsRejectsInvalidNames(t *testing.T) {
	for _, name := range []string{"", `x\u001b]0;title\u0007\u001b[2J`, "Upper", "a b"} {
		_, err := ReadStats(strings.NewReader(`[{"name": "app"}, {"name": "` + name + `"}]`))
		if err == nil || !strings.Contains(err.Error(), "statistics entry 2: invalid repository name") {
			t.Errorf("name %q: error = %v", name, err)
		}
	}
}

func TestSortStatsBy(t *testing.T) {
	cases := []struct {
		key  string
		want []string
	}{
		{"unique", []string{"alloy", "binfmt", "authservice"}},
		{"total", []string{"alloy", "binfmt", "authservice"}},
		{"shared", []string{"authservice", "alloy", "binfmt"}},
		{"tagged", []string{"alloy", "binfmt", "authservice"}},
		{"untagged", []string{"alloy", "binfmt", "authservice"}},
		{"count", []string{"alloy", "binfmt", "authservice"}},
		{"running", []string{"authservice", "alloy", "binfmt"}},
		{"name", []string{"alloy", "authservice", "binfmt"}},
		{"newest", []string{"alloy", "binfmt", "authservice"}},
		{"oldest", []string{"binfmt", "alloy", "authservice"}},
	}
	for _, tc := range cases {
		stats := readTestStats(t)
		if err := SortStatsBy(stats, tc.key); err != nil {
			t.Fatalf("sort by %s: %v", tc.key, err)
		}
		got := names(stats)
		for i := range tc.want {
			if got[i] != tc.want[i] {
				t.Fatalf("sort by %s: got %v, want %v", tc.key, got, tc.want)
			}
		}
	}
}

func TestSortStatsByUnknownKey(t *testing.T) {
	err := SortStatsBy(nil, "bogus")
	if err == nil {
		t.Fatal("expected an error for an unknown sort key")
	}
	if !strings.Contains(err.Error(), "unique") {
		t.Fatalf("error should list valid keys, got: %v", err)
	}
}

func TestWriteStatsTable(t *testing.T) {
	stats := readTestStats(t)
	var buf strings.Builder
	if err := WriteStatsTable(&buf, stats, 2); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected header plus 2 rows, got %d lines:\n%s", len(lines), out)
	}
	if !strings.HasPrefix(lines[0], "NAME") {
		t.Fatalf("missing header: %q", lines[0])
	}
	// Sizes must be human readable and shared a percentage.
	if !strings.Contains(lines[1], "2.8 GB") || !strings.Contains(lines[1], "3.2 GB") {
		t.Fatalf("sizes not humanized: %q", lines[1])
	}
	if !strings.Contains(lines[1], "12.7%") {
		t.Fatalf("shared not formatted as percent: %q", lines[1])
	}
	if !strings.Contains(lines[1], "2026-06-08") {
		t.Fatalf("newest not formatted as date: %q", lines[1])
	}
}

func TestWriteStatsTableAllRows(t *testing.T) {
	stats := readTestStats(t)
	var buf strings.Builder
	if err := WriteStatsTable(&buf, stats, 0); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != len(stats)+1 {
		t.Fatalf("top<=0 should print all rows, got %d lines", len(lines))
	}
}

func TestStatSortKeysCoverFields(t *testing.T) {
	keys := StatSortKeys()
	for _, want := range []string{"name", "unique", "total", "shared", "tagged", "untagged", "count", "newest", "oldest", "running"} {
		if !strings.Contains(strings.Join(keys, ","), want) {
			t.Fatalf("missing sort key %q in %v", want, keys)
		}
	}
}

// TestWriteStatsTableUnknownDates: a repository whose manifests carry no
// timestamps shows dashes rather than the year 1.
func TestWriteStatsTableUnknownDates(t *testing.T) {
	var buf strings.Builder
	if err := WriteStatsTable(&buf, []RepositoryStats{{Name: "undated"}}, 0); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if fields := strings.Fields(lines[len(lines)-1]); len(lines) != 2 || !slices.Equal(fields[len(fields)-2:], []string{"-", "-"}) {
		t.Errorf("table = %q, want dashes for the dates", buf.String())
	}
}

// failingWriter fails every write.
type failingWriter struct{ err error }

func (w failingWriter) Write([]byte) (int, error) { return 0, w.err }

func TestWriteStatsTableReportsWriteErrors(t *testing.T) {
	boom := errors.New("boom")
	if err := WriteStatsTable(failingWriter{boom}, readTestStats(t), 0); !errors.Is(err, boom) {
		t.Errorf("error = %v, want the write failure", err)
	}
}
