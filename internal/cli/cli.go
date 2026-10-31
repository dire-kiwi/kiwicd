package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"
)

const usage = `kiwicd: inspect and start kiwicd runs, and configure secrets and variables.

Usage:
  kiwicd login [--url URL] [--token TOKEN | --password-stdin] [--name NAME] [--expires DAYS]
  kiwicd logout
  kiwicd whoami

  kiwicd repo list
  kiwicd repo view OWNER/NAME
  kiwicd repo sync OWNER/NAME
  kiwicd workflow list OWNER/NAME [--ref REF]

  kiwicd run list [--repo OWNER/NAME] [--limit N]
  kiwicd run view RUN
  kiwicd run start OWNER/NAME WORKFLOW [--ref REF] [-i NAME=VALUE]... [--watch]
  kiwicd run watch RUN
  kiwicd run logs RUN [--job JOB] [--failed] [--follow] [--timestamps]
  kiwicd run cancel RUN
  kiwicd run rerun RUN [--watch]

  kiwicd secret list [--repo OWNER/NAME]
  kiwicd secret set NAME [--repo OWNER/NAME] [--value VALUE | --from-file PATH]
  kiwicd secret delete NAME [--repo OWNER/NAME]
  kiwicd variable list [--repo OWNER/NAME]
  kiwicd variable set NAME [VALUE] [--repo OWNER/NAME]
  kiwicd variable delete NAME [--repo OWNER/NAME]

  kiwicd runner list
  kiwicd token list
  kiwicd token create NAME [--expires DAYS]
  kiwicd token revoke ID

  kiwicd mcp [--read-only]     serve the API to agents over MCP (stdio)
  kiwicd api METHOD PATH [--data JSON]
  kiwicd version

Without --repo, secrets and variables are the global ones, which every
repository gets. List and view commands take --json. kiwicd login saves the
server and an API token in ~/.config/kiwicd/cli.json; KIWICD_URL and
KIWICD_API_TOKEN override them.
`

// App is one invocation of the command.
type App struct {
	Stdin          io.Reader
	Stdout, Stderr io.Writer
	Version        string
	// stdinFD is the terminal stdin is, for prompting without echo; -1 if
	// it isn't one.
	stdinFD int

	cfg    Config
	client *Client
}

type command func(ctx context.Context, a *App, args []string) error

var commands = map[string]map[string]command{
	"repo":     {"list": repoList, "view": repoView, "sync": repoSync},
	"workflow": {"list": workflowList},
	"run": {"list": runList, "view": runView, "start": runStart, "watch": runWatch, "logs": runLogs,
		"cancel": runCancel, "rerun": runRerun},
	"secret":   {"list": settingList("secrets"), "set": secretSet, "delete": settingDelete("secrets")},
	"variable": {"list": settingList("variables"), "set": variableSet, "delete": settingDelete("variables")},
	"runner":   {"list": runnerList},
	"token":    {"list": tokenList, "create": tokenCreate, "revoke": tokenRevoke},
}

var topCommands = map[string]command{
	"login": login, "logout": logout, "whoami": whoami, "mcp": mcpCommand, "api": apiCommand,
}

var aliases = map[string]string{
	"repos": "repo", "workflows": "workflow", "runs": "run", "secrets": "secret", "variables": "variable",
	"vars": "variable", "var": "variable", "runners": "runner", "tokens": "token",
	"ls": "list", "show": "view", "rm": "delete", "remove": "delete", "dispatch": "start", "log": "logs",
}

// Main runs the command with args (without the program name) and returns
// its exit status.
func Main(args []string, version string) int {
	a := &App{Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr, Version: version, stdinFD: -1}
	if fd := int(os.Stdin.Fd()); term.IsTerminal(fd) {
		a.stdinFD = fd
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return a.Run(ctx, args)
}

func (a *App) Run(ctx context.Context, args []string) int {
	err := a.run(ctx, args)
	var exit exitError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &exit):
		return int(exit)
	case errors.Is(err, flag.ErrHelp):
		return 0
	case errors.Is(err, errUsage):
		fmt.Fprint(a.Stderr, usage)
		return 2
	}
	fmt.Fprintln(a.Stderr, "kiwicd:", err)
	return 1
}

// exitError ends the command with a status, having said why already.
type exitError int

func (e exitError) Error() string { return "exit status " + strconv.Itoa(int(e)) }

var errUsage = errors.New("usage")

