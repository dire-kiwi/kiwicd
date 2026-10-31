package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/dire-kiwi/kiwicd/internal/protocol"
)

// Runs show on GitHub as check runs, through the GitHub App (see github.go):
// one per job, named "workflow / job" like GitHub Actions' (matrix values are
// in the job's name), created when the job is queued and updated when it
// starts and finishes. Branch protection can then require them.
//
// A run is reported to the GitHub repository its commit belongs to:
//
//   - A mirror's own runs (push, workflow_dispatch, pull_request) go to the
//     mirrored repository, on the pushed commit, or on the pull request's
//     head for pull requests.
//   - repository_push and repository_pull_request runs belong to the
//     listening repository, often a private one holding a public one's
//     deployment, so reporting them on the source repository's commit would
//     publish its job names and links there. They are reported only if the
//     listening repository's report_checks setting is on, named
//     "listener: workflow / job". So are runs of uploaded workflows, which
//     are kept out of the repository for the same reason.
//
// Reporting happens in the background, one run at a time, after a run or
// its jobs change: talking to GitHub never fails or holds up a run. Failed
// calls are logged and retried a few times; github_checks records what was
// last reported, so a later change also catches up.
//
// Clicking Re-run on GitHub sends check_run (one check) or check_suite (all
// of a commit's) rerequested events, which re-run the runs they belong to.

type checkReporter struct {
	once  sync.Once
	wake  chan struct{}
	mu    sync.Mutex
	dirty map[int64]bool // run IDs to report
	tries map[int64]int  // failed attempts in a row, by run ID
}

// checkRetries is how many times a run's checks are retried after GitHub
// fails, checkRetryDelay apart.
const checkRetries = 5

var checkRetryDelay = time.Minute

// noChecksTime is how long kiwicd waits before trying a repository again
// whose installation lacks the checks permission. Installation events (as
// when the permission is accepted) end the wait sooner.
const noChecksTime = 10 * time.Minute

