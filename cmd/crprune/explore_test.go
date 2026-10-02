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

	"github.com/JohanLindvall/crprune/internal/explore"
	"github.com/JohanLindvall/crprune/internal/pruner"
	"github.com/JohanLindvall/crprune/internal/registry"
	"github.com/urfave/cli/v3"
)

func TestExploreLocalFileAndStdin(t *testing.T) {
	original := runExplorer
	t.Cleanup(func() { runExplorer = original })
	calls := 0
	runExplorer = func(_ context.Context, stats []pruner.RepositoryStats, opts explore.Options) error {
		calls++
		if len(stats) != 1 || stats[0].Name != "app" || opts.LiveStats || opts.Client != nil || opts.Sort != "count" || !opts.Reverse || opts.Filter != "app" {
			t.Fatalf("stats=%v opts=%+v", stats, opts)
		}
		if opts.Reload != nil {
			again, err := opts.Reload()
			if err != nil || len(again) != 1 {
				t.Fatal(again, err)
			}
		}
		return nil
	}
	file := filepath.Join(t.TempDir(), "stats.json")
	doc := `[{"name":"app","count":2}]`
	if err := os.WriteFile(file, []byte(doc), 0600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{file, "-", ""} {
		withStdin(t, doc)
		cmd := newCommandWithConnector(func(context.Context, *cli.Command, registry.Address, *slog.Logger) (*registry.Registry, error) {
			t.Fatal("connected for local stats")
			return nil, nil
		})
		out, err := captureStdout(t, func() error {
			return cmd.Run(t.Context(), []string{"crprune", "explore", "--sort", "count", "--reverse", "--filter", "app", "--input", path})
		})
		if err != nil || out != "" {
			t.Fatal(out, err)
		}
	}
	if calls != 3 {
		t.Fatal(calls)
	}
}

func TestExploreWithoutStats(t *testing.T) {
	for _, tt := range []struct {
		name, registry string
		terminal       bool
	}{
		{"terminal ACR", "myreg", true},
		{"terminal GHCR", "ghcr.io/acme", true},
		{"empty stdin", "ghcr.io/acme", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if tt.terminal {
				terminalStdin(t)
			} else {
				withStdin(t, "")
			}
			original := runExplorer
			t.Cleanup(func() { runExplorer = original })
			called := false
			runExplorer = func(_ context.Context, stats []pruner.RepositoryStats, opts explore.Options) error {
				called = true
				if stats != nil || !opts.LiveStats || opts.Client == nil || opts.Reload != nil || !strings.Contains(opts.Source, tt.registry) {
					t.Fatalf("stats=%v opts=%+v", stats, opts)
				}
				return nil
			}
			cmd := newCommandWithConnector(func(context.Context, *cli.Command, registry.Address, *slog.Logger) (*registry.Registry, error) {
				t.Fatal("connected before opening the explorer")
				return nil, nil
			})
			out, err := captureStdout(t, func() error {
				return cmd.Run(t.Context(), []string{"crprune", "-r", tt.registry, "explore"})
			})
			if err != nil || out != "" || !called {
				t.Fatalf("out=%q err=%v called=%t", out, err, called)
			}
		})
	}
}

func TestExplorePipedSnapshotTakesPrecedenceOverRegistry(t *testing.T) {
	original := runExplorer
	t.Cleanup(func() { runExplorer = original })
	for _, doc := range []string{`[]`, `[{"name":"app"}]`} {
		withStdin(t, doc)
		called := false
		runExplorer = func(_ context.Context, stats []pruner.RepositoryStats, opts explore.Options) error {
			called = true
			if stats == nil || opts.LiveStats || opts.Client == nil || opts.Source != "stdin" {
				t.Fatalf("stats=%v opts=%+v", stats, opts)
			}
			return nil
		}
		cmd := newCommandWithConnector(func(context.Context, *cli.Command, registry.Address, *slog.Logger) (*registry.Registry, error) {
			t.Fatal("connected when a snapshot was supplied")
			return nil, nil
		})
		if err := cmd.Run(t.Context(), []string{"crprune", "-r", "ghcr.io/acme", "explore"}); err != nil || !called {
			t.Fatalf("err=%v called=%t", err, called)
		}
	}
}

func TestExploreMissingOrInvalidInputDoesNotScan(t *testing.T) {
	captureStderr(t)
	original := runExplorer
	t.Cleanup(func() { runExplorer = original })
	runExplorer = func(context.Context, []pruner.RepositoryStats, explore.Options) error {
		t.Fatal("opened explorer for invalid input")
		return nil
	}
	file := filepath.Join(t.TempDir(), "empty.json")
	if err := os.WriteFile(file, nil, 0600); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name, stdin string
		args        []string
	}{
		{"empty file", "", []string{file}},
		{"missing file", "", []string{file + ".missing"}},
		{"explicit empty stdin", "", []string{"--input", "-"}},
		{"positional empty stdin", "", []string{"-"}},
		{"invalid piped JSON", "broken", nil},
		{"null piped JSON", "null", nil},
		{"whitespace piped JSON", " \n", nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			withStdin(t, tt.stdin)
			cmd := newCommandWithConnector(func(context.Context, *cli.Command, registry.Address, *slog.Logger) (*registry.Registry, error) {
				t.Fatal("connected for invalid input")
				return nil, nil
			})
			err := cmd.Run(t.Context(), append([]string{"crprune", "-r", "ghcr.io/acme", "explore"}, tt.args...))
			if err == nil || !strings.Contains(err.Error(), "statistics file") {
				t.Fatal(err)
			}
		})
	}
}

