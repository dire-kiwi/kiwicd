package server

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"maps"
	"slices"
	"sync"
	"time"
	_ "time/tzdata" // for schedules' timezone, on hosts without zoneinfo

	"github.com/dire-kiwi/kiwicd/internal/wf"
)

// Scheduled workflows (`on: schedule`) run as on GitHub: workflows
// (committed and uploaded) are read from the repository's default branch,
// and each slot of one of their cron entries starts a run of its latest
// commit, with github.event.schedule set to the cron string.
//
// The table schedules holds each entry's last fired slot, so a restart
// neither fires a slot twice nor forgets one that came due while kiwicd was
// down. After a longer outage, only the latest missed slot runs. The entries
// themselves are kept in memory, and a repository's are read again when its
// default branch moves, its uploaded workflows change or its settings do.

type scheduleEntry struct {
	file  string
	sched wf.Schedule
	cron  *wf.CronSchedule // nil if the entry is invalid
}

type scheduleState struct {
	mu      sync.Mutex
	entries map[int64][]scheduleEntry // by repository ID
	dirty   map[int64]bool            // repositories to read again
	all     bool                      // read every repository again
	wake    chan struct{}
	now     func() time.Time // the clock; tests replace it
}

func newScheduleState() scheduleState {
	return scheduleState{entries: map[int64][]scheduleEntry{}, dirty: map[int64]bool{}, all: true, wake: make(chan struct{}, 1), now: time.Now}
}

// schedulesChanged has the scheduler read a repository's schedules again.
func (s *Server) schedulesChanged(repoID int64) {
	s.sched.mu.Lock()
	if s.sched.dirty == nil {
		s.sched.dirty = map[int64]bool{}
	}
	s.sched.dirty[repoID] = true
	s.sched.mu.Unlock()
	select {
	case s.sched.wake <- struct{}{}:
	default:
	}
}

// scheduleLoop starts scheduled runs until ctx is done.
func (s *Server) scheduleLoop(ctx context.Context) {
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		case <-s.sched.wake:
		}
		now := s.sched.now()
		next := s.runSchedules(ctx, now)
		// Look again at least every minute, in case the clock jumps.
		wait := time.Minute
		if !next.IsZero() {
			wait = min(wait, max(next.Sub(now), 0))
		}
		timer.Reset(wait)
	}
}

// runSchedules reads changed repositories' schedules, starts the runs of the
// slots due at now and returns when the next slot is due (zero if none is).
func (s *Server) runSchedules(ctx context.Context, now time.Time) time.Time {
	s.loadSchedules(ctx, now)
	state, err := s.scheduleRows()
	if err != nil {
		s.log.Error("schedules", "err", err)
		return time.Time{}
	}
	s.sched.mu.Lock()
	entries := maps.Clone(s.sched.entries)
	s.sched.mu.Unlock()
	var next time.Time
	for _, repoID := range sortedIDs(entries) {
		for _, e := range entries[repoID] {
			row, ok := state[scheduleKey{repoID, e.file, e.sched}]
			if e.cron == nil || !ok {
				continue
			}
			due := e.cron.Due(row.since, row.last)
			if due.IsZero() {
				continue // never, like February 30
			}
			if !due.After(now) {
				slot := e.cron.Latest(due, now)
				s.fireSchedule(ctx, repoID, e, row.last, slot)
				due = e.cron.Due(row.since, slot)
			}
			if !due.IsZero() && (next.IsZero() || due.Before(next)) {
				next = due
			}
		}
	}
	return next
}

type scheduleKey struct {
	repoID int64
	file   string
	sched  wf.Schedule
}

type scheduleRow struct{ since, last time.Time }

func msTime(ms int64) time.Time {
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms)
}

