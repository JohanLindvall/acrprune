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
		if len(stats) != 1 || stats[0].Name != "app" || opts.Client != nil || opts.Sort != "count" || !opts.Reverse || opts.Filter != "app" {
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
	for _, path := range []string{file, "-"} {
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
	if calls != 2 {
		t.Fatal(calls)
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
		if opts.Client == nil || opts.Client.KeepYounger != 48*time.Hour {
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
	if err != nil || !strings.Contains(out, "all snapshot repositories") {
		t.Fatal(out, err)
	}
}
