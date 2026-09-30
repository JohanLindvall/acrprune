package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/urfave/cli/v3"

	"github.com/JohanLindvall/acrprune/internal/registry"
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
	if ruleSet, err := loadRunningRules("", addr); ruleSet != nil || err != nil {
		t.Errorf("no file should yield no rules, got %v, %v", ruleSet, err)
	}

	path := filepath.Join(t.TempDir(), "running.txt")
	if err := os.WriteFile(path, []byte("ghcr.io/acme/app:v1\nmyreg.azurecr.io/app:v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ruleSet, err := loadRunningRules(path, addr)
	if err != nil {
		t.Fatal(err)
	}
	if len(ruleSet) != 1 || !ruleSet[0].Repo.MatchString("app") {
		t.Errorf("running rules = %+v, want one for app", ruleSet)
	}

	if _, err := loadRunningRules(filepath.Join(t.TempDir(), "missing"), addr); err == nil {
		t.Error("a missing running file should fail")
	}
}

// TestConnectACR builds the ACR backend, which needs no network access until
// it is used.
func TestConnectACR(t *testing.T) {
	addr, err := registry.ParseAddress("myreg")
	if err != nil {
		t.Fatal(err)
	}
	if backend, err := connectACR(addr, 250); err != nil || backend == nil {
		t.Errorf("connectACR = %v, %v", backend, err)
	}
	if _, err := connectACR(addr, 0); err == nil {
		t.Error("a zero page size should be refused")
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