func (s *Server) scheduleRows() (map[scheduleKey]scheduleRow, error) {
	rows, err := s.db.Query(`SELECT repo_id, workflow_file, cron, timezone, created_at, last_slot FROM schedules`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[scheduleKey]scheduleRow{}
	for rows.Next() {
		var k scheduleKey
		var since, last int64
		if err := rows.Scan(&k.repoID, &k.file, &k.sched.Cron, &k.sched.Timezone, &since, &last); err != nil {
			return nil, err
		}
		out[k] = scheduleRow{msTime(since), msTime(last)}
	}
	return out, rows.Err()
}

// loadSchedules reads the schedules of the repositories that changed (all of
// them the first time) and records new entries as seen at now: slots before
// then don't run.
func (s *Server) loadSchedules(ctx context.Context, now time.Time) {
	s.sched.mu.Lock()
	all, dirty := s.sched.all, s.sched.dirty
	s.sched.all, s.sched.dirty = false, map[int64]bool{}
	s.sched.mu.Unlock()
	if all {
		ids, err := s.repoIDs()
		if err != nil {
			s.log.Error("schedules: list repositories", "err", err)
			s.sched.mu.Lock()
			s.sched.all = true
			s.sched.mu.Unlock()
			return
		}
		for _, id := range ids {
			dirty[id] = true
		}
		// Forget repositories that are gone.
		_, _ = s.db.Exec(`DELETE FROM schedules WHERE repo_id NOT IN (SELECT id FROM repos)`)
	}
	for id := range dirty {
		entries, err := s.repoSchedules(ctx, id)
		if errors.Is(err, sql.ErrNoRows) {
			s.sched.mu.Lock()
			delete(s.sched.entries, id)
			s.sched.mu.Unlock()
			continue // its rows went with it
		}
		if err == nil {
			err = s.saveSchedules(id, entries, now)
		}
		if err != nil {
			// Keep the entries it had; the next change reads it again.
			s.log.Error("schedules", "repo", id, "err", err)
			continue
		}
		s.sched.mu.Lock()
		s.sched.entries[id] = entries
		s.sched.mu.Unlock()
	}
}

func (s *Server) repoIDs() ([]int64, error) {
	rows, err := s.db.Query(`SELECT id FROM repos ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// repoSchedules returns the schedule entries of the workflows on a
// repository's default branch. One without commits has none: there would
// be nothing to run.
func (s *Server) repoSchedules(ctx context.Context, id int64) ([]scheduleEntry, error) {
	repo, err := s.getRepoByID(id)
	if err != nil {
		return nil, err
	}
	sha, err := resolveRef(ctx, s.repoPath(repo.Name), "refs/heads/"+repo.DefaultBranch)
	if err != nil {
		return nil, nil
	}
	files, err := s.defaultBranchWorkflows(ctx, repo, sha)
	if err != nil {
		return nil, err
	}
	var out []scheduleEntry
	for _, file := range sortedKeys(files) {
		for _, sch := range fileSchedules(files[file]) {
			c, _ := sch.Parse()
			out = append(out, scheduleEntry{file: file, sched: sch, cron: c})
		}
	}
	return out, nil
}

// fileSchedules returns a workflow file's schedule entries; none if it's invalid.
func fileSchedules(src []byte) []wf.Schedule {
	if !bytes.Contains(src, []byte("schedule")) {
		return nil // only schedules need parsing
	}
	w, err := wf.Parse(src)
	if err != nil {
		return nil
	}
	return wf.Schedules(w)
}

// saveSchedules makes a repository's rows in schedules match its entries:
// new ones are seen at now, and removed ones forget when they last ran.
func (s *Server) saveSchedules(repoID int64, entries []scheduleEntry, now time.Time) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	keep := map[scheduleKey]bool{}
	for _, e := range entries {
		keep[scheduleKey{repoID, e.file, e.sched}] = true
		if _, err := tx.Exec(`INSERT OR IGNORE INTO schedules (repo_id, workflow_file, cron, timezone, created_at) VALUES (?, ?, ?, ?, ?)`,
			repoID, e.file, e.sched.Cron, e.sched.Timezone, now.UnixMilli()); err != nil {
			return err
		}
	}
	rows, err := tx.Query(`SELECT workflow_file, cron, timezone FROM schedules WHERE repo_id = ?`, repoID)
	if err != nil {
		return err
	}
	var gone []scheduleKey
	for rows.Next() {
		k := scheduleKey{repoID: repoID}
		if err := rows.Scan(&k.file, &k.sched.Cron, &k.sched.Timezone); err != nil {
			rows.Close()
			return err
		}
		if !keep[k] {
			gone = append(gone, k)
		}
	}
	rows.Close()
	for _, k := range gone {
		if _, err := tx.Exec(`DELETE FROM schedules WHERE repo_id = ? AND workflow_file = ? AND cron = ? AND timezone = ?`,
			repoID, k.file, k.sched.Cron, k.sched.Timezone); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// fireSchedule starts the run of an entry's slot. The slot is recorded
// first, so a crash in between skips it rather than running it twice.
func (s *Server) fireSchedule(ctx context.Context, repoID int64, e scheduleEntry, last, slot time.Time) {
	lastMS := int64(0)
	if !last.IsZero() {
		lastMS = last.UnixMilli()
	}
	res, err := s.db.Exec(`UPDATE schedules SET last_slot = ? WHERE repo_id = ? AND workflow_file = ? AND cron = ? AND timezone = ? AND last_slot = ?`,
		slot.UnixMilli(), repoID, e.file, e.sched.Cron, e.sched.Timezone, lastMS)
	if err != nil {
		s.log.Error("schedule", "repo", repoID, "workflow", e.file, "err", err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return // removed, or fired already
	}
	if err := s.startScheduledRun(ctx, repoID, e); err != nil {
		s.log.Error("scheduled run", "repo", repoID, "workflow", e.file, "cron", e.sched.Cron, "err", err)
	}
}

// startScheduledRun runs an entry's workflow at the latest commit of the
// default branch, if the workflow there still has the entry.
func (s *Server) startScheduledRun(ctx context.Context, repoID int64, e scheduleEntry) error {
	repo, err := s.getRepoByID(repoID)
	if err != nil {
		return err
	}
	path := s.repoPath(repo.Name)
	ref := "refs/heads/" + repo.DefaultBranch
	sha, err := resolveRef(ctx, path, ref)
	if err != nil {
		return err
	}
	files, err := s.defaultBranchWorkflows(ctx, repo, sha)
	if err != nil {
		return err
	}
	src := files[e.file]
	w, err := wf.Parse(src)
	if err != nil || !slices.Contains(wf.Schedules(w), e.sched) {
		// It changed since the entries were read; read them again.
		s.schedulesChanged(repoID)
		return nil
	}
	name := w.Name
	if name == "" {
		name = e.file
	}
	commit := readCommit(ctx, path, sha)
	event := map[string]any{
		"schedule":   e.sched.Cron,
		"workflow":   e.file,
		"repository": repoEventInfo(repo, s.cfg.PublicURL),
		"sender":     map[string]any{"login": commit.Author},
	}
	_, err = s.createRun(ctx, runSpec{repo: repo, file: e.file, yaml: string(src), name: name, event: "schedule",
		payload: event, ref: ref, sha: sha, actor: commit.Author, title: "Scheduled: " + commit.Subject})
	return err
}

// scheduleInfo describes a workflow's schedule entry for the UI.
type scheduleInfo struct {
	Cron     string `json:"cron"`
	Timezone string `json:"timezone,omitempty"`
	// When it next runs; 0 if it doesn't, because the workflow isn't on the
	// default branch or the entry is invalid.
	NextRunAt int64  `json:"next_run_at,omitempty"`
	Error     string `json:"error,omitempty"`
}

// workflowSchedules describes a workflow's schedule entries. onDefault says
// whether it's the workflow on the default branch, the one that runs them.
func (s *Server) workflowSchedules(repoID int64, file string, list []wf.Schedule, onDefault bool, now time.Time) []scheduleInfo {
	out := []scheduleInfo{}
	for _, sch := range list {
		info := scheduleInfo{Cron: sch.Cron, Timezone: sch.Timezone}
		c, err := sch.Parse()
		if err != nil {
			info.Error = err.Error()
		} else if onDefault {
			var since, last int64
			err := s.db.QueryRow(`SELECT created_at, last_slot FROM schedules WHERE repo_id = ? AND workflow_file = ? AND cron = ? AND timezone = ?`,
				repoID, file, sch.Cron, sch.Timezone).Scan(&since, &last)
			due := c.Next(now) // not read yet: it's about to be
			if err == nil {
				due = c.Due(msTime(since), msTime(last))
			}
			if due.Before(now) {
				due = now // due, and about to run
			}
			if !due.IsZero() {
				info.NextRunAt = due.UnixMilli()
			}
		}
		out = append(out, info)
	}
	return out
}

func sortedIDs[V any](m map[int64]V) []int64 {
	ids := make([]int64, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}