// reportChecks reports a run's jobs to GitHub soon, if the run is one that's
// reported. It doesn't block.
func (s *Server) reportChecks(runID int64) {
	c := &s.checks
	c.once.Do(func() {
		c.dirty, c.tries = map[int64]bool{}, map[int64]int{}
		c.wake = make(chan struct{}, 1)
		go s.checkWorker()
	})
	c.mu.Lock()
	c.dirty[runID] = true
	c.mu.Unlock()
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

func (s *Server) checkWorker() {
	c := &s.checks
	for range c.wake {
		for {
			c.mu.Lock()
			var runID int64
			for id := range c.dirty {
				runID = id
				break
			}
			delete(c.dirty, runID)
			c.mu.Unlock()
			if runID == 0 {
				break
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			err := s.syncChecks(ctx, runID)
			cancel()
			c.mu.Lock()
			if err == nil {
				delete(c.tries, runID)
			} else if c.tries[runID]++; c.tries[runID] <= checkRetries {
				s.log.Warn("reporting a run to GitHub", "run", runID, "err", err)
				time.AfterFunc(checkRetryDelay, func() { s.reportChecks(runID) })
			} else {
				s.log.Warn("reporting a run to GitHub; giving up", "run", runID, "err", err)
				delete(c.tries, runID)
			}
			c.mu.Unlock()
		}
	}
}

var shaRE = regexp.MustCompile(`^[0-9a-f]{40}$`)

// checkTarget is where a run is reported.
type checkTarget struct {
	repo   string // owner/name on GitHub
	sha    string
	prefix string // before the check runs' names
}

// checkTarget returns where run is reported, or ok false if it isn't.
func (s *Server) checkTarget(run *Run, repo *Repo) (t checkTarget, ok bool) {
	ev := run.EventJSON
	src := repo
	t.sha = run.Sha
	switch run.Event {
	case "repository_push", "repository_pull_request":
		if !repo.ReportChecks {
			return t, false
		}
		// The source repository, by its name in kiwicd.
		srcRepo, _ := ev["repository"].(map[string]any)
		name, _ := srcRepo["full_name"].(string)
		var err error
		if src, err = s.getRepo(name); err != nil {
			return t, false
		}
		t.sha, _ = ev["after"].(string)
		t.prefix = repo.Name + ": "
	default:
		if strings.HasPrefix(run.WorkflowFile, uploadedPrefix) && !repo.ReportChecks {
			return t, false
		}
	}
	// Pull requests' checks go on their head, as GitHub's do.
	if pr, _ := ev["pull_request"].(map[string]any); pr != nil {
		head, _ := pr["head"].(map[string]any)
		if sha, _ := head["sha"].(string); sha != "" {
			t.sha = sha
		}
	}
	if src.Kind != "mirror" || !shaRE.MatchString(t.sha) {
		return t, false
	}
	t.repo, _ = s.githubRepoName(src.URL)
	return t, t.repo != ""
}

// checkRun is a check run as kiwicd reports it.
type checkRun struct {
	Name        string       `json:"name,omitempty"`
	HeadSHA     string       `json:"head_sha,omitempty"`
	DetailsURL  string       `json:"details_url,omitempty"`
	ExternalID  string       `json:"external_id,omitempty"`
	Status      string       `json:"status"`
	Conclusion  string       `json:"conclusion,omitempty"`
	StartedAt   string       `json:"started_at,omitempty"`
	CompletedAt string       `json:"completed_at,omitempty"`
	Output      *checkOutput `json:"output,omitempty"`
}

// checkOutput is what GitHub shows on a check run's page. Text holds a
// finished job's step summaries and Annotations its ::error file=…:: and the
// like (see reportCheckOutput).
type checkOutput struct {
	Title       string            `json:"title"`
	Summary     string            `json:"summary"`
	Text        string            `json:"text,omitempty"`
	Annotations []checkAnnotation `json:"annotations,omitempty"`
}

type checkAnnotation struct {
	Path            string `json:"path"`
	StartLine       int    `json:"start_line"`
	EndLine         int    `json:"end_line"`
	StartColumn     int    `json:"start_column,omitempty"`
	EndColumn       int    `json:"end_column,omitempty"`
	AnnotationLevel string `json:"annotation_level"` // notice, warning or failure
	Message         string `json:"message"`
	Title           string `json:"title,omitempty"`
}

// state is what github_checks remembers of a reported check run.
func (c *checkRun) state() string { return c.Status + "/" + c.Conclusion }

// syncChecks creates and updates the check runs of a run's jobs to match
// them.
func (s *Server) syncChecks(ctx context.Context, runID int64) error {
	run, err := s.getRun(runID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil // deleted
	}
	if err != nil {
		return err
	}
	repo, err := s.getRepoByID(run.RepoID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	target, ok := s.checkTarget(run, repo)
	if !ok {
		return nil
	}
	a, err := s.githubApp()
	if errors.Is(err, errNoGitHubApp) {
		return nil
	}
	if err != nil {
		return err
	}
	key := strings.ToLower(target.repo)
	s.gh.mu.Lock()
	until, waiting := s.gh.noChecks[key]
	s.gh.mu.Unlock()
	if waiting && s.gh.now().Before(until) {
		return nil
	}

	jobs, err := s.runJobs(runID)
	if err != nil {
		return err
	}
	want := map[int64]*checkRun{}
	var order []int64
	called := map[int64]bool{}
	for _, j := range jobs {
		called[j.ParentID] = true
	}
	for _, j := range jobs {
		// As on GitHub, a job that calls a reusable workflow shows as the
		// called jobs ("caller / job"), unless it finished without any.
		if j.Uses != "" && (called[j.ID] || j.Status != protocol.StatusCompleted) {
			continue
		}
		want[j.ID] = s.jobCheckRun(run, j, target)
		order = append(order, j.ID)
	}
	if len(jobs) == 0 && run.Status == protocol.StatusCompleted && run.Error != "" {
		// An invalid workflow: GitHub shows a failed run for those too.
		want[0] = s.runCheckRun(run, target)
		order = append(order, 0)
	}
	type reported struct {
		id    int64
		state string
	}
	have := map[int64]reported{}
	rows, err := s.db.Query(`SELECT job_id, check_run_id, state FROM github_checks WHERE run_id = ?`, runID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var jobID int64
		var r reported
		if err := rows.Scan(&jobID, &r.id, &r.state); err != nil {
			rows.Close()
			return err
		}
		have[jobID] = r
	}
	rows.Close()

	auth := ""
	for _, jobID := range order {
		c := want[jobID]
		old, exists := have[jobID]
		if exists && old.state == c.state() {
			continue
		}
		if auth == "" {
			token, err := s.installationToken(ctx, a, target.repo, map[string]string{"checks": "write"})
			if token == "" && err == nil {
				return nil // the app isn't installed on the repository
			}
			if isGitHubStatus(err, 422) {
				s.log.Warn("the GitHub App can't report check runs: give it the checks permission and accept it on the installation",
					"repo", target.repo, "err", err)
				s.gh.mu.Lock()
				s.gh.noChecks[key] = s.gh.now().Add(noChecksTime)
				s.gh.mu.Unlock()
				return nil
			}
			if err != nil {
				return err
			}
			auth = "token " + token
		}
		var res struct {
			ID         int64 `json:"id"`
			CheckSuite struct {
				ID int64 `json:"id"`
			} `json:"check_suite"`
		}
		if exists {
			// The name, commit and link stay as they were created.
			upd := *c
			upd.Name, upd.HeadSHA, upd.DetailsURL, upd.ExternalID = "", "", "", ""
			err = s.githubAPI(ctx, "PATCH", fmt.Sprintf("/repos/%s/check-runs/%d", target.repo, old.id), auth, &upd, &res)
		} else {
			err = s.githubAPI(ctx, "POST", "/repos/"+target.repo+"/check-runs", auth, c, &res)
		}
		if err != nil {
			return fmt.Errorf("check run %q: %w", c.Name, err)
		}
		if exists {
			_, err = s.db.Exec(`UPDATE github_checks SET state = ?, updated_at = ? WHERE run_id = ? AND job_id = ?`,
				c.state(), nowMS(), runID, jobID)
		} else {
			_, err = s.db.Exec(`INSERT INTO github_checks (run_id, job_id, repo, check_run_id, check_suite_id, state, updated_at)
				VALUES (?, ?, ?, ?, ?, ?, ?) ON CONFLICT (run_id, job_id) DO UPDATE SET check_run_id = excluded.check_run_id,
				check_suite_id = excluded.check_suite_id, state = excluded.state, updated_at = excluded.updated_at`,
				runID, jobID, target.repo, res.ID, res.CheckSuite.ID, c.state(), nowMS())
		}
		if err != nil {
			if errors.Is(err, sql.ErrConnDone) || strings.Contains(err.Error(), "FOREIGN KEY") {
				return nil // the run was deleted meanwhile
			}
			return err
		}
	}
	return nil
}

func (s *Server) checkName(run *Run, target checkTarget, job string) string {
	return target.prefix + run.WorkflowName + " / " + job
}

func (s *Server) detailsURL(path string) string {
	if !strings.HasPrefix(s.cfg.PublicURL, "http") {
		return "" // GitHub wants a URL it can link to
	}
	return strings.TrimRight(s.cfg.PublicURL, "/") + path
}

func checkTime(ms int64) string {
	if ms == 0 {
		return ""
	}
	return time.UnixMilli(ms).UTC().Format(time.RFC3339)
}

// jobCheckRun is the check run for a job as it is now.
func (s *Server) jobCheckRun(run *Run, j *Job, target checkTarget) *checkRun {
	c := &checkRun{
		Name:       s.checkName(run, target, j.Name),
		HeadSHA:    target.sha,
		DetailsURL: s.detailsURL(fmt.Sprintf("/runs/%d/jobs/%d", run.ID, j.ID)),
		ExternalID: fmt.Sprint(j.ID),
		Status:     "queued",
		StartedAt:  checkTime(j.StartedAt),
	}
	out := &checkOutput{Title: "Queued", Summary: fmt.Sprintf("**%s** is waiting for a runner.", j.Name)}
	switch j.Status {
	case protocol.StatusInProgress:
		c.Status = "in_progress"
		out.Title, out.Summary = "In progress", fmt.Sprintf("**%s** is running.", j.Name)
	case protocol.StatusCompleted:
		c.Status, c.Conclusion = "completed", checkConclusion(j.Conclusion)
		c.CompletedAt = checkTime(j.FinishedAt)
		if c.StartedAt == "" {
			c.StartedAt = c.CompletedAt
		}
		out.Title = checkTitle(c.Conclusion)
		out.Summary = fmt.Sprintf("**%s** %s", j.Name, strings.ToLower(out.Title))
		if j.StartedAt > 0 && j.FinishedAt >= j.StartedAt {
			out.Summary += " after " + time.Duration((j.FinishedAt-j.StartedAt)*int64(time.Millisecond)).Round(time.Second).String()
		}
		out.Summary += "."
		if j.Error != "" {
			out.Summary += "\n\n" + quoteMarkdown(j.Error)
		}
		if rep, err := s.jobReport(j.ID); err == nil {
			reportCheckOutput(out, rep)
		} else {
			s.log.Warn("job report for its check run", "job", j.ID, "err", err)
		}
	}
	if len(j.Steps) > 0 {
		out.Summary += "\n\n| Step | Result |\n|---|---|\n"
		for _, st := range j.Steps {
			result := st.Conclusion
			if result == "" {
				result = strings.ReplaceAll(st.Status, "_", " ")
			}
			out.Summary += "| " + tableCell(st.Name) + " | " + stepIcon(st.Conclusion) + result + " |\n"
		}
	}
	out.Summary = limitSummary(out.Summary)
	c.Output = out
	return c
}

// runCheckRun is the check run for a run that failed before it had jobs.
func (s *Server) runCheckRun(run *Run, target checkTarget) *checkRun {
	at := checkTime(run.FinishedAt)
	return &checkRun{
		Name:        target.prefix + run.WorkflowName,
		HeadSHA:     target.sha,
		DetailsURL:  s.detailsURL(fmt.Sprintf("/runs/%d", run.ID)),
		ExternalID:  "run:" + fmt.Sprint(run.ID),
		Status:      "completed",
		Conclusion:  "failure",
		StartedAt:   at,
		CompletedAt: at,
		Output:      &checkOutput{Title: "Failed", Summary: limitSummary(quoteMarkdown(run.Error))},
	}
}

func checkConclusion(result string) string {
	switch result {
	case protocol.ResultSuccess, protocol.ResultFailure, protocol.ResultCancelled, protocol.ResultSkipped:
		return result
	}
	return "neutral"
}

func checkTitle(conclusion string) string {
	switch conclusion {
	case "success":
		return "Succeeded"
	case "failure":
		return "Failed"
	case "cancelled":
		return "Cancelled"
	case "skipped":
		return "Skipped"
	}
	return "Finished"
}

func stepIcon(conclusion string) string {
	switch conclusion {
	case protocol.ResultSuccess:
		return "✅ "
	case protocol.ResultFailure:
		return "❌ "
	case protocol.ResultCancelled, protocol.ResultSkipped:
		return "⚪ "
	}
	return ""
}

// maxSummary is a little under GitHub's limit on a check run's summary.
const maxSummary = 60000

func limitSummary(s string) string {
	if len(s) <= maxSummary {
		return s
	}
	return strings.ToValidUTF8(s[:maxSummary], "") + "\n\n…"
}

func quoteMarkdown(s string) string {
	return "> " + strings.ReplaceAll(strings.TrimSpace(s), "\n", "\n> ")
}

func tableCell(s string) string {
	return strings.NewReplacer("|", `\|`, "\n", " ").Replace(s)
}

// ---------------------------------------------------------------- re-runs

// onCheckRerequest re-runs the runs of a check_run or check_suite
// rerequested event: Re-run on GitHub. It returns the new runs' IDs.
func (s *Server) onCheckRerequest(event string, body []byte) ([]int64, error) {
	var ev struct {
		Action   string `json:"action"`
		CheckRun struct {
			ID int64 `json:"id"`
		} `json:"check_run"`
		CheckSuite struct {
			ID int64 `json:"id"`
		} `json:"check_suite"`
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
		Sender struct {
			Login string `json:"login"`
		} `json:"sender"`
	}
	if err := json.Unmarshal(body, &ev); err != nil {
		return nil, err
	}
	out := []int64{}
	if ev.Action != "rerequested" {
		return out, nil
	}
	q, id := `SELECT DISTINCT run_id FROM github_checks WHERE check_run_id = ? AND repo = ? COLLATE NOCASE`, ev.CheckRun.ID
	if event == "check_suite" {
		q, id = `SELECT DISTINCT run_id FROM github_checks WHERE check_suite_id = ? AND repo = ? COLLATE NOCASE`, ev.CheckSuite.ID
	}
	if id == 0 {
		return nil, errors.New("no id")
	}
	rows, err := s.db.Query(q+` ORDER BY run_id`, id, ev.Repository.FullName)
	if err != nil {
		return nil, err
	}
	var runIDs []int64
	for rows.Next() {
		var runID int64
		if err := rows.Scan(&runID); err != nil {
			rows.Close()
			return nil, err
		}
		runIDs = append(runIDs, runID)
	}
	rows.Close()
	seen := map[string]bool{}
	for _, runID := range runIDs {
		run, err := s.getRun(runID)
		if err != nil {
			continue
		}
		// The latest attempt, once; one that's still going isn't re-run.
		key := fmt.Sprint(run.RepoID, "#", run.Number)
		if seen[key] {
			continue
		}
		seen[key] = true
		var latest int64
		if err := s.db.QueryRow(`SELECT id FROM runs WHERE repo_id = ? AND number = ? ORDER BY attempt DESC LIMIT 1`,
			run.RepoID, run.Number).Scan(&latest); err != nil {
			continue
		}
		if run, err = s.getRun(latest); err != nil || run.Status != protocol.StatusCompleted {
			continue
		}
		newID, err := s.rerun(context.Background(), run, rerunOptions{})
		if err != nil {
			s.log.Error("re-run from GitHub", "run", run.ID, "err", err)
			continue
		}
		s.log.Info("re-run from GitHub", "run", run.ID, "new", newID, "repo", ev.Repository.FullName, "by", ev.Sender.Login)
		out = append(out, newID)
	}
	return out, nil
}