func (a *App) run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errUsage
	}
	name := args[0]
	if n, ok := aliases[name]; ok {
		name = n
	}
	switch name {
	case "help", "-h", "--help":
		fmt.Fprint(a.Stdout, usage)
		return nil
	case "version", "--version":
		fmt.Fprintln(a.Stdout, "kiwicd", a.Version)
		return nil
	}
	if cmd, ok := topCommands[name]; ok {
		return cmd(ctx, a, args[1:])
	}
	verbs, ok := commands[name]
	if !ok {
		return fmt.Errorf("unknown command %q; see kiwicd help", args[0])
	}
	// A plural noun is its list: kiwicd runs, kiwicd workflows OWNER/NAME.
	plural := name != args[0] && strings.HasSuffix(args[0], "s")
	verb := "list"
	rest := args[1:]
	if len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
		v := rest[0]
		if a, ok := aliases[v]; ok {
			v = a
		}
		if _, ok := verbs[v]; ok || !plural {
			verb, rest = v, rest[1:]
		}
	}
	cmd, ok := verbs[verb]
	if !ok {
		var names []string
		for v := range verbs {
			names = append(names, v)
		}
		sort.Strings(names)
		return fmt.Errorf("unknown command %q; kiwicd %s takes %s", strings.Join(args[:2], " "), name, strings.Join(names, ", "))
	}
	return cmd(ctx, a, rest)
}

// api returns the client for the configured server.
func (a *App) api() (*Client, error) {
	if a.client != nil {
		return a.client, nil
	}
	cfg, err := loadConfig()
	if err != nil {
		return nil, err
	}
	if cfg.URL == "" {
		return nil, errors.New("not logged in: run kiwicd login --url URL, or set KIWICD_URL and KIWICD_API_TOKEN")
	}
	a.cfg = cfg
	a.client = &Client{URL: cfg.URL, Token: cfg.Token}
	return a.client, nil
}

// flags parses args with flags and positional arguments in any order, and
// checks there are between min and max positional ones (max < 0: any).
func flags(fs *flag.FlagSet, args []string, min, max int) ([]string, error) {
	fs.SetOutput(io.Discard)
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil, fmt.Errorf("%s takes: %s", fs.Name(), flagNames(fs))
			}
			return nil, fmt.Errorf("%s: %w", fs.Name(), err)
		}
		args = fs.Args()
		if len(args) == 0 {
			break
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
	if len(pos) < min || max >= 0 && len(pos) > max {
		return nil, fmt.Errorf("%s: wrong number of arguments; see kiwicd help", fs.Name())
	}
	return pos, nil
}

func flagNames(fs *flag.FlagSet) string {
	var names []string
	fs.VisitAll(func(f *flag.Flag) { names = append(names, "--"+f.Name) })
	if len(names) == 0 {
		return "no flags"
	}
	return strings.Join(names, ", ")
}

func parseID(what, s string) (int64, error) {
	id, err := strconv.ParseInt(strings.TrimPrefix(s, "#"), 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("%q isn't a %s ID", s, what)
	}
	return id, nil
}

