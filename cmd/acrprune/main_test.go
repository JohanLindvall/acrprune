package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/urfave/cli/v3"
)

// flagByName returns the root-level flag exposing the given name, or nil.
func flagByName(cmd *cli.Command, name string) cli.Flag {
	for _, f := range cmd.Flags {
		if slices.Contains(f.Names(), name) {
			return f
		}
	}
	return nil
}

// TestVerboseOwnsShortV guards against the version flag reclaiming -v.
// The urfave/cli version flag defaults to the -v alias, which would shadow the
// verbose flag and make "-v" print the version and exit instead of enabling
// debug logging.
func TestVerboseOwnsShortV(t *testing.T) {
	cmd := newCommand()

	verbose := flagByName(cmd, "verbose")
	if verbose == nil {
		t.Fatal("no verbose flag found")
	}
	if !slices.Contains(verbose.Names(), "v") {
		t.Fatalf("verbose flag should own -v, got names %v", verbose.Names())
	}

	if slices.Contains(cli.VersionFlag.Names(), "v") {
		t.Fatalf("version flag must not claim -v, got names %v", cli.VersionFlag.Names())
	}
}

// TestVersionFlag confirms --version still prints the version and short-circuits
// before any command runs (no registry/Azure access).
func TestVersionFlag(t *testing.T) {
	version = "test-1.2.3"

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stdout
	os.Stdout = w
	runErr := newCommand().Run(context.Background(), []string{"acrprune", "--version"})
	os.Stdout = orig
	_ = w.Close()

	buf := make([]byte, 4096)
	n, _ := r.Read(buf)
	got := string(buf[:n])

	if runErr != nil {
		t.Fatalf("--version returned error: %v", runErr)
	}
	if !strings.Contains(got, "test-1.2.3") {
		t.Fatalf("--version output %q does not contain the version", got)
	}
}

// TestTopRunsWithoutRegistry confirms the top command works on a local stats
// file without the --registry flag and without any Azure access.
func TestTopRunsWithoutRegistry(t *testing.T) {
	statsFile := filepath.Join(t.TempDir(), "stats.json")
	statsJSON := `[
		{"name": "alloy", "unique": 2816819627, "total": 3226969527, "shared": 0.127,
		 "tagged": 11, "untagged": 22, "count": 33,
		 "newest": "2026-06-08T11:15:20Z", "oldest": "2025-10-13T08:56:53Z", "running": 0},
		{"name": "binfmt", "unique": 152837093, "total": 152837093, "shared": 0,
		 "tagged": 3, "untagged": 8, "count": 11,
		 "newest": "2026-03-16T08:44:37Z", "oldest": "2025-06-11T08:08:27Z", "running": 0}
	]`
	if err := os.WriteFile(statsFile, []byte(statsJSON), 0644); err != nil {
		t.Fatal(err)
	}

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stdout
	os.Stdout = w
	runErr := newCommand().Run(context.Background(), []string{"acrprune", "top", "--input", statsFile, "-k", "1"})
	os.Stdout = orig
	_ = w.Close()

	out, _ := io.ReadAll(r)
	got := string(out)

	if runErr != nil {
		t.Fatalf("top returned error: %v", runErr)
	}
	if !strings.Contains(got, "alloy") || strings.Contains(got, "binfmt") {
		t.Fatalf("expected only the top-1 row (alloy), got:\n%s", got)
	}
	if !strings.Contains(got, "2.8 GB") || !strings.Contains(got, "12.7%") {
		t.Fatalf("expected humanized size and percent, got:\n%s", got)
	}
}
