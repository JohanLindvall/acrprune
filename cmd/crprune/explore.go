package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/JohanLindvall/crprune/internal/explore"
	"github.com/JohanLindvall/crprune/internal/pruner"
	"github.com/JohanLindvall/crprune/internal/registry"
	"github.com/JohanLindvall/crprune/internal/rules"
	"github.com/urfave/cli/v3"
)

var runExplorer = explore.Run

type explorerWriteKey struct{}

func exploreCommand(connect func(context.Context, *cli.Command, registry.Address, *slog.Logger) (*registry.Registry, error), logger *slog.Logger) *cli.Command {
	return &cli.Command{
		Name: "explore", Usage: "browse live registry statistics or a snapshot; optionally review and confirm cleanup",
		ArgsUsage: "[stats.json]",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "input", Aliases: []string{"in", "infile"}, Usage: "statistics JSON file or - for stdin (defaults to piped input, otherwise scans --registry)"},
			&cli.StringFlag{Name: "sort", Aliases: []string{"s"}, Value: "unique", Usage: "initial sort key: " + strings.Join(pruner.StatSortKeys(), ", ")},
			&cli.BoolFlag{Name: "reverse", Usage: "reverse the initial sort order"},
			&cli.StringFlag{Name: "filter", Aliases: []string{"f"}, Usage: "initial repository name search"},
			&cli.StringFlag{Name: "rules", Usage: "initial rule file for p (current repository) or P (marked repositories or a wildcard/regex selection); l selects available rules"},
			&cli.StringFlag{Name: "rules-dir", Value: "rules", Usage: "directory of JSON rule files to load alongside the bundled examples"},
			&ruleDurationFlag{Name: "keep-younger", Value: 24 * time.Hour, Usage: "protect images updated within this duration during cleanup (e.g. 24h, 7d)"},
			&cli.BoolFlag{Name: "include-locked", Usage: "allow reviewed cleanup to unlock and delete locked images (ACR)"},
			&cli.StringFlag{Name: "running", Usage: "file of running images to annotate live statistics and protect during cleanup"},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			if err := pruner.SortStatsBy(nil, cmd.String("sort")); err != nil {
				return err
			}
			if cmd.Duration("keep-younger") < 0 {
				return errors.New("--keep-younger must not be negative")
			}
			path, err := statisticsPath(cmd)
			if err != nil {
				return err
			}
			stats, err := exploreStats(path)
			if err != nil {
				return err
			}
			opts := explore.Options{Source: inputName(path), Sort: cmd.String("sort"), Reverse: cmd.Bool("reverse"), Filter: cmd.String("filter"), Logger: logger, LiveStats: stats == nil}
			if path != "" && path != "-" {
				opts.Reload = func() ([]pruner.RepositoryStats, error) { return parseInput(path, "statistics file", pruner.ReadStats) }
			}
			if in := interruptsOf(ctx); in != nil {
				opts.OnLogger = func(log *slog.Logger) { in.display.Store(log) }
			}
			if cmd.String("registry") != "" || opts.LiveStats {
				addr, err := address(cmd)
				if err != nil {
					return err
				}
				protect, err := loadRunningRules(cmd.String("running"), addr, logger)
				if err != nil {
					return err
				}
				client := &explore.Client{Registry: addr.String(), KeepYounger: cmd.Duration("keep-younger"), IncludeLocked: cmd.Bool("include-locked"), Protect: protect, RuleSource: cmd.String("rules")}
				client.Connect = func(ctx context.Context, logger *slog.Logger, write bool) (*registry.Registry, error) {
					if in := interruptsOf(ctx); in != nil {
						in.unlocking.Store(write && client.IncludeLocked && addr.Kind == registry.ACR)
					}
					return connect(context.WithValue(ctx, explorerWriteKey{}, write), cmd, addr, logger)
				}
				if client.RuleSource != "" {
					if client.RuleSource == "-" {
						return errors.New("--rules requires a named file")
					}
					client.Rules, err = parseInput(client.RuleSource, "rule file", func(r io.Reader) ([]*rules.RepoRule, error) {
						specs, err := rules.ParseSpecs(r)
						if err != nil {
							return nil, err
						}
						return rules.Compile(specs)
					})
					if err != nil {
						return err
					}
				}
				opts.Client = client
				if opts.LiveStats {
					opts.Source = "Live statistics from " + addr.String()
				}
			} else if cmd.String("rules") != "" || cmd.String("running") != "" || cmd.Bool("include-locked") {
				return errors.New("--rules, --running and --include-locked need --registry for live explorer actions")
			}
			if cmd.IsSet("rules-dir") {
				info, err := os.Stat(cmd.String("rules-dir"))
				if err != nil {
					return fmt.Errorf("--rules-dir: %w", err)
				}
				if !info.IsDir() {
					return fmt.Errorf("--rules-dir: %s is not a directory", cmd.String("rules-dir"))
				}
			}
			opts.RuleFiles = explore.DiscoverRules(cmd.String("rules-dir"))
			return runExplorer(ctx, stats, opts)
		},
	}
}

// exploreStats returns nil only when no input was supplied, so the explorer
// can scan the registry. Explicit files and stdin must contain valid JSON;
// even an empty JSON array is a snapshot, not a request for a live scan.
func exploreStats(path string) ([]pruner.RepositoryStats, error) {
	if path == "" && stdinIsTerminal() {
		return nil, nil
	}
	return parseInput(path, "statistics file", func(r io.Reader) ([]pruner.RepositoryStats, error) {
		if path == "" {
			buffered := bufio.NewReader(r)
			if _, err := buffered.Peek(1); err == io.EOF {
				return nil, nil
			}
			r = buffered
		}
		return pruner.ReadStats(r)
	})
}

func statisticsPath(cmd *cli.Command) (string, error) {
	if cmd.Args().Len() > 1 {
		return "", fmt.Errorf("expected at most one stats file argument, got %d", cmd.Args().Len())
	}
	path := cmd.String("input")
	if path != "" && cmd.Args().Len() > 0 {
		return "", errors.New("--input and a stats file argument are mutually exclusive")
	}
	if path == "" {
		path = cmd.Args().First()
	}
	return path, nil
}