// prompt asks for a line, without echoing it if hidden and stdin is a
// terminal.
func (a *App) prompt(msg string, hidden bool) (string, error) {
	fmt.Fprint(a.Stderr, msg)
	if hidden && a.stdinFD >= 0 {
		b, err := term.ReadPassword(a.stdinFD)
		fmt.Fprintln(a.Stderr)
		return string(b), err
	}
	line, err := bufio.NewReader(a.Stdin).ReadString('\n')
	if err != nil && (line == "" || !errors.Is(err, io.EOF)) {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// ---------------------------------------------------------------- login

func login(ctx context.Context, a *App, args []string) error {
	fs := flag.NewFlagSet("login", flag.ContinueOnError)
	serverURL := fs.String("url", "", "server URL")
	token := fs.String("token", "", "an API token made in the web UI (Settings)")
	passwordStdin := fs.Bool("password-stdin", false, "read the admin password from stdin")
	host, _ := os.Hostname()
	name := fs.String("name", "kiwicd CLI on "+orDash(host), "the new token's name")
	expires := fs.Int("expires", 0, "days until the new token expires (0: never)")
	if _, err := flags(fs, args, 0, 0); err != nil {
		return err
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	if *serverURL != "" {
		cfg.URL = strings.TrimRight(*serverURL, "/")
	}
	if cfg.URL == "" {
		if cfg.URL, err = a.prompt("Server URL: ", false); err != nil {
			return err
		}
		cfg.URL = strings.TrimRight(strings.TrimSpace(cfg.URL), "/")
	}
	if !strings.HasPrefix(cfg.URL, "http://") && !strings.HasPrefix(cfg.URL, "https://") {
		return fmt.Errorf("%q isn't a URL; give it as https://HOST", cfg.URL)
	}
	if *token == "" {
		var password string
		if *passwordStdin {
			b, err := io.ReadAll(a.Stdin)
			if err != nil {
				return err
			}
			password = strings.TrimRight(string(b), "\r\n")
		} else if password, err = a.prompt("Admin password (or make a token in the web UI and use --token): ", true); err != nil {
			return err
		}
		c := &Client{URL: cfg.URL, Token: password}
		t, _, err := c.CreateToken(ctx, *name, *expires)
		if err != nil {
			return err
		}
		*token = t
	}
	c := &Client{URL: cfg.URL, Token: *token}
	var me Me
	if err := c.Get(ctx, "/api/me", &me); err != nil {
		return err
	}
	cfg.Token = *token
	p, err := saveConfig(cfg)
	if err != nil {
		return err
	}
	fmt.Fprintf(a.Stdout, "Logged in to %s as %s (server %s); saved to %s\n", cfg.URL, me.User, me.Version, p)
	return nil
}

func logout(ctx context.Context, a *App, args []string) error {
	if _, err := flags(flag.NewFlagSet("logout", flag.ContinueOnError), args, 0, 0); err != nil {
		return err
	}
	c, err := a.api()
	if err != nil {
		return err
	}
	if strings.HasPrefix(c.Token, "kiwicd_") {
		if err := c.RevokeToken(ctx, "current"); err != nil {
			fmt.Fprintln(a.Stderr, "kiwicd: couldn't revoke the token:", err)
		}
	}
	if err := removeConfig(); err != nil {
		return err
	}
	fmt.Fprintln(a.Stdout, "Logged out of", c.URL)
	return nil
}

func whoami(ctx context.Context, a *App, args []string) error {
	if _, err := flags(flag.NewFlagSet("whoami", flag.ContinueOnError), args, 0, 0); err != nil {
		return err
	}
	c, err := a.api()
	if err != nil {
		return err
	}
	var me Me
	if err := c.Get(ctx, "/api/me", &me); err != nil {
		return err
	}
	fmt.Fprintf(a.Stdout, "%s on %s (server %s)\n", me.User, c.URL, me.Version)
	return nil
}

// ---------------------------------------------------------------- repos

func repoList(ctx context.Context, a *App, args []string) error {
	fs := flag.NewFlagSet("repo list", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "print JSON")
	if _, err := flags(fs, args, 0, 0); err != nil {
		return err
	}
	c, err := a.api()
	if err != nil {
		return err
	}
	repos, err := c.Repos(ctx)
	if err != nil {
		return err
	}
	if *asJSON {
		return writeJSON(a.Stdout, repos)
	}
	var rows [][]string
	for _, r := range repos {
		sync := ago(r.LastSyncAt)
		if r.LastSyncError != "" {
			sync = "failed: " + truncate(r.LastSyncError, 60)
		}
		rows = append(rows, []string{r.Name, r.Kind, r.DefaultBranch, sync})
	}
	return table(a.Stdout, "NAME\tKIND\tDEFAULT BRANCH\tLAST SYNC", rows)
}

func repoView(ctx context.Context, a *App, args []string) error {
	fs := flag.NewFlagSet("repo view", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "print JSON")
	pos, err := flags(fs, args, 1, 1)
	if err != nil {
		return err
	}
	c, err := a.api()
	if err != nil {
		return err
	}
	r, err := c.Repo(ctx, pos[0])
	if err != nil {
		return err
	}
	if *asJSON {
		return writeJSON(a.Stdout, r)
	}
	w := a.Stdout
	fmt.Fprintf(w, "%s (%s)\n", r.Name, r.Kind)
	if r.URL != "" {
		fmt.Fprintf(w, "Origin:          %s\n", r.URL)
	}
	fmt.Fprintf(w, "Clone URL:       %s\n", orDash(r.CloneURL))
	fmt.Fprintf(w, "Default branch:  %s\n", r.DefaultBranch)
	if len(r.ProtectedRefs) > 0 {
		fmt.Fprintf(w, "Protected refs:  %s\n", strings.Join(r.ProtectedRefs, ", "))
	}
	if len(r.Readers) > 0 {
		fmt.Fprintf(w, "Readers:         %s\n", strings.Join(r.Readers, ", "))
	}
	fmt.Fprintf(w, "Last sync:       %s\n", ago(r.LastSyncAt))
	if r.LastSyncError != "" {
		fmt.Fprintf(w, "Sync error:      %s\n", r.LastSyncError)
	}
	var refs [][]string
	for ref, sha := range r.Refs {
		refs = append(refs, []string{ref, shortSHA(sha)})
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i][0] < refs[j][0] })
	if len(refs) > 0 {
		fmt.Fprintln(w)
		return table(w, "REF\tSHA", refs)
	}
	return nil
}

func repoSync(ctx context.Context, a *App, args []string) error {
	pos, err := flags(flag.NewFlagSet("repo sync", flag.ContinueOnError), args, 1, 1)
	if err != nil {
		return err
	}
	c, err := a.api()
	if err != nil {
		return err
	}
	if err := c.SyncRepo(ctx, pos[0]); err != nil {
		return err
	}
	fmt.Fprintln(a.Stdout, "Synced", pos[0])
	return nil
}

func workflowList(ctx context.Context, a *App, args []string) error {
	fs := flag.NewFlagSet("workflow list", flag.ContinueOnError)
	ref := fs.String("ref", "", "branch, tag or ref (default: the default branch)")
	asJSON := fs.Bool("json", false, "print JSON")
	pos, err := flags(fs, args, 1, 1)
	if err != nil {
		return err
	}
	c, err := a.api()
	if err != nil {
		return err
	}
	ws, err := c.Workflows(ctx, pos[0], *ref)
	if err != nil {
		return err
	}
	if *asJSON {
		return writeJSON(a.Stdout, ws)
	}
	var rows [][]string
	for _, w := range ws {
		events := slices.Clone(w.Events)
		for i, e := range events {
			if e != "schedule" || len(w.Schedules) == 0 {
				continue
			}
			var crons []string
			for _, sch := range w.Schedules {
				c := sch.Cron
				if sch.Timezone != "" {
					c += " " + sch.Timezone
				}
				crons = append(crons, c)
			}
			events[i] = "schedule (" + strings.Join(crons, "; ") + ")"
		}
		on := strings.Join(events, ", ")
		if w.Error != "" {
			on = "invalid: " + truncate(w.Error, 60)
		}
		var inputs []string
		for _, in := range w.Inputs {
			s := in.Name
			if in.Required {
				s += "*"
			}
			inputs = append(inputs, s)
		}
		rows = append(rows, []string{w.File, w.Name, on, orDash(strings.Join(inputs, ", "))})
	}
	return table(a.Stdout, "FILE\tNAME\tON\tINPUTS", rows)
}

// ---------------------------------------------------------------- runs

func runList(ctx context.Context, a *App, args []string) error {
	fs := flag.NewFlagSet("run list", flag.ContinueOnError)
	repo := fs.String("repo", "", "only this repository's runs (OWNER/NAME)")
	limit := fs.Int("limit", 20, "how many runs, newest first (at most 500)")
	asJSON := fs.Bool("json", false, "print JSON")
	if _, err := flags(fs, args, 0, 0); err != nil {
		return err
	}
	c, err := a.api()
	if err != nil {
		return err
	}
	runs, err := c.Runs(ctx, *repo, *limit)
	if err != nil {
		return err
	}
	if *asJSON {
		return writeJSON(a.Stdout, runs)
	}
	var rows [][]string
	for _, r := range runs {
		rows = append(rows, []string{strconv.FormatInt(r.ID, 10), stateLabel(r.Status, r.Conclusion), r.Repo,
			runName(r), truncate(r.Title, 50), r.Event, shortRef(r.Ref), ago(r.CreatedAt), elapsed(r.StartedAt, r.FinishedAt)})
	}
	return table(a.Stdout, "ID\tSTATUS\tREPO\tWORKFLOW\tTITLE\tEVENT\tREF\tCREATED\tDURATION", rows)
}

// runName is a run's workflow and number: CI #12, or CI #12.2 for a rerun.
func runName(r Run) string {
	s := fmt.Sprintf("%s #%d", r.WorkflowName, r.Number)
	if r.Attempt > 1 {
		s += fmt.Sprintf(".%d", r.Attempt)
	}
	return s
}

func runView(ctx context.Context, a *App, args []string) error {
	fs := flag.NewFlagSet("run view", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "print JSON")
	pos, err := flags(fs, args, 1, 1)
	if err != nil {
		return err
	}
	id, err := parseID("run", pos[0])
	if err != nil {
		return err
	}
	c, err := a.api()
	if err != nil {
		return err
	}
	d, err := c.Run(ctx, id)
	if err != nil {
		return err
	}
	if *asJSON {
		return writeJSON(a.Stdout, d)
	}
	printRun(a.Stdout, c, d)
	return nil
}

func printRun(w io.Writer, c *Client, d *RunDetail) {
	r := d.Run
	fmt.Fprintf(w, "%s · %s · run %d\n", runName(r), r.Repo, r.ID)
	fmt.Fprintf(w, "%s\n", r.Title)
	fmt.Fprintf(w, "%s on %s at %s by %s, %s\n", r.Event, shortRef(r.Ref), shortSHA(r.Sha), r.Actor, timestamp(r.CreatedAt))
	fmt.Fprintf(w, "Status: %s", stateLabel(r.Status, r.Conclusion))
	if r.StartedAt != 0 {
		fmt.Fprintf(w, " in %s", elapsed(r.StartedAt, r.FinishedAt))
	}
	if r.Cancel && r.Status != "completed" {
		fmt.Fprint(w, " (cancelling)")
	}
	fmt.Fprintln(w)
	if !r.Protected {
		fmt.Fprintln(w, "Not a protected ref: the run gets no secrets.")
	}
	if len(d.ProtectedLabels) > 0 {
		fmt.Fprintf(w, "Runners labelled %s won't take its jobs.\n", strings.Join(d.ProtectedLabels, ", "))
	}
	if r.Error != "" {
		fmt.Fprintf(w, "Error: %s\n", r.Error)
	}
	fmt.Fprintf(w, "%s/runs/%d\n", strings.TrimRight(c.URL, "/"), r.ID)
	if len(d.Jobs) == 0 {
		return
	}
	fmt.Fprintln(w, "\nJobs:")
	for _, j := range d.Jobs {
		fmt.Fprintf(w, "  %s  %s  (job %d", stateLabel(j.Status, j.Conclusion), j.Name, j.ID)
		if j.StartedAt != 0 {
			fmt.Fprintf(w, ", %s", elapsed(j.StartedAt, j.FinishedAt))
		}
		if j.RunnerName != "" {
			fmt.Fprintf(w, ", on %s", j.RunnerName)
		}
		fmt.Fprintln(w, ")")
		if j.Error != "" {
			fmt.Fprintf(w, "      error: %s\n", j.Error)
		}
		for _, s := range j.Steps {
			if s.Name == "" {
				continue
			}
			fmt.Fprintf(w, "      %s  %s", stateLabel(s.Status, s.Conclusion), s.Name)
			if s.StartedAt != 0 {
				fmt.Fprintf(w, "  %s", elapsed(s.StartedAt, s.FinishedAt))
			}
			fmt.Fprintln(w)
		}
	}
}

func runStart(ctx context.Context, a *App, args []string) error {
	fs := flag.NewFlagSet("run start", flag.ContinueOnError)
	ref := fs.String("ref", "", "branch, tag or ref to run at (default: the default branch)")
	inputs := map[string]string{}
	input := func(s string) error {
		k, v, ok := strings.Cut(s, "=")
		if !ok || k == "" {
			return errors.New("inputs are NAME=VALUE")
		}
		inputs[k] = v
		return nil
	}
	fs.Func("i", "an input, NAME=VALUE (repeatable)", input)
	fs.Func("input", "an input, NAME=VALUE (repeatable)", input)
	watch := fs.Bool("watch", false, "wait for the run to finish, and fail if it does")
	pos, err := flags(fs, args, 2, 2)
	if err != nil {
		return err
	}
	c, err := a.api()
	if err != nil {
		return err
	}
	id, err := c.Dispatch(ctx, pos[0], pos[1], *ref, inputs)
	if err != nil {
		return err
	}
	fmt.Fprintf(a.Stdout, "Started run %d: %s/runs/%d\n", id, strings.TrimRight(c.URL, "/"), id)
	if *watch {
		return watchRun(ctx, a, c, id)
	}
	return nil
}

func runWatch(ctx context.Context, a *App, args []string) error {
	pos, err := flags(flag.NewFlagSet("run watch", flag.ContinueOnError), args, 1, 1)
	if err != nil {
		return err
	}
	id, err := parseID("run", pos[0])
	if err != nil {
		return err
	}
	c, err := a.api()
	if err != nil {
		return err
	}
	return watchRun(ctx, a, c, id)
}

// watchRun prints a run's jobs as they change until it finishes, then the
// run, and fails unless it succeeded.
func watchRun(ctx context.Context, a *App, c *Client, id int64) error {
	seen := map[int64]string{}
	runState := ""
	d, err := c.WaitRun(ctx, id, 0, func(d *RunDetail) {
		if s := state(d.Run.Status, d.Run.Conclusion); s != runState && d.Run.Status != "completed" {
			runState = s
			fmt.Fprintf(a.Stderr, "%s: %s\n", runName(d.Run), stateLabel(d.Run.Status, d.Run.Conclusion))
		}
		for _, j := range d.Jobs {
			s := state(j.Status, j.Conclusion)
			if seen[j.ID] == s {
				continue
			}
			seen[j.ID] = s
			line := "  " + stateLabel(j.Status, j.Conclusion) + "  " + j.Name
			if j.RunnerName != "" && j.Status == "in_progress" {
				line += " on " + j.RunnerName
			}
			fmt.Fprintln(a.Stderr, line)
		}
	})
	if err != nil {
		return err
	}
	fmt.Fprintln(a.Stdout)
	printRun(a.Stdout, c, d)
	if d.Run.Conclusion != "success" {
		return exitError(1)
	}
	return nil
}

func runCancel(ctx context.Context, a *App, args []string) error {
	pos, err := flags(flag.NewFlagSet("run cancel", flag.ContinueOnError), args, 1, 1)
	if err != nil {
		return err
	}
	id, err := parseID("run", pos[0])
	if err != nil {
		return err
	}
	c, err := a.api()
	if err != nil {
		return err
	}
	if err := c.Cancel(ctx, id); err != nil {
		return err
	}
	fmt.Fprintf(a.Stdout, "Cancelling run %d\n", id)
	return nil
}

func runRerun(ctx context.Context, a *App, args []string) error {
	fs := flag.NewFlagSet("run rerun", flag.ContinueOnError)
	watch := fs.Bool("watch", false, "wait for the new run to finish, and fail if it does")
	pos, err := flags(fs, args, 1, 1)
	if err != nil {
		return err
	}
	id, err := parseID("run", pos[0])
	if err != nil {
		return err
	}
	c, err := a.api()
	if err != nil {
		return err
	}
	newID, err := c.Rerun(ctx, id)
	if err != nil {
		return err
	}
	fmt.Fprintf(a.Stdout, "Started run %d: %s/runs/%d\n", newID, strings.TrimRight(c.URL, "/"), newID)
	if *watch {
		return watchRun(ctx, a, c, newID)
	}
	return nil
}

// selectJobs picks a run's jobs by ID, key or name (sel "" for all), and
// only failed ones if failed.
func selectJobs(jobs []Job, sel string, failed bool) ([]Job, error) {
	var out []Job
	id, _ := strconv.ParseInt(sel, 10, 64)
	for _, j := range jobs {
		if sel != "" && j.ID != id && j.Key != sel && j.Name != sel && !strings.HasPrefix(j.Name, sel+" (") {
			continue
		}
		if failed && (j.Status != "completed" || j.Conclusion == "success" || j.Conclusion == "skipped") {
			continue
		}
		out = append(out, j)
	}
	if len(out) == 0 && sel != "" {
		return nil, fmt.Errorf("the run has no job %q", sel)
	}
	return out, nil
}

func runLogs(ctx context.Context, a *App, args []string) error {
	fs := flag.NewFlagSet("run logs", flag.ContinueOnError)
	job := fs.String("job", "", "only this job: its ID, key or name")
	failed := fs.Bool("failed", false, "only failed jobs")
	follow := fs.Bool("follow", false, "keep printing new lines until the run finishes")
	fs.BoolVar(follow, "f", false, "same as --follow")
	stamps := fs.Bool("timestamps", false, "start lines with their time")
	pos, err := flags(fs, args, 1, 1)
	if err != nil {
		return err
	}
	id, err := parseID("run", pos[0])
	if err != nil {
		return err
	}
	c, err := a.api()
	if err != nil {
		return err
	}
	d, err := c.Run(ctx, id)
	if err != nil {
		return err
	}
	jobs, err := selectJobs(d.Jobs, *job, *failed)
	if err != nil {
		return err
	}
	w := bufio.NewWriter(a.Stdout)
	defer w.Flush()
	type cursor struct {
		after int64
		step  int
	}
	cursors := map[int64]*cursor{}
	// The job whose lines were printed last: when following, lines of jobs
	// running at the same time come in turns, each under its job's header.
	var last int64
	header := func(j Job, cur *cursor) {
		if last != 0 {
			fmt.Fprintln(w)
		}
		fmt.Fprintf(w, "==> %s (%s)\n", j.Name, stateLabel(j.Status, j.Conclusion))
		last, cur.step = j.ID, -2
	}
	// printNew prints a job's lines after its cursor.
	printNew := func(j Job) error {
		cur := cursors[j.ID]
		if cur == nil {
			cur = &cursor{step: -2}
			cursors[j.ID] = cur
			if !*follow {
				header(j, cur)
			}
		}
		var err error
		cur.after, err = c.Logs(ctx, j.ID, cur.after, func(l LogLine) {
			if last != j.ID {
				header(j, cur)
			}
			if l.Step != cur.step {
				cur.step = l.Step
				if l.Step >= 0 && l.Step < len(j.Steps) && j.Steps[l.Step].Name != "" {
					fmt.Fprintf(w, "--- %s\n", j.Steps[l.Step].Name)
				}
			}
			if *stamps {
				fmt.Fprint(w, time.UnixMilli(l.TS).Local().Format("2006-01-02T15:04:05.000"), " ")
			}
			fmt.Fprintln(w, l.Text)
		})
		return err
	}
	for {
		for _, j := range jobs {
			if err := printNew(j); err != nil {
				return err
			}
		}
		if err := w.Flush(); err != nil {
			return err
		}
		if !*follow || d.Run.Status == "completed" {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollInterval):
		}
		if d, err = c.Run(ctx, id); err != nil {
			return err
		}
		// Pick up jobs that have started since, with their steps' names.
		if jobs, err = selectJobs(d.Jobs, *job, *failed); err != nil {
			return err
		}
	}
}

