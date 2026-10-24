// Package agent is the kiwicd agent: it registers with a server like a GitHub
// self-hosted runner, long-polls for jobs and runs each job with act.
package agent

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dire-kiwi/kiwicd/internal/protocol"
	"github.com/nektos/act/pkg/common"
	"github.com/nektos/act/pkg/model"
	"github.com/nektos/act/pkg/runner"
	"github.com/sirupsen/logrus"
	"gopkg.in/yaml.v3"
)

// Credentials is the agent's registration, stored in the .runner file.
type Credentials struct {
	URL      string   `json:"url"`
	Name     string   `json:"name"`
	RunnerID int64    `json:"runner_id"`
	Token    string   `json:"token"`
	Labels   []string `json:"labels"` // label specs, see ParseLabel
}

func LoadCredentials(path string) (*Credentials, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Credentials
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &c, nil
}

func (c *Credentials) Save(path string) error {
	b, _ := json.MarshalIndent(c, "", "  ")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

// Label is a runner label and where jobs selecting it run, in act_runner's
// syntax: "name" or "name:host" runs on this machine, "name:docker://image"
// runs in a container.
type Label struct {
	Name  string
	Image string // "" for host
}

func ParseLabel(spec string) (Label, error) {
	name, rest, found := strings.Cut(strings.TrimSpace(spec), ":")
	if name == "" {
		return Label{}, fmt.Errorf("empty label")
	}
	if !found || rest == "host" {
		return Label{Name: name}, nil
	}
	if img, ok := strings.CutPrefix(rest, "docker://"); ok && img != "" {
		return Label{Name: name, Image: img}, nil
	}
	return Label{}, fmt.Errorf("label %q: expected name, name:host or name:docker://image", spec)
}

// Register exchanges a registration token for runner credentials.
func Register(ctx context.Context, url, token, name string, labelSpecs []string, version string) (*Credentials, error) {
	var names []string
	for _, s := range labelSpecs {
		l, err := ParseLabel(s)
		if err != nil {
			return nil, err
		}
		names = append(names, l.Name)
	}
	if name == "" {
		name, _ = os.Hostname()
	}
	c := newClient(url, "")
	resp, err := c.register(ctx, protocol.RegisterRequest{
		Token: token, Name: name, Labels: names, OS: runtime.GOOS, Arch: runtime.GOARCH, Version: version, Protocol: protocol.Version,
	})
	if err != nil {
		return nil, err
	}
	return &Credentials{URL: strings.TrimRight(url, "/"), Name: resp.Name, RunnerID: resp.RunnerID, Token: resp.RunnerToken, Labels: labelSpecs}, nil
}

type Config struct {
	Creds    *Credentials
	WorkDir  string
	Capacity int
	Version  string
	// ContainerDaemonSocket is passed to act for docker:// labels and container jobs.
	ContainerDaemonSocket string
	// ActionsURL is where `uses: owner/repo@ref` actions are cloned from.
	ActionsURL string
}

type Agent struct {
	cfg       Config
	c         *client
	log       *slog.Logger
	platforms map[string]string
}

func New(cfg Config) (*Agent, error) {
	if cfg.Capacity < 1 {
		cfg.Capacity = 1
	}
	if cfg.ActionsURL == "" {
		cfg.ActionsURL = "https://github.com"
	}
	abs, err := filepath.Abs(cfg.WorkDir)
	if err != nil {
		return nil, err
	}
	cfg.WorkDir = abs
	a := &Agent{cfg: cfg, c: newClient(cfg.Creds.URL, cfg.Creds.Token), log: slog.Default(), platforms: map[string]string{}}
	// GitHub's implicit labels run on the host unless a label overrides them.
	for _, l := range []string{"self-hosted", "linux", "macos", "windows", "x64", "arm64"} {
		a.platforms[l] = "-self-hosted"
	}
	for _, spec := range cfg.Creds.Labels {
		l, err := ParseLabel(spec)
		if err != nil {
			return nil, err
		}
		img := l.Image
		if img == "" {
			img = "-self-hosted"
		}
		a.platforms[strings.ToLower(l.Name)] = img
	}
	return a, nil
}

// Run polls for jobs until ctx is cancelled. Running jobs finish first.
func (a *Agent) Run(ctx context.Context) error {
	a.log.Info("agent started", "name", a.cfg.Creds.Name, "server", a.cfg.Creds.URL, "capacity", a.cfg.Capacity, "labels", a.cfg.Creds.Labels)
	var wg sync.WaitGroup
	errs := make(chan error, a.cfg.Capacity)
	for i := 0; i < a.cfg.Capacity; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := a.worker(ctx); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	return <-errs
}

func (a *Agent) worker(ctx context.Context) error {
	backoff := time.Second
	for ctx.Err() == nil {
		resp, err := a.c.poll(ctx, protocol.PollRequest{Version: a.cfg.Version, Protocol: protocol.Version})
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			var apiErr *apiError
			if errors.As(err, &apiErr) && apiErr.Status == 401 {
				return fmt.Errorf("the server rejected this agent's token (%s); register it again", apiErr.Msg)
			}
			a.log.Warn("poll failed", "err", err, "retry", backoff)
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
			}
			backoff = min(backoff*2, 30*time.Second)
			continue
		}
		backoff = time.Second
		if resp.Job != nil {
			// Jobs keep running through agent shutdown, like the runner's graceful stop.
			a.runJob(context.WithoutCancel(ctx), resp.Job)
		}
	}
	return nil
}

