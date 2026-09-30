package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/JohanLindvall/acrprune/internal/progress"
	"github.com/JohanLindvall/acrprune/internal/pruner"
	"github.com/JohanLindvall/acrprune/internal/registry"
	"github.com/JohanLindvall/acrprune/internal/registry/ghcr"
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

// TestPruneCommandProtectsRunningImages: --running keeps the images it lists,
// whatever the rule file says.
func TestPruneCommandProtectsRunningImages(t *testing.T) {
	dir := t.TempDir()
	rulePath := filepath.Join(dir, "rules.json")
	if err := os.WriteFile(rulePath, []byte(`[{"repo":"^app$","tagged":[{"keep":false}]}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	runningPath := filepath.Join(dir, "running.txt")
	if err := os.WriteFile(runningPath, []byte("myreg.azurecr.io/app:v1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fake := registrytest.New()
	old := time.Now().Add(-48 * time.Hour)
	running := fake.Add("app", registrytest.Image("v1"), registry.Attributes{Tags: []string{"v1"}, LastUpdated: old})
	fake.Add("app", registrytest.Image("v0"), registry.Attributes{Tags: []string{"v0"}, LastUpdated: old})

	args := []string{"acrprune", "--progress=plain", "-r", "myreg", "prune", "--dry-run=false", "--input", rulePath, "--running", runningPath}
	if err := commandForBackend(fake).Run(t.Context(), args); err != nil {
		t.Fatal(err)
	}
	if got := fake.Digests("app"); len(got) != 1 || got[0] != running {
		t.Errorf("app = %v, want only the running image left", got)
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

// TestPruneCommandGracePeriod: --keep-younger reaches the pruner, and takes
// durations as rule files write them.
func TestPruneCommandGracePeriod(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rules.json")
	if err := os.WriteFile(path, []byte(`[{"repo":"^app$","untagged":[{"keep":false}]}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		age     time.Duration
		args    []string
		deleted bool
	}{
		{time.Hour, nil, false},
		{time.Hour, []string{"--keep-younger=0"}, true},
		{36 * time.Hour, nil, true},
		{36 * time.Hour, []string{"--keep-younger", "2d"}, false},
		{36 * time.Hour, []string{"--keepyounger=1d"}, true},
		{10 * 24 * time.Hour, []string{"--keep-younger=2w"}, false},
	} {
		fake := registrytest.New()
		fake.Add("app", registrytest.Image("a"), registry.Attributes{LastUpdated: time.Now().Add(-tt.age)})
		args := append([]string{"acrprune", "--progress=plain", "-r", "myreg", "prune", "--dry-run=false", "--input", path}, tt.args...)
		if err := commandForBackend(fake).Run(t.Context(), args); err != nil {
			t.Fatal(err)
		}
		if deleted := len(fake.DeletedRepositories()) == 1; deleted != tt.deleted {
			t.Errorf("manifest %v old, %v: deleted = %v, want %v", tt.age, tt.args, deleted, tt.deleted)
		}
	}
}

// TestPruneCommandIncludeLocked: locked manifests are kept, and unlocked and
// deleted only with --include-locked.
func TestPruneCommandIncludeLocked(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rules.json")
	if err := os.WriteFile(path, []byte(`[{"repo":"^app$","tagged":[{"keep":false}]}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, includeLocked := range []bool{false, true} {
		fake := registrytest.New()
		fake.EnforceLocks = true
		old := time.Now().Add(-48 * time.Hour)
		locked := fake.Add("app", registrytest.Image("v1"), registry.Attributes{Tags: []string{"v1"}, LastUpdated: old, Locked: true})
		fake.Add("app", registrytest.Image("v0"), registry.Attributes{Tags: []string{"v0"}, LastUpdated: old})
		args := []string{"acrprune", "--progress=plain", "-r", "myreg", "prune", "--dry-run=false", "--input", path}
		if includeLocked {
			args = append(args, "--include-locked")
		}
		if err := commandForBackend(fake).Run(t.Context(), args); err != nil {
			t.Fatalf("--include-locked=%v: %v", includeLocked, err)
		}
		if includeLocked {
			if len(fake.Unlocked()) == 0 || len(fake.DeletedRepositories()) != 1 {
				t.Errorf("--include-locked: unlocked %v, deleted repositories %v; want the locked image unlocked and the repository deleted", fake.Unlocked(), fake.DeletedRepositories())
			}
		} else if got := fake.Digests("app"); len(fake.Unlocked()) != 0 || len(got) != 1 || got[0] != locked {
			t.Errorf("unlocked %v, app = %v; want only the locked image kept, still locked", fake.Unlocked(), got)
		}
	}
}

// TestStatsThrottlesSnapshots: the output file used to be rewritten after
// every repository, which takes time quadratic in their number. It is now
// written after the first repository, then at most every few seconds, and
// once more at the end.
func TestStatsThrottlesSnapshots(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stats.json")
	fake := registrytest.New()
	for _, name := range []string{"a", "b", "c"} {
		fake.Add(name, registrytest.Image(name), registry.Attributes{})
	}
	var snapshot []byte
	fake.Fail = func(op, repository string) error {
		if op == "ListManifests" && repository == "c" {
			snapshot, _ = os.ReadFile(path)
		}
		return nil
	}
	if err := commandForBackend(fake).Run(t.Context(), []string{"acrprune", "--progress=plain", "-r", "myreg", "stats", "-o", path}); err != nil {
		t.Fatal(err)
	}
	if stats, err := pruner.ReadStats(bytes.NewReader(snapshot)); err != nil || len(stats) != 1 {
		t.Errorf("snapshot while scanning c = %s, %v; want only the first repository", snapshot, err)
	}
	final, _ := os.ReadFile(path)
	if stats, err := pruner.ReadStats(bytes.NewReader(final)); err != nil || len(stats) != 3 {
		t.Errorf("final statistics = %s, %v; want every repository", final, err)
	}
}

// TestStatsWritesStdoutAfterDisplay: the terminal display draws on an
// alternate screen, discarded when it closes, so statistics JSON for stdout
// must wait until the display has restored the terminal.
func TestStatsWritesStdoutAfterDisplay(t *testing.T) {
	screen, err := os.Create(filepath.Join(t.TempDir(), "screen"))
	if err != nil {
		t.Fatal(err)
	}
	orig := runProgress
	t.Cleanup(func() { runProgress = orig })
	runProgress = func(ctx context.Context, _ progress.Options, logger *slog.Logger, work func(context.Context, *slog.Logger) error) error {
		// Stand in for the alternate screen: what reaches stdout while the
		// display runs is lost.
		stdout := os.Stdout
		os.Stdout = screen
		defer func() { os.Stdout = stdout }()
		return work(ctx, logger)
	}

	fake := registrytest.New()
	fake.Add("app", registrytest.Image("a"), registry.Attributes{})
	out, err := captureStdout(t, func() error {
		return commandForBackend(fake).Run(t.Context(), []string{"acrprune", "-r", "myreg", "stats"})
	})
	if err != nil {
		t.Fatal(err)
	}
	if stats, err := pruner.ReadStats(strings.NewReader(out)); err != nil || len(stats) != 1 || stats[0].Name != "app" {
		t.Errorf("stdout = %q, %v; want the statistics of app", out, err)
	}
	if lost, _ := os.ReadFile(screen.Name()); len(lost) != 0 {
		t.Errorf("wrote %q while the display ran", lost)
	}
}

// TestGHCRNeedsDeleteScopeOnlyForLivePrunes: a token without delete:packages
// is refused up front for a run that deletes, and accepted for the others.
func TestGHCRNeedsDeleteScopeOnlyForLivePrunes(t *testing.T) {
	t.Setenv("GH_TOKEN", "token")
	path := filepath.Join(t.TempDir(), "rules.json")
	if err := os.WriteFile(path, []byte(`[]`), 0o600); err != nil {
		t.Fatal(err)
	}
	orig := newGHCR
	t.Cleanup(func() { newGHCR = orig })
	stop := errors.New("stop before connecting")
	for _, tt := range []struct {
		args []string
		want bool
	}{
		{[]string{"prune", "--input", path}, false},
		{[]string{"prune", "--input", path, "--dry-run=false"}, true},
		{[]string{"stats"}, false},
	} {
		var got *bool
		newGHCR = func(_ context.Context, opts ghcr.Options) (*ghcr.Backend, error) {
			got = &opts.NeedDelete
			return nil, stop
		}
		err := newCommand().Run(t.Context(), append([]string{"acrprune", "--progress=plain", "-r", "ghcr.io/acme"}, tt.args...))
		if !errors.Is(err, stop) || got == nil || *got != tt.want {
			t.Errorf("%v: error %v, NeedDelete %v; want %v", tt.args, err, got, tt.want)
		}
	}
}

// TestCommandRejectsInputsBeforeConnecting: invalid flags and inputs fail
// before any registry access, and so before any deletion.
func TestCommandRejectsInputsBeforeConnecting(t *testing.T) {
	captureStderr(t)
	terminalStdin(t)
	dir := t.TempDir()
	file := func(name, content string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	valid := file("valid.json", `[{"repo":"^app$","untagged":[{"keep":false}]}]`)
	malformed := file("malformed.json", `[{"repo":"^app$",}]`)
	badRegex := file("regex.json", `[{"repo":"("}]`)
	badRunning := file("running.txt", "myreg.azurecr.io/App:v1\n")
	missing := filepath.Join(dir, "missing")
	for _, args := range [][]string{
		{"prune", "unrecognized.json"}, {"stats", "ignored"}, {"generate", "ignored"},
		{"top", "--top=-1"}, {"top", "--sort=unknown"}, {"--progress=invalid", "stats"},
		{"--page-size=0", "stats"}, {"--parallelism=0", "stats"},
		{"prune", "--input", valid, "--keep-younger=-1h"},
		{"prune", "--input", valid, "--keep-younger=2x"},
		{"prune", "--input", missing},
		{"prune", "--input", malformed},
		{"prune", "--input", badRegex},
		{"prune", "--input", valid, "--running", badRunning},
		{"prune", "--input", valid, "--running", missing},
		{"stats", "--running", badRunning},
		// stdin is a terminal
		{"prune"}, {"generate"}, {"top"},
	} {
		cmd := newCommandWithConnector(func(context.Context, *cli.Command, registry.Address, *slog.Logger) (*registry.Registry, error) {
			t.Errorf("%v: connected before validating inputs", args)
			return nil, errors.New("unexpected connection")
		})
		if err := cmd.Run(t.Context(), append([]string{"acrprune", "--progress=plain", "-r", "myreg"}, args...)); err == nil {
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
