package main

import (
	"context"
	"os"
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