var unsafeChars = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func (a *Agent) runJob(parent context.Context, req *protocol.JobRequest) {
	log := a.log.With("job", req.JobID, "name", req.DisplayName, "repo", req.Repository)
	log.Info("job started")
	start := time.Now()

	workflow, err := model.ReadWorkflow(strings.NewReader(req.WorkflowYAML), false)
	var job *model.Job
	if err == nil {
		job = workflow.GetJob(req.JobKey)
		if job == nil {
			err = fmt.Errorf("job %q not found in workflow", req.JobKey)
		}
	}
	if err != nil {
		_ = a.c.complete(parent, req.JobID, protocol.JobComplete{Result: protocol.ResultFailure,
			Logs: []protocol.LogLine{{Step: 0, TS: nowMS(), Text: "##[error]" + err.Error()}}})
		return
	}

	timeout := time.Duration(req.TimeoutMinutes) * time.Minute
	if tm := strings.TrimSpace(job.TimeoutMinutes); tm != "" {
		if n, err := strconv.ParseFloat(tm, 64); err == nil && n > 0 {
			timeout = time.Duration(n * float64(time.Minute))
		}
	}
	if timeout <= 0 {
		timeout = 6 * time.Hour
	}
	// Like GitHub, cancelling (or timing out) is graceful first: act re-evaluates
	// step conditions, so `if: always()` steps still run. Five minutes later
	// the job is stopped outright.
	forceCtx, forceCancel := context.WithCancel(parent)
	defer forceCancel()
	graceful, gracefulCancel := context.WithTimeout(forceCtx, timeout)
	defer gracefulCancel()
	go func() {
		select {
		case <-graceful.Done():
			select {
			case <-time.After(5 * time.Minute):
				forceCancel()
			case <-forceCtx.Done():
			}
		case <-forceCtx.Done():
		}
	}()
	ctx := common.WithJobCancelContext(forceCtx, graceful)

	rep := newReporter(a.c, req.JobID, job, req.Secrets, gracefulCancel)
	flushCtx, stopFlush := context.WithCancel(parent)
	flushDone := make(chan struct{})
	go func() {
		rep.flushLoop(flushCtx)
		close(flushDone)
	}()

	result, outputs := a.execute(ctx, req, workflow, job, rep)
	if errors.Is(graceful.Err(), context.DeadlineExceeded) && parent.Err() == nil {
		rep.logf(rep.lastStep, "##[error]The job exceeded its timeout of %s.", timeout)
		result = protocol.ResultFailure
	} else if rep.cancelled {
		result = protocol.ResultCancelled
	}

	stopFlush()
	<-flushDone
	logs, steps := rep.finish(result)
	done := protocol.JobComplete{Result: result, Outputs: outputs, Logs: logs, Steps: steps}
	for attempt := 0; ; attempt++ {
		err := a.c.complete(parent, req.JobID, done)
		if err == nil || attempt >= 10 {
			if err != nil {
				log.Error("could not report job result", "err", err)
			}
			break
		}
		time.Sleep(time.Duration(attempt+1) * time.Second)
	}
	log.Info("job finished", "result", result, "duration", time.Since(start).Round(time.Millisecond))
}

