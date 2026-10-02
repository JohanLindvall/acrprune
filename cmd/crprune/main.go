// Command crprune prunes manifests and repositories of Azure Container
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
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/urfave/cli/v3"
	"golang.org/x/term"

	"github.com/JohanLindvall/crprune/internal/fileio"
	"github.com/JohanLindvall/crprune/internal/progress"
	"github.com/JohanLindvall/crprune/internal/pruner"
	"github.com/JohanLindvall/crprune/internal/registry"
	"github.com/JohanLindvall/crprune/internal/registry/acr"
	"github.com/JohanLindvall/crprune/internal/registry/ghcr"
	"github.com/JohanLindvall/crprune/internal/rules"
)

// version is set at build time via -ldflags "-X main.version=...". Other
// builds report the module version Go recorded in the binary, if any: see
// resolveVersion.
var version = "dev"

// errRegistryRequired is returned by commands that need the registry when the
// --registry flag was not given. The flag is not marked required so that
// local-only commands such as top can run without it.
var errRegistryRequired = errors.New("required flag --registry not set")

// snapshotInterval is the least time between two rewrites of a statistics
// output file during a scan.
const snapshotInterval = 5 * time.Second

// Seams for tests: whether stdin is a terminal, the progress display, and the
// GHCR backend.
var (
	stdinIsTerminal = func() bool { return term.IsTerminal(int(os.Stdin.Fd())) }
	runProgress     = progress.Run
	newGHCR         = ghcr.New
)

func main() {
	os.Exit(run(os.Args))
}

// run runs the command line args and returns the exit status, as exitCode
// determines it. SIGINT and SIGTERM end the run as interrupts describes.
func run(args []string) int {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	done := make(chan struct{})
	defer close(done)
	in := &interrupts{}
	go in.handle(signals, done, cancel)
	err := newCommand().Run(withInterrupts(ctx, in), args)
	return exitCode(err, context.Cause(ctx))
}

// exitCode returns the exit status of a run that returned err, whose context
// was canceled with cause, if at all: 0 on success, 1 on failure, and for a
// run cut short 128 plus the number of the signal, as shells report a process
// that signal killed.
func exitCode(err, cause error) int {
	if err == nil {
		return 0
	}
	if c, ok := errors.AsType[signalCause](cause); ok {
		return signalStatus(c.Signal)
	}
	// Ctrl-C in the terminal display is a key, not a signal, but interrupts
	// the run all the same.
	if errors.Is(err, context.Canceled) {
		return signalStatus(syscall.SIGINT)
	}
	return 1
}

// resolveVersion returns the version to report: the one set at build time,
// else the module version that go install recorded in the binary, as
// debug.ReadBuildInfo returns it, else "dev".
func resolveVersion(info *debug.BuildInfo, ok bool) string {
	if version != "dev" || !ok || info.Main.Version == "" || info.Main.Version == "(devel)" {
		return version
	}
	return info.Main.Version
}

