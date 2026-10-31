package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/dire-kiwi/kiwicd/internal/protocol"
	"github.com/dire-kiwi/kiwicd/internal/wf"
)

// A re-run of some of a run's jobs is a new attempt that keeps the other
// jobs: their results, outputs, steps and logs are copied into it, so jobs
// that need them see the same needs context. What it keeps is decided when
// the re-run is made (rerunPlan) and carried out when the attempt starts
// (reuseJobs), which may be later if it waits for its concurrency group.

// reusePlan is stored in runs.reuse_jobs: IDs of the earlier attempt's jobs
// to copy as they are, and of matrix jobs to queue again, while the rest of
// their matrix is kept.
type reusePlan struct {
	Keep    []int64 `json:"keep,omitempty"`
	Requeue []int64 `json:"requeue,omitempty"`
}

var errNothingToRerun = errors.New("no job of this run failed or was cancelled")

// rerunPlan works out what re-running some of run's jobs keeps. With
// failedOnly it re-runs the jobs that failed or were cancelled; otherwise the
// job jobID. Either way the jobs that need them, directly or not, run again
// too, as on GitHub. A matrix job runs again only in the combinations that
// failed (or the one picked) unless the jobs it needs run again as well. A
// job that calls a reusable workflow is kept or runs again whole, with its
// called workflow's jobs; picking one of those picks the job that calls it.
func rerunPlan(run *Run, jobs []*Job, failedOnly bool, jobID int64) (*reusePlan, error) {
	workflow, err := wf.Parse([]byte(run.WorkflowYAML))
	if err != nil {
		return nil, fmt.Errorf("the workflow file is invalid: %w", err)
	}
	order, err := wf.Order(workflow)
	if err != nil {
		return nil, err
	}
	byID := map[int64]*Job{}
	for _, j := range jobs {
		byID[j.ID] = j
	}
	if j := byID[jobID]; j != nil {
		for j.ParentID != 0 && byID[j.ParentID] != nil {
			j = byID[j.ParentID]
		}
		jobID = j.ID
	}
	byKey := map[string][]*Job{}
	for _, j := range jobs {
		if j.ParentID == 0 {
			byKey[j.Key] = append(byKey[j.Key], j)
		}
	}
	// again picks the rows of a job to run again.
	again := func(j *Job) bool {
		if failedOnly {
			return j.Conclusion == protocol.ResultFailure || j.Conclusion == protocol.ResultCancelled
		}
		return j.ID == jobID
	}
	seeds := map[string]bool{}
	for key, rows := range byKey {
		for _, j := range rows {
			if again(j) {
				seeds[key] = true
			}
		}
	}
	if len(seeds) == 0 {
		if failedOnly {
			return nil, errNothingToRerun
		}
		return nil, fmt.Errorf("job %d is not part of this run", jobID)
	}
	// Jobs run again whole: those that need one that runs again.
	whole := map[string]bool{}
	for _, key := range order {
		for _, n := range workflow.GetJob(key).Needs() {
			if seeds[n] || whole[n] {
				whole[key] = true
			}
		}
	}
	plan := &reusePlan{}
	for _, key := range order {
		rows := byKey[key]
		if whole[key] || len(rows) == 0 {
			continue
		}
		if !seeds[key] {
			for _, j := range rows {
				plan.Keep = append(plan.Keep, j.ID)
			}
			continue
		}
		// Part of a matrix runs again, if it ran on runners before: rows the
		// server failed itself (a bad runs-on, say) are made again whole.
		var keep, requeue []int64
		partial := len(rows) > 1
		for _, j := range rows {
			if !again(j) {
				keep = append(keep, j.ID)
			} else if len(j.RunsOn) == 0 || j.Uses != "" {
				partial = false
			} else {
				requeue = append(requeue, j.ID)
			}
		}
		if partial && len(keep) > 0 {
			plan.Keep = append(plan.Keep, keep...)
			plan.Requeue = append(plan.Requeue, requeue...)
		}
	}
	return plan, nil
}