func TestExploreWithoutStatsRequiresRegistry(t *testing.T) {
	captureStderr(t)
	for _, terminal := range []bool{false, true} {
		t.Run(map[bool]string{false: "empty stdin", true: "terminal"}[terminal], func(t *testing.T) {
			if terminal {
				terminalStdin(t)
			} else {
				withStdin(t, "")
			}
			if err := newCommand().Run(t.Context(), []string{"crprune", "explore"}); !errors.Is(err, errRegistryRequired) {
				t.Fatal(err)
			}
		})
	}
}

func TestExploreLoadsDefaultAndCustomRuleDirectories(t *testing.T) {
	t.Chdir(t.TempDir())
	for _, dir := range []string{"rules", "custom rules"} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "custom.json"), []byte(`[{"repo":"^app$"}]`), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, dir := range []string{"rules", "custom rules"} {
		t.Run(dir, func(t *testing.T) {
			withStdin(t, "[]")
			original := runExplorer
			t.Cleanup(func() { runExplorer = original })
			called := false
			runExplorer = func(_ context.Context, _ []pruner.RepositoryStats, opts explore.Options) error {
				called = true
				var bundled, local int
				for _, file := range opts.RuleFiles {
					if file.Err != nil {
						t.Fatal(file.Err)
					}
					if strings.HasPrefix(file.Source, "bundled:") {
						bundled++
					} else if file.Source == filepath.Join(dir, "custom.json") {
						local++
					} else {
						t.Fatal("loaded rules from the wrong directory", file.Source)
					}
				}
				if bundled == 0 || local != 1 {
					t.Fatalf("bundled=%d local=%d", bundled, local)
				}
				return nil
			}
			args := []string{"crprune", "explore"}
			if dir != "rules" {
				args = append(args, "--rules-dir", dir)
			}
			if err := newCommand().Run(t.Context(), args); err != nil || !called {
				t.Fatalf("err=%v called=%t", err, called)
			}
		})
	}
}

func TestExploreRejectsInvalidExplicitRuleDirectory(t *testing.T) {
	captureStderr(t)
	original := runExplorer
	t.Cleanup(func() { runExplorer = original })
	runExplorer = func(context.Context, []pruner.RepositoryStats, explore.Options) error {
		t.Fatal("opened explorer with invalid --rules-dir")
		return nil
	}
	file := filepath.Join(t.TempDir(), "file.json")
	if err := os.WriteFile(file, []byte(`[]`), 0600); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{file, file + ".missing", ""} {
		withStdin(t, "[]")
		if err := newCommand().Run(t.Context(), []string{"crprune", "explore", "--rules-dir", dir}); err == nil || !strings.Contains(err.Error(), "--rules-dir") {
			t.Fatal(err)
		}
	}
}

func TestExploreLiveConnectionIsLazyAndRequestsDeletionScope(t *testing.T) {
	original := runExplorer
	t.Cleanup(func() { runExplorer = original })
	file := filepath.Join(t.TempDir(), "stats.json")
	if err := os.WriteFile(file, []byte(`[]`), 0600); err != nil {
		t.Fatal(err)
	}
	var scopes []bool
	cmd := newCommandWithConnector(func(ctx context.Context, _ *cli.Command, addr registry.Address, _ *slog.Logger) (*registry.Registry, error) {
		if addr.String() != "ghcr.io/acme" {
			t.Fatal(addr)
		}
		scopes = append(scopes, ctx.Value(explorerWriteKey{}) == true)
		return nil, errors.New("test connection")
	})
	runExplorer = func(ctx context.Context, _ []pruner.RepositoryStats, opts explore.Options) error {
		if len(scopes) != 0 {
			t.Fatal("connected before requesting action")
		}
		if opts.LiveStats || opts.Client == nil || opts.Client.KeepYounger != 48*time.Hour {
			t.Fatal(opts.Client)
		}
		for _, write := range []bool{false, true} {
			_, _ = opts.Client.Connect(ctx, slog.New(slog.DiscardHandler), write)
		}
		return nil
	}
	if err := cmd.Run(t.Context(), []string{"crprune", "-r", "ghcr.io/acme", "explore", file, "--keep-younger", "2d"}); err != nil {
		t.Fatal(err)
	}
	if len(scopes) != 2 || scopes[0] || !scopes[1] {
		t.Fatal(scopes)
	}
}

func TestExploreRejectsBadInputsBeforeOpeningTerminal(t *testing.T) {
	captureStderr(t)
	terminalStdin(t)
	original := runExplorer
	t.Cleanup(func() { runExplorer = original })
	runExplorer = func(context.Context, []pruner.RepositoryStats, explore.Options) error {
		t.Fatal("opened terminal before validation")
		return nil
	}
	file := filepath.Join(t.TempDir(), "stats.json")
	if err := os.WriteFile(file, []byte(`[]`), 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"--sort=unknown"}, {"--keep-younger=-1h"}, {file, file}, {"--input", file, file}, {}, {file, "--rules=rules.json"}} {
		if err := newCommand().Run(t.Context(), append([]string{"crprune", "explore"}, args...)); err == nil {
			t.Fatal(args)
		}
	}
}

func TestExploreHelpExplainsSnapshotScope(t *testing.T) {
	out, err := captureStdout(t, func() error { return newCommand().Run(t.Context(), []string{"crprune", "explore", "--help"}) })
	if err != nil || !strings.Contains(out, "all snapshot repositories") || !strings.Contains(out, "--rules-dir") || !strings.Contains(out, "scans --registry") {
		t.Fatal(out, err)
	}
}