// newCommand builds the crprune CLI command tree. It is split out from main
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
		Name:    "crprune",
		Version: resolveVersion(debug.ReadBuildInfo()),
		Usage:   "prune Azure Container Registry and GitHub Container Registry manifests using declarative rules",
		// Only an unknown command reaches the root action with arguments;
		// urfave/cli would report it as a missing help topic.
		Action: func(ctx context.Context, cmd *cli.Command) error {
			if !cmd.Args().Present() {
				return cli.ShowRootCommandHelp(cmd)
			}
			name := cmd.Args().First()
			if suggestion := cli.SuggestCommand(cmd.Commands, name); suggestion != "" && nearCommand(name, suggestion) {
				return fmt.Errorf("unknown command %q; did you mean %q? (see --help)", name, suggestion)
			}
			return fmt.Errorf("unknown command %q (see --help)", name)
		},
		Commands: []*cli.Command{
			{
				Name:    "statistics",
				Aliases: []string{"stats"},
				Usage:   "write per-repository size and manifest statistics as JSON",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "output", Aliases: []string{"o", "out", "outfile"}, Usage: "statistics JSON file, rewritten during the scan (defaults to stdout)"},
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
					runningRules, err := loadRunningRules(cmd.String("running"), addr, logger)
					if err != nil {
						return err
					}
					outPath := cmd.String("output")
					toStdout := outPath == "" || outPath == "-"
					// Statistics for stdout are written once the progress
					// display has restored the terminal: written while it
					// runs, they would land on its alternate screen, which is
					// discarded when it closes.
					var stdoutStats []pruner.RepositoryStats
					err = display(ctx, progress.Options{Mode: cmd.String("progress"), Operation: "statistics", Registry: addr.String()}, logger, func(ctx context.Context, logger *slog.Logger) error {
						reg, err := connect(ctx, cmd, addr, logger)
						if err != nil {
							return err
						}

						var onUpdate func([]pruner.RepositoryStats) error
						if !toStdout {
							// Rewrite the file periodically so partial results
							// survive an interrupted run. Rewriting it after
							// every repository would take time quadratic in
							// their number.
							onUpdate = throttled(snapshotInterval, time.Now, func(stats []pruner.RepositoryStats) error {
								return writeOutput(outPath, stats)
							})
						}

						// Denied repositories yield partial stats and an error;
						// write what was collected either way.
						stats, err := pruner.CollectRegistryStats(ctx, reg, runningRules, onUpdate)
						if toStdout {
							stdoutStats = stats
							return err
						}
						if stats == nil {
							return err
						}
						// Write once more so the file holds every repository,
						// and an empty registry still leaves valid JSON behind
						// rather than an empty file.
						return errors.Join(writeOutput(outPath, stats), err)
					})
					if stdoutStats != nil {
						err = errors.Join(writeOutput(outPath, stdoutStats), err)
					}
					return err
				},
			},
			{
				Name:  "prune",
				Usage: "delete manifests and empty repositories according to a rule file",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "input", Aliases: []string{"in", "infile"}, Usage: "rule file (defaults to stdin)"},
					&cli.BoolFlag{Name: "dry-run", Aliases: []string{"dryrun"}, Value: true, Usage: "only log what would be deleted (default: true); pass --dry-run=false to delete"},
					&ruleDurationFlag{Name: "keep-younger", Aliases: []string{"keepyounger"}, Value: 24 * time.Hour, Usage: "never delete manifests updated within this period (e.g. 36h, 7d, 2w)"},
					&cli.BoolFlag{Name: "include-locked", Aliases: []string{"includelocked"}, Usage: "delete locked manifests too, unlocking them and their tags just before (ACR); they are kept by default"},
					&cli.StringFlag{Name: "running", Usage: "file of running images never to delete, whatever the rules say"},
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
					if in := interruptsOf(ctx); in != nil && addr.Kind == registry.ACR && cmd.Bool("include-locked") && !cmd.Bool("dry-run") {
						in.unlocking.Store(true) // GHCR has no locks
					}

					ruleSet, err := parseInput(cmd.String("input"), "rule file", func(r io.Reader) ([]*rules.RepoRule, error) {
						specs, err := rules.ParseSpecs(r)
						if err != nil {
							return nil, err
						}
						return rules.Compile(specs)
					})
					if err != nil {
						return err
					}
					warnings := rules.Warnings(ruleSet)
					runningRules, err := loadRunningRules(cmd.String("running"), addr, logger)
					if err != nil {
						return err
					}

					return display(ctx, progress.Options{Mode: cmd.String("progress"), Operation: "prune", Registry: addr.String(), DryRun: cmd.Bool("dry-run")}, logger, func(ctx context.Context, logger *slog.Logger) error {
						for _, warning := range warnings {
							logger.Warn("Rule never applies", "file", inputName(cmd.String("input")), "detail", warning)
						}
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
							Protect:       runningRules,
						}
						return p.Prune(ctx, ruleSet)
					})
				},
			},
			{
				Name:  "generate",
				Usage: "generate a rule file keeping only the images listed in the input",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "input", Aliases: []string{"in", "infile"}, Usage: "image list, one reference per line (defaults to stdin)"},
					&cli.StringFlag{Name: "output", Aliases: []string{"o", "out", "outfile"}, Usage: "rule file to write (defaults to stdout)"},
				},
				Action: func(ctx context.Context, cmd *cli.Command) error {
					if err := noArguments(cmd); err != nil {
						return err
					}
					addr, err := address(cmd)
					if err != nil {
						return err
					}
					var counts rules.ImageListCounts
					specs, err := parseInput(cmd.String("input"), "image list", func(r io.Reader) ([]*rules.RepoRuleSpec, error) {
						specs, c, err := rules.KeepRulesFromImageList(r, addr.String())
						counts = c
						return specs, err
					})
					if err != nil {
						return err
					}
					if len(specs) == 0 {
						// Safe, as an empty rule file prunes nothing, but most
						// likely a failed inventory or the wrong --registry.
						logger.Warn("No image in the input belongs to this registry; the rule file is empty",
							"lines", counts.Lines, "expected_prefix", addr.String()+"/")
					}
					logger.Info("Built rules", "rules", len(specs), "lines", counts.Lines,
						"matched", counts.Matched, "ignored", counts.Lines-counts.Matched)
					return writeOutput(cmd.String("output"), specs)
				},
			},
			exploreCommand(connect, logger),
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
					inPath, err := statisticsPath(cmd)
					if err != nil {
						return err
					}
					stats, err := parseInput(inPath, "statistics file", pruner.ReadStats)
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
			if in := interruptsOf(ctx); in != nil {
				in.base.Store(logger)
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
			&cli.StringFlag{Name: "registry", Aliases: []string{"r"}, Usage: "ACR registry name (myreg) or login server (myreg.azurecr.io), or ghcr.io/<owner> (required except for top and local explore)"},
			&cli.StringFlag{Name: "cache", Aliases: []string{"c"}, Usage: "directory for caching downloaded manifests, created if missing"},
			&cli.IntFlag{Name: "page-size", Aliases: []string{"pagesize"}, Value: 250, Usage: "items per listing request (GHCR caps it at 100)"},
			&cli.IntFlag{Name: "parallelism", Value: 16, Usage: "number of concurrent registry requests"},
			&cli.StringFlag{Name: "progress", Value: "auto", Usage: "batch progress display: auto, plain or tui"},
			&cli.BoolFlag{Name: "verbose", Aliases: []string{"v"}, Usage: "log debug messages"},
		},
		ExitErrHandler: func(ctx context.Context, cmd *cli.Command, err error) {
			// Keep the interrupt exit status without reporting the user's
			// cancellation as a failure (e.g. Ctrl-C in the explorer).
			if cancellationOnly(err) {
				return
			}
			logger.Error("An error occurred", "err", err)
		},
	}

	return cmd
}

