package server

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/dire-kiwi/kiwicd/internal/protocol"
)

// A pull request from a fork runs the fork's code on the server's runners,
// so with fork_pull_requests on, the repository's fork_approval says which of
// them wait for someone to approve them first, like GitHub's "approve and
// run":
//
//   - first_time (the default): authors who haven't contributed to the
//     repository before (GitHub's author_association isn't OWNER, MEMBER,
//     COLLABORATOR or CONTRIBUTOR) and haven't had a run approved here.
//     Approving a run remembers its author, whose later pull requests run
//     straight away.
//   - outside: every author who isn't an owner, member or collaborator,
//     every time.
//   - none: none wait.
//
// Pull requests from the repository's own branches never wait: their authors
// can push to it. A waiting run has status waiting and no jobs; approving it
// queues it with the other waiting runs of the same pull request. Until then
// it isn't in its concurrency group (its workflow may be anyone's, and mustn't
// cancel or hold up other runs), but a newer run of the group cancels it as
// it would any run that hasn't started. A newer event of the same pull
// request replaces its waiting runs of the same workflow, closing the pull
// request cancels them, and runs that wait longer than approvalExpiry are
// cancelled.

const (
	approvalRequired = "required"
	approvalApproved = "approved"

	forkApprovalFirstTime = "first_time"
	forkApprovalOutside   = "outside"
	forkApprovalNone      = "none"
)

// approvalExpiry is how long a run waits for approval before it's cancelled.
var approvalExpiry = 30 * 24 * time.Hour

func validForkApproval(p string) bool {
	return p == forkApprovalFirstTime || p == forkApprovalOutside || p == forkApprovalNone
}

// forkApprovalStrictness orders the settings by how many runs wait.
func forkApprovalStrictness(p string) int {
	switch p {
	case forkApprovalNone:
		return 0
	case forkApprovalOutside:
		return 2
	}
	return 1
}

// needsApproval reports whether a pull request event's runs of the
// repository's own workflows wait for approval.
func (s *Server) needsApproval(repo *Repo, ev *pullRequestEvent) bool {
	if !ev.fork() {
		return false
	}
	switch strings.ToUpper(ev.PullRequest.AuthorAssociation) {
	case "OWNER", "MEMBER", "COLLABORATOR":
		return false
	case "CONTRIBUTOR":
		return repo.ForkApproval == forkApprovalOutside
	}
	switch repo.ForkApproval {
	case forkApprovalNone:
		return false
	case forkApprovalOutside:
		return true
	}
	if ev.PullRequest.User.ID == 0 {
		return true
	}
	var approved bool
	err := s.db.QueryRow(`SELECT EXISTS (SELECT 1 FROM approved_contributors WHERE repo_id = ? AND user_id = ?)`,
		repo.ID, ev.PullRequest.User.ID).Scan(&approved)
	return err != nil || !approved
}

// advanceWaiting leaves a run waiting for approval as it is, unless it was
// cancelled. s.mu is held.
func (s *Server) advanceWaiting(run *Run) error {
	if !run.Cancel {
		s.publishRun(run.ID)
		return nil
	}
	return s.finishWaiting(run.ID, "")
}

// finishWaiting cancels a run waiting for approval, saying why. s.mu is held.
func (s *Server) finishWaiting(runID int64, why string) error {
	now := nowMS()
	res, err := s.db.Exec(`UPDATE runs SET status = ?, conclusion = ?, error = ?, cancel_requested = 1, started_at = ?, finished_at = ?
		WHERE id = ? AND status = ?`, protocol.StatusCompleted, protocol.ResultCancelled, why, now, now, runID, protocol.StatusWaiting)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil
	}
	s.log.Info("run cancelled before it was approved", "run", runID, "why", why)
	s.publishRun(runID)
	s.queueWorkflowRun(runID, "completed")
	return nil
}

