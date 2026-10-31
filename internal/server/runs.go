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
	// Concurrency group; see concurrency.go.
	ConcurrencyGroup string `json:"concurrency_group,omitempty"`
	// workflow_run events that led to it; see triggers.go.
	TriggerDepth int64 `json:"-"`
	CreatedAt    int64 `json:"created_at"`
	StartedAt    int64 `json:"started_at"`
	FinishedAt   int64 `json:"finished_at"`
}

const runCols = `r.id, r.repo_id, p.name, r.number, r.attempt, r.workflow_file, r.workflow_name, r.workflow_yaml, r.title, r.event, r.event_json,
	r.ref, r.sha, r.actor, r.status, r.conclusion, r.error, r.cancel_requested, r.protected, r.concurrency_group, r.trigger_depth, r.created_at, r.started_at, r.finished_at`

func scanRun(row interface{ Scan(...any) error }) (*Run, error) {
	var r Run
	var ev string
	if err := row.Scan(&r.ID, &r.RepoID, &r.Repo, &r.Number, &r.Attempt, &r.WorkflowFile, &r.WorkflowName, &r.WorkflowYAML, &r.Title, &r.Event, &ev,
		&r.Ref, &r.Sha, &r.Actor, &r.Status, &r.Conclusion, &r.Error, &r.Cancel, &r.Protected, &r.ConcurrencyGroup, &r.TriggerDepth, &r.CreatedAt, &r.StartedAt, &r.FinishedAt); err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(ev), &r.EventJSON)
	return &r, nil
}

func (s *Server) getRun(id int64) (*Run, error) {
	return scanRun(s.db.QueryRow(`SELECT `+runCols+` FROM runs r JOIN repos p ON p.id = r.repo_id WHERE r.id = ?`, id))
}

type Job struct {
	ID    int64  `json:"id"`
	RunID int64  `json:"run_id"`
	Key   string `json:"key"`
	Name  string `json:"name"`
	// ParentID is the calling job's row, for a job of a reusable workflow.
	ParentID int64 `json:"parent_id"`
	// Uses is the workflow a calling job calls, and Inputs the inputs it
	// gives it; see called.go.
	Uses       string               `json:"uses,omitempty"`
	Inputs     map[string]any       `json:"inputs,omitempty"`
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

const jobCols = `id, run_id, job_key, name, parent_id, uses, inputs, matrix, runs_on, status, conclusion, error, runner_id, runner_name, outputs, steps,
	cancel_requested, log_seq, created_at, started_at, finished_at, heartbeat_at`

func scanJob(row interface{ Scan(...any) error }) (*Job, error) {
	var j Job
	var inputs, matrix, runsOn, outputs, steps string
	if err := row.Scan(&j.ID, &j.RunID, &j.Key, &j.Name, &j.ParentID, &j.Uses, &inputs, &matrix, &runsOn, &j.Status, &j.Conclusion, &j.Error,
		&j.RunnerID, &j.RunnerName, &outputs, &steps, &j.Cancel, &j.LogSeq, &j.CreatedAt, &j.StartedAt, &j.FinishedAt, &j.Heartbeat); err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(inputs), &j.Inputs)
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
	depth                  int64 // see Run.TriggerDepth
	// called are the reusable workflows the run's jobs call, for a re-run;
	// createRun reads them otherwise.
	called []*calledWorkflow
}

