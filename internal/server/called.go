package server

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/dire-kiwi/kiwicd/internal/wf"
	"github.com/nektos/act/pkg/model"
)

// Reusable workflows. A job with `uses:` calls a workflow with
// `on: workflow_call`:
//
//   - ./.github/workflows/x.yml is from the same repository and commit as the
//     workflow calling it.
//   - owner/repo/.github/workflows/x.yml@ref is from the kiwicd repository
//     owner/repo, if there is one and it lists the run's repository as a
//     reader (or is the run's repository). Otherwise it's from GitHub: a
//     public repository, fetched without credentials, or one the GitHub App
//     is installed on, fetched with an installation token for runs from
//     protected refs only, so that a branch anyone can push can't read
//     private workflows through the app.
//
// As on GitHub, the called workflows are read when a run is created, down to
// wf.MaxCallDepth levels, and a run that can't read them fails as an invalid
// workflow. Re-runs use the same ones.
//
// The scheduler (advance) runs a called workflow's jobs as part of the run.
// The calling job gets a row of its own for each matrix combination, with
// the inputs its with: gives; that row is in progress while the called
// workflow's jobs run, as rows whose parent_id is the calling row, and
// finishes with their result and the called workflow's outputs. Jobs of a
// called workflow get only the secrets their callers pass down (see
// calledSecrets), and still none on unprotected refs.

// calledWorkflow is a workflow one of a run's jobs calls.
type calledWorkflow struct {
	// Path is the keys of the calling jobs, from the run's workflow down,
	// joined with "/": build, or build/deploy for a job deploy in the
	// workflow build calls. Job keys can't contain "/".
	Path   string `json:"path"`
	Uses   string `json:"uses"`   // as the calling job has it
	Source string `json:"source"` // sourceKiwicd or sourceGitHub
	Repo   string `json:"repo"`   // owner/name there
	File   string `json:"file"`   // .github/workflows/x.yml
	Sha    string `json:"sha"`
	YAML   string `json:"yaml"`

	parsed *model.Workflow
}

const (
	sourceKiwicd = "kiwicd"
	sourceGitHub = "github"
)

// workflowSite is where a workflow file comes from.
type workflowSite struct {
	source, repo, sha, file string
}

func (w workflowSite) same(o workflowSite) bool {
	return w.source == o.source && strings.EqualFold(w.repo, o.repo) && w.sha == o.sha && w.file == o.file
}

// resolveCalls reads the workflows that w, the workflow file of a run of
// repo at sha, calls, and the ones they call.
func (s *Server) resolveCalls(ctx context.Context, repo *Repo, protected bool, file, sha string, w *model.Workflow) ([]*calledWorkflow, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	var out []*calledWorkflow
	var walk func(w *model.Workflow, chain []workflowSite, prefix string) error
	walk = func(w *model.Workflow, chain []workflowSite, prefix string) error {
		at := chain[len(chain)-1]
		keys := make([]string, 0, len(w.Jobs))
		for k := range w.Jobs {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, key := range keys {
			job := w.GetJob(key)
			if job.Uses == "" {
				continue
			}
			if strings.Contains(key, "/") {
				return fmt.Errorf("job %q calls a workflow, so its name can't contain /", key)
			}
			path := prefix + key
			fail := func(err error) error {
				return fmt.Errorf("job %s: %s: %w", strings.ReplaceAll(path, "/", " / "), job.Uses, err)
			}
			if len(chain) >= wf.MaxCallDepth {
				return fail(fmt.Errorf("reusable workflows can only be nested %d levels deep, counting the run's own workflow", wf.MaxCallDepth))
			}
			if len(out) >= wf.MaxCalls {
				return fail(fmt.Errorf("a run can call at most %d reusable workflows", wf.MaxCalls))
			}
			if len(job.Steps) > 0 || len(job.RunsOn()) > 0 {
				return fail(errors.New("a job that calls a workflow can't have steps or runs-on"))
			}
			u, err := wf.ParseUses(job.Uses)
			if err != nil {
				return fmt.Errorf("job %s: %w", strings.ReplaceAll(path, "/", " / "), err)
			}
			var site workflowSite
			var src []byte
			if u.Local {
				site = workflowSite{at.source, at.repo, at.sha, u.Path}
				src, err = s.readSiteWorkflow(ctx, site, protected)
			} else {
				site, src, err = s.readRemoteWorkflow(ctx, repo, protected, u)
			}
			if err != nil {
				return fail(err)
			}
			for _, c := range chain {
				if c.same(site) {
					return fail(errors.New("workflows call each other in a loop"))
				}
			}
			callee, err := wf.Parse(src)
			if err != nil {
				return fail(err)
			}
			if err := wf.CheckCall(job, callee); err != nil {
				return fail(err)
			}
			out = append(out, &calledWorkflow{Path: path, Uses: job.Uses, Source: site.source, Repo: site.repo, File: site.file, Sha: site.sha,
				YAML: string(src), parsed: callee})
			if err := walk(callee, append(chain[:len(chain):len(chain)], site), path+"/"); err != nil {
				return err
			}
		}
		return nil
	}
	return out, walk(w, []workflowSite{{sourceKiwicd, repo.Name, sha, file}}, "")
}

// readRemoteWorkflow reads owner/repo/path@ref for a run of repo.
func (s *Server) readRemoteWorkflow(ctx context.Context, repo *Repo, protected bool, u wf.Uses) (workflowSite, []byte, error) {
	other, err := s.repoNamed(u.Repo)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return workflowSite{}, nil, err
	}
	if other != nil {
		if !strings.EqualFold(other.Name, repo.Name) && !containsFold(other.Readers, repo.Name) {
			return workflowSite{}, nil, fmt.Errorf("%s doesn't list %s as a reader in its Settings → Job access, so %s's runs can't use its workflows",
				other.Name, repo.Name, repo.Name)
		}
		sha, err := resolveRef(ctx, s.repoPath(other.Name), u.Ref)
		if err != nil {
			return workflowSite{}, nil, fmt.Errorf("%s has no branch, tag or commit %s", other.Name, u.Ref)
		}
		site := workflowSite{sourceKiwicd, other.Name, sha, u.Path}
		src, err := s.readSiteWorkflow(ctx, site, protected)
		return site, src, err
	}
	auth := s.githubWorkflowAuth(ctx, u.Repo, protected)
	var commit struct {
		SHA string `json:"sha"`
	}
	if err := s.githubAPI(ctx, "GET", "/repos/"+u.Repo+"/commits/"+escapeRef(u.Ref), auth, nil, &commit); err != nil {
		return workflowSite{}, nil, githubWorkflowError(u.Repo, protected, err)
	}
	site := workflowSite{sourceGitHub, u.Repo, commit.SHA, u.Path}
	src, err := s.readSiteWorkflow(ctx, site, protected)
	return site, src, err
}

