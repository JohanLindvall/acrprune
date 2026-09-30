// Command acrprune prunes manifests and repositories of Azure Container
// Registry and GitHub Container Registry using declarative JSON rules.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/containers/azcontainerregistry"
	"github.com/urfave/cli/v3"

	"github.com/JohanLindvall/acrprune/internal/fileio"
	"github.com/JohanLindvall/acrprune/internal/progress"
	"github.com/JohanLindvall/acrprune/internal/pruner"
	"github.com/JohanLindvall/acrprune/internal/registry"
	"github.com/JohanLindvall/acrprune/internal/registry/acr"
	"github.com/JohanLindvall/acrprune/internal/registry/ghcr"
	"github.com/JohanLindvall/acrprune/internal/rules"
)

// version is set at build time via -ldflags "-X main.version=...".
var version = "dev"

// errRegistryRequired is returned by commands that need the registry when the
// --registry flag was not given. The flag is not marked required so that
// local-only commands such as top can run without it.
var errRegistryRequired = errors.New("required flag --registry not set")

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	cmd := newCommand()
	err := cmd.Run(ctx, os.Args)
	stop()
	if err != nil {
		os.Exit(1)
	}
}

// newCommand builds the acrprune CLI command tree. It is split out from main
// so tests can inspect the flag wiring.
func newCommand() *cli.Command {
	return newCommandWithConnector(connect)
}

