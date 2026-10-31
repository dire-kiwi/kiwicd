package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/dire-kiwi/kiwicd/internal/protocol"
	"github.com/dire-kiwi/kiwicd/internal/wf"
	"github.com/nektos/act/pkg/model"
)

// Events besides pushes, pull requests and dispatches, as on GitHub:
//
//   - workflow_run: a run of another workflow of the same repository was
//     requested, started or completed. Listeners run from the default
//     branch, with its secrets, whatever ref the run they follow was for.
//   - repository_dispatch: POST /api/repos/OWNER/NAME/dispatches, or GitHub's
//     repository_dispatch webhook to the GitHub App for a mirror. Listeners
//     run from the default branch.
//   - create and delete: a branch or tag appeared or went away, in a push or
//     a mirror's fetch. create runs the workflows of the new ref's commit,
//     delete those of the default branch.

// maxTriggerDepth is how many workflow_run events can follow one another, as
// on GitHub: in A → B → C → D → E, where B to E follow the one before with
// workflow_run, E doesn't run. It stops workflows that follow each other (or
// themselves) from running forever.
const maxTriggerDepth = 3

type workflowRunActivity struct {
	runID  int64
	action string // requested, in_progress or completed
}

// queueWorkflowRun hands a run's activity to the worker that starts the
// workflow_run listeners, one at a time in the order they happened. It
// doesn't block: s.mu may be held.
func (s *Server) queueWorkflowRun(runID int64, action string) {
	s.wrOnce.Do(func() {
		s.wrQueue = make(chan workflowRunActivity, 1024)
		go func() {
			for a := range s.wrQueue {
				if err := s.onWorkflowRun(context.Background(), a); err != nil {
					s.log.Error("workflow_run trigger", "run", a.runID, "action", a.action, "err", err)
				}
			}
		}()
	})
	select {
	case s.wrQueue <- workflowRunActivity{runID: runID, action: action}:
	default:
		s.log.Warn("too many workflow_run events waiting; dropped one", "run", runID, "action", action)
	}
}

// onWorkflowRun starts the workflows of the run's repository that listen to
// its workflow with workflow_run.
func (s *Server) onWorkflowRun(ctx context.Context, a workflowRunActivity) error {
	run, err := s.getRun(a.runID)
	if err != nil {
		return nil // deleted since
	}
	repo, err := s.getRepoByID(run.RepoID)
	if err != nil {
		return nil
	}
	branch := runBranch(run)
	matches := func(w *model.Workflow) bool {
		return wf.MatchesWorkflowRun(w, wf.WorkflowRun{Workflow: run.WorkflowName, Action: a.action, Branch: branch})
	}
	if run.TriggerDepth >= maxTriggerDepth {
		// Only say so if something would have run.
		if files, _ := s.matchingDefaultBranchWorkflows(ctx, repo, "workflow_run", matches); len(files) > 0 {
			s.log.Warn("not starting workflow_run listeners: too many workflow_run events in a row", "run", run.ID,
				"depth", run.TriggerDepth, "workflows", files)
		}
		return nil
	}
	spec := runSpec{event: "workflow_run", payload: s.workflowRunPayload(run, repo, a.action), actor: run.Actor,
		title: run.Title, depth: run.TriggerDepth + 1}
	_, err = s.startDefaultBranchRuns(ctx, repo, spec, nil, matches)
	return err
}

// runBranch is a run's head_branch in workflow_run events: its branch or
// tag name, or for a pull request's run the pull request's head branch, as
// on GitHub.
func runBranch(run *Run) string {
	if run.Event == "pull_request" {
		pr, _ := run.EventJSON["pull_request"].(map[string]any)
		head, _ := pr["head"].(map[string]any)
		if ref, ok := head["ref"].(string); ok {
			return ref
		}
	}
	return strings.TrimPrefix(strings.TrimPrefix(run.Ref, "refs/heads/"), "refs/tags/")
}

// matchingDefaultBranchWorkflows returns the files of the workflows on the
// default branch that listen for event and that matches accepts.
func (s *Server) matchingDefaultBranchWorkflows(ctx context.Context, repo *Repo, event string, matches func(*model.Workflow) bool) ([]string, error) {
	sha, err := resolveRef(ctx, s.repoPath(repo.Name), "refs/heads/"+repo.DefaultBranch)
	if err != nil {
		return nil, nil
	}
	files, err := s.defaultBranchWorkflows(ctx, repo, sha)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, file := range sortedKeys(files) {
		if !strings.Contains(string(files[file]), event) {
			continue
		}
		if w, err := wf.Parse(files[file]); err == nil && matches(w) {
			out = append(out, file)
		}
	}
	return out, nil
}