// cancelWaitingRuns cancels the runs of a pull request's merge ref that wait
// for approval: those of file, or all if file is "", and created before
// before, if it isn't 0.
func (s *Server) cancelWaitingRuns(repoID int64, ref, file string, before int64, why string) {
	q := `SELECT id FROM runs WHERE repo_id = ? AND ref = ? AND event = 'pull_request' AND status = ?`
	args := []any{repoID, ref, protocol.StatusWaiting}
	if file != "" {
		q, args = q+` AND workflow_file = ?`, append(args, file)
	}
	if before > 0 {
		q, args = q+` AND id < ?`, append(args, before)
	}
	s.finishWaitingRuns(q, args, why)
}

// expireWaitingRuns cancels runs that have waited for approval for longer
// than approvalExpiry.
func (s *Server) expireWaitingRuns() {
	s.finishWaitingRuns(`SELECT id FROM runs WHERE status = ? AND created_at < ?`,
		[]any{protocol.StatusWaiting, time.Now().Add(-approvalExpiry).UnixMilli()},
		fmt.Sprintf("Nobody approved it within %d days.", int(approvalExpiry.Hours()/24)))
}

func (s *Server) finishWaitingRuns(q string, args []any, why string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids, err := s.queryIDs(q+` ORDER BY id`, args...)
	if err != nil {
		s.log.Error("runs waiting for approval", "err", err)
		return
	}
	for _, id := range ids {
		if err := s.finishWaiting(id, why); err != nil {
			s.log.Error("cancel a run waiting for approval", "run", id, "err", err)
		}
	}
}