// ruleDurationFlag is a duration flag accepting the syntax of rule file
// durations, days and weeks included.
type ruleDurationFlag = cli.FlagBase[time.Duration, cli.NoConfig, ruleDuration]

// ruleDuration is the cli.Value of a ruleDurationFlag.
type ruleDuration time.Duration

// Create points the flag's value at p, initialized to val.
func (ruleDuration) Create(val time.Duration, p *time.Duration, _ cli.NoConfig) cli.Value {
	*p = val
	return (*ruleDuration)(p)
}

// ToString formats val for the help text.
func (ruleDuration) ToString(val time.Duration) string {
	return val.String()
}

// Set parses s with rules.ParseDuration.
func (d *ruleDuration) Set(s string) error {
	v, err := rules.ParseDuration(s)
	if err != nil {
		return err
	}
	*d = ruleDuration(v)
	return nil
}

// Get returns the duration as a time.Duration, for cli.Command.Duration.
func (d *ruleDuration) Get() any {
	return time.Duration(*d)
}

// String formats the duration as time.Duration does.
func (d *ruleDuration) String() string {
	return time.Duration(*d).String()
}

// openInput opens path for reading, "-" meaning stdin. An empty path means
// stdin too, unless stdin is a terminal: a forgotten --input would then wait
// silently for the input, what, to be typed. The returned function closes the
// file, and does nothing for stdin.
func openInput(path, what string) (io.Reader, func(), error) {
	switch path {
	case "":
		if stdinIsTerminal() {
			return nil, nil, fmt.Errorf("no %s given and stdin is a terminal: pass --input FILE, pipe it to stdin, or use --input - to type it", what)
		}
		fallthrough
	case "-":
		return os.Stdin, func() {}, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", what, err)
	}
	return f, func() { _ = f.Close() }, nil
}

// inputName names the input at path in messages.
func inputName(path string) string {
	if path == "" || path == "-" {
		return "stdin"
	}
	return path
}

