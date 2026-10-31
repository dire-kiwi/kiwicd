package server

import (
	"context"

	"github.com/dire-kiwi/kiwicd/internal/protocol"
	"github.com/dire-kiwi/kiwicd/internal/wf"
)

// A workflow's top-level concurrency puts its runs in a group of their
// repository, of which only one runs at a time, as on GitHub:
//
//	concurrency:
//	  group: pr-${{ github.event.number }}
//	  cancel-in-progress: true
//
// A new run in a group waits until the group's earlier runs have finished,
// and cancels those that are still waiting, so at most one waits. With
// cancel-in-progress it cancels the running one too. A run holds its group
// from when its first jobs are created; the group is evaluated once, when the
// run is created, and stored with it. That keeps runs for one pull request in
// order: a preview removed on close isn't brought back by a slower deploy of
// an earlier push.

// evaluateRun evaluates what a new run takes from its workflow before any
// job: the concurrency group, run-name and permissions. It stores them, or
// fails the run if one is invalid. s.mu is held.
func (s *Server) evaluateRun(runID int64) (wf.Concurrency, error) {
	run, err := s.getRun(runID)
	if err != nil {
		return wf.Concurrency{}, err
	}
	w, err := wf.Parse([]byte(run.WorkflowYAML))
	if err != nil {
		return wf.Concurrency{}, nil // advance reports it
	}
	x, err := wf.ReadExtras([]byte(run.WorkflowYAML))
	if err != nil {
		return wf.Concurrency{}, nil
	}
	inputs, _ := run.EventJSON["inputs"].(map[string]any)
	env := &wf.Env{Workflow: w, Github: s.githubContext(run), Vars: s.varsFor(run.RepoID), Inputs: inputs}
	title, terr := env.RunName(x)
	c, cerr := env.EvalConcurrency(x.Concurrency)
	if cerr == nil {
		cerr = terr
	}
	if cerr != nil {
		_, err = s.db.Exec(`UPDATE runs SET status = ?, conclusion = ?, error = ?, finished_at = ? WHERE id = ?`,
			protocol.StatusCompleted, protocol.ResultFailure, "Invalid workflow file: "+cerr.Error(), nowMS(), runID)
		return wf.Concurrency{}, err
	}
	if title == "" {
		title = run.Title
	}
	_, err = s.db.Exec(`UPDATE runs SET concurrency_group = ?, title = ?, permissions = ? WHERE id = ?`,
		c.Group, title, mustJSON(x.WorkflowPermissions()), runID)
	return c, err
}

// supersede cancels the earlier runs of the new run's group that are waiting, and
// with cancel-in-progress the running one too.
func (s *Server) supersede(ctx context.Context, repoID, newRun int64, c wf.Concurrency) {
	rows, err := s.db.Query(`SELECT r.id, EXISTS (SELECT 1 FROM jobs j WHERE j.run_id = r.id) FROM runs r
		WHERE r.repo_id = ? AND r.concurrency_group = ? AND r.id < ? AND r.status != ?`, repoID, c.Group, newRun, protocol.StatusCompleted)
	if err != nil {
		s.log.Error("concurrency group", "run", newRun, "err", err)
		return
	}
	var cancel []int64
	for rows.Next() {
		var id int64
		var started bool
		if rows.Scan(&id, &started) == nil && (!started || c.CancelInProgress) {
			cancel = append(cancel, id)
		}
	}
	rows.Close()
	for _, id := range cancel {
		s.log.Info("cancelling a run superseded in its concurrency group", "run", id, "by", newRun, "group", c.Group)
		if err := s.cancelRun(ctx, id); err != nil {
			s.log.Error("cancel superseded run", "run", id, "err", err)
		}
	}
}

// waitingForGroup reports whether run hasn't started and an earlier run of
// its group hasn't finished. A cancelled run doesn't wait: its jobs are all
// cancelled at once.
func (s *Server) waitingForGroup(run *Run) bool {
	if run.ConcurrencyGroup == "" || run.Cancel {
		return false
	}
	var waiting bool
	err := s.db.QueryRow(`SELECT NOT EXISTS (SELECT 1 FROM jobs WHERE run_id = ?) AND EXISTS (SELECT 1 FROM runs
		WHERE repo_id = ? AND concurrency_group = ? AND id < ? AND status != ?)`,
		run.ID, run.RepoID, run.ConcurrencyGroup, run.ID, protocol.StatusCompleted).Scan(&waiting)
	return err == nil && waiting
}

