// Command acrprune prunes Azure Container Registry manifests and
// repositories using declarative JSON rules.
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
	"path/filepath"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/containers/azcontainerregistry"
	"github.com/urfave/cli/v3"

	"github.com/JohanLindvall/acrprune/internal/pruner"
	"github.com/JohanLindvall/acrprune/internal/registry"
	"github.com/JohanLindvall/acrprune/internal/rules"
)

// version is set at build time via -ldflags "-X main.version=...".
var version = "dev"

// errRegistryRequired is returned by commands that access the registry when
// the --registry flag was not given. The flag is not marked required so that
// local-only commands such as top can run without it.
var errRegistryRequired = errors.New("required flag --registry not set")

func main() {
	cmd := newCommand()
	if err := cmd.Run(context.Background(), os.Args); err != nil {
		os.Exit(1)
	}
}

// newCommand builds the acrprune CLI command tree. It is split out from main
// so tests can inspect the flag wiring.
func newCommand() *cli.Command {
	logLevel := new(slog.LevelVar)
	logLevel.Set(slog.LevelInfo)

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: logLevel,
	}))

	var reg *registry.Registry

	// Keep -v bound to the verbose flag below; expose the version only as
	// --version so the auto-generated version flag does not claim -v.
	cli.VersionFlag = &cli.BoolFlag{Name: "version", Usage: "print the version"}

	cmd := &cli.Command{
		Name:    "acrprune",
		Version: version,
		Usage:   "prune Azure Container Registry manifests using declarative rules",
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
					if reg == nil {
						return errRegistryRequired
					}
					runningRules, err := loadRunningRules(cmd.String("running"), cmd.String("registry"))
					if err != nil {
						return err
					}

					outPath := cmd.String("output")
					out, closeOut, err := openOutput(outPath)
					if err != nil {
						return err
					}
					defer closeOut()
					var onUpdate func([]pruner.RepositoryStats) error
					if outPath != "" {
						// Rewrite the file after each repository so partial
						// results survive an interrupted run.
						onUpdate = func(stats []pruner.RepositoryStats) error {
							return rewriteJSON(out, stats)
						}
					}

					// Denied repositories yield partial stats and an error;
					// write what was collected either way.
					stats, err := pruner.CollectRegistryStats(ctx, reg, runningRules, onUpdate)
					if stats == nil {
						return err
					}
					if outPath == "" {
						return errors.Join(writeJSON(out, stats), err)
					}
					// Write once more so an empty registry still leaves valid
					// JSON behind rather than an empty file.
					return errors.Join(rewriteJSON(out, stats), err)
				},
			},
			{
				Name:  "prune",
				Usage: "delete manifests and empty repositories according to a rule file",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "input", Aliases: []string{"in", "infile"}, Usage: "rule file (defaults to stdin)"},
					&cli.BoolFlag{Name: "dry-run", Aliases: []string{"dryrun"}, Value: true},
					&cli.DurationFlag{Name: "keep-younger", Aliases: []string{"keepyounger"}, Value: 24 * time.Hour, Usage: "never delete manifests updated within this period"},
					&cli.BoolFlag{Name: "include-locked", Aliases: []string{"includelocked"}, Usage: "unlock delete/write-disabled manifests and tags before deleting them"},
				},
				Action: func(ctx context.Context, cmd *cli.Command) error {
					if reg == nil {
						return errRegistryRequired
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

					p := &pruner.Pruner{
						Registry:      reg,
						Logger:        logger,
						DryRun:        cmd.Bool("dry-run"),
						KeepYounger:   keepYounger,
						IncludeLocked: cmd.Bool("include-locked"),
					}
					return p.Prune(ctx, ruleSet)
				},
			},
			{
				Name:  "generate",
				Usage: "generate a rule file keeping only the images listed on stdin",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "output", Aliases: []string{"out", "outfile"}},
				},
				Action: func(ctx context.Context, cmd *cli.Command) error {
					if cmd.String("registry") == "" {
						return errRegistryRequired
					}
					specs, err := rules.KeepRulesFromImageList(os.Stdin, cmd.String("registry"))
					if err != nil {
						return err
					}
					logger.Debug("Built rules", "rules", len(specs))
					out, closeOut, err := openOutput(cmd.String("output"))
					if err != nil {
						return err
					}
					defer closeOut()
					return writeJSON(out, specs)
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

			registryName := cmd.String("registry")
			if registryName == "" {
				// Local-only commands (top) run without Azure credentials;
				// commands that access the registry fail with
				// errRegistryRequired instead.
				return ctx, nil
			}
			var cache *registry.Cache
			if dir := cmd.String("cache"); dir != "" {
				cache = registry.NewCache(filepath.Join(dir, registryName))
			}

			cred, err := azidentity.NewDefaultAzureCredential(nil)
			if err != nil {
				return ctx, err
			}

			client, err := azcontainerregistry.NewClient(loginServerURL(registryName), cred, &azcontainerregistry.ClientOptions{
				ClientOptions: azcore.ClientOptions{Telemetry: policy.TelemetryOptions{ApplicationID: "acrprune"}},
			})
			if err != nil {
				return ctx, err
			}

			reg, err = registry.New(client, logger, pageSize, parallelism, cache)
			return ctx, err
		},

		Flags: []cli.Flag{
			&cli.StringFlag{Name: "registry", Aliases: []string{"r"}, Usage: "registry name or full login server (required except for local-only commands)"},
			&cli.StringFlag{Name: "cache", Aliases: []string{"c"}, Usage: "directory for caching downloaded manifests"},
			&cli.IntFlag{Name: "page-size", Aliases: []string{"pagesize"}, Value: 250},
			&cli.IntFlag{Name: "parallelism", Value: 16},
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
	if path == "" {
		return os.Stdin, func() {}, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	return f, func() { _ = f.Close() }, nil
}

// openOutput opens path for writing, creating or truncating it, and falls
// back to stdout when path is empty. The returned function closes the file,
// and does nothing for stdout.
func openOutput(path string) (*os.File, func(), error) {
	if path == "" {
		return os.Stdout, func() {}, nil
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return nil, nil, err
	}
	return f, func() { _ = f.Close() }, nil
}

// loadRunningRules compiles the keep-rules describing the images listed in
// path, or returns nil when no file was given.
func loadRunningRules(path, registryName string) ([]*rules.RepoRule, error) {
	if path == "" {
		return nil, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("failed to open running file: %w", err)
	}
	defer func() { _ = f.Close() }()

	specs, err := rules.KeepRulesFromImageList(f, registryName)
	if err != nil {
		return nil, err
	}
	return rules.Compile(specs)
}

// loginServerURL turns a registry name into its login server URL; names
// containing a dot are treated as complete login servers, so sovereign-cloud
// registries can be passed directly.
func loginServerURL(registryName string) string {
	return "https://" + registry.LoginServer(registryName)
}

func writeJSON(w io.Writer, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	_, err = w.Write(append(data, '\n'))
	return err
}

// rewriteJSON replaces the whole file with v, truncating whatever was there
// before so a shorter document cannot leave a tail of the previous one behind.
func rewriteJSON(f *os.File, v any) error {
	if err := f.Truncate(0); err != nil {
		return err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	return writeJSON(f, v)
}