func (s *Server) createRun(ctx context.Context, spec runSpec) (int64, error) {
	title := spec.title
	if title == "" {
		title = spec.name
	}
	if spec.parseErr == nil && spec.called == nil {
		// The workflows its jobs call are read now, as on GitHub, and a run
		// that can't read them fails like one with an invalid workflow.
		if w, err := wf.Parse([]byte(spec.yaml)); err == nil {
			spec.called, spec.parseErr = s.resolveCalls(ctx, spec.repo, refProtected(spec.repo, spec.ref), spec.file, spec.sha, w)
		}
	}
	status, conclusion, errText := protocol.StatusQueued, "", ""
	finished := int64(0)
	if spec.parseErr != nil {
		status, conclusion, errText = protocol.StatusCompleted, protocol.ResultFailure, "Invalid workflow file: "+spec.parseErr.Error()
		finished = nowMS()
	}
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
	res, err := s.db.Exec(`INSERT INTO runs (repo_id, number, attempt, workflow_file, workflow_name, workflow_yaml, title, event, event_json, ref, sha, actor,
		status, conclusion, error, protected, trigger_depth, created_at, finished_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		spec.repo.ID, number, attempt, spec.file, spec.name, spec.yaml, title, spec.event, mustJSON(spec.payload), spec.ref, spec.sha, spec.actor,
		status, conclusion, errText, refProtected(spec.repo, spec.ref), spec.depth, nowMS(), finished)
	var id int64
	var conc wf.Concurrency
	if err == nil {
		id, _ = res.LastInsertId()
		if spec.parseErr == nil {
			err = s.saveCalledWorkflows(id, spec.called)
		}
	}
	if err == nil {
		// The group can use github.run_id and run_number, so it's evaluated
		// once the run has them, and set before any run can advance.
		if spec.parseErr == nil {
			conc, err = s.setRunConcurrency(id)
		}
	}
	s.mu.Unlock()
	if err != nil {
		return 0, err
	}
	s.log.Info("run created", "repo", spec.repo.Name, "run", id, "number", number, "workflow", spec.file, "event", spec.event, "ref", spec.ref,
		"secrets", refProtected(spec.repo, spec.ref), "concurrency", conc.Group)
	s.queueWorkflowRun(id, "requested")
	if run, err := s.getRun(id); err == nil && run.Status == protocol.StatusCompleted {
		// The workflow or its concurrency was invalid.
		s.queueWorkflowRun(id, "completed")
		return id, nil
	}
	if conc.Group != "" {
		s.supersede(ctx, spec.repo.ID, id, spec.event, conc)
	}
	if err := s.advance(ctx, id); err != nil {
		s.log.Error("advance", "run", id, "err", err)
	}
	if spec.parseErr != nil {
		s.reportChecks(id) // advance leaves finished runs alone
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
	ghc.SetBaseAndHeadRef() // pull_request: from the event
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

// newJob is a job row to insert.
type newJob struct {
	parentID                  int64
	key, name                 string
	matrix                    map[string]any
	runsOn                    []string
	status, conclusion, error string
	uses                      string
	inputs                    map[string]any
}

func (s *Server) insertJob(runID int64, j newJob) error {
	now := nowMS()
	finished, started := int64(0), int64(0)
	if j.status == protocol.StatusCompleted {
		finished = now
	}
	if j.uses != "" && j.status == protocol.StatusInProgress {
		started = now
	}
	if j.runsOn == nil {
		j.runsOn = []string{}
	}
	_, err := s.db.Exec(`INSERT INTO jobs (run_id, parent_id, job_key, name, uses, inputs, matrix, runs_on, status, conclusion, error,
		created_at, started_at, finished_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		runID, j.parentID, j.key, j.name, j.uses, mustJSON(j.inputs), mustJSON(j.matrix), mustJSON(j.runsOn), j.status, j.conclusion, j.error,
		now, started, finished)
	return err
}

// advance moves a run forward: it creates jobs whose needs are satisfied
// (evaluating if, matrix and runs-on), finishes calling jobs whose called
// workflow is done, and completes the run when all jobs are done.
func (s *Server) advance(ctx context.Context, runID int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.advanceLocked(ctx, runID)
}

// scope is one workflow of a run: the run's own, or a reusable workflow that
// a matrix combination of one of its jobs calls.
type scope struct {
	caller   *Job   // the calling job's row; nil for the run's own workflow
	path     string // see calledWorkflow.Path; "" for the run's own
	workflow *model.Workflow
	inputs   map[string]any
	cancel   bool // the run or a calling job was cancelled
}

func (sc *scope) parentID() int64 {
	if sc.caller == nil {
		return 0
	}
	return sc.caller.ID
}

// jobName is a job's name in the run: "caller / job" in a called workflow,
// as on GitHub.
func (sc *scope) jobName(name string) string {
	if sc.caller == nil {
		return name
	}
	return sc.caller.Name + " / " + name
}

func (s *Server) advanceLocked(ctx context.Context, runID int64) error {
	run, err := s.getRun(runID)
	if err != nil {
		return err
	}
	if run.Status == protocol.StatusCompleted {
		return nil
	}
	workflow, err := wf.Parse([]byte(run.WorkflowYAML))
	if err == nil {
		var called map[string]*calledWorkflow
		if called, err = s.calledWorkflows(runID); err == nil {
			return s.advanceWorkflows(ctx, run, workflow, called)
		}
	}
	_, err = s.db.Exec(`UPDATE runs SET status = ?, conclusion = ?, error = ?, finished_at = ? WHERE id = ?`,
		protocol.StatusCompleted, protocol.ResultFailure, "Invalid workflow file: "+err.Error(), nowMS(), runID)
	s.publishRun(runID)
	s.queueWorkflowRun(runID, "completed")
	return err
}

func (s *Server) advanceWorkflows(ctx context.Context, run *Run, workflow *model.Workflow, called map[string]*calledWorkflow) error {
	runID := run.ID
	if s.waitingForGroup(run) {
		s.publishRun(runID)
		return nil
	}
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
		tree := newJobTree(jobs)
		scopes := []*scope{{workflow: workflow, inputs: inputs, cancel: run.Cancel}}
		for _, j := range jobs {
			if j.Uses == "" || j.Status == protocol.StatusCompleted {
				continue
			}
			path := tree.path(j)
			c := called[path]
			if c == nil {
				if err := s.finishCaller(j, protocol.ResultFailure, "the called workflow is missing", nil); err != nil {
					return err
				}
				changed = true
				continue
			}
			in := j.Inputs
			if in == nil {
				in = map[string]any{}
			}
			scopes = append(scopes, &scope{caller: j, path: path, workflow: c.parsed, inputs: in, cancel: run.Cancel || tree.cancelled(j)})
		}
		for _, sc := range scopes {
			rows := tree.children[sc.parentID()]
			c, q, err := s.advanceScope(run, sc, rows, ghc, vars, called)
			if err != nil {
				return err
			}
			changed, queued = changed || c, queued || q
			if sc.caller != nil && !c {
				// Calling jobs whose workflow is done finish with its
				// result and outputs.
				if c, err = s.finishCalled(sc, rows, ghc, vars); err != nil {
					return err
				}
				changed = changed || c
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
		if j.ParentID == 0 {
			keys[j.Key] = true
		}
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
		if err == nil {
			s.queueWorkflowRun(runID, "completed")
		}
		if err == nil && run.ConcurrencyGroup != "" {
			defer s.startNextInGroup(ctx, run)
		}
	case anyStarted && run.Status == protocol.StatusQueued:
		_, err = s.db.Exec(`UPDATE runs SET status = ?, started_at = ? WHERE id = ?`, protocol.StatusInProgress, nowMS(), runID)
		if err == nil {
			s.queueWorkflowRun(runID, "in_progress")
		}
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

// advanceScope creates the jobs of one of a run's workflows whose needs are
// done. rows are the workflow's job rows so far.
func (s *Server) advanceScope(run *Run, sc *scope, rows []*Job, ghc *model.GithubContext, vars map[string]string,
	called map[string]*calledWorkflow) (changed, queued bool, err error) {
	exists := map[string]bool{}
	for _, j := range rows {
		exists[j.Key] = true
	}
	done := results(rows)
	order, _ := wf.Order(sc.workflow)
	insert := func(j newJob) error {
		j.parentID = sc.parentID()
		return s.insertJob(run.ID, j)
	}
	for _, key := range order {
		if exists[key] {
			continue
		}
		job := sc.workflow.GetJob(key)
		ready := true
		for _, n := range job.Needs() {
			if _, ok := done[n]; !ok {
				ready = false
			}
		}
		if !ready {
			continue
		}
		changed = true
		env := &wf.Env{Workflow: sc.workflow, JobID: key, Github: ghc, Vars: vars, Inputs: sc.inputs, Results: done}
		name := sc.jobName(env.Name())
		if sc.cancel {
			if err := insert(newJob{key: key, name: name, status: protocol.StatusCompleted, conclusion: protocol.ResultCancelled}); err != nil {
				return changed, queued, err
			}
			continue
		}
		ok, err := env.JobIf()
		if err != nil || !ok {
			j := newJob{key: key, name: name, status: protocol.StatusCompleted, conclusion: protocol.ResultSkipped}
			if err != nil {
				j.conclusion, j.error = protocol.ResultFailure, "if: "+err.Error()
			}
			if err := insert(j); err != nil {
				return changed, queued, err
			}
			continue
		}
		combos, err := env.Matrixes()
		if err != nil {
			if err := insert(newJob{key: key, name: name, status: protocol.StatusCompleted, conclusion: protocol.ResultFailure, error: err.Error()}); err != nil {
				return changed, queued, err
			}
			continue
		}
		for _, combo := range combos {
			if len(combo) == 0 {
				combo = nil
			}
			env.Matrix = combo
			j := newJob{key: key, name: sc.jobName(env.Name()), matrix: combo, status: protocol.StatusCompleted, conclusion: protocol.ResultFailure}
			if job.Uses != "" {
				// A calling job is in progress while the workflow it calls
				// runs; advanceWorkflows runs that as a scope of its own.
				path := key
				if sc.path != "" {
					path = sc.path + "/" + key
				}
				j.uses = job.Uses
				if c := called[path]; c == nil {
					j.error = "the called workflow wasn't read when the run was created"
				} else if j.inputs, err = env.CallInputs(c.parsed); err != nil {
					j.error = err.Error()
				} else {
					j.status, j.conclusion = protocol.StatusInProgress, ""
				}
				if err := insert(j); err != nil {
					return changed, queued, err
				}
				continue
			}
			labels, err := env.RunsOn()
			if err == nil && len(labels) == 0 {
				err = fmt.Errorf("runs-on is empty")
			}
			if err != nil {
				j.error = "runs-on: " + err.Error()
			} else if j.runsOn, j.error = labels, s.labelDenied(run, labels); j.error == "" {
				j.status, j.conclusion = protocol.StatusQueued, ""
				queued = true
			}
			if err := insert(j); err != nil {
				return changed, queued, err
			}
		}
	}
	return changed, queued, nil
}

// finishCalled finishes a calling job once every job of the workflow it
// calls is done, with their result and the workflow's outputs. It reports
// whether it did.
func (s *Server) finishCalled(sc *scope, rows []*Job, ghc *model.GithubContext, vars map[string]string) (bool, error) {
	done := results(rows)
	for key := range sc.workflow.Jobs {
		if _, ok := done[key]; !ok {
			return false, nil
		}
	}
	res, _ := aggregate(rows)
	env := &wf.Env{Workflow: sc.workflow, Github: ghc, Vars: vars, Inputs: sc.inputs, Jobs: done}
	outputs, err := env.CallOutputs()
	errText := ""
	if err != nil {
		res.Result, errText = protocol.ResultFailure, err.Error()
	}
	return true, s.finishCaller(sc.caller, res.Result, errText, outputs)
}

func (s *Server) finishCaller(j *Job, result, errText string, outputs map[string]string) error {
	if outputs == nil {
		outputs = map[string]string{}
	}
	_, err := s.db.Exec(`UPDATE jobs SET status = ?, conclusion = ?, error = ?, outputs = ?, finished_at = ? WHERE id = ?`,
		protocol.StatusCompleted, result, errText, mustJSON(outputs), nowMS(), j.ID)
	if err != nil {
		return err
	}
	s.log.Info("called workflow finished", "job", j.ID, "run", j.RunID, "name", j.Name, "result", result)
	if result == protocol.ResultFailure {
		s.failFast(j)
	}
	if fresh, err := s.getJob(j.ID); err == nil {
		s.publishJob(fresh)
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
	s.reportChecks(runID)
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

// failFast cancels the other matrix jobs of a failed job, and for a calling
// job the jobs of the workflows they call.
func (s *Server) failFast(job *Job) {
	run, err := s.getRun(job.RunID)
	if err != nil {
		return
	}
	w, err := s.jobWorkflow(run, job)
	if err != nil {
		return
	}
	def := w.GetJob(job.Key)
	if def == nil || def.Strategy == nil || !wf.FailFast(def) {
		return
	}
	rows, err := s.db.Query(`SELECT id FROM jobs WHERE run_id = ? AND parent_id = ? AND job_key = ? AND id != ? AND status != ?`,
		job.RunID, job.ParentID, job.Key, job.ID, protocol.StatusCompleted)
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
	s.cancelJobs(ids)
}

// cancelJobs cancels queued jobs and asks agents to stop running ones, and
// does the same for the jobs of the workflows calling jobs among them call.
func (s *Server) cancelJobs(ids []int64) {
	for depth := 0; len(ids) > 0 && depth <= 2*wf.MaxCallDepth; depth++ {
		var children []int64
		for _, id := range ids {
			_, _ = s.db.Exec(`UPDATE jobs SET status = ?, conclusion = ?, finished_at = ? WHERE id = ? AND status IN (?, ?)`,
				protocol.StatusCompleted, protocol.ResultCancelled, nowMS(), id, protocol.StatusQueued, protocol.StatusPending)
			_, _ = s.db.Exec(`UPDATE jobs SET cancel_requested = 1 WHERE id = ? AND status = ?`, id, protocol.StatusInProgress)
			rows, err := s.db.Query(`SELECT id FROM jobs WHERE parent_id = ? AND status != ?`, id, protocol.StatusCompleted)
			if err != nil {
				continue
			}
			for rows.Next() {
				var c int64
				_ = rows.Scan(&c)
				children = append(children, c)
			}
			rows.Close()
		}
		ids = children
	}
}

// jobWorkflow returns the workflow a job row belongs to: the run's, or the
// reusable workflow its calling job calls.
func (s *Server) jobWorkflow(run *Run, job *Job) (*model.Workflow, error) {
	if job.ParentID == 0 {
		return wf.Parse([]byte(run.WorkflowYAML))
	}
	path := ""
	for p, depth := job, 0; p.ParentID != 0 && depth < 2*wf.MaxCallDepth; depth++ {
		parent, err := s.getJob(p.ParentID)
		if err != nil {
			return nil, err
		}
		path = strings.TrimSuffix(parent.Key+"/"+path, "/")
		p = parent
	}
	var src string
	if err := s.db.QueryRow(`SELECT yaml FROM run_workflows WHERE run_id = ? AND path = ?`, run.ID, path).Scan(&src); err != nil {
		return nil, fmt.Errorf("called workflow %s: %w", path, err)
	}
	return wf.Parse([]byte(src))
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
		s.failFast(job)
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
	rows, err := s.db.Query(`SELECT id FROM jobs WHERE status = ? AND heartbeat_at < ? AND uses = ''`, protocol.StatusInProgress, cutoff)
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