func (s *Server) queryIDs(q string, args ...any) ([]int64, error) {
	rows, err := s.db.Query(q, args...)
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

// approveRuns approves run and the other runs of its pull request that wait
// for approval, and starts them. With fork_approval first_time it remembers
// the pull request's author, whose later pull requests don't wait. It
// returns the runs it approved.
func (s *Server) approveRuns(ctx context.Context, run *Run, by string) ([]int64, error) {
	repo, err := s.getRepoByID(run.RepoID)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	ids, err := s.queryIDs(`SELECT id FROM runs WHERE repo_id = ? AND ref = ? AND event = 'pull_request' AND status = ? AND cancel_requested = 0
		ORDER BY id`, run.RepoID, run.Ref, protocol.StatusWaiting)
	var approved []int64
	for _, id := range ids {
		if err != nil {
			break
		}
		_, err = s.db.Exec(`UPDATE runs SET status = ?, approval = ?, approved_by = ?, approved_at = ? WHERE id = ? AND status = ?`,
			protocol.StatusQueued, approvalApproved, by, nowMS(), id, protocol.StatusWaiting)
		if err == nil {
			approved = append(approved, id)
		}
	}
	s.mu.Unlock()
	if err != nil {
		return approved, err
	}
	if repo.ForkApproval == forkApprovalFirstTime {
		pr, _ := run.EventJSON["pull_request"].(map[string]any)
		user, _ := pr["user"].(map[string]any)
		id, _ := user["id"].(float64)
		login, _ := user["login"].(string)
		if id > 0 {
			if _, err := s.db.Exec(`INSERT INTO approved_contributors (repo_id, user_id, login, approved_by, approved_at) VALUES (?, ?, ?, ?, ?)
				ON CONFLICT (repo_id, user_id) DO UPDATE SET login = excluded.login`, repo.ID, int64(id), login, by, nowMS()); err != nil {
				s.log.Error("remember an approved contributor", "repo", repo.Name, "login", login, "err", err)
			}
		}
	}
	for _, id := range approved {
		s.log.Info("run approved", "run", id, "by", by)
		// Now it enters its concurrency group, as a new run would.
		s.mu.Lock()
		conc, err := s.evaluateRun(id)
		s.mu.Unlock()
		if err != nil {
			s.log.Error("evaluate an approved run", "run", id, "err", err)
		} else if r, err := s.getRun(id); err == nil && r.Status == protocol.StatusCompleted {
			// Its concurrency or run-name is invalid now.
			s.publishRun(id)
			s.queueWorkflowRun(id, "completed")
			continue
		} else if conc.Group != "" {
			s.supersede(ctx, run.RepoID, id, "pull_request", conc)
		}
		if err := s.advance(ctx, id); err != nil {
			s.log.Error("advance", "run", id, "err", err)
		}
	}
	return approved, nil
}

// handleApproveRun approves a run that waits for approval, with the other
// runs of its pull request that do.
func (s *Server) handleApproveRun(w http.ResponseWriter, r *http.Request) {
	run, err := s.getRun(pathID(r))
	if err != nil {
		httpError(w, 404, "no such run")
		return
	}
	if run.Status != protocol.StatusWaiting || run.Cancel {
		httpError(w, 409, "the run isn't waiting for approval")
		return
	}
	by := requestPrincipal(r).user
	ids, err := s.approveRuns(r.Context(), run, by)
	if err != nil {
		httpError(w, 500, "%v", err)
		return
	}
	author := ""
	if pr, _ := run.EventJSON["pull_request"].(map[string]any); pr != nil {
		if user, _ := pr["user"].(map[string]any); user != nil {
			author, _ = user["login"].(string)
		}
	}
	s.audit(r, "run.approve", run.Repo, fmt.Sprintf("run %d", run.Number), fmt.Sprintf("%s by %s (%d runs)", strings.TrimPrefix(run.Ref, "refs/"), author, len(ids)))
	if ids == nil {
		ids = []int64{}
	}
	writeJSON(w, map[string]any{"approved": ids})
}

// ApprovedContributor is an author of pull requests from forks whose runs
// don't wait for approval any more.
type ApprovedContributor struct {
	UserID     int64  `json:"user_id"`
	Login      string `json:"login"`
	ApprovedBy string `json:"approved_by"`
	ApprovedAt int64  `json:"approved_at"`
}

func (s *Server) handleListApprovedContributors(w http.ResponseWriter, r *http.Request) {
	repo, err := s.getRepo(r.PathValue("owner") + "/" + r.PathValue("name"))
	if err != nil {
		httpError(w, 404, "no such repository")
		return
	}
	rows, err := s.db.Query(`SELECT user_id, login, approved_by, approved_at FROM approved_contributors WHERE repo_id = ? ORDER BY login COLLATE NOCASE`, repo.ID)
	if err != nil {
		httpError(w, 500, "%v", err)
		return
	}
	defer rows.Close()
	out := []ApprovedContributor{}
	for rows.Next() {
		var c ApprovedContributor
		if err := rows.Scan(&c.UserID, &c.Login, &c.ApprovedBy, &c.ApprovedAt); err != nil {
			httpError(w, 500, "%v", err)
			return
		}
		out = append(out, c)
	}
	writeJSON(w, out)
}

// handleDeleteApprovedContributor forgets an approved contributor, whose pull
// requests from forks wait for approval again.
func (s *Server) handleDeleteApprovedContributor(w http.ResponseWriter, r *http.Request) {
	repo, err := s.getRepo(r.PathValue("owner") + "/" + r.PathValue("name"))
	if err != nil {
		httpError(w, 404, "no such repository")
		return
	}
	id, err := strconv.ParseInt(r.PathValue("user"), 10, 64)
	if err != nil {
		httpError(w, 400, "the user is GitHub's numeric user ID")
		return
	}
	var login string
	if err := s.db.QueryRow(`DELETE FROM approved_contributors WHERE repo_id = ? AND user_id = ? RETURNING login`, repo.ID, id).Scan(&login); err != nil {
		httpError(w, 404, "no such approved contributor")
		return
	}
	s.audit(r, "repo.approved_contributor.delete", repo.Name, login, "")
	writeJSON(w, map[string]bool{"ok": true})
}