func newCommandWithConnector(connect func(context.Context, *cli.Command, registry.Address, *slog.Logger) (*registry.Registry, error)) *cli.Command {
	logLevel := new(slog.LevelVar)
	logLevel.Set(slog.LevelInfo)

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: logLevel,
	}))

	// Keep -v bound to the verbose flag below; expose the version only as
	// --version so the auto-generated version flag does not claim -v.
	cli.VersionFlag = &cli.BoolFlag{Name: "version", Usage: "print the version"}

	cmd := &cli.Command{
		Name:    "acrprune",
		Version: version,
		Usage:   "prune Azure Container Registry and GitHub Container Registry manifests using declarative rules",
		Commands: []*cli.Command{
			{
				Name:    "statistics",
				Aliases: []string{"stats"},
				Usage:   "write per-repository size and manifest statistics as JSON",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "output", Aliases: []string{"o", "out", "outfile"}},
					&cli.StringFlag{Name: "running", Usage: "file of running images used to annotate stats"},
				},
				Action: func(ctx context.Context, cmd *cli.Command) error {
					if err := noArguments(cmd); err != nil {
						return err
					}
					addr, err := address(cmd)
					if err != nil {
						return err
					}
					runningRules, err := loadRunningRules(cmd.String("running"), addr)
					if err != nil {
						return err
					}
					return progress.Run(ctx, progress.Options{Mode: cmd.String("progress"), Operation: "statistics", Registry: addr.String()}, logger, func(ctx context.Context, logger *slog.Logger) error {
						reg, err := connect(ctx, cmd, addr, logger)
						if err != nil {
							return err
						}

						outPath := cmd.String("output")
						var onUpdate func([]pruner.RepositoryStats) error
						if outPath != "" && outPath != "-" {
							// Rewrite the file after each repository so partial
							// results survive an interrupted run.
							onUpdate = func(stats []pruner.RepositoryStats) error {
								return writeOutput(outPath, stats)
							}
						}

						// Denied repositories yield partial stats and an error;
						// write what was collected either way.
						stats, err := pruner.CollectRegistryStats(ctx, reg, runningRules, onUpdate)
						if stats == nil {
							return err
						}
						// Write once more so an empty registry still leaves valid
						// JSON behind rather than an empty file.
						return errors.Join(writeOutput(outPath, stats), err)
					})
				},
			},
			{
				Name:  "prune",
				Usage: "delete manifests and empty repositories according to a rule file",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "input", Aliases: []string{"in", "infile"}, Usage: "rule file (defaults to stdin)"},
					&cli.BoolFlag{Name: "dry-run", Aliases: []string{"dryrun"}, Value: true},
					&cli.DurationFlag{Name: "keep-younger", Aliases: []string{"keepyounger"}, Value: 24 * time.Hour, Usage: "never delete manifests updated within this period"},
					&cli.BoolFlag{Name: "include-locked", Aliases: []string{"includelocked"}, Usage: "unlock delete/write-disabled manifests and tags before deleting them (ACR)"},
				},
				Action: func(ctx context.Context, cmd *cli.Command) error {
					if err := noArguments(cmd); err != nil {
						return err
					}
					addr, err := address(cmd)
					if err != nil {
						return err
					}
					keepYounger := cmd.Duration("keep-younger")
					if keepYounger < 0 {
						return errors.New("--keep-younger must not be negative")
					}

					in, closeIn, err := openInput(cmd.String("input"))
					if err != nil {
						return err
					}
					defer closeIn()

					specs, err := rules.ParseSpecs(in)
					if err != nil {
						return err
					}
					ruleSet, err := rules.Compile(specs)
					if err != nil {
						return err
					}

					return progress.Run(ctx, progress.Options{Mode: cmd.String("progress"), Operation: "prune", Registry: addr.String(), DryRun: cmd.Bool("dry-run")}, logger, func(ctx context.Context, logger *slog.Logger) error {
						reg, err := connect(ctx, cmd, addr, logger)
						if err != nil {
							return err
						}
						p := &pruner.Pruner{
							Registry:      reg,
							Logger:        logger,
							DryRun:        cmd.Bool("dry-run"),
							KeepYounger:   keepYounger,
							IncludeLocked: cmd.Bool("include-locked"),
						}
						return p.Prune(ctx, ruleSet)
					})
				},
			},
			{
				Name:  "generate",
				Usage: "generate a rule file keeping only the images listed on stdin",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "output", Aliases: []string{"out", "outfile"}},
				},
				Action: func(ctx context.Context, cmd *cli.Command) error {
					if err := noArguments(cmd); err != nil {
						return err
					}
					addr, err := address(cmd)
					if err != nil {
						return err
					}
					specs, err := rules.KeepRulesFromImageList(os.Stdin, addr.String())
					if err != nil {
						return err
					}
					logger.Debug("Built rules", "rules", len(specs))
					return writeOutput(cmd.String("output"), specs)
				},
			},
			{
				Name:  "top",
				Usage: "print the top repositories from a statistics JSON file as a table",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "input", Aliases: []string{"in", "infile"}, Usage: "statistics JSON file (defaults to stdin)"},
					&cli.StringFlag{Name: "sort", Aliases: []string{"s"}, Value: "unique", Usage: "sort key: " + strings.Join(pruner.StatSortKeys(), ", ")},
					&cli.IntFlag{Name: "top", Aliases: []string{"k", "n"}, Value: 20, Usage: "number of rows to print (0 for all)"},
				},
				ArgsUsage: "[stats.json]",
				Action: func(ctx context.Context, cmd *cli.Command) error {
					if cmd.Int("top") < 0 {
						return errors.New("--top must not be negative (use 0 for all rows)")
					}
					if err := pruner.SortStatsBy(nil, cmd.String("sort")); err != nil {
						return err
					}
					// Refuse ambiguity instead of silently ignoring an input.
					if cmd.Args().Len() > 1 {
						return fmt.Errorf("expected at most one stats file argument, got %d", cmd.Args().Len())
					}
					inPath := cmd.String("input")
					if inPath != "" && cmd.Args().Len() > 0 {
						return errors.New("--input and a stats file argument are mutually exclusive")
					}
					if inPath == "" {
						inPath = cmd.Args().First()
					}
					in, closeIn, err := openInput(inPath)
					if err != nil {
						return err
					}
					defer closeIn()

					stats, err := pruner.ReadStats(in)
					if err != nil {
						return err
					}
					if err := pruner.SortStatsBy(stats, cmd.String("sort")); err != nil {
						return err
					}
					return pruner.WriteStatsTable(os.Stdout, stats, cmd.Int("top"))
				},
			},
		},

		Before: func(ctx context.Context, cmd *cli.Command) (context.Context, error) {
			if err := progress.ValidateMode(cmd.String("progress")); err != nil {
				return ctx, err
			}
			if cmd.Bool("verbose") {
				logLevel.Set(slog.LevelDebug)
			}

			pageSize := cmd.Int("page-size")
			if pageSize < 1 || pageSize > math.MaxInt32 {
				return ctx, fmt.Errorf("--page-size must be between 1 and %d, got %d", math.MaxInt32, pageSize)
			}
			// A parallelism of zero would stall every download group forever.
			parallelism := cmd.Int("parallelism")
			if parallelism < 1 {
				return ctx, fmt.Errorf("--parallelism must be at least 1, got %d", parallelism)
			}
			return ctx, nil
		},

		Flags: []cli.Flag{
			&cli.StringFlag{Name: "registry", Aliases: []string{"r"}, Usage: "ACR registry name or login server, or ghcr.io/<owner> (required except for local-only commands)"},
			&cli.StringFlag{Name: "cache", Aliases: []string{"c"}, Usage: "directory for caching downloaded manifests"},
			&cli.IntFlag{Name: "page-size", Aliases: []string{"pagesize"}, Value: 250, Usage: "items per listing request (GHCR caps it at 100)"},
			&cli.IntFlag{Name: "parallelism", Value: 16},
			&cli.StringFlag{Name: "progress", Value: "auto", Usage: "batch progress display: auto, plain or tui"},
			&cli.BoolFlag{Name: "verbose", Aliases: []string{"v"}},
		},
		ExitErrHandler: func(ctx context.Context, cmd *cli.Command, err error) {
			logger.Error("An error occurred", "err", err)
		},
	}

	return cmd
}

