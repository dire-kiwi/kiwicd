package server

import (
	"context"
	"fmt"
	"time"

	"github.com/dire-kiwi/kiwicd/internal/protocol"
)

// Deletion works in small transactions: the database has one connection, and
// holding it for seconds would delay agent heartbeats past reapLostJobs' cutoff.
const (
	logDeleteBatch = 5000 // log lines per transaction
	vacuumStep     = 1000 // pages released per incremental_vacuum
	pruneInterval  = time.Hour
)

// pruneLoop deletes runs older than the retention period, at startup and then
// hourly.
func (s *Server) pruneLoop(ctx context.Context) {
	if s.cfg.RetentionDays <= 0 {
		return
	}
	t := time.NewTicker(pruneInterval)
	defer t.Stop()
	for {
		cutoff := time.Now().Add(-time.Duration(s.cfg.RetentionDays) * 24 * time.Hour)
		if n, err := s.pruneRuns(ctx, cutoff); err != nil {
			s.log.Error("prune runs", "err", err)
		} else if n > 0 {
			s.log.Info("pruned old runs", "runs", n, "older_than", cutoff.Format(time.DateOnly))
		}
		if err := s.incrementalVacuum(ctx); err != nil {
			s.log.Error("incremental vacuum", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// pruneRuns deletes completed runs that finished before cutoff, one at a time.
// Queued and in-progress runs are kept however old they are.
func (s *Server) pruneRuns(ctx context.Context, cutoff time.Time) (int, error) {
	n := 0
	for {
		rows, err := s.db.QueryContext(ctx, `SELECT id FROM runs WHERE status = ? AND finished_at < ? ORDER BY id LIMIT 100`,
			protocol.StatusCompleted, cutoff.UnixMilli())
		if err != nil {
			return n, err
		}
		var ids []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return n, err
			}
			ids = append(ids, id)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return n, err
		}
		if len(ids) == 0 {
			return n, nil
		}
		for _, id := range ids {
			if err := s.deleteRun(ctx, id); err != nil {
				return n, err
			}
			n++
		}
	}
}

// deleteRun deletes a run with its jobs and their logs. All run deletion goes
// through here, so anything stored outside the database for a job, such as
// its log archive, is removed here too.
func (s *Server) deleteRun(ctx context.Context, runID int64) error {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM jobs WHERE run_id = ?`, runID)
	if err != nil {
		return err
	}
	var jobIDs []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		jobIDs = append(jobIDs, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, id := range jobIDs {
		if err := s.deleteJobLogs(ctx, id); err != nil {
			return err
		}
	}
	// The logs are gone, so cascading to the jobs is cheap.
	if _, err := s.db.ExecContext(ctx, `DELETE FROM runs WHERE id = ?`, runID); err != nil {
		return err
	}
	s.removeLogArchives(jobIDs)
	return nil
}

// deleteJobLogs deletes a job's log lines, a batch per transaction.
func (s *Server) deleteJobLogs(ctx context.Context, jobID int64) error {
	for {
		res, err := s.db.ExecContext(ctx, `DELETE FROM log_lines WHERE job_id = ? AND seq IN
			(SELECT seq FROM log_lines WHERE job_id = ? ORDER BY seq LIMIT ?)`, jobID, jobID, logDeleteBatch)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n < logDeleteBatch {
			return nil
		}
	}
}

// incrementalVacuum returns free pages to the filesystem a step at a time.
func (s *Server) incrementalVacuum(ctx context.Context) error {
	last := int64(-1)
	for {
		var free int64
		if err := s.db.QueryRowContext(ctx, `PRAGMA freelist_count`).Scan(&free); err != nil {
			return err
		}
		// Stop when nothing is left, or nothing was released (auto_vacuum
		// is not incremental).
		if free == 0 || free == last {
			return nil
		}
		last = free
		if _, err := s.db.ExecContext(ctx, fmt.Sprintf(`PRAGMA incremental_vacuum(%d)`, vacuumStep)); err != nil {
			return err
		}
	}
}