// reuseJobs carries out a re-run's plan when it starts: it copies the jobs it
// keeps from the earlier attempt (with the jobs of the workflows they call)
// and queues the matrix jobs it runs again.
// It reports whether it queued any. s.mu is held, and the run has no jobs
// yet. If the earlier attempt is gone (pruned), every job runs again.
func (s *Server) reuseJobs(run *Run, planJSON string) (bool, error) {
	var plan reusePlan
	if err := json.Unmarshal([]byte(planJSON), &plan); err != nil {
		return false, fmt.Errorf("re-run plan: %w", err)
	}
	load := func(ids []int64) ([]*Job, bool) {
		var out []*Job
		for _, id := range ids {
			j, err := s.getJob(id)
			if err != nil {
				return nil, false
			}
			out = append(out, j)
		}
		return out, true
	}
	keep, ok1 := load(plan.Keep)
	requeue, ok2 := load(plan.Requeue)
	if !ok1 || !ok2 {
		s.log.Warn("the attempt a re-run keeps jobs from is gone; running every job again", "run", run.ID)
		return false, nil
	}
	for _, j := range keep {
		if err := s.copyJob(run.ID, 0, j); err != nil {
			return false, err
		}
	}
	for _, j := range requeue {
		if err := s.insertJob(run.ID, newJob{key: j.Key, name: j.Name, matrix: j.Matrix, runsOn: j.RunsOn, status: protocol.StatusQueued}); err != nil {
			return false, err
		}
	}
	return len(requeue) > 0, nil
}

// copyJob copies a finished job, with its log, into the run runID, under
// the job parentID (0 for one at the top), and then the jobs of the workflow
// it calls, if it calls one.
func (s *Server) copyJob(runID, parentID int64, j *Job) error {
	from, attempt := j.ID, int64(0)
	if j.CopiedFrom != 0 {
		// A copy of a copy: point at the attempt that ran it.
		from, attempt = j.CopiedFrom, j.CopiedAttempt
	} else if err := s.db.QueryRow(`SELECT attempt FROM runs WHERE id = ?`, j.RunID).Scan(&attempt); err != nil {
		return err
	}
	res, err := s.db.Exec(`INSERT INTO jobs (run_id, parent_id, job_key, name, uses, inputs, matrix, runs_on, status, conclusion, error,
		runner_id, runner_name, outputs, steps, log_seq, log_bytes, log_truncated, created_at, started_at, finished_at, copied_from, copied_attempt)
		SELECT ?, ?, job_key, name, uses, inputs, matrix, runs_on, status, conclusion, error,
		runner_id, runner_name, outputs, steps, log_seq, log_bytes, log_truncated, created_at, started_at, finished_at, ?, ? FROM jobs WHERE id = ?`,
		runID, parentID, from, attempt, j.ID)
	if err != nil {
		return err
	}
	id, _ := res.LastInsertId()
	if err := s.copyJobLogs(j.ID, id); err != nil {
		return err
	}
	rows, err := s.db.Query(`SELECT `+jobCols+` FROM jobs WHERE parent_id = ? ORDER BY id`, j.ID)
	if err != nil {
		return err
	}
	var children []*Job
	for rows.Next() {
		c, err := scanJob(rows)
		if err != nil {
			rows.Close()
			return err
		}
		children = append(children, c)
	}
	rows.Close()
	for _, c := range children {
		if err := s.copyJob(runID, id, c); err != nil {
			return err
		}
	}
	return nil
}

// copyJobLogs gives job to the log of job from: a link to (or else a copy
// of) its archive, or a copy of its lines if it isn't archived.
func (s *Server) copyJobLogs(from, to int64) error {
	s.archiveMu.Lock()
	defer s.archiveMu.Unlock()
	var archive string
	if err := s.db.QueryRow(`SELECT log_archive FROM jobs WHERE id = ?`, from).Scan(&archive); err != nil {
		return err
	}
	if archive == "" {
		if _, err := s.db.Exec(`INSERT INTO log_lines (job_id, seq, step, ts, text) SELECT ?, seq, step, ts, text FROM log_lines WHERE job_id = ?`, to, from); err != nil {
			return err
		}
		// Once this returns and archiveMu is free.
		go func() {
			if err := s.archiveJobLogs(to); err != nil {
				s.log.Error("archive job logs", "job", to, "err", err)
			}
		}()
		return nil
	}
	src, dst := s.logArchivePath(from), s.logArchivePath(to)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if err := os.Link(src, dst); err != nil {
		if err := copyFile(src, dst); err != nil {
			// Not worth failing the re-run for: the job shows no log.
			s.log.Warn("copy job log archive", "from", from, "to", to, "err", err)
			return nil
		}
	}
	_, err := s.db.Exec(`UPDATE jobs SET log_archive = ? WHERE id = ?`, archive, to)
	return err
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.CreateTemp(filepath.Dir(dst), ".copy-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(out.Name()) // fails harmlessly once renamed
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Rename(out.Name(), dst)
}