// openInput opens path for reading, falling back to stdin when it is empty.
// The returned function closes the file, and does nothing for stdin.
func openInput(path string) (io.Reader, func(), error) {
	if path == "" || path == "-" {
		return os.Stdin, func() {}, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	return f, func() { _ = f.Close() }, nil
}

// noArguments prevents a mistyped input filename from silently falling back
// to stdin, or an extra argument from being ignored before deleting data.
func noArguments(cmd *cli.Command) error {
	if cmd.Args().Len() != 0 {
		return fmt.Errorf("%s does not accept positional arguments: %q", cmd.Name, cmd.Args().Slice())
	}
	return nil
}

// address parses --registry for the commands that need it.
func address(cmd *cli.Command) (registry.Address, error) {
	name := cmd.String("registry")
	if name == "" {
		return registry.Address{}, errRegistryRequired
	}
	return registry.ParseAddress(name)
}

// connect opens the registry for the commands that access it, authenticating
// with Azure credentials on ACR and a GitHub token on GHCR.
func connect(ctx context.Context, cmd *cli.Command, addr registry.Address, logger *slog.Logger) (*registry.Registry, error) {
	var backend registry.Backend
	var err error
	switch addr.Kind {
	case registry.GHCR:
		backend, err = connectGHCR(ctx, addr, cmd.Int("page-size"), logger)
	default:
		backend, err = connectACR(addr, cmd.Int("page-size"))
	}
	if err != nil {
		return nil, err
	}
	var cache *registry.Cache
	if dir := cmd.String("cache"); dir != "" {
		cache = registry.NewCache(filepath.Join(dir, addr.String()))
	}
	return registry.New(backend, logger, cmd.Int("parallelism"), cache)
}

// connectACR builds the ACR backend, authenticating with
// DefaultAzureCredential.
func connectACR(addr registry.Address, pageSize int) (registry.Backend, error) {
	cred, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		return nil, err
	}
	client, err := azcontainerregistry.NewClient("https://"+addr.Host, cred, &azcontainerregistry.ClientOptions{
		ClientOptions: azcore.ClientOptions{Telemetry: policy.TelemetryOptions{ApplicationID: "acrprune"}},
	})
	if err != nil {
		return nil, err
	}
	backend, err := acr.New(client, pageSize)
	if err != nil {
		return nil, err
	}
	return backend, nil
}

// connectGHCR builds the GHCR backend for the address's owner.
func connectGHCR(ctx context.Context, addr registry.Address, pageSize int, logger *slog.Logger) (registry.Backend, error) {
	token, err := githubToken(ctx)
	if err != nil {
		return nil, err
	}
	backend, err := ghcr.New(ctx, ghcr.Options{
		Owner:    addr.Owner,
		Token:    token,
		Username: os.Getenv("GITHUB_ACTOR"),
		PageSize: pageSize,
		Logger:   logger,
	})
	if err != nil {
		return nil, err
	}
	return backend, nil
}

// githubToken returns the GitHub token for GHCR: $GH_TOKEN or $GITHUB_TOKEN,
// in the GitHub CLI's order of precedence, and otherwise the GitHub CLI's own
// login.
func githubToken(ctx context.Context) (string, error) {
	for _, name := range []string{"GH_TOKEN", "GITHUB_TOKEN"} {
		if token := strings.TrimSpace(os.Getenv(name)); token != "" {
			return token, nil
		}
	}
	if token, err := ghAuthToken(ctx); err == nil && token != "" {
		return token, nil
	}
	return "", errors.New("no GitHub token: set GH_TOKEN or GITHUB_TOKEN to a token with the read:packages scope (and delete:packages to prune), or log in with the GitHub CLI: gh auth login --scopes read:packages,delete:packages")
}

// ghAuthToken asks the GitHub CLI for its token; tests replace it.
var ghAuthToken = func(ctx context.Context) (string, error) {
	out, err := exec.CommandContext(ctx, "gh", "auth", "token", "--hostname", "github.com").Output()
	return strings.TrimSpace(string(out)), err
}

// loadRunningRules compiles the keep-rules describing the images listed in
// path, or returns nil when no file was given.
func loadRunningRules(path string, addr registry.Address) ([]*rules.RepoRule, error) {
	if path == "" {
		return nil, nil
	}
	f, closeIn, err := openInput(path)
	if err != nil {
		return nil, fmt.Errorf("failed to open running file: %w", err)
	}
	defer closeIn()

	specs, err := rules.KeepRulesFromImageList(f, addr.String())
	if err != nil {
		return nil, err
	}
	return rules.Compile(specs)
}

func writeJSON(w io.Writer, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	_, err = w.Write(append(data, '\n'))
	return err
}

// writeOutput publishes a complete JSON snapshot. Named files are replaced
// atomically, so interruptions and failed writes preserve the last snapshot.
func writeOutput(path string, v any) error {
	if path == "" || path == "-" {
		return writeJSON(os.Stdout, v)
	}
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return fileio.WriteFile(path, append(data, '\n'), 0o600)
}