// parseInput parses the input, what, that openInput opens at path. Parse
// errors are prefixed with what and the file name, or stdin.
func parseInput[T any](path, what string, parse func(io.Reader) (T, error)) (T, error) {
	var zero T
	in, closeIn, err := openInput(path, what)
	if err != nil {
		return zero, err
	}
	defer closeIn()
	v, err := parse(in)
	if err != nil {
		return zero, fmt.Errorf("%s %s: %w", what, inputName(path), err)
	}
	return v, nil
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
		// Only a run that deletes needs to delete; checking the token for
		// that up front saves a scan that would end in denied deletions.
		needDelete := cmd.Name == "prune" && !cmd.Bool("dry-run") || ctx.Value(explorerWriteKey{}) == true
		backend, err = connectGHCR(ctx, addr, cmd.Int("page-size"), needDelete, logger)
	default:
		backend, err = connectACR(addr, cmd.Int("page-size"), logger)
	}
	if err != nil {
		return nil, err
	}
	var cache *registry.Cache
	if dir := cmd.String("cache"); dir != "" {
		cache, err = registry.NewCache(filepath.Join(dir, addr.String()))
		if err != nil {
			return nil, fmt.Errorf("--cache: %w", err)
		}
	}
	return registry.New(backend, logger, cmd.Int("parallelism"), cache)
}

// connectACR builds the ACR backend, authenticating with
// DefaultAzureCredential in the login server's cloud.
func connectACR(addr registry.Address, pageSize int, logger *slog.Logger) (registry.Backend, error) {
	cred, err := azidentity.NewDefaultAzureCredential(acr.CredentialOptions(addr.Host))
	if err != nil {
		return nil, err
	}
	backend, err := acr.New(acr.Options{
		Endpoint:   "https://" + addr.Host,
		Credential: cred,
		PageSize:   pageSize,
		Logger:     logger,
	})
	if err != nil {
		return nil, err
	}
	return backend, nil
}

// connectGHCR builds the GHCR backend for the address's owner. needDelete
// makes it refuse a token that reports its scopes without delete:packages.
func connectGHCR(ctx context.Context, addr registry.Address, pageSize int, needDelete bool, logger *slog.Logger) (registry.Backend, error) {
	token, err := githubToken(ctx)
	if err != nil {
		return nil, err
	}
	backend, err := newGHCR(ctx, ghcr.Options{
		Owner:      addr.Owner,
		Token:      token,
		NeedDelete: needDelete,
		Username:   os.Getenv("GITHUB_ACTOR"),
		PageSize:   pageSize,
		Logger:     logger,
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
// path, or returns nil when no file was given. An empty file is refused, and
// a file naming none of the registry's images draws a warning: either would
// treat every image as not running.
func loadRunningRules(path string, addr registry.Address, logger *slog.Logger) ([]*rules.RepoRule, error) {
	if path == "" {
		return nil, nil
	}
	var counts rules.ImageListCounts
	specs, err := parseInput(path, "running file", func(r io.Reader) ([]*rules.RepoRuleSpec, error) {
		specs, c, err := rules.KeepRulesFromImageList(r, addr.String())
		counts = c
		return specs, err
	})
	if err != nil {
		return nil, err
	}
	if counts.Lines == 0 {
		// get_pod_images.sh prints nothing at all when a query fails.
		return nil, fmt.Errorf("running file %s lists no images; no image would count as running", inputName(path))
	}
	if counts.Matched == 0 {
		logger.Warn("No image in the running file belongs to this registry; no image counts as running",
			"file", inputName(path), "lines", counts.Lines, "expected_prefix", addr.String()+"/")
	}
	return rules.Compile(specs)
}

// throttled returns a function passing its argument to write at most once per
// interval, as measured by now, and dropping the calls in between. The first
// call always writes.
func throttled[T any](interval time.Duration, now func() time.Time, write func(T) error) func(T) error {
	var last time.Time
	return func(v T) error {
		if t := now(); last.IsZero() || t.Sub(last) >= interval {
			last = t
			return write(v)
		}
		return nil
	}
}

// writeOutput publishes a complete JSON document to path, or to stdout for ""
// and "-". Named files are replaced atomically, so interruptions and failed
// writes preserve the last snapshot.
func writeOutput(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if path == "" || path == "-" {
		_, err = os.Stdout.Write(data)
		return err
	}
	return fileio.WriteFile(path, data, 0o600)
}