// ---------------------------------------------------------------- secrets and variables

func settingList(kind string) command {
	return func(ctx context.Context, a *App, args []string) error {
		fs := flag.NewFlagSet(strings.TrimSuffix(kind, "s")+" list", flag.ContinueOnError)
		repo := fs.String("repo", "", "a repository's (OWNER/NAME) rather than the global ones")
		asJSON := fs.Bool("json", false, "print JSON")
		if _, err := flags(fs, args, 0, 0); err != nil {
			return err
		}
		c, err := a.api()
		if err != nil {
			return err
		}
		list, err := c.Settings(ctx, kind, *repo)
		if err != nil {
			return err
		}
		if *asJSON {
			return writeJSON(a.Stdout, list)
		}
		var rows [][]string
		for _, s := range list {
			row := []string{s.Name}
			if kind == "variables" {
				row = append(row, truncate(strings.ReplaceAll(s.Value, "\n", `\n`), 60))
			}
			rows = append(rows, append(row, ago(s.UpdatedAt)))
		}
		header := "NAME\tUPDATED"
		if kind == "variables" {
			header = "NAME\tVALUE\tUPDATED"
		}
		return table(a.Stdout, header, rows)
	}
}

func scopeName(repo string) string {
	if repo == "" {
		return "global"
	}
	return repo
}

