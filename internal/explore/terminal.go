package explore

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/JohanLindvall/crprune/internal/progress"
	"github.com/JohanLindvall/crprune/internal/pruner"
	"github.com/JohanLindvall/crprune/internal/registry"
	"github.com/JohanLindvall/crprune/internal/rules"
	"github.com/JohanLindvall/crprune/internal/tui"
	"github.com/gdamore/tcell/v2"
	"golang.org/x/term"
)

// Options identifies the statistics source and optional live registry connection.
type Options struct {
	Source, Sort, Filter string
	Reverse              bool
	LiveStats            bool // Scan Client on startup and when reloading statistics.
	Reload               func() ([]pruner.RepositoryStats, error)
	Client               *Client
	RuleFiles            []RuleFile
	Logger               *slog.Logger
	OnLogger             func(*slog.Logger)
}

var (
	newScreen        = tcell.NewScreen
	stderrIsTerminal = func() bool { return term.IsTerminal(int(os.Stderr.Fd())) }
)

// Run owns the controlling terminal, leaving stdin available for a piped file.
// Closing an active operation waits for its requests and lock restores.
func Run(ctx context.Context, stats []pruner.RepositoryStats, opts Options) error {
	if opts.Sort == "" {
		opts.Sort = "unique"
	}
	if err := pruner.SortStatsBy(nil, opts.Sort); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !stderrIsTerminal() || os.Getenv("TERM") == "dumb" {
		return errors.New("explore needs an interactive terminal on stderr; use crprune top for plain output")
	}
	screen, err := newScreen()
	if err != nil {
		return fmt.Errorf("initialize explorer: %w", err)
	}
	if err := screen.Init(); err != nil {
		tui.Release(screen)
		return fmt.Errorf("initialize explorer: %w", err)
	}
	tui.Own(screen)
	a := newApp(stats, opts)
	defer func() {
		tui.Disown(screen)
		for _, r := range a.completed {
			a.opts.Logger.Info("Explorer cleanup", "confirmed", r.outcome, "error", r.err)
		}
	}()
	if opts.OnLogger != nil {
		opts.OnLogger(a.logger)
		defer opts.OnLogger(nil)
	}
	return a.run(ctx, screen)
}

type result struct {
	kind, repository, source string
	stats                    []pruner.RepositoryStats
	manifests                []*registry.Manifest
	plan                     *pruner.DeletionPlan
	rules                    []*rules.RepoRule
	outcome                  pruner.PruneStats
	err                      error
}

type job struct {
	title    string
	cancel   context.CancelFunc
	done     chan result
	stopping bool
}

type app struct {
	opts                             Options
	repos, manifests                 listState
	repository                       string
	byDigest                         map[string]*registry.Manifest
	parents                          map[string][]*registry.Manifest
	panel, input, status             string
	message                          []string
	panelOffset, sortCursor          int
	plan                             *pruner.DeletionPlan
	targets                          []pruner.DeletionTarget
	planCursor, planOffset, planRows int
	scope                            string
	ruleCursor, ruleOffset, ruleRows int
	ruleRequest                      *request
	ruleScope                        string
	stale                            bool
	reviewable                       bool
	tracker                          *progress.Tracker
	logger                           *slog.Logger
	job                              *job
	completed                        []result
}

