package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/dire-kiwi/kiwicd/internal/protocol"
	"github.com/dire-kiwi/kiwicd/internal/wf"
	"github.com/nektos/act/pkg/model"
)

type Run struct {
	ID           int64          `json:"id"`
	RepoID       int64          `json:"repo_id"`
	Repo         string         `json:"repo"`
	Number       int64          `json:"number"`
	Attempt      int64          `json:"attempt"`
	WorkflowFile string         `json:"workflow_file"`
	WorkflowName string         `json:"workflow_name"`
	WorkflowYAML string         `json:"workflow_yaml,omitempty"`
	Title        string         `json:"title"`
	Event        string         `json:"event"`
	EventJSON    map[string]any `json:"event_payload,omitempty"`
	Ref          string         `json:"ref"`
	Sha          string         `json:"sha"`
	Actor        string         `json:"actor"`
	Status       string         `json:"status"`
	Conclusion   string         `json:"conclusion"`
	Error        string         `json:"error"`
	Cancel       bool           `json:"cancel_requested"`
	Protected    bool           `json:"protected"` // its ref got secrets; see protectedrefs.go
	CreatedAt    int64          `json:"created_at"`
	StartedAt    int64          `json:"started_at"`
	FinishedAt   int64          `json:"finished_at"`
}

const runCols = `r.id, r.repo_id, p.name, r.number, r.attempt, r.workflow_file, r.workflow_name, r.workflow_yaml, r.title, r.event, r.event_json,
	r.ref, r.sha, r.actor, r.status, r.conclusion, r.error, r.cancel_requested, r.protected, r.created_at, r.started_at, r.finished_at`

func scanRun(row interface{ Scan(...any) error }) (*Run, error) {
	var r Run
	var ev string
	if err := row.Scan(&r.ID, &r.RepoID, &r.Repo, &r.Number, &r.Attempt, &r.WorkflowFile, &r.WorkflowName, &r.WorkflowYAML, &r.Title, &r.Event, &ev,
		&r.Ref, &r.Sha, &r.Actor, &r.Status, &r.Conclusion, &r.Error, &r.Cancel, &r.Protected, &r.CreatedAt, &r.StartedAt, &r.FinishedAt); err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(ev), &r.EventJSON)
	return &r, nil
}

func (s *Server) getRun(id int64) (*Run, error) {
	return scanRun(s.db.QueryRow(`SELECT `+runCols+` FROM runs r JOIN repos p ON p.id = r.repo_id WHERE r.id = ?`, id))
}

type Job struct {
	ID         int64                `json:"id"`
	RunID      int64                `json:"run_id"`
	Key        string               `json:"key"`
	Name       string               `json:"name"`
	Matrix     map[string]any       `json:"matrix"`
	RunsOn     []string             `json:"runs_on"`
	Status     string               `json:"status"`
	Conclusion string               `json:"conclusion"`
	Error      string               `json:"error"`
	RunnerID   int64                `json:"runner_id"`
	RunnerName string               `json:"runner_name"`
	Outputs    map[string]string    `json:"outputs"`
	Steps      []protocol.StepState `json:"steps"`
	Cancel     bool                 `json:"cancel_requested"`
	LogSeq     int64                `json:"log_seq"`
	CreatedAt  int64                `json:"created_at"`
	StartedAt  int64                `json:"started_at"`
	FinishedAt int64                `json:"finished_at"`
	Heartbeat  int64                `json:"-"`
}

const jobCols = `id, run_id, job_key, name, matrix, runs_on, status, conclusion, error, runner_id, runner_name, outputs, steps,
	cancel_requested, log_seq, created_at, started_at, finished_at, heartbeat_at`

func scanJob(row interface{ Scan(...any) error }) (*Job, error) {
	var j Job
	var matrix, runsOn, outputs, steps string
	if err := row.Scan(&j.ID, &j.RunID, &j.Key, &j.Name, &matrix, &runsOn, &j.Status, &j.Conclusion, &j.Error, &j.RunnerID, &j.RunnerName,
		&outputs, &steps, &j.Cancel, &j.LogSeq, &j.CreatedAt, &j.StartedAt, &j.FinishedAt, &j.Heartbeat); err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(matrix), &j.Matrix)
	_ = json.Unmarshal([]byte(runsOn), &j.RunsOn)
	_ = json.Unmarshal([]byte(outputs), &j.Outputs)
	_ = json.Unmarshal([]byte(steps), &j.Steps)
	if j.RunsOn == nil {
		j.RunsOn = []string{}
	}
	if j.Steps == nil {
		j.Steps = []protocol.StepState{}
	}
	return &j, nil
}