func secretSet(ctx context.Context, a *App, args []string) error {
	fs := flag.NewFlagSet("secret set", flag.ContinueOnError)
	repo := fs.String("repo", "", "a repository's (OWNER/NAME) rather than a global one")
	value := fs.String("value", "", "the value (visible to other users of this machine; prefer stdin)")
	file := fs.String("from-file", "", "read the value from this file")
	pos, err := flags(fs, args, 1, 1)
	if err != nil {
		return err
	}
	v, err := a.readValue(*value, *file, "Value for "+pos[0]+": ", true)
	if err != nil {
		return err
	}
	c, err := a.api()
	if err != nil {
		return err
	}
	if err := c.SetSetting(ctx, "secrets", *repo, pos[0], v); err != nil {
		return err
	}
	fmt.Fprintf(a.Stdout, "Set secret %s (%s)\n", pos[0], scopeName(*repo))
	return nil
}

func variableSet(ctx context.Context, a *App, args []string) error {
	fs := flag.NewFlagSet("variable set", flag.ContinueOnError)
	repo := fs.String("repo", "", "a repository's (OWNER/NAME) rather than a global one")
	file := fs.String("from-file", "", "read the value from this file")
	pos, err := flags(fs, args, 1, 2)
	if err != nil {
		return err
	}
	value := ""
	if len(pos) == 2 {
		value = pos[1]
		if *file != "" {
			return errors.New("give a value or --from-file, not both")
		}
	}
	v, err := a.readValue(value, *file, "Value for "+pos[0]+": ", false)
	if err != nil {
		return err
	}
	c, err := a.api()
	if err != nil {
		return err
	}
	if err := c.SetSetting(ctx, "variables", *repo, pos[0], v); err != nil {
		return err
	}
	fmt.Fprintf(a.Stdout, "Set variable %s (%s)\n", pos[0], scopeName(*repo))
	return nil
}

