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

// setRunConcurrency evaluates a new run's concurrency and stores its group,
// or fails the run if the concurrency is invalid. s.mu is held.
func (s *Server) setRunConcurrency(runID int64) (wf.Concurrency, error) {
	run, err := s.getRun(runID)
	if err != nil {
		return wf.Concurrency{}, err
	}
	w, err := wf.Parse([]byte(run.WorkflowYAML))
	if err != nil {
		return wf.Concurrency{}, nil // advance reports it
	}
	inputs, _ := run.EventJSON["inputs"].(map[string]any)
	env := &wf.Env{Workflow: w, Github: s.githubContext(run), Vars: s.varsFor(run.RepoID, nil), Inputs: inputs}
	c, cerr := wf.WorkflowConcurrency([]byte(run.WorkflowYAML), env)
	if cerr != nil {
		_, err = s.db.Exec(`UPDATE runs SET status = ?, conclusion = ?, error = ?, finished_at = ? WHERE id = ?`,
			protocol.StatusCompleted, protocol.ResultFailure, "Invalid workflow file: "+cerr.Error(), nowMS(), runID)
		return wf.Concurrency{}, err
	}
	if c.Group != "" {
		_, err = s.db.Exec(`UPDATE runs SET concurrency_group = ? WHERE id = ?`, c.Group, runID)
	}
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