// workflowRunPayload is the workflow_run event for a run's activity, with
// the parts of GitHub's that kiwicd has. Its status is the one the action
// is about, which the run may have moved on from.
func (s *Server) workflowRunPayload(run *Run, repo *Repo, action string) map[string]any {
	status, conclusion := protocol.StatusQueued, any(nil)
	switch action {
	case "in_progress":
		status = protocol.StatusInProgress
	case "completed":
		status, conclusion = protocol.StatusCompleted, run.Conclusion
	}
	branch := runBranch(run)
	stamp := func(ms int64) any {
		if ms == 0 {
			return nil
		}
		return time.UnixMilli(ms).UTC().Format(time.RFC3339)
	}
	updated := max(run.CreatedAt, run.StartedAt, run.FinishedAt)
	info := repoEventInfo(repo, s.cfg.PublicURL)
	commit := readCommit(context.Background(), s.repoPath(repo.Name), run.Sha)
	return map[string]any{
		"action": action,
		"workflow_run": map[string]any{
			"id":               run.ID,
			"name":             run.WorkflowName,
			"display_title":    run.Title,
			"path":             run.WorkflowFile,
			"run_number":       run.Number,
			"run_attempt":      run.Attempt,
			"event":            run.Event,
			"status":           status,
			"conclusion":       conclusion,
			"head_branch":      branch,
			"head_sha":         run.Sha,
			"head_commit":      map[string]any{"id": run.Sha, "message": commit.Subject, "author": map[string]any{"name": commit.Author, "email": commit.Email}},
			"html_url":         s.cfg.PublicURL + fmt.Sprintf("/runs/%d", run.ID),
			"url":              s.cfg.PublicURL + fmt.Sprintf("/api/runs/%d", run.ID),
			"created_at":       stamp(run.CreatedAt),
			"run_started_at":   stamp(run.StartedAt),
			"updated_at":       stamp(updated),
			"actor":            map[string]any{"login": run.Actor},
			"triggering_actor": map[string]any{"login": run.Actor},
			"pull_requests":    []any{},
			"repository":       info,
			"head_repository":  info,
		},
		"workflow":   map[string]any{"name": run.WorkflowName, "path": run.WorkflowFile},
		"repository": info,
		"sender":     map[string]any{"login": run.Actor},
	}
}

// ---------------------------------------------------------------- repository_dispatch

const maxDispatchPayload = 64 << 10