func newApp(stats []pruner.RepositoryStats, opts Options) *app {
	if opts.Sort == "" {
		opts.Sort = "unique"
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	t := progress.NewTracker()
	a := &app{opts: opts, repos: newList(stats, opts.Sort, opts.Filter, opts.Reverse), tracker: t, logger: t.Logger(opts.Logger)}
	if opts.Client != nil && opts.Client.RuleSource != "" {
		a.rememberRules(RuleFile{Source: opts.Client.RuleSource, Rules: opts.Client.Rules})
	}
	return a
}

func (a *app) list() *listState {
	if a.repository != "" {
		return &a.manifests
	}
	return &a.repos
}

func (a *app) keys() []string {
	if a.repository != "" {
		return manifestSortKeys
	}
	return sortKeys
}

func (a *app) run(ctx context.Context, screen tcell.Screen) error {
	events := make(chan tcell.Event, 8)
	quit, stopped := make(chan struct{}), make(chan struct{})
	go func() { defer close(stopped); screen.ChannelEvents(events, quit) }()
	defer func() { close(quit); <-stopped }()
	// Even an unexpected UI panic must wait for a mutation's cleanup.
	defer func() {
		if a.job != nil {
			a.job.cancel()
			r := <-a.job.done
			if r.kind == "execute" {
				a.completed = append(a.completed, r)
			}
		}
	}()
	tick := time.NewTicker(125 * time.Millisecond)
	defer tick.Stop()
	if a.opts.LiveStats {
		a.loadStatistics(ctx)
	}
	canceled := ctx.Done()
	var closing error
	for {
		tui.OnScreen(screen, func() { a.draw(screen, time.Now()) })
		var done <-chan result
		var refresh <-chan time.Time
		if a.job != nil {
			refresh = tick.C
		}
		if a.job != nil {
			done = a.job.done
		}
		select {
		case <-canceled:
			if a.job == nil {
				return ctx.Err()
			}
			a.job.cancel()
			a.job.stopping = true
			closing, canceled = ctx.Err(), nil
		case r := <-done:
			a.job.cancel()
			a.job = nil
			a.finish(r)
			if closing != nil {
				return closing
			}
		case <-refresh:
		case event, ok := <-events:
			if !ok {
				return errors.New("terminal event stream closed")
			}
			switch event := event.(type) {
			case *tcell.EventError:
				return event
			case *tcell.EventResize:
				tui.OnScreen(screen, screen.Sync)
			case *tcell.EventKey:
				quit, err := a.key(ctx, event)
				if quit {
					if a.job == nil {
						return err
					}
					a.job.cancel()
					a.job.stopping = true
					closing = context.Canceled
				}
			}
		}
	}
}

func (a *app) start(ctx context.Context, title string, work func(context.Context) result) {
	a.tracker.Reset()
	ctx, cancel := context.WithCancel(a.tracker.Context(ctx))
	j := &job{title: title, cancel: cancel, done: make(chan result, 1)}
	a.job, a.status = j, ""
	go func() {
		defer func() {
			if p := recover(); p != nil {
				j.done <- result{err: fmt.Errorf("operation panicked: %v", p)}
			}
		}()
		j.done <- work(ctx)
	}()
}

func (a *app) live() bool {
	if a.opts.Client == nil || a.opts.Client.Connect == nil {
		a.showMessage("REGISTRY REQUIRED", "Reopen with --registry to browse or delete live images.", "Example: crprune -r ghcr.io/OWNER explore "+a.opts.Source)
		return false
	}
	return true
}

func (a *app) prepare(ctx context.Context, req request, scope string) {
	if !a.live() {
		return
	}
	if len(req.repositories) == 0 {
		a.status = "No repositories selected"
		return
	}
	if req.kind == "rules" && a.opts.Client.RuleSource == "" && len(a.opts.Client.Rules) == 0 {
		a.chooseRules(&req, scope)
		return
	}
	a.scope, a.panel, a.plan = scope, "", nil
	a.start(ctx, "Preparing deletion preview", func(ctx context.Context) result {
		plan, err := a.opts.Client.prepare(ctx, a.logger, req)
		return result{kind: "preview", plan: plan, err: err}
	})
}

func (a *app) loadManifests(ctx context.Context, name string) {
	if name == "" || !a.live() {
		return
	}
	a.start(ctx, "Loading live manifests", func(ctx context.Context) result {
		manifests, err := a.opts.Client.manifests(ctx, a.logger, name)
		return result{kind: "manifests", repository: name, manifests: manifests, err: err}
	})
}

func (a *app) loadStatistics(ctx context.Context) {
	if !a.live() {
		return
	}
	a.start(ctx, "Scanning registry statistics", func(ctx context.Context) result {
		stats, err := a.opts.Client.statistics(ctx, a.logger)
		return result{kind: "statistics", stats: stats, err: err}
	})
}

func (a *app) showMessage(title string, lines ...string) {
	a.panel, a.panelOffset = "message", 0
	a.message = append([]string{title, ""}, lines...)
}

func (a *app) finish(r result) {
	if r.kind == "execute" {
		a.completed = append(a.completed, r)
		a.plan, a.targets, a.panel = nil, nil, ""
		a.stale = true
		a.repos.marked, a.manifests.marked = map[string]bool{}, map[string]bool{}
		refresh := "Run stats again to refresh byte counts."
		if a.opts.LiveStats {
			refresh = "Use R in the repository view to rescan statistics."
		}
		lines := []string{fmt.Sprintf("Confirmed deletions: %d manifests, %d repositories.", r.outcome.DeletedManifests, r.outcome.Repositories-r.outcome.KeptRepositories),
			"The loaded statistics snapshot is now potentially stale. " + refresh + " Use R in the manifest view to reload live images."}
		if r.err != nil {
			lines = append(lines, "Stopped: "+r.err.Error(), "Some deletions may have succeeded. Prepare a new preview before retrying.")
		}
		a.showMessage("DELETION RESULT", lines...)
		return
	}
	if r.err != nil && (r.kind != "statistics" || r.stats == nil) {
		a.showMessage("ACTION FAILED", r.err.Error())
		return
	}
	switch r.kind {
	case "preview":
		a.plan, a.targets = r.plan, r.plan.Targets()
		if len(a.targets) == 0 {
			a.plan = nil
			a.showMessage("NOTHING TO DELETE", "No manifests passed the rules and protections for "+a.scope+".",
				"Recent, running and locked images, required dependencies, and GHCR's last tag may be retained.")
			return
		}
		a.panel, a.planCursor, a.planOffset, a.input = "plan", 0, 0, ""
	case "manifests":
		a.repository, a.byDigest = r.repository, map[string]*registry.Manifest{}
		a.parents = map[string][]*registry.Manifest{}
		var data []pruner.RepositoryStats
		labels := map[string]string{}
		for _, m := range r.manifests {
			a.byDigest[m.Digest] = m
			for _, child := range m.Manifests {
				digest := string(child.Digest)
				if !slices.Contains(a.parents[digest], m) {
					a.parents[digest] = append(a.parents[digest], m)
				}
			}
			s := pruner.RepositoryStats{Name: m.Digest, Count: 1, Total: manifestBytes(m), Newest: m.LastUpdated, Oldest: m.LastUpdated}
			if len(m.Tags) > 0 {
				s.Tagged = 1
			} else {
				s.Untagged = 1
			}
			data = append(data, s)
		}
		for _, m := range r.manifests {
			labels[m.Digest] = strings.Join(m.Tags, " ") + " " + manifestKind(m) + " " + a.manifestSummary(m)
		}
		a.manifests = newList(data, "newest", "", false)
		a.manifests.labels = labels
		a.panel = ""
		a.status = fmt.Sprintf("Loaded %d live manifests from %s", len(data), r.repository)
	case "reload", "statistics":
		old := a.repos
		a.repos = newList(r.stats, old.sortKey, old.query, old.reverse)
		a.repos.refilter(old.current())
		a.status = "Reloaded snapshot; it may predate live changes"
		if r.kind == "statistics" {
			a.status = fmt.Sprintf("Loaded live statistics for %d repositories", len(r.stats))
			if r.err == nil {
				a.stale = false
			} else {
				a.status = fmt.Sprintf("Loaded partial statistics for %d repositories", len(r.stats))
				a.showMessage("PARTIAL STATISTICS", a.status+". Press Esc to browse them, then R to retry the scan.", r.err.Error())
			}
		}
	case "rules":
		a.opts.Client.Rules, a.opts.Client.RuleSource = r.rules, r.source
		a.rememberRules(RuleFile{Source: r.source, Rules: r.rules})
		a.panel, a.status = "", fmt.Sprintf("Loaded %d repository rules from %s", len(r.rules), r.source)
	}
}

func manifestBytes(m *registry.Manifest) uint64 {
	var size uint64
	for _, blobSize := range m.Blobs() {
		size += blobSize
	}
	return size
}

// key returns true only when the interface should close. A deletion is launched
// exclusively from the confirm panel after an exact phrase and Enter.
func (a *app) key(ctx context.Context, key *tcell.EventKey) (bool, error) {
	if key.Key() == tcell.KeyCtrlC {
		return true, context.Canceled
	}
	if a.job != nil {
		if key.Key() == tcell.KeyEsc {
			a.job.cancel()
			a.job.stopping = true
		}
		return key.Rune() == 'q', nil
	}
	l := a.list()
	if l.searching {
		switch key.Key() {
		case tcell.KeyEsc:
			l.query, l.searching = l.beforeQuery, false
		case tcell.KeyEnter:
			l.searching = false
		default:
			editText(&l.query, key)
		}
		l.refilter(l.current())
		return false, nil
	}
	if a.panel == "confirm" {
		if key.Key() == tcell.KeyEsc {
			a.panel, a.input = "plan", ""
			return false, nil
		}
		if key.Key() == tcell.KeyEnter {
			if a.input == a.confirmPhrase() && a.plan != nil && a.reviewable {
				plan := a.plan
				a.panel, a.plan = "", nil
				a.start(ctx, "Deleting confirmed targets", func(ctx context.Context) result {
					outcome, err := plan.Execute(ctx)
					return result{kind: "execute", outcome: outcome, err: err}
				})
			} else if !a.reviewable {
				a.status = "Enlarge the terminal before confirming; nothing was deleted"
			} else {
				a.status = "Confirmation did not match; nothing was deleted"
			}
		} else {
			editText(&a.input, key)
		}
		return false, nil
	}
	if a.panel == "rules" {
		if key.Key() == tcell.KeyEsc {
			a.panel = ""
			return false, nil
		}
		if key.Key() == tcell.KeyEnter {
			path := a.input
			if path == "" || path == "-" {
				a.status = "Enter a named rule file"
				return false, nil
			}
			a.start(ctx, "Loading rules", func(context.Context) result {
				f, err := os.Open(path)
				if err != nil {
					return result{err: err}
				}
				defer func() { _ = f.Close() }()
				specs, err := rules.ParseSpecs(f)
				if err != nil {
					return result{err: err}
				}
				compiled, err := rules.Compile(specs)
				return result{kind: "rules", rules: compiled, source: path, err: err}
			})
		} else {
			editText(&a.input, key)
		}
		return false, nil
	}
	if key.Rune() == 'q' {
		return true, nil
	}
	if a.panel == "rule-picker" {
		switch {
		case key.Key() == tcell.KeyEsc:
			a.panel, a.ruleRequest = "", nil
		case key.Rune() == 'L':
			if a.live() {
				a.manualRules()
			}
		case key.Key() == tcell.KeyUp || key.Rune() == 'k':
			a.ruleCursor--
		case key.Key() == tcell.KeyDown || key.Rune() == 'j':
			a.ruleCursor++
		case key.Key() == tcell.KeyPgUp:
			a.ruleCursor -= max(1, a.ruleRows)
		case key.Key() == tcell.KeyPgDn:
			a.ruleCursor += max(1, a.ruleRows)
		case key.Key() == tcell.KeyHome:
			a.ruleCursor = 0
		case key.Key() == tcell.KeyEnd:
			a.ruleCursor = len(a.opts.RuleFiles) - 1
		case key.Rune() == 'i':
			if len(a.opts.RuleFiles) > 0 {
				a.showMessage("RULE FILE", ruleDetails(a.opts.RuleFiles[a.ruleCursor])...)
				a.panel = "rule-details"
			}
		case key.Key() == tcell.KeyEnter:
			if len(a.opts.RuleFiles) > 0 {
				file := a.opts.RuleFiles[a.ruleCursor]
				if file.Err != nil {
					a.status = "Cannot select invalid rules: " + file.Err.Error()
				} else if a.live() {
					a.opts.Client.Rules, a.opts.Client.RuleSource = file.Rules, file.Source
					a.panel, a.status = "", "Selected rules: "+file.Source
					if req := a.ruleRequest; req != nil {
						a.ruleRequest = nil
						a.prepare(ctx, *req, a.ruleScope)
					}
				}
			}
		}
		a.ruleCursor = max(0, min(a.ruleCursor, len(a.opts.RuleFiles)-1))
		return false, nil
	}
	if a.panel == "plan" {
		switch {
		case key.Key() == tcell.KeyEsc:
			a.panel, a.plan, a.targets = "", nil, nil
		case key.Rune() == 'c':
			if a.reviewable {
				a.panel, a.input, a.status = "confirm", "", ""
			}
		case key.Rune() == 'w':
			lines := []string{}
			for _, entry := range a.tracker.Snapshot().WarningLogs {
				lines = append(lines, entry.Text)
			}
			if len(lines) == 0 {
				lines = append(lines, "No warnings")
			}
			a.showMessage("PREVIEW WARNINGS", lines...)
		case key.Key() == tcell.KeyEnter:
			t := a.targets[a.planCursor]
			a.showMessage("DELETION TARGET", t.Repository, t.Digest, "Tags: "+tags(t.Tags), fmt.Sprintf("Delete repository: %t; unlock: %t", t.WholeRepository, t.Unlock))
		case key.Key() == tcell.KeyUp || key.Rune() == 'k':
			a.planCursor--
		case key.Key() == tcell.KeyDown || key.Rune() == 'j':
			a.planCursor++
		case key.Key() == tcell.KeyPgUp:
			a.planCursor -= max(1, a.planRows)
		case key.Key() == tcell.KeyPgDn:
			a.planCursor += max(1, a.planRows)
		case key.Key() == tcell.KeyHome:
			a.planCursor = 0
		case key.Key() == tcell.KeyEnd:
			a.planCursor = len(a.targets) - 1
		}
		a.planCursor = max(0, min(a.planCursor, len(a.targets)-1))
		return false, nil
	}
	if a.panel == "sort" {
		keys := a.keys()
		switch {
		case key.Key() == tcell.KeyEsc:
			a.panel = ""
		case key.Key() == tcell.KeyUp || key.Rune() == 'k':
			a.sortCursor = (a.sortCursor + len(keys) - 1) % len(keys)
		case key.Key() == tcell.KeyDown || key.Rune() == 'j':
			a.sortCursor = (a.sortCursor + 1) % len(keys)
		case key.Key() == tcell.KeyEnter:
			l.sortKey = keys[a.sortCursor]
			l.refilter(l.current())
			a.panel = ""
		}
		return false, nil
	}
	if a.panel != "" {
		if a.panel == "repository-details" && (key.Key() == tcell.KeyEnter || key.Rune() == 'm') {
			name := a.repository
			if name == "" {
				name = a.repos.current()
			}
			a.loadManifests(ctx, name)
			return false, nil
		}
		switch {
		case key.Key() == tcell.KeyEsc || key.Rune() == '?':
			if a.panel == "rule-details" {
				a.panel = "rule-picker"
			} else if a.plan != nil {
				a.panel = "plan"
			} else {
				a.panel = ""
			}
		case key.Key() == tcell.KeyUp || key.Rune() == 'k':
			a.panelOffset--
		case key.Key() == tcell.KeyDown || key.Rune() == 'j':
			a.panelOffset++
		case key.Key() == tcell.KeyPgUp:
			a.panelOffset -= max(1, l.rows)
		case key.Key() == tcell.KeyPgDn:
			a.panelOffset += max(1, l.rows)
		case key.Key() == tcell.KeyHome:
			a.panelOffset = 0
		case key.Key() == tcell.KeyEnd:
			a.panelOffset = 1 << 30
		}
		return false, nil
	}
	if l.navigate(key) {
		return false, nil
	}
	switch {
	case key.Key() == tcell.KeyEsc:
		if a.repository != "" {
			a.repository = ""
		} else if l.query != "" {
			l.query = ""
			l.refilter(l.current())
		}
	case key.Key() == tcell.KeyEnter:
		if a.repository == "" && a.opts.Client != nil {
			a.loadManifests(ctx, l.current())
		} else {
			a.details()
		}
	case key.Rune() == 'i':
		a.repositoryDetails()
	case key.Rune() == '/':
		l.searching, l.beforeQuery = true, l.query
	case key.Rune() == 's':
		a.panel, a.sortCursor = "sort", max(0, slices.Index(a.keys(), l.sortKey))
	case key.Rune() == 'r':
		l.reverse = !l.reverse
		l.refilter(l.current())
	case key.Key() == tcell.KeyLeft:
		l.cycleSort(a.keys(), -1)
	case key.Key() == tcell.KeyRight || key.Key() == tcell.KeyTab:
		l.cycleSort(a.keys(), 1)
	case key.Rune() == ' ':
		l.toggle()
	case key.Rune() == 'a':
		l.toggleVisible()
	case key.Rune() == 'c':
		l.marked = map[string]bool{}
	case key.Rune() == '?':
		a.panel, a.panelOffset = "help", 0
	case key.Rune() == 'm':
		if a.repository == "" {
			a.loadManifests(ctx, l.current())
		}
	case key.Rune() == 'R':
		if a.repository != "" {
			a.loadManifests(ctx, a.repository)
		} else if a.opts.LiveStats {
			a.loadStatistics(ctx)
		} else if a.opts.Reload != nil {
			a.start(ctx, "Reloading statistics", func(context.Context) result {
				stats, err := a.opts.Reload()
				return result{kind: "reload", stats: stats, err: err}
			})
		} else {
			a.status = "Cannot reload stdin; reopen explore with a named file"
		}
	case key.Rune() == 'L':
		if a.live() {
			a.manualRules()
		}
	case key.Rune() == 'l':
		a.chooseRules(nil, "")
	case key.Rune() == 'd':
		req := request{kind: "repositories", repositories: l.selected()}
		if a.repository != "" {
			req = request{kind: "manifests", repositories: []string{a.repository}, digests: l.selected()}
		}
		a.prepare(ctx, req, fmt.Sprintf("%d selected %s", len(l.selected()), req.kind))
	case key.Rune() == 'p':
		name := a.repository
		if name == "" {
			name = l.current()
		}
		if name != "" {
			a.prepare(ctx, request{kind: "rules", repositories: []string{name}}, "rules for "+name)
		}
	case key.Rune() == 'P':
		names := make([]string, len(a.repos.data))
		for i, s := range a.repos.data {
			names[i] = s.Name
		}
		a.prepare(ctx, request{kind: "rules", repositories: names}, fmt.Sprintf("rules for all %d snapshot repositories (including filtered-out rows)", len(names)))
	}
	return false, nil
}

func (a *app) confirmPhrase() string { return "delete " + a.opts.Client.Registry }

func tags(values []string) string {
	if len(values) == 0 {
		return "(untagged)"
	}
	return strings.Join(values, ", ")
}