// readValue is value, or file's contents, or what's on stdin: a line typed
// at a prompt if it's a terminal, else all of it without a final newline.
func (a *App) readValue(value, file, prompt string, hidden bool) (string, error) {
	switch {
	case value != "" && file != "":
		return "", errors.New("give --value or --from-file, not both")
	case value != "":
		return value, nil
	case file != "":
		b, err := os.ReadFile(file)
		return string(b), err
	case a.stdinFD >= 0:
		return a.prompt(prompt, hidden)
	}
	b, err := io.ReadAll(a.Stdin)
	if err != nil {
		return "", err
	}
	return strings.TrimSuffix(strings.TrimSuffix(string(b), "\n"), "\r"), nil
}

func settingDelete(kind string) command {
	return func(ctx context.Context, a *App, args []string) error {
		what := strings.TrimSuffix(kind, "s")
		fs := flag.NewFlagSet(what+" delete", flag.ContinueOnError)
		repo := fs.String("repo", "", "a repository's (OWNER/NAME) rather than a global one")
		pos, err := flags(fs, args, 1, 1)
		if err != nil {
			return err
		}
		c, err := a.api()
		if err != nil {
			return err
		}
		if err := c.DeleteSetting(ctx, kind, *repo, pos[0]); err != nil {
			return err
		}
		fmt.Fprintf(a.Stdout, "Deleted %s %s (%s)\n", what, pos[0], scopeName(*repo))
		return nil
	}
}