// readSiteWorkflow reads a workflow file.
func (s *Server) readSiteWorkflow(ctx context.Context, site workflowSite, protected bool) ([]byte, error) {
	if site.source == sourceKiwicd {
		out, err := git(ctx, s.repoPath(site.repo), "show", site.sha+":"+site.file)
		if err != nil {
			return nil, fmt.Errorf("%s has no %s at %s", site.repo, site.file, short(site.sha))
		}
		return []byte(out), nil
	}
	var content struct {
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
	}
	path := "/repos/" + site.repo + "/contents/" + site.file + "?ref=" + url.QueryEscape(site.sha)
	if err := s.githubAPI(ctx, "GET", path, s.githubWorkflowAuth(ctx, site.repo, protected), nil, &content); err != nil {
		return nil, githubWorkflowError(site.repo, protected, err)
	}
	if content.Encoding != "base64" {
		return nil, fmt.Errorf("GitHub sent %s/%s as %q", site.repo, site.file, content.Encoding)
	}
	return base64.StdEncoding.DecodeString(strings.ReplaceAll(content.Content, "\n", ""))
}

// githubWorkflowAuth returns the Authorization header for reading a
// repository's workflows on GitHub: an installation token of the GitHub App
// for runs from protected refs, if it's installed there, or nothing.
func (s *Server) githubWorkflowAuth(ctx context.Context, name string, protected bool) string {
	if !protected {
		return ""
	}
	a, err := s.githubApp()
	if err != nil {
		return ""
	}
	token, err := s.installationToken(ctx, a, name, map[string]string{"contents": "read"})
	if err != nil {
		s.log.Warn("GitHub App installation token", "repo", name, "err", err)
	}
	if token == "" {
		return ""
	}
	return "Bearer " + token
}

func githubWorkflowError(name string, protected bool, err error) error {
	if isGitHubStatus(err, 404) || isGitHubStatus(err, 422) {
		why := "a private repository needs the GitHub App installed on it"
		if !protected {
			why = "runs from refs that aren't protected only read public repositories"
		}
		return fmt.Errorf("not found on GitHub (%s)", why)
	}
	return fmt.Errorf("reading it from GitHub: %w", err)
}

