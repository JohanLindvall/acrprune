package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/JohanLindvall/acrprune/internal/pruner"
	"github.com/JohanLindvall/acrprune/internal/registry"
	"github.com/JohanLindvall/acrprune/internal/registry/registrytest"
	"github.com/urfave/cli/v3"
)

func commandForBackend(fake registry.Backend) *cli.Command {
	return newCommandWithConnector(func(_ context.Context, cmd *cli.Command, _ registry.Address, logger *slog.Logger) (*registry.Registry, error) {
		return registry.New(fake, logger, cmd.Int("parallelism"), nil)
	})
}

func TestPruneCommandDryRunAndDeletion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rules.json")
	if err := os.WriteFile(path, []byte(`[{"repo":"^app$","untagged":[{"keep":false}]}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	fake := registrytest.New()
	fake.Add("app", registrytest.Image("old"), registry.Attributes{LastUpdated: time.Now().Add(-48 * time.Hour)})
	args := []string{"acrprune", "--progress=plain", "-r", "myreg", "prune", "--input", path}
	if err := commandForBackend(fake).Run(t.Context(), args); err != nil {
		t.Fatal(err)
	}
	if len(fake.DeletedRepositories()) != 0 {
		t.Fatal("default dry run deleted data")
	}
	if err := commandForBackend(fake).Run(t.Context(), append(args, "--dry-run=false")); err != nil {
		t.Fatal(err)
	}
	if len(fake.DeletedRepositories()) != 1 {
		t.Fatal("explicit prune did not delete")
	}
}

func TestStatsKeepsSnapshotsOnFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stats.json")
	previous := `[{"name":"previous"}]`
	if err := os.WriteFile(path, []byte(previous), 0o600); err != nil {
		t.Fatal(err)
	}
	boom := errors.New("API unavailable")
	fake := registrytest.New()
	fake.Fail = func(string, string) error { return boom }
	args := []string{"acrprune", "--progress=plain", "-r", "myreg", "stats", "--output", path}
	if err := commandForBackend(fake).Run(t.Context(), args); !errors.Is(err, boom) {
		t.Fatalf("error=%v", err)
	}
	if got, _ := os.ReadFile(path); string(got) != previous {
		t.Fatalf("old snapshot lost: %s", got)
	}
	fake.Add("a", registrytest.Image("a"), registry.Attributes{})
	fake.Add("b", registrytest.Image("b"), registry.Attributes{})
	fake.Fail = func(_, repo string) error {
		if repo == "b" {
			return boom
		}
		return nil
	}
	if err := commandForBackend(fake).Run(t.Context(), args); !errors.Is(err, boom) {
		t.Fatalf("error=%v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	stats, err := pruner.ReadStats(strings.NewReader(string(got)))
	if err != nil || len(stats) != 1 || stats[0].Name != "a" {
		t.Fatalf("partial snapshot=%s, %v", got, err)
	}
	// A catalog may be visible even when every repository is denied.
	// Preserve the last usable snapshot when that yields no completed scan.
	fake.Fail = func(op, repo string) error {
		if repo != "" {
			return registrytest.Forbidden(op)
		}
		return nil
	}
	if err := commandForBackend(fake).Run(t.Context(), args); err == nil {
		t.Fatal("a completely denied scan must report failure")
	}
	if after, _ := os.ReadFile(path); string(after) != string(got) {
		t.Fatalf("denied scan replaced the last snapshot: %s", after)
	}
}

func TestCommandRejectsInputsBeforeConnecting(t *testing.T) {
	for _, args := range [][]string{
		{"prune", "unrecognized.json"}, {"stats", "ignored"}, {"generate", "ignored"},
		{"top", "--top=-1"}, {"top", "--sort=unknown"}, {"--progress=invalid", "stats"},
	} {
		cmd := newCommandWithConnector(func(context.Context, *cli.Command, registry.Address, *slog.Logger) (*registry.Registry, error) {
			t.Error("connected before validating inputs")
			return nil, errors.New("unexpected connection")
		})
		if err := cmd.Run(t.Context(), append([]string{"acrprune", "-r", "myreg"}, args...)); err == nil {
			t.Errorf("accepted %v", args)
		}
	}
}

func TestExplicitStandardStreams(t *testing.T) {
	withStdin(t, "[]")
	out, err := captureStdout(t, func() error { return newCommand().Run(t.Context(), []string{"acrprune", "top", "--input=-"}) })
	if err != nil || !strings.Contains(out, "NAME") {
		t.Fatalf("stdout=%q error=%v", out, err)
	}
	fake := registrytest.New()
	out, err = captureStdout(t, func() error {
		return commandForBackend(fake).Run(t.Context(), []string{"acrprune", "--progress=plain", "-r", "test", "stats", "--out=-"})
	})
	if err != nil || strings.TrimSpace(out) != "[]" {
		t.Fatalf("JSON stdout=%q error=%v", out, err)
	}
}