func (s *Server) getJob(id int64) (*Job, error) {
	return scanJob(s.db.QueryRow(`SELECT `+jobCols+` FROM jobs WHERE id = ?`, id))
}

func (s *Server) runJobs(runID int64) ([]*Job, error) {
	rows, err := s.db.Query(`SELECT `+jobCols+` FROM jobs WHERE run_id = ? ORDER BY id`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

type runSpec struct {
	repo                   *Repo
	file, yaml, name       string
	event                  string
	payload                map[string]any
	ref, sha, actor, title string
	parseErr               error
	number, attempt        int64
}

func (s *Server) createRun(ctx context.Context, spec runSpec) (int64, error) {
	s.mu.Lock()
	number := spec.number
	if number == 0 {
		if err := s.db.QueryRow(`SELECT COALESCE(MAX(number), 0) + 1 FROM runs WHERE repo_id = ?`, spec.repo.ID).Scan(&number); err != nil {
			s.mu.Unlock()
			return 0, err
		}
	}
	attempt := spec.attempt
	if attempt == 0 {
		attempt = 1
	}
	title := spec.title
	if title == "" {
		title = spec.name
	}
	status, conclusion, errText := protocol.StatusQueued, "", ""
	finished := int64(0)
	if spec.parseErr != nil {
		status, conclusion, errText = protocol.StatusCompleted, protocol.ResultFailure, "Invalid workflow file: "+spec.parseErr.Error()
		finished = nowMS()
	}
	res, err := s.db.Exec(`INSERT INTO runs (repo_id, number, attempt, workflow_file, workflow_name, workflow_yaml, title, event, event_json, ref, sha, actor,
		status, conclusion, error, protected, created_at, finished_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		spec.repo.ID, number, attempt, spec.file, spec.name, spec.yaml, title, spec.event, mustJSON(spec.payload), spec.ref, spec.sha, spec.actor,
		status, conclusion, errText, refProtected(spec.repo, spec.ref), nowMS(), finished)
	s.mu.Unlock()
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	s.log.Info("run created", "repo", spec.repo.Name, "run", id, "number", number, "workflow", spec.file, "event", spec.event, "ref", spec.ref,
		"secrets", refProtected(spec.repo, spec.ref))
	if err := s.advance(ctx, id); err != nil {
		s.log.Error("advance", "run", id, "err", err)
	}
	return id, nil
}

// githubContext builds the github context for job-level expressions.
func (s *Server) githubContext(run *Run) *model.GithubContext {
	owner, _, _ := strings.Cut(run.Repo, "/")
	ghc := &model.GithubContext{
		Event:           run.EventJSON,
		EventName:       run.Event,
		Workflow:        run.WorkflowName,
		RunID:           fmt.Sprint(run.ID),
		RunNumber:       fmt.Sprint(run.Number),
		RunAttempt:      fmt.Sprint(run.Attempt),
		Actor:           run.Actor,
		Repository:      run.Repo,
		RepositoryOwner: owner,
		Sha:             run.Sha,
		Ref:             run.Ref,
		ServerURL:       s.cfg.PublicURL,
	}
	if ghc.Event == nil {
		ghc.Event = map[string]any{}
	}
	ghc.SetRefTypeAndName()
	return ghc
}

// aggregate combines the rows of one workflow job (several for a matrix).
func aggregate(rows []*Job) (wf.JobResult, bool) {
	res := wf.JobResult{Outputs: map[string]string{}}
	allSkipped := true
	failed, cancelled := false, false
	for _, j := range rows {
		if j.Status != protocol.StatusCompleted {
			return res, false
		}
		switch j.Conclusion {
		case protocol.ResultFailure:
			failed = true
		case protocol.ResultCancelled:
			cancelled = true
		}
		if j.Conclusion != protocol.ResultSkipped {
			allSkipped = false
		}
		for k, v := range j.Outputs {
			res.Outputs[k] = v
		}
	}
	switch {
	case failed:
		res.Result = protocol.ResultFailure
	case cancelled:
		res.Result = protocol.ResultCancelled
	case allSkipped && len(rows) > 0:
		res.Result = protocol.ResultSkipped
	default:
		res.Result = protocol.ResultSuccess
	}
	return res, true
}

func (s *Server) varsFor(repoID int64) map[string]string {
	return s.settingsMap("variables", repoID)
}

// secretsFor decrypts the secrets a repository's jobs get.
func (s *Server) secretsFor(repoID int64) (map[string]string, error) {
	out := s.settingsMap("secrets", repoID)
	for k, v := range out {
		plain, err := s.box.open(purposeSecret, v)
		if err != nil {
			return nil, fmt.Errorf("secret %s: %w", k, err)
		}
		out[k] = plain
	}
	return out, nil
}

func (s *Server) settingsMap(table string, repoID int64) map[string]string {
	out := map[string]string{}
	rows, err := s.db.Query(`SELECT name, value FROM `+table+` WHERE repo_id = 0 OR repo_id = ? ORDER BY repo_id`, repoID)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var k, v string
		_ = rows.Scan(&k, &v)
		out[k] = v // repository values override global ones
	}
	return out
}

func (s *Server) insertJob(runID int64, key, name string, matrix map[string]any, runsOn []string, status, conclusion, errText string) error {
	finished := int64(0)
	if status == protocol.StatusCompleted {
		finished = nowMS()
	}
	if runsOn == nil {
		runsOn = []string{}
	}
	_, err := s.db.Exec(`INSERT INTO jobs (run_id, job_key, name, matrix, runs_on, status, conclusion, error, created_at, finished_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, runID, key, name, mustJSON(matrix), mustJSON(runsOn), status, conclusion, errText, nowMS(), finished)
	return err
}

// advance moves a run forward: it creates jobs whose needs are satisfied
// (evaluating if, matrix and runs-on) and completes the run when all jobs are done.
func (s *Server) advance(ctx context.Context, runID int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.advanceLocked(ctx, runID)
}

func (s *Server) advanceLocked(_ context.Context, runID int64) error {
	run, err := s.getRun(runID)
	if err != nil {
		return err
	}
	if run.Status == protocol.StatusCompleted {
		return nil
	}
	workflow, err := wf.Parse([]byte(run.WorkflowYAML))
	if err != nil {
		_, err = s.db.Exec(`UPDATE runs SET status = ?, conclusion = ?, error = ?, finished_at = ? WHERE id = ?`,
			protocol.StatusCompleted, protocol.ResultFailure, "Invalid workflow file: "+err.Error(), nowMS(), runID)
		s.publishRun(runID)
		return err
	}
	order, _ := wf.Order(workflow)
	ghc := s.githubContext(run)
	vars := s.varsFor(run.RepoID)
	inputs, _ := run.EventJSON["inputs"].(map[string]any)
	queued := false

	for changed := true; changed; {
		changed = false
		jobs, err := s.runJobs(runID)
		if err != nil {
			return err
		}
		byKey := map[string][]*Job{}
		for _, j := range jobs {
			byKey[j.Key] = append(byKey[j.Key], j)
		}
		results := map[string]wf.JobResult{}
		for key, rows := range byKey {
			if r, done := aggregate(rows); done {
				results[key] = r
			}
		}
		for _, key := range order {
			if _, exists := byKey[key]; exists {
				continue
			}
			job := workflow.GetJob(key)
			ready := true
			for _, n := range job.Needs() {
				if _, done := results[n]; !done {
					ready = false
				}
			}
			if !ready {
				continue
			}
			changed = true
			env := &wf.Env{Workflow: workflow, JobID: key, Github: ghc, Vars: vars, Inputs: inputs, Results: results}
			if run.Cancel {
				if err := s.insertJob(runID, key, env.Name(), nil, nil, protocol.StatusCompleted, protocol.ResultCancelled, ""); err != nil {
					return err
				}
				continue
			}
			ok, err := env.JobIf()
			if err != nil {
				if err := s.insertJob(runID, key, env.Name(), nil, nil, protocol.StatusCompleted, protocol.ResultFailure, "if: "+err.Error()); err != nil {
					return err
				}
				continue
			}
			if !ok {
				if err := s.insertJob(runID, key, env.Name(), nil, nil, protocol.StatusCompleted, protocol.ResultSkipped, ""); err != nil {
					return err
				}
				continue
			}
			if job.Uses != "" {
				if err := s.insertJob(runID, key, env.Name(), nil, nil, protocol.StatusCompleted, protocol.ResultFailure,
					"reusable workflows (jobs.<id>.uses) are not supported yet"); err != nil {
					return err
				}
				continue
			}
			combos, err := env.Matrixes()
			if err != nil {
				if err := s.insertJob(runID, key, env.Name(), nil, nil, protocol.StatusCompleted, protocol.ResultFailure, err.Error()); err != nil {
					return err
				}
				continue
			}
			for _, combo := range combos {
				if len(combo) == 0 {
					combo = nil
				}
				env.Matrix = combo
				name := env.Name()
				labels, err := env.RunsOn()
				if err == nil && len(labels) == 0 {
					err = fmt.Errorf("runs-on is empty")
				}
				if err != nil {
					if err := s.insertJob(runID, key, name, combo, nil, protocol.StatusCompleted, protocol.ResultFailure, "runs-on: "+err.Error()); err != nil {
						return err
					}
					continue
				}
				if l, denied := s.runDenied(run, labels); denied {
					msg := fmt.Sprintf("runs-on: the runner label %q is reserved for other repositories or branches", l)
					if err := s.insertJob(runID, key, name, combo, labels, protocol.StatusCompleted, protocol.ResultFailure, msg); err != nil {
						return err
					}
					continue
				}
				if err := s.insertJob(runID, key, name, combo, labels, protocol.StatusQueued, "", ""); err != nil {
					return err
				}
				queued = true
			}
		}
	}

	jobs, err := s.runJobs(runID)
	if err != nil {
		return err
	}
	keys := map[string]bool{}
	allDone := true
	anyStarted := false
	failed, cancelled := false, run.Cancel
	for _, j := range jobs {
		keys[j.Key] = true
		if j.Status != protocol.StatusCompleted {
			allDone = false
		}
		if j.Status == protocol.StatusInProgress || j.Status == protocol.StatusCompleted {
			anyStarted = true
		}
		switch j.Conclusion {
		case protocol.ResultFailure:
			failed = true
		case protocol.ResultCancelled:
			cancelled = true
		}
	}
	if len(keys) < len(workflow.Jobs) {
		allDone = false
		// A pending job whose needs can never complete would block forever;
		// that cannot happen because every finished need yields a job row.
	}
	switch {
	case allDone:
		conclusion := protocol.ResultSuccess
		if failed {
			conclusion = protocol.ResultFailure
		} else if cancelled {
			conclusion = protocol.ResultCancelled
		}
		_, err = s.db.Exec(`UPDATE runs SET status = ?, conclusion = ?, finished_at = ?, started_at = CASE WHEN started_at = 0 THEN ? ELSE started_at END WHERE id = ?`,
			protocol.StatusCompleted, conclusion, nowMS(), nowMS(), runID)
		s.log.Info("run completed", "run", runID, "conclusion", conclusion)
	case anyStarted && run.Status == protocol.StatusQueued:
		_, err = s.db.Exec(`UPDATE runs SET status = ?, started_at = ? WHERE id = ?`, protocol.StatusInProgress, nowMS(), runID)
	}
	if err != nil {
		return err
	}
	s.publishRun(runID)
	if queued {
		go s.notifyJobsReady()
	}
	return nil
}

func (s *Server) publishRun(runID int64) {
	run, err := s.getRun(runID)
	if err != nil {
		return
	}
	run.WorkflowYAML = ""
	run.EventJSON = nil
	s.hub.publish("runs", "run", run)
	s.hub.publish(fmt.Sprintf("run:%d", runID), "run", run)
}

func (s *Server) publishJob(j *Job) {
	s.hub.publish(fmt.Sprintf("job:%d", j.ID), "job", j)
	s.hub.publish(fmt.Sprintf("run:%d", j.RunID), "job", j)
}

// cancelRun cancels queued jobs now and asks agents to stop running ones.
func (s *Server) cancelRun(ctx context.Context, runID int64) error {
	s.mu.Lock()
	_, err := s.db.Exec(`UPDATE runs SET cancel_requested = 1 WHERE id = ? AND status != ?`, runID, protocol.StatusCompleted)
	if err == nil {
		_, err = s.db.Exec(`UPDATE jobs SET status = ?, conclusion = ?, finished_at = ? WHERE run_id = ? AND status IN (?, ?)`,
			protocol.StatusCompleted, protocol.ResultCancelled, nowMS(), runID, protocol.StatusQueued, protocol.StatusPending)
	}
	if err == nil {
		_, err = s.db.Exec(`UPDATE jobs SET cancel_requested = 1 WHERE run_id = ? AND status = ?`, runID, protocol.StatusInProgress)
	}
	s.mu.Unlock()
	if err != nil {
		return err
	}
	return s.advance(ctx, runID)
}

// failFast cancels the other matrix jobs of a failed job.
func (s *Server) failFast(job *Job, workflowYAML string) {
	w, err := wf.Parse([]byte(workflowYAML))
	if err != nil {
		return
	}
	def := w.GetJob(job.Key)
	if def == nil || def.Strategy == nil || !wf.FailFast(def) {
		return
	}
	_, _ = s.db.Exec(`UPDATE jobs SET status = ?, conclusion = ?, finished_at = ? WHERE run_id = ? AND job_key = ? AND status = ?`,
		protocol.StatusCompleted, protocol.ResultCancelled, nowMS(), job.RunID, job.Key, protocol.StatusQueued)
	_, _ = s.db.Exec(`UPDATE jobs SET cancel_requested = 1 WHERE run_id = ? AND job_key = ? AND status = ?`,
		job.RunID, job.Key, protocol.StatusInProgress)
}

// completeJob records a job's result and advances its run.
func (s *Server) completeJob(ctx context.Context, jobID int64, result, errText string, outputs map[string]string, steps []protocol.StepState) error {
	s.mu.Lock()
	job, err := s.getJob(jobID)
	if err != nil {
		s.mu.Unlock()
		return err
	}
	if job.Status == protocol.StatusCompleted {
		s.mu.Unlock()
		return nil
	}
	if outputs == nil {
		outputs = map[string]string{}
	}
	if steps == nil {
		steps = job.Steps
	}
	_, err = s.db.Exec(`UPDATE jobs SET status = ?, conclusion = ?, error = ?, outputs = ?, steps = ?, finished_at = ? WHERE id = ?`,
		protocol.StatusCompleted, result, errText, mustJSON(outputs), mustJSON(steps), nowMS(), jobID)
	if err == nil && result == protocol.ResultFailure {
		run, rerr := s.getRun(job.RunID)
		if rerr == nil {
			s.failFast(job, run.WorkflowYAML)
		}
	}
	s.mu.Unlock()
	if err != nil {
		return err
	}
	if j, err := s.getJob(jobID); err == nil {
		s.publishJob(j)
	}
	s.log.Info("job completed", "job", jobID, "run", job.RunID, "name", job.Name, "result", result)
	err = s.advance(ctx, job.RunID)
	if aerr := s.archiveJobLogs(jobID); aerr != nil {
		// The lines stay in the database; archivePendingLogs retries at startup.
		s.log.Error("archive job logs", "job", jobID, "err", aerr)
	}
	return err
}

// reapLostJobs fails jobs whose agent stopped sending heartbeats.
func (s *Server) reapLostJobs(ctx context.Context) {
	cutoff := nowMS() - 120_000
	rows, err := s.db.Query(`SELECT id FROM jobs WHERE status = ? AND heartbeat_at < ?`, protocol.StatusInProgress, cutoff)
	if err != nil {
		return
	}
	var ids []int64
	for rows.Next() {
		var id int64
		_ = rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	for _, id := range ids {
		s.log.Warn("job lost its runner", "job", id)
		_ = s.appendLogs(id, []protocol.LogLine{{Step: 0, TS: nowMS(), Text: "##[error]The runner stopped responding; the job was marked as failed."}})
		_ = s.completeJob(ctx, id, protocol.ResultFailure, "lost communication with the runner", nil, nil)
	}
}

var _ = sql.ErrNoRows