// handleRepositoryDispatch takes GitHub's "create a repository dispatch
// event" request: {"event_type": "...", "client_payload": {...}}. It starts
// the default branch's repository_dispatch workflows whose types include
// event_type and returns their runs.
func (s *Server) handleRepositoryDispatch(w http.ResponseWriter, r *http.Request) {
	repo, err := s.getRepo(r.PathValue("owner") + "/" + r.PathValue("name"))
	if err != nil {
		httpError(w, 404, "no such repository")
		return
	}
	var req struct {
		EventType     string          `json:"event_type"`
		ClientPayload json.RawMessage `json:"client_payload"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxDispatchPayload)
	if err := readJSON(r, &req); err != nil {
		httpError(w, 400, "bad request: %v", err)
		return
	}
	payload, err := clientPayload(req.ClientPayload)
	if err != nil {
		httpError(w, 400, "%v", err)
		return
	}
	ids, err := s.repositoryDispatch(r.Context(), repo, req.EventType, payload, requestUser(r))
	if err != nil {
		if _, bad := err.(dispatchError); bad {
			httpError(w, 400, "%v", err)
		} else {
			httpError(w, 500, "%v", err)
		}
		return
	}
	if ids == nil {
		ids = []int64{}
	}
	writeJSON(w, map[string]any{"run_ids": ids})
}

type dispatchError string

func (e dispatchError) Error() string { return string(e) }

// clientPayload checks a repository_dispatch's client_payload: an object,
// or nothing.
func clientPayload(raw json.RawMessage) (map[string]any, error) {
	out := map[string]any{}
	if len(raw) == 0 || string(raw) == "null" {
		return out, nil
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("client_payload must be a JSON object")
	}
	return out, nil
}

// repositoryDispatch starts repo's repository_dispatch workflows for
// eventType.
func (s *Server) repositoryDispatch(ctx context.Context, repo *Repo, eventType string, payload map[string]any, actor string) ([]int64, error) {
	eventType = strings.TrimSpace(eventType)
	if eventType == "" || len(eventType) > 100 {
		return nil, dispatchError("event_type must be 1 to 100 characters")
	}
	event := map[string]any{
		"action":         eventType,
		"branch":         repo.DefaultBranch,
		"client_payload": payload,
		"repository":     repoEventInfo(repo, s.cfg.PublicURL),
		"sender":         map[string]any{"login": actor},
	}
	s.log.Info("repository dispatch", "repo", repo.Name, "event_type", eventType, "actor", actor)
	spec := runSpec{event: "repository_dispatch", payload: event, actor: actor, title: eventType}
	broken := func(src []byte) bool { return strings.Contains(string(src), "repository_dispatch") }
	return s.startDefaultBranchRuns(ctx, repo, spec, broken, func(w *model.Workflow) bool {
		return wf.MatchesRepositoryDispatch(w, eventType)
	})
}

// githubRepositoryDispatch handles GitHub's repository_dispatch webhook for a
// mirror: it fetches the mirror, so that the runs are of the default
// branch's current tip, and dispatches.
func (s *Server) githubRepositoryDispatch(repo *Repo, body []byte) {
	var ev struct {
		Action        string          `json:"action"`
		ClientPayload json.RawMessage `json:"client_payload"`
		Sender        struct {
			Login string `json:"login"`
		} `json:"sender"`
	}
	ctx := context.Background()
	if err := json.Unmarshal(body, &ev); err != nil {
		s.log.Warn("bad repository_dispatch event", "repo", repo.Name, "err", err)
		return
	}
	payload, err := clientPayload(ev.ClientPayload)
	if err != nil {
		s.log.Warn("bad repository_dispatch event", "repo", repo.Name, "err", err)
		return
	}
	if err := s.syncRepo(ctx, repo.ID, false); err != nil {
		s.log.Warn("repository_dispatch sync", "repo", repo.Name, "err", err)
	}
	if fresh, err := s.getRepoByID(repo.ID); err == nil {
		repo = fresh
	}
	if _, err := s.repositoryDispatch(ctx, repo, ev.Action, payload, ev.Sender.Login); err != nil {
		s.log.Error("repository_dispatch", "repo", repo.Name, "err", err)
	}
}

// ---------------------------------------------------------------- create and delete

func refType(ref string) (name, typ string) {
	if name, ok := strings.CutPrefix(ref, "refs/tags/"); ok {
		return name, "tag"
	}
	return strings.TrimPrefix(ref, "refs/heads/"), "branch"
}

// onCreateRef starts the create workflows of a new branch or tag's commit.
func (s *Server) onCreateRef(ctx context.Context, repo *Repo, ref, sha string) error {
	files, err := s.workflowsAt(ctx, repo, sha)
	if err != nil {
		return err
	}
	commit := readCommit(ctx, s.repoPath(repo.Name), sha)
	name, typ := refType(ref)
	event := map[string]any{
		"ref":           name,
		"ref_type":      typ,
		"master_branch": repo.DefaultBranch,
		"description":   nil,
		"pusher_type":   "user",
		"repository":    repoEventInfo(repo, s.cfg.PublicURL),
		"sender":        map[string]any{"login": commit.Author},
	}
	for _, file := range sortedKeys(files) {
		src := files[file]
		if !strings.Contains(string(src), "create") {
			continue
		}
		w, err := wf.Parse(src)
		if err != nil || !wf.HasEvent(w, "create") {
			continue // a push of the commit reports broken workflows
		}
		wname := w.Name
		if wname == "" {
			wname = file
		}
		if _, err := s.createRun(ctx, runSpec{repo: repo, file: file, yaml: string(src), name: wname, event: "create",
			payload: event, ref: ref, sha: sha, actor: commit.Author, title: fmt.Sprintf("Created %s %s", typ, name)}); err != nil {
			return err
		}
	}
	return nil
}

// onDeleteRef starts the default branch's delete workflows for a branch or
// tag that's gone.
func (s *Server) onDeleteRef(ctx context.Context, repo *Repo, ref, oldSha string) error {
	commit := readCommit(ctx, s.repoPath(repo.Name), oldSha)
	name, typ := refType(ref)
	event := map[string]any{
		"ref":         name,
		"ref_type":    typ,
		"pusher_type": "user",
		"repository":  repoEventInfo(repo, s.cfg.PublicURL),
		"sender":      map[string]any{"login": commit.Author},
	}
	spec := runSpec{event: "delete", payload: event, actor: commit.Author, title: fmt.Sprintf("Deleted %s %s", typ, name)}
	_, err := s.startDefaultBranchRuns(ctx, repo, spec, nil, func(w *model.Workflow) bool { return wf.HasEvent(w, "delete") })
	return err
}
