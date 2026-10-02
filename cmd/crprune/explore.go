package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
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
		Name: "explore", Usage: "browse a statistics snapshot; optionally review and confirm live cleanup",
		ArgsUsage: "[stats.json]",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "input", Aliases: []string{"in", "infile"}, Usage: "statistics JSON file (defaults to stdin)"},
			&cli.StringFlag{Name: "sort", Aliases: []string{"s"}, Value: "unique", Usage: "initial sort key: " + strings.Join(pruner.StatSortKeys(), ", ")},
			&cli.BoolFlag{Name: "reverse", Usage: "reverse the initial sort order"},
			&cli.StringFlag{Name: "filter", Aliases: []string{"f"}, Usage: "initial repository name search"},
			&cli.StringFlag{Name: "rules", Usage: "rule file to apply using p (current repository) or P (all snapshot repositories)"},
			&ruleDurationFlag{Name: "keep-younger", Value: 24 * time.Hour, Usage: "protect images updated within this duration during cleanup (e.g. 24h, 7d)"},
			&cli.BoolFlag{Name: "include-locked", Usage: "allow reviewed cleanup to unlock and delete locked images (ACR)"},
			&cli.StringFlag{Name: "running", Usage: "file of running images to protect during cleanup"},
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
			stats, err := parseInput(path, "statistics file", pruner.ReadStats)
			if err != nil {
				return err
			}
			opts := explore.Options{Source: inputName(path), Sort: cmd.String("sort"), Reverse: cmd.Bool("reverse"), Filter: cmd.String("filter"), Logger: logger}
			if path != "" && path != "-" {
				opts.Reload = func() ([]pruner.RepositoryStats, error) { return parseInput(path, "statistics file", pruner.ReadStats) }
			}
			if in := interruptsOf(ctx); in != nil {
				opts.OnLogger = func(log *slog.Logger) { in.display.Store(log) }
			}
			if cmd.String("registry") != "" {
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
						return errors.New("--rules requires a named file; stdin is used for statistics")
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
			} else if cmd.String("rules") != "" || cmd.String("running") != "" || cmd.Bool("include-locked") {
				return errors.New("--rules, --running and --include-locked need --registry for live explorer actions")
			}
			return runExplorer(ctx, stats, opts)
		},
	}
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