func escapeRef(ref string) string {
	parts := strings.Split(ref, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// repoNamed finds a repository by name, ignoring case as GitHub does. It
// returns nil and sql.ErrNoRows if there is none.
func (s *Server) repoNamed(name string) (*Repo, error) {
	return scanRepo(s.db.QueryRow(`SELECT `+repoCols+` FROM repos WHERE name = ? COLLATE NOCASE`, name))
}

func (s *Server) saveCalledWorkflows(runID int64, called []*calledWorkflow) error {
	for _, c := range called {
		if _, err := s.db.Exec(`INSERT INTO run_workflows (run_id, path, uses, source, repo, file, sha, yaml) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			runID, c.Path, c.Uses, c.Source, c.Repo, c.File, c.Sha, c.YAML); err != nil {
			return err
		}
	}
	return nil
}

// calledWorkflows returns the workflows a run's jobs call, by path, parsed.
func (s *Server) calledWorkflows(runID int64) (map[string]*calledWorkflow, error) {
	rows, err := s.db.Query(`SELECT path, uses, source, repo, file, sha, yaml FROM run_workflows WHERE run_id = ?`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]*calledWorkflow{}
	for rows.Next() {
		var c calledWorkflow
		if err := rows.Scan(&c.Path, &c.Uses, &c.Source, &c.Repo, &c.File, &c.Sha, &c.YAML); err != nil {
			return nil, err
		}
		if c.parsed, err = wf.Parse([]byte(c.YAML)); err != nil {
			return nil, fmt.Errorf("called workflow %s: %w", c.Uses, err)
		}
		out[c.Path] = &c
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------- job tree

// jobTree is a run's job rows, with the rows of called workflows under the
// calling job's rows.
type jobTree struct {
	byID     map[int64]*Job
	children map[int64][]*Job // by parent_id; 0 for the run's own workflow
}

func newJobTree(jobs []*Job) *jobTree {
	t := &jobTree{byID: map[int64]*Job{}, children: map[int64][]*Job{}}
	for _, j := range jobs {
		t.byID[j.ID] = j
		t.children[j.ParentID] = append(t.children[j.ParentID], j)
	}
	return t
}

// path returns a calling job row's calledWorkflow path.
func (t *jobTree) path(j *Job) string {
	p := j.Key
	for depth := 0; j.ParentID != 0 && depth < 2*wf.MaxCallDepth; depth++ {
		j = t.byID[j.ParentID]
		if j == nil {
			break
		}
		p = j.Key + "/" + p
	}
	return p
}

// cancelled reports whether a row or one of its callers was cancelled.
func (t *jobTree) cancelled(j *Job) bool {
	for depth := 0; j != nil && depth < 2*wf.MaxCallDepth; depth++ {
		if j.Cancel {
			return true
		}
		j = t.byID[j.ParentID]
	}
	return false
}

// results returns the results of the finished jobs among rows, by key.
func results(rows []*Job) map[string]wf.JobResult {
	byKey := map[string][]*Job{}
	for _, j := range rows {
		byKey[j.Key] = append(byKey[j.Key], j)
	}
	out := map[string]wf.JobResult{}
	for key, rows := range byKey {
		if r, done := aggregate(rows); done {
			out[key] = r
		}
	}
	return out
}

// scopeOf returns the workflow a row belongs to and its inputs context.
func (t *jobTree) scopeOf(run *Run, top *model.Workflow, called map[string]*calledWorkflow, j *Job) (*model.Workflow, map[string]any) {
	if j.ParentID == 0 {
		inputs, _ := run.EventJSON["inputs"].(map[string]any)
		return top, inputs
	}
	parent := t.byID[j.ParentID]
	if parent == nil {
		return nil, nil
	}
	c := called[t.path(parent)]
	if c == nil {
		return nil, nil
	}
	inputs := parent.Inputs
	if inputs == nil {
		inputs = map[string]any{}
	}
	return c.parsed, inputs
}

// calledSecrets returns the secrets the jobs of the workflow a calling row
// calls get: base, the run's secrets, passed down through each calling job
// from the run's workflow to this one by its `secrets:`.
func (s *Server) calledSecrets(run *Run, top *model.Workflow, called map[string]*calledWorkflow, t *jobTree, caller *Job,
	base map[string]string) (map[string]string, error) {
	var chain []*Job
	for j := caller; j != nil && len(chain) < 2*wf.MaxCallDepth; j = t.byID[j.ParentID] {
		chain = append([]*Job{j}, chain...)
		if j.ParentID == 0 {
			break
		}
	}
	ghc := s.githubContext(run)
	vars := s.varsFor(run.RepoID)
	secrets := base
	for _, c := range chain {
		w, inputs := t.scopeOf(run, top, called, c)
		if w == nil || w.GetJob(c.Key) == nil {
			return nil, fmt.Errorf("the workflow of job %s is missing", c.Name)
		}
		env := &wf.Env{Workflow: w, JobID: c.Key, Github: ghc, Vars: vars, Inputs: inputs, Matrix: c.Matrix,
			Results: results(t.children[c.ParentID]), Secrets: secrets}
		var err error
		if secrets, err = env.CallSecrets(); err != nil {
			return nil, fmt.Errorf("job %s: %w", c.Name, err)
		}
	}
	return secrets, nil
}