func (a *Agent) execute(ctx context.Context, req *protocol.JobRequest, workflow *model.Workflow, job *model.Job, rep *reporter) (string, map[string]string) {
	rep.logf(0, "Runner name: %s", a.cfg.Creds.Name)
	rep.logf(0, "Job: %s (%s, run #%d)", req.DisplayName, req.WorkflowFile, req.RunNumber)
	rep.logf(0, "Repository: %s @ %s (%s)", req.Repository, short(req.Sha), req.Ref)

	jobDir := filepath.Join(a.cfg.WorkDir, "jobs", strconv.FormatInt(req.JobID, 10))
	defer os.RemoveAll(jobDir)
	repoName := unsafeChars.ReplaceAllString(req.Repository[strings.LastIndex(req.Repository, "/")+1:], "_")
	workspace := filepath.Join(jobDir, repoName)
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		rep.logf(0, "##[error]%v", err)
		return protocol.ResultFailure, nil
	}
	rep.logf(0, "Downloading source into %s", workspace)
	var buf bytes.Buffer
	if err := a.c.download(ctx, req.SourcePath, &buf); err != nil {
		rep.logf(0, "##[error]Downloading source failed: %v", err)
		return protocol.ResultFailure, nil
	}
	if err := extractTarGz(&buf, workspace); err != nil {
		rep.logf(0, "##[error]Extracting source failed: %v", err)
		return protocol.ResultFailure, nil
	}

	// Other jobs' results feed the needs context and the status functions.
	for id, res := range req.JobResults {
		if other := workflow.GetJob(id); other != nil {
			other.Result = res.Result
			other.Outputs = res.Outputs
		}
	}
	// The server already evaluated the job's `if`; run exactly one matrix combination.
	job.If = yaml.Node{}
	if len(req.Matrix) > 0 {
		m := map[string][]any{}
		for k, v := range req.Matrix {
			m[k] = []any{v}
		}
		var node yaml.Node
		if err := node.Encode(m); err != nil {
			rep.logf(0, "##[error]matrix: %v", err)
			return protocol.ResultFailure, nil
		}
		if job.Strategy == nil {
			job.Strategy = &model.Strategy{}
		}
		job.Strategy.RawMatrix = node
	} else {
		job.Strategy = nil
	}

	eventPath := filepath.Join(jobDir, "event.json")
	eventJSON, _ := json.Marshal(req.Event)
	if err := os.WriteFile(eventPath, eventJSON, 0o600); err != nil {
		rep.logf(0, "##[error]%v", err)
		return protocol.ResultFailure, nil
	}
	env := map[string]string{}
	for k, v := range req.Env {
		env[k] = v
	}
	env["RUNNER_NAME"] = a.cfg.Creds.Name

	cfg := &runner.Config{
		Actor:                 req.Actor,
		Workdir:               workspace,
		ActionCacheDir:        filepath.Join(a.cfg.WorkDir, "actions"),
		EventName:             req.EventName,
		EventPath:             eventPath,
		DefaultBranch:         "main",
		LogOutput:             true,
		Env:                   env,
		Secrets:               req.Secrets,
		Vars:                  req.Vars,
		Token:                 req.Secrets["GITHUB_TOKEN"],
		Platforms:             a.platforms,
		GitHubInstance:        "github.com",
		ActionsURL:            a.cfg.ActionsURL,
		AutoRemove:            true,
		ContainerDaemonSocket: a.cfg.ContainerDaemonSocket,
		ConcurrentJobs:        1,
	}
	if cfg.Secrets == nil {
		cfg.Secrets = map[string]string{}
	}
	r, err := runner.New(cfg)
	if err != nil {
		rep.logf(0, "##[error]%v", err)
		return protocol.ResultFailure, nil
	}
	plan := &model.Plan{Stages: []*model.Stage{{Runs: []*model.Run{{Workflow: workflow, JobID: req.JobKey}}}}}

	logger := logrus.New()
	logger.SetOutput(io.Discard)
	logger.SetLevel(logrus.DebugLevel)
	logger.SetFormatter(&formatter{r: rep})
	ctx = runner.WithJobLoggerFactory(ctx, loggerFactory{logger})

	execErr := r.NewPlanExecutor(plan)(ctx)
	rep.markDirty()

	result := job.Result
	switch {
	case ctx.Err() != nil || common.JobCancelContext(ctx).Err() != nil:
		result = protocol.ResultCancelled
	case result == "":
		if execErr != nil {
			rep.logf(rep.lastStep, "##[error]%v", execErr)
			result = protocol.ResultFailure
		} else {
			// act marks a job it decided not to run (e.g. unsupported runs-on) with no result.
			result = protocol.ResultSkipped
		}
	}
	return result, job.Outputs
}

type loggerFactory struct{ l *logrus.Logger }

func (f loggerFactory) WithJobLogger() *logrus.Logger { return f.l }

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

func extractTarGz(r io.Reader, dest string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		target := filepath.Join(dest, filepath.Clean("/"+h.Name))
		if !strings.HasPrefix(target, filepath.Clean(dest)+string(os.PathSeparator)) {
			continue
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(h.Mode)&0o777|0o600)
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return err
			}
			f.Close()
		case tar.TypeSymlink:
			_ = os.MkdirAll(filepath.Dir(target), 0o755)
			_ = os.Symlink(h.Linkname, target)
		}
	}
}