// startNextInGroup starts the run waiting in a finished run's group. s.mu is
// held.
func (s *Server) startNextInGroup(ctx context.Context, done *Run) {
	var next int64
	err := s.db.QueryRow(`SELECT r.id FROM runs r WHERE r.repo_id = ? AND r.concurrency_group = ? AND r.status != ?
		AND NOT EXISTS (SELECT 1 FROM jobs j WHERE j.run_id = r.id) ORDER BY r.id LIMIT 1`,
		done.RepoID, done.ConcurrencyGroup, protocol.StatusCompleted).Scan(&next)
	if err != nil {
		return // none waiting
	}
	if err := s.advanceLocked(ctx, next); err != nil {
		s.log.Error("advance", "run", next, "err", err)
	}
}

// A job's concurrency (jobs.<id>.concurrency) works the same way within the
// repository's jobs: a job whose group has a queued or running job waits as
// pending, a new job cancels the group's pending one, and with
// cancel-in-progress the queued and running ones too. Job groups are apart
// from workflow groups: a job doesn't wait for a run of the same name. The
// group is evaluated when the job is created, so it can use needs and matrix.

// enterJobGroup makes room in a new job's group and returns the status the
// job starts with, and the runs whose jobs it cancelled. s.mu is held.
func (s *Server) enterJobGroup(repoID int64, c wf.Concurrency) (string, []int64, error) {
	rows, err := s.db.Query(`SELECT j.id, j.run_id, j.status FROM jobs j JOIN runs r ON r.id = j.run_id
		WHERE r.repo_id = ? AND j.concurrency_group = ? AND j.status != ?`, repoID, c.Group, protocol.StatusCompleted)
	if err != nil {
		return "", nil, err
	}
	type member struct {
		id, run int64
		status  string
	}
	var members []member
	for rows.Next() {
		var m member
		if err := rows.Scan(&m.id, &m.run, &m.status); err != nil {
			rows.Close()
			return "", nil, err
		}
		members = append(members, m)
	}
	rows.Close()
	status := protocol.StatusQueued
	var affected []int64
	for _, m := range members {
		switch {
		case m.status == protocol.StatusPending || (m.status == protocol.StatusQueued && c.CancelInProgress):
			s.log.Info("cancelling a job superseded in its concurrency group", "job", m.id, "run", m.run, "group", c.Group)
			if _, err := s.db.Exec(`UPDATE jobs SET status = ?, conclusion = ?, finished_at = ? WHERE id = ?`,
				protocol.StatusCompleted, protocol.ResultCancelled, nowMS(), m.id); err != nil {
				return "", nil, err
			}
			affected = append(affected, m.run)
		case m.status == protocol.StatusInProgress && c.CancelInProgress:
			s.log.Info("cancelling a job superseded in its concurrency group", "job", m.id, "run", m.run, "group", c.Group)
			if _, err := s.db.Exec(`UPDATE jobs SET cancel_requested = 1 WHERE id = ?`, m.id); err != nil {
				return "", nil, err
			}
			status = protocol.StatusPending // until it stops
		default:
			status = protocol.StatusPending
		}
		if j, err := s.getJob(m.id); err == nil {
			s.publishJob(j)
		}
	}
	return status, affected, nil
}

// startWaitingJobs queues the pending job of each of the repository's job
// groups that have no queued or running job. It reports whether it queued
// any. s.mu is held.
func (s *Server) startWaitingJobs(repoID int64) bool {
	rows, err := s.db.Query(`SELECT j.id, j.concurrency_group FROM jobs j JOIN runs r ON r.id = j.run_id
		WHERE r.repo_id = ? AND j.status = ? AND j.concurrency_group != '' AND NOT EXISTS (SELECT 1 FROM jobs k JOIN runs q ON q.id = k.run_id
			WHERE q.repo_id = r.repo_id AND k.concurrency_group = j.concurrency_group AND k.status IN (?, ?))
		ORDER BY j.id`, repoID, protocol.StatusPending, protocol.StatusQueued, protocol.StatusInProgress)
	if err != nil {
		s.log.Error("job concurrency groups", "err", err)
		return false
	}
	var start []int64
	seen := map[string]bool{}
	for rows.Next() {
		var id int64
		var group string
		if rows.Scan(&id, &group) == nil && !seen[group] {
			seen[group] = true
			start = append(start, id)
		}
	}
	rows.Close()
	for _, id := range start {
		if _, err := s.db.Exec(`UPDATE jobs SET status = ? WHERE id = ? AND status = ?`, protocol.StatusQueued, id, protocol.StatusPending); err != nil {
			s.log.Error("start waiting job", "job", id, "err", err)
			continue
		}
		if j, err := s.getJob(id); err == nil {
			s.publishJob(j)
		}
	}
	return len(start) > 0
}
