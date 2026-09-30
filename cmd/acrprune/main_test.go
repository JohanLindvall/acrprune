package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/urfave/cli/v3"

	"github.com/JohanLindvall/acrprune/internal/registry"
	"github.com/JohanLindvall/acrprune/internal/registry/registrytest"
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

// captureStdout runs fn with os.Stdout redirected to a pipe and returns what
// it printed alongside fn's error.
func captureStdout(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stdout
	os.Stdout = w
	runErr := fn()
	os.Stdout = orig
	_ = w.Close()
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(out), runErr
}

// TestVersionFlag confirms --version still prints the version and short-circuits
// before any command runs (no registry/Azure access).
func TestVersionFlag(t *testing.T) {
	version = "test-1.2.3"

	got, runErr := captureStdout(t, func() error {
		return newCommand().Run(context.Background(), []string{"acrprune", "--version"})
	})
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

	got, runErr := captureStdout(t, func() error {
		return newCommand().Run(context.Background(), []string{"acrprune", "top", "--input", statsFile, "-k", "1"})
	})
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

// TestTopRejectsAmbiguousInputs pins the argument validation: extra inputs
// used to be silently ignored, making it look like the wrong file was ranked.
func TestTopRejectsAmbiguousInputs(t *testing.T) {
	statsFile := filepath.Join(t.TempDir(), "stats.json")
	if err := os.WriteFile(statsFile, []byte(`[]`), 0o644); err != nil {
		t.Fatal(err)
	}

	for name, args := range map[string][]string{
		"two positional files":    {"acrprune", "top", statsFile, statsFile},
		"--input plus positional": {"acrprune", "top", "--input", statsFile, statsFile},
	} {
		_, runErr := captureStdout(t, func() error {
			return newCommand().Run(context.Background(), args)
		})
		if runErr == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

// withStdin feeds input to the command under test as its standard input.
func withStdin(t *testing.T, input string) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		_, _ = io.WriteString(w, input)
		_ = w.Close()
	}()
	orig := os.Stdin
	os.Stdin = r
	t.Cleanup(func() {
		os.Stdin = orig
		_ = r.Close()
	})
}

// noGitHubCredentials removes every source of a GitHub token.
func noGitHubCredentials(t *testing.T) {
	t.Helper()
	t.Setenv("GH_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "")
	orig := ghAuthToken
	ghAuthToken = func(context.Context) (string, error) { return "", errors.New("gh: not logged in") }
	t.Cleanup(func() { ghAuthToken = orig })
}

// TestGenerateForGHCR: generate runs offline — no GitHub token needed — and
// maps ghcr.io/<owner>/<package> references to package rules.
func TestGenerateForGHCR(t *testing.T) {
	noGitHubCredentials(t)
	withStdin(t, "ghcr.io/acme/app:v1\nghcr.io/acme/team/api@sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc\nghcr.io/other/app:v2\n")

	got, runErr := captureStdout(t, func() error {
		return newCommand().Run(context.Background(), []string{"acrprune", "-r", "ghcr.io/Acme", "generate"})
	})
	if runErr != nil {
		t.Fatalf("generate returned error: %v", runErr)
	}
	var specs []struct {
		Repo string `json:"repo"`
	}
	if err := json.Unmarshal([]byte(got), &specs); err != nil {
		t.Fatalf("generate output is not a rule file: %v\n%s", err, got)
	}
	if len(specs) != 2 || specs[0].Repo != "^app$" || specs[1].Repo != "^team/api$" {
		t.Errorf("rules = %+v, want app and team/api", specs)
	}
}

// TestRegistryFlagValidation: a missing or malformed --registry fails before
// any credential or network access.
func TestRegistryFlagValidation(t *testing.T) {
	noGitHubCredentials(t)
	rules := filepath.Join(t.TempDir(), "rules.json")
	if err := os.WriteFile(rules, []byte(`[{"repo": ".+"}]`), 0o644); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		args []string
		want string
	}{
		{[]string{"acrprune", "stats"}, "--registry"},
		{[]string{"acrprune", "prune", "--in", rules}, "--registry"},
		{[]string{"acrprune", "generate"}, "--registry"},
		{[]string{"acrprune", "-r", "ghcr.io", "stats"}, "ghcr.io/<owner>"},
		{[]string{"acrprune", "-r", "quay.io/acme", "prune", "--in", rules}, "unsupported registry"},
		{[]string{"acrprune", "-r", "ghcr.io/acme", "stats"}, "no GitHub token"},
		{[]string{"acrprune", "-r", "ghcr.io/acme", "prune", "--in", rules}, "no GitHub token"},
	}
	for _, tt := range tests {
		_, runErr := captureStdout(t, func() error {
			return newCommand().Run(context.Background(), tt.args)
		})
		if runErr == nil || !strings.Contains(runErr.Error(), tt.want) {
			t.Errorf("%v: error = %v, want it to mention %q", tt.args, runErr, tt.want)
		}
	}
}

func TestGitHubToken(t *testing.T) {
	noGitHubCredentials(t)
	ctx := context.Background()

	if _, err := githubToken(ctx); err == nil || !strings.Contains(err.Error(), "read:packages") {
		t.Errorf("error = %v, want advice naming the scopes", err)
	}

	ghAuthToken = func(context.Context) (string, error) { return "from-gh", nil }
	if token, err := githubToken(ctx); token != "from-gh" || err != nil {
		t.Errorf("githubToken = %q, %v; want the GitHub CLI's token", token, err)
	}

	t.Setenv("GITHUB_TOKEN", " from-github-token\n")
	if token, _ := githubToken(ctx); token != "from-github-token" {
		t.Errorf("githubToken = %q, want $GITHUB_TOKEN over the GitHub CLI", token)
	}

	t.Setenv("GH_TOKEN", "from-gh-token")
	if token, _ := githubToken(ctx); token != "from-gh-token" {
		t.Errorf("githubToken = %q, want $GH_TOKEN first, as the GitHub CLI does", token)
	}
}

func TestLoadRunningRules(t *testing.T) {
	addr, err := registry.ParseAddress("ghcr.io/acme")
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.DiscardHandler)
	if ruleSet, err := loadRunningRules("", addr, logger); ruleSet != nil || err != nil {
		t.Errorf("no file should yield no rules, got %v, %v", ruleSet, err)
	}

	path := filepath.Join(t.TempDir(), "running.txt")
	if err := os.WriteFile(path, []byte("ghcr.io/acme/app:v1\nmyreg.azurecr.io/app:v1\nGHCR.IO/Acme/tools:v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ruleSet, err := loadRunningRules(path, addr, logger)
	if err != nil {
		t.Fatal(err)
	}
	if len(ruleSet) != 2 || !ruleSet[0].Repo.MatchString("app") || !ruleSet[1].Repo.MatchString("tools") {
		t.Errorf("running rules = %+v, want app and tools, whatever the host's case", ruleSet)
	}

	if _, err := loadRunningRules(filepath.Join(t.TempDir(), "missing"), addr, logger); err == nil {
		t.Error("a missing running file should fail")
	}
}

// TestLoadRunningRulesRefusesUselessInventories: get_pod_images.sh prints
// nothing when a query fails, and an inventory of another registry matches
// nothing. Either used to report running=0 for every repository without a
// word, making every repository look unused.
func TestLoadRunningRulesRefusesUselessInventories(t *testing.T) {
	addr, err := registry.ParseAddress("myreg")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty.txt")
	if err := os.WriteFile(empty, []byte("\n  \n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadRunningRules(empty, addr, slog.New(slog.DiscardHandler)); err == nil || !strings.Contains(err.Error(), "lists no images") {
		t.Errorf("error = %v, want an empty running file refused", err)
	}

	other := filepath.Join(dir, "other.txt")
	if err := os.WriteFile(other, []byte("otherreg.azurecr.io/app:v1\ndocker.io/library/nginx:1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var log strings.Builder
	ruleSet, err := loadRunningRules(other, addr, slog.New(slog.NewTextHandler(&log, nil)))
	if err != nil || len(ruleSet) != 0 {
		t.Fatalf("loadRunningRules = %v, %v; want no rules and no error", ruleSet, err)
	}
	for _, want := range []string{"level=WARN", "No image in the running file belongs to this registry", "lines=2", "expected_prefix=myreg.azurecr.io/"} {
		if !strings.Contains(log.String(), want) {
			t.Errorf("log %q does not contain %q", log.String(), want)
		}
	}
}

// captureStderr redirects os.Stderr to a file for the rest of the test and
// returns a function reading what was written to it. Build commands after
// calling it: their logger binds os.Stderr when created.
func captureStderr(t *testing.T) func() string {
	t.Helper()
	f, err := os.Create(filepath.Join(t.TempDir(), "stderr"))
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stderr
	os.Stderr = f
	t.Cleanup(func() {
		os.Stderr = orig
		_ = f.Close()
	})
	return func() string {
		data, err := os.ReadFile(f.Name())
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
}

// TestGenerateWarnsWhenNothingMatches: an inventory naming none of the
// registry's images yields an empty rule file, which is safe but most likely
// a mistake, and used to pass without a word.
func TestGenerateWarnsWhenNothingMatches(t *testing.T) {
	stderr := captureStderr(t)
	withStdin(t, "otherreg.azurecr.io/app:v1\n")
	got, err := captureStdout(t, func() error {
		return newCommand().Run(context.Background(), []string{"acrprune", "-r", "myreg", "generate"})
	})
	if err != nil || strings.TrimSpace(got) != "[]" {
		t.Fatalf("generate = %q, %v; want an empty rule file", got, err)
	}
	for _, want := range []string{"level=WARN", "No image in the input belongs to this registry", "lines=1", "expected_prefix=myreg.azurecr.io/"} {
		if !strings.Contains(stderr(), want) {
			t.Errorf("log %q does not contain %q", stderr(), want)
		}
	}
}

// TestPruneReportsRuleProblems: rule file errors name the file and position,
// and rules that can never apply are reported before pruning.
func TestPruneReportsRuleProblems(t *testing.T) {
	stderr := captureStderr(t)
	dir := t.TempDir()
	broken := filepath.Join(dir, "broken.json")
	if err := os.WriteFile(broken, []byte("[\n  {\"repo\": \".+\", \"repo\": \"^app$\"}\n]"), 0o600); err != nil {
		t.Fatal(err)
	}
	fake := registrytest.New()
	err := commandForBackend(fake).Run(t.Context(), []string{"acrprune", "--progress=plain", "-r", "myreg", "prune", "--input", broken})
	if want := "rule file " + broken + ": line 2, column 18: duplicate key \"repo\""; err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("error = %v, want it to contain %q", err, want)
	}

	shadowed := filepath.Join(dir, "shadowed.json")
	if err := os.WriteFile(shadowed, []byte(`[{"repo": ".+", "untagged": [{"match_older": "30d", "keep": false}]}, {"repo": "^app$"}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := commandForBackend(fake).Run(t.Context(), []string{"acrprune", "--progress=plain", "-r", "myreg", "prune", "--input", shadowed}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"level=WARN", "Rule never applies", "rule 2 never applies: rule 1 (repo \\\".+\\\") before it matches every repository"} {
		if !strings.Contains(stderr(), want) {
			t.Errorf("log %q does not contain %q", stderr(), want)
		}
	}
}

// TestConnectACR builds the ACR backend, which needs no network access until
// it is used.
func TestConnectACR(t *testing.T) {
	addr, err := registry.ParseAddress("myreg")
	if err != nil {
		t.Fatal(err)
	}
	if backend, err := connectACR(addr, 250, nil); err != nil || backend == nil {
		t.Errorf("connectACR = %v, %v", backend, err)
	}
	if _, err := connectACR(addr, 0, nil); err == nil {
		t.Error("a zero page size should be refused")
	}
}

// TestUnusableCacheFailsFast: a --cache that cannot hold a directory fails the
// command before any registry access, instead of silently caching nothing.
func TestUnusableCacheFailsFast(t *testing.T) {
	file := filepath.Join(t.TempDir(), "cache")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := captureStdout(t, func() error {
		return newCommand().Run(context.Background(), []string{"acrprune", "-r", "myreg", "--cache", file, "--progress", "plain", "stats"})
	})
	if err == nil || !strings.Contains(err.Error(), "--cache") {
		t.Errorf("error = %v, want the unusable --cache reported", err)
	}
}

// TestWriteOutput: rewriting a shorter document must not leave the tail of
// the previous one behind.
func TestWriteOutput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stats.json")
	if err := writeOutput(path, []string{"a long first document", "with two entries"}); err != nil {
		t.Fatal(err)
	}
	if err := writeOutput(path, []string{"short"}); err != nil {
		t.Fatal(err)
	}
	if err := writeOutput(path, make(chan int)); err == nil {
		t.Fatal("encoding failure should leave the last snapshot untouched")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	if err := json.Unmarshal(data, &got); err != nil || len(got) != 1 || got[0] != "short" {
		t.Errorf("file = %q, %v; want only the last document", data, err)
	}
}

// TestExitCode: scripts must be able to tell an interrupted run from a failed
// one, as they can for processes a signal killed.
func TestExitCode(t *testing.T) {
	boom := errors.New("boom")
	for _, tt := range []struct {
		name       string
		err, cause error
		want       int
	}{
		{"success", nil, nil, 0},
		{"success despite a late signal", nil, signalCause{syscall.SIGINT}, 0},
		{"failure", boom, nil, 1},
		{"failure after cancellation without a signal", boom, context.Canceled, 1},
		{"interrupted", context.Canceled, signalCause{syscall.SIGINT}, 130},
		{"terminated", boom, signalCause{syscall.SIGTERM}, 143},
		{"canceled from the terminal display", fmt.Errorf("prune: %w", context.Canceled), nil, 130},
	} {
		if got := exitCode(tt.err, tt.cause); got != tt.want {
			t.Errorf("%s: exit code %d, want %d", tt.name, got, tt.want)
		}
	}
}

// TestResolveVersion: binaries installed with go install used to report
// "dev", although Go records the module version they were built from.
func TestResolveVersion(t *testing.T) {
	orig := version
	t.Cleanup(func() { version = orig })
	built := func(v string) *debug.BuildInfo { return &debug.BuildInfo{Main: debug.Module{Version: v}} }

	version = "dev"
	for _, tt := range []struct {
		info *debug.BuildInfo
		ok   bool
		want string
	}{
		{built("v0.1.23"), true, "v0.1.23"},
		{built("v0.1.24-0.20260930191259-0f4a75d5a339+dirty"), true, "v0.1.24-0.20260930191259-0f4a75d5a339+dirty"},
		{built("(devel)"), true, "dev"},
		{built(""), true, "dev"},
		{nil, false, "dev"},
	} {
		if got := resolveVersion(tt.info, tt.ok); got != tt.want {
			t.Errorf("resolveVersion(%+v, %v) = %q, want %q", tt.info, tt.ok, got, tt.want)
		}
	}

	version = "v1.2.3"
	if got := resolveVersion(built("v0.1.23"), true); got != "v1.2.3" {
		t.Errorf("resolveVersion = %q, want the version set at build time", got)
	}
}

// terminalStdin makes the commands see a terminal on stdin.
func terminalStdin(t *testing.T) {
	t.Helper()
	orig := stdinIsTerminal
	stdinIsTerminal = func() bool { return true }
	t.Cleanup(func() { stdinIsTerminal = orig })
}

// TestImplicitStdinOnTerminal: a forgotten --input used to wait silently for
// input typed at the terminal. "-" still reads it on purpose.
func TestImplicitStdinOnTerminal(t *testing.T) {
	terminalStdin(t)
	for _, tt := range []struct {
		args []string
		want string
	}{
		{[]string{"-r", "myreg", "prune"}, "no rule file given and stdin is a terminal"},
		{[]string{"-r", "myreg", "generate"}, "no image list given and stdin is a terminal"},
		{[]string{"top"}, "no statistics file given and stdin is a terminal"},
	} {
		cmd := newCommandWithConnector(func(context.Context, *cli.Command, registry.Address, *slog.Logger) (*registry.Registry, error) {
			t.Error("connected without input")
			return nil, errors.New("unexpected connection")
		})
		err := cmd.Run(t.Context(), append([]string{"acrprune", "--progress=plain"}, tt.args...))
		if err == nil || !strings.Contains(err.Error(), tt.want) || !strings.Contains(err.Error(), "--input -") {
			t.Errorf("%v: error = %v, want %q and the --input - hint", tt.args, err, tt.want)
		}
	}

	withStdin(t, "[]")
	out, err := captureStdout(t, func() error { return newCommand().Run(t.Context(), []string{"acrprune", "top", "--input", "-"}) })
	if err != nil || !strings.Contains(out, "NAME") {
		t.Errorf("top --input - on a terminal: output %q, error %v", out, err)
	}
}

// TestTopInputs: the statistics file may be given as an argument, and errors
// name it, with the position of malformed JSON.
func TestTopInputs(t *testing.T) {
	dir := t.TempDir()
	statsFile := filepath.Join(dir, "stats.json")
	if err := os.WriteFile(statsFile, []byte(`[{"name": "alloy", "unique": 2816819627}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := captureStdout(t, func() error { return newCommand().Run(t.Context(), []string{"acrprune", "top", statsFile}) })
	if err != nil || !strings.Contains(out, "alloy") {
		t.Errorf("top FILE: output %q, error %v", out, err)
	}

	broken := filepath.Join(dir, "broken.json")
	if err := os.WriteFile(broken, []byte("[\n  {\"name\": \"alloy\",}\n]"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct{ path, want string }{
		{broken, "statistics file " + broken + ": line 2, column 20: invalid character '}'"},
		{filepath.Join(dir, "missing.json"), "statistics file: open " + filepath.Join(dir, "missing.json")},
	} {
		_, err := captureStdout(t, func() error { return newCommand().Run(t.Context(), []string{"acrprune", "top", tt.path}) })
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("error = %v, want it to contain %q", err, tt.want)
		}
	}
}

// TestRunningFileErrorsNameTheFile: a malformed line in the running file
// used to be reported without saying which file it was in.
func TestRunningFileErrorsNameTheFile(t *testing.T) {
	addr, err := registry.ParseAddress("myreg")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "running.txt")
	if err := os.WriteFile(path, []byte("myreg.azurecr.io/app:v1\nmyreg.azurecr.io/App:v1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadRunningRules(path, addr, slog.New(slog.DiscardHandler)); err == nil || !strings.HasPrefix(err.Error(), "running file "+path+": image list line 2: ") {
		t.Errorf("error = %v, want the file and line named", err)
	}
}

// TestGenerateFlags: generate takes -o like statistics, and --input like the
// other commands reading a file.
func TestGenerateFlags(t *testing.T) {
	dir := t.TempDir()
	images := filepath.Join(dir, "images.txt")
	if err := os.WriteFile(images, []byte("myreg.azurecr.io/app:v1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "rules.json")
	if err := newCommand().Run(t.Context(), []string{"acrprune", "-r", "myreg", "generate", "--input", images, "-o", out}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var specs []struct {
		Repo string `json:"repo"`
	}
	if err := json.Unmarshal(data, &specs); err != nil || len(specs) != 1 || specs[0].Repo != "^app$" {
		t.Errorf("rule file = %s, %v; want a rule for app", data, err)
	}
}

// TestFlagsExplainThemselves: --help used to list --dry-run, the switch
// between planning and deleting, and other flags with no description.
func TestFlagsExplainThemselves(t *testing.T) {
	root := newCommand()
	for _, cmd := range append([]*cli.Command{root}, root.Commands...) {
		for _, f := range cmd.Flags {
			if doc, ok := f.(cli.DocGenerationFlag); !ok || doc.GetUsage() == "" {
				t.Errorf("%s: flag %v has no usage text", cmd.Name, f.Names())
			}
		}
	}
	prune := root.Command("prune")
	if usage := prune.Flags[slices.IndexFunc(prune.Flags, func(f cli.Flag) bool { return f.Names()[0] == "dry-run" })].(cli.DocGenerationFlag).GetUsage(); !strings.Contains(usage, "--dry-run=false") || !strings.Contains(usage, "default: true") {
		t.Errorf("--dry-run usage %q should give the default and how to delete", usage)
	}
}

// TestUnknownCommandSuggests: a mistyped command used to be reported as "No
// help topic", without the command meant; a command like none suggested "".
func TestUnknownCommandSuggests(t *testing.T) {
	_, err := captureStdout(t, func() error { return newCommand().Run(t.Context(), []string{"acrprune", "stat"}) })
	if err == nil || !strings.Contains(err.Error(), `unknown command "stat"; did you mean "stats"?`) {
		t.Errorf("error = %v, want a suggestion", err)
	}
	_, err = captureStdout(t, func() error { return newCommand().Run(t.Context(), []string{"acrprune", "xyzzy"}) })
	if err == nil || err.Error() != `unknown command "xyzzy" (see --help)` {
		t.Errorf("error = %v, want no suggestion", err)
	}
	out, err := captureStdout(t, func() error { return newCommand().Run(t.Context(), []string{"acrprune"}) })
	if err != nil || !strings.Contains(out, "COMMANDS:") {
		t.Errorf("no command: output %q, error %v; want the help", out, err)
	}
}

// TestThrottled: rewriting the statistics file after every repository cost
// time quadratic in their number.
func TestThrottled(t *testing.T) {
	now := time.Unix(0, 0)
	var written []int
	write := throttled(5*time.Second, func() time.Time { return now }, func(n int) error {
		written = append(written, n)
		return nil
	})
	for n := range 12 {
		if err := write(n); err != nil {
			t.Fatal(err)
		}
		now = now.Add(time.Second)
	}
	if want := []int{0, 5, 10}; !slices.Equal(written, want) {
		t.Errorf("wrote %v, want %v", written, want)
	}

	boom := errors.New("disk full")
	if err := throttled(time.Second, time.Now, func(int) error { return boom })(1); err != boom {
		t.Errorf("error = %v, want the write's", err)
	}
}