// ---------------------------------------------------------------- runners and tokens

func runnerList(ctx context.Context, a *App, args []string) error {
	fs := flag.NewFlagSet("runner list", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "print JSON")
	if _, err := flags(fs, args, 0, 0); err != nil {
		return err
	}
	c, err := a.api()
	if err != nil {
		return err
	}
	runners, err := c.Runners(ctx)
	if err != nil {
		return err
	}
	if *asJSON {
		return writeJSON(a.Stdout, runners)
	}
	var rows [][]string
	for _, r := range runners {
		st := "offline"
		switch {
		case r.Busy:
			st = fmt.Sprintf("busy: run %d, %s", r.RunID, r.JobName)
		case r.Online:
			st = "idle"
		}
		rows = append(rows, []string{r.Name, st, strings.Join(r.Labels, ","), r.OS + "/" + r.Arch, r.Version, ago(r.LastSeenAt)})
	}
	return table(a.Stdout, "NAME\tSTATUS\tLABELS\tPLATFORM\tVERSION\tLAST SEEN", rows)
}

func tokenList(ctx context.Context, a *App, args []string) error {
	fs := flag.NewFlagSet("token list", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "print JSON")
	if _, err := flags(fs, args, 0, 0); err != nil {
		return err
	}
	c, err := a.api()
	if err != nil {
		return err
	}
	tokens, err := c.Tokens(ctx)
	if err != nil {
		return err
	}
	if *asJSON {
		return writeJSON(a.Stdout, tokens)
	}
	var rows [][]string
	for _, t := range tokens {
		exp := "never"
		if t.ExpiresAt != 0 {
			exp = timestamp(t.ExpiresAt)
		}
		rows = append(rows, []string{strconv.FormatInt(t.ID, 10), t.Name, t.User, timestamp(t.CreatedAt), exp, ago(t.LastUsedAt)})
	}
	return table(a.Stdout, "ID\tNAME\tUSER\tCREATED\tEXPIRES\tLAST USED", rows)
}

