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
					var runningRules []*rules.RepoRule
					if runningFile := cmd.String("running"); runningFile != "" {
						f, err := os.Open(runningFile)
						if err != nil {
							return fmt.Errorf("failed to open running file: %w", err)
						}
						specs := rules.KeepRulesFromImageList(f, cmd.String("registry"))
						_ = f.Close()
						if runningRules, err = rules.Compile(specs); err != nil {
							return err
						}
					}

					var out *os.File
					var onUpdate func([]pruner.RepositoryStats) error
					if outPath := cmd.String("output"); outPath != "" {
						var err error
						if out, err = os.OpenFile(outPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644); err != nil {
							return err
						}
						defer func() { _ = out.Close() }()
						// Rewrite the file after each repository so partial
						// results survive an interrupted run.
						onUpdate = func(stats []pruner.RepositoryStats) error {
							if _, err := out.Seek(0, io.SeekStart); err != nil {
								return err
							}
							return writeJSON(out, stats)
						}
					}

					stats, err := pruner.CollectRegistryStats(ctx, reg, runningRules, onUpdate)
					if err != nil {
						return err
					}
					if out == nil {
						return writeJSON(os.Stdout, stats)
					}
					return nil
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
					var in io.Reader = os.Stdin
					if inPath := cmd.String("input"); inPath != "" {
						f, err := os.Open(inPath)
						if err != nil {
							return err
						}
						defer func() { _ = f.Close() }()
						in = f
					}
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
						KeepYounger:   cmd.Duration("keep-younger"),
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
					specs := rules.KeepRulesFromImageList(os.Stdin, cmd.String("registry"))
					logger.Debug("Built rules", "rules", len(specs))
					out := os.Stdout
					if outPath := cmd.String("output"); outPath != "" {
						var err error
						if out, err = os.OpenFile(outPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644); err != nil {
							return err
						}
						defer func() { _ = out.Close() }()
					}
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
					inPath := cmd.String("input")
					if inPath == "" {
						inPath = cmd.Args().First()
					}
					var in io.Reader = os.Stdin
					if inPath != "" {
						f, err := os.Open(inPath)
						if err != nil {
							return err
						}
						defer func() { _ = f.Close() }()
						in = f
					}
					stats, err := pruner.ReadStats(in)
					if err != nil {
						return err
					}
					if err := pruner.SortStatsBy(stats, cmd.String("sort")); err != nil {
						return err
					}
					return pruner.WriteStatsTable(os.Stdout, stats, int(cmd.Int("top")))
				},
			},
		},

		Before: func(ctx context.Context, cmd *cli.Command) (context.Context, error) {
			if cmd.Bool("verbose") {
				logLevel.Set(slog.LevelDebug)
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

			reg = registry.New(client, logger, int(cmd.Int("page-size")), int(cmd.Int("parallelism")), cache)
			return ctx, nil
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

// loginServerURL turns a bare registry name into its azurecr.io login server
// URL; names containing a dot are treated as complete login servers, so
// sovereign-cloud registries can be passed directly.
func loginServerURL(registryName string) string {
	if strings.Contains(registryName, ".") {
		return "https://" + registryName
	}
	return fmt.Sprintf("https://%s.azurecr.io", registryName)
}

func writeJSON(w io.Writer, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	_, err = w.Write(data)
	return err
}