func tokenCreate(ctx context.Context, a *App, args []string) error {
	fs := flag.NewFlagSet("token create", flag.ContinueOnError)
	expires := fs.Int("expires", 0, "days until it expires (0: never)")
	pos, err := flags(fs, args, 1, 1)
	if err != nil {
		return err
	}
	c, err := a.api()
	if err != nil {
		return err
	}
	token, info, err := c.CreateToken(ctx, pos[0], *expires)
	if err != nil {
		return err
	}
	fmt.Fprintln(a.Stdout, token)
	if info.ExpiresAt != 0 {
		fmt.Fprintf(a.Stderr, "Token %d expires %s. It's shown only once.\n", info.ID, timestamp(info.ExpiresAt))
	} else {
		fmt.Fprintf(a.Stderr, "Token %d doesn't expire. It's shown only once.\n", info.ID)
	}
	return nil
}

func tokenRevoke(ctx context.Context, a *App, args []string) error {
	pos, err := flags(flag.NewFlagSet("token revoke", flag.ContinueOnError), args, 1, 1)
	if err != nil {
		return err
	}
	if _, err := parseID("token", pos[0]); err != nil {
		return err
	}
	c, err := a.api()
	if err != nil {
		return err
	}
	if err := c.RevokeToken(ctx, pos[0]); err != nil {
		return err
	}
	fmt.Fprintln(a.Stdout, "Revoked token", pos[0])
	return nil
}

// ---------------------------------------------------------------- api

func apiCommand(ctx context.Context, a *App, args []string) error {
	fs := flag.NewFlagSet("api", flag.ContinueOnError)
	data := fs.String("data", "", "JSON request body, or @FILE, or @- for stdin")
	pos, err := flags(fs, args, 1, 2)
	if err != nil {
		return err
	}
	method, path := http.MethodGet, pos[0]
	if len(pos) == 2 {
		method, path = strings.ToUpper(pos[0]), pos[1]
	}
	if !strings.HasPrefix(path, "/") {
		path = "/api/" + path
	}
	var body any
	if *data != "" {
		b := []byte(*data)
		if name, ok := strings.CutPrefix(*data, "@"); ok {
			if name == "-" {
				b, err = io.ReadAll(a.Stdin)
			} else {
				b, err = os.ReadFile(name)
			}
			if err != nil {
				return err
			}
		}
		if !json.Valid(b) {
			return errors.New("--data isn't JSON")
		}
		body = json.RawMessage(b)
	}
	c, err := a.api()
	if err != nil {
		return err
	}
	var out json.RawMessage
	if err := c.Do(ctx, method, path, body, &out); err != nil {
		return err
	}
	var v any
	if json.Unmarshal(out, &v) == nil {
		return writeJSON(a.Stdout, v)
	}
	_, err = a.Stdout.Write(out)
	return err
}
