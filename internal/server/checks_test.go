package server

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dire-kiwi/kiwicd/internal/protocol"
)

const checksWorkflow = `name: CI
on: push
jobs:
  build:
    strategy:
      matrix:
        go: ["1.22", "1.23"]
    runs-on: self-hosted
    steps:
      - run: go test ./...
  never:
    if: false
    runs-on: self-hosted
    steps:
      - run: echo never
`

// latestChecks returns the last request for each check run the fake has,
// with the name and commit it was created with, by name.
func (f *fakeGitHub) latestChecks() map[string]checkReq {
	f.mu.Lock()
	defer f.mu.Unlock()
	created := map[string]checkRun{}
	out := map[string]checkReq{}
	for i, c := range f.checkReqs {
		if c.Method == "POST" {
			c.ID = fmt.Sprint(1000 + i + 1)
			created[c.ID] = c.Body
		} else if first, ok := created[c.ID]; ok {
			c.Body.Name, c.Body.HeadSHA, c.Body.DetailsURL, c.Body.ExternalID = first.Name, first.HeadSHA, first.DetailsURL, first.ExternalID
		} else {
			continue
		}
		out[c.Body.Name] = c
	}
	return out
}

func (f *fakeGitHub) checkCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.checkReqs)
}

func TestGitHubChecks(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	g := newGitHubTest(t)
	g.createApp()
	ctx := context.Background()

	work := filepath.Join(t.TempDir(), "work")
	origin := filepath.Join(g.gh.origins, "gh", "app.git")
	git := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("", "init", "-q", "--bare", origin)
	git("", "init", "-q", "-b", "main", work)
	_ = os.MkdirAll(filepath.Join(work, ".github", "workflows"), 0o755)
	if err := os.WriteFile(filepath.Join(work, ".github", "workflows", "ci.yml"), []byte(checksWorkflow), 0o644); err != nil {
		t.Fatal(err)
	}
	git(work, "add", ".")
	commit := func(msg string) string {
		git(work, "commit", "-q", "--allow-empty", "-m", msg)
		git(work, "push", "-q", origin, "HEAD:main")
		return git(work, "rev-parse", "HEAD")
	}
	commit("first")
	if resp := g.do(nil, "POST", "/api/repos", `{"name":"gh/app","url":"`+g.gh.ts.URL+`/gh/app.git"}`, nil); resp.StatusCode != 200 {
		t.Fatalf("create mirror: %d", resp.StatusCode)
	}
	repo, err := g.srv.getRepo("gh/app")
	if err != nil {
		t.Fatal(err)
	}
	// push runs the workflow on a new commit and returns the run.
	push := func(msg string) (string, *Run) {
		t.Helper()
		sha := commit(msg)
		if err := g.srv.syncRepo(ctx, repo.ID, false); err != nil {
			t.Fatal(err)
		}
		var id int64
		if err := g.srv.db.QueryRow(`SELECT id FROM runs WHERE sha = ? ORDER BY id DESC LIMIT 1`, sha).Scan(&id); err != nil {
			t.Fatalf("no run for %s: %v", msg, err)
		}
		run, _ := g.srv.getRun(id)
		return sha, run
	}
	jobsOf := func(runID int64) map[string]*Job {
		t.Helper()
		jobs, err := g.srv.runJobs(runID)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]*Job{}
		for _, j := range jobs {
			out[j.Name] = j
		}
		return out
	}
	waitChecks := func(what string, want map[string]string) map[string]checkReq {
		t.Helper()
		var got map[string]checkReq
		waitFor(t, what, func() bool {
			got = g.gh.latestChecks()
			for name, state := range want {
				if c, ok := got[name]; !ok || c.Body.state() != state {
					return false
				}
			}
			return true
		})
		return got
	}

	sha, run := push("second")
	jobs := jobsOf(run.ID)
	checks := waitChecks("queued check runs", map[string]string{
		"CI / build (1.22)": "queued/", "CI / build (1.23)": "queued/", "CI / never": "completed/skipped"})
	if len(checks) != 3 {
		t.Errorf("check runs %v", checks)
	}
	c := checks["CI / build (1.22)"]
	j := jobs["build (1.22)"]
	if c.Method != "POST" || c.Body.HeadSHA != sha || c.Body.ExternalID != fmt.Sprint(j.ID) ||
		c.Body.DetailsURL != fmt.Sprintf("%s/runs/%d/jobs/%d", g.ts.URL, run.ID, j.ID) || c.Body.Output == nil || c.Body.Output.Title != "Queued" {
		t.Errorf("queued check run %+v", c)
	}

	// Starting and finishing jobs updates their check runs.
	if _, err := g.srv.db.Exec(`UPDATE jobs SET status = ?, started_at = ? WHERE id = ?`, protocol.StatusInProgress, nowMS(), j.ID); err != nil {
		t.Fatal(err)
	}
	if err := g.srv.advance(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	waitChecks("an in-progress check run", map[string]string{"CI / build (1.22)": "in_progress/"})
	steps := []protocol.StepState{{Name: "Set up job", Status: protocol.StatusCompleted, Conclusion: protocol.ResultSuccess},
		{Name: "Run go test ./...", Status: protocol.StatusCompleted, Conclusion: protocol.ResultFailure}}
	if err := g.srv.saveJobReport(j.ID, []protocol.StepSummary{{Step: 1, Markdown: "## Tests\n1 failed"}},
		[]protocol.Annotation{{Level: protocol.AnnotationError, Message: "TestX failed", File: "x_test.go", Line: 7}}); err != nil {
		t.Fatal(err)
	}
	if err := g.srv.completeJob(ctx, j.ID, protocol.ResultFailure, "exit status 1", nil, steps); err != nil {
		t.Fatal(err)
	}
	// fail-fast cancels the other one.
	checks = waitChecks("finished check runs", map[string]string{"CI / build (1.22)": "completed/failure", "CI / build (1.23)": "completed/cancelled"})
	c = checks["CI / build (1.22)"]
	if c.Method != "PATCH" || c.Body.CompletedAt == "" || c.Body.StartedAt == "" || c.Body.Output == nil ||
		!strings.Contains(c.Body.Output.Summary, "> exit status 1") || !strings.Contains(c.Body.Output.Summary, "| Run go test ./... | ❌ failure |") {
		t.Errorf("failed check run %+v %+v", c, c.Body.Output)
	}
	// With the job's step summary and annotations.
	if out := c.Body.Output; out == nil || out.Text != "## Tests\n1 failed" || len(out.Annotations) != 1 ||
		out.Annotations[0] != (checkAnnotation{Path: "x_test.go", StartLine: 7, EndLine: 7, AnnotationLevel: "failure", Message: "TestX failed"}) {
		t.Errorf("failed check run's report %+v", out)
	}
	g.gh.mu.Lock()
	for _, r := range g.gh.checkReqs {
		if r.Method == "PATCH" && (r.Body.Name != "" || r.Body.HeadSHA != "") {
			t.Errorf("update changes the check run's name or commit: %+v", r.Body)
		}
	}
	g.gh.mu.Unlock()
	// Nothing changes, nothing is sent.
	n := g.gh.checkCount()
	if err := g.srv.syncChecks(ctx, run.ID); err != nil || g.gh.checkCount() != n {
		t.Errorf("sync without changes: %v, %d requests after %d", err, g.gh.checkCount(), n)
	}

	// Re-run on GitHub: one check run, then its whole suite.
	if code, body := g.hook("check_run", `{"action":"rerequested","check_run":{"id":1001},"repository":{"full_name":"gh/other"}}`,
		fakeWebhookSecret); code != 200 || body != `{"ok":true,"runs":[]}` {
		t.Errorf("re-request in another repository: %d %s", code, body)
	}
	code, body := g.hook("check_run", `{"action":"rerequested","check_run":{"id":1001},"repository":{"full_name":"GH/app"},"sender":{"login":"x"}}`,
		fakeWebhookSecret)
	var second *Run
	if err := g.srv.db.QueryRow(`SELECT id FROM runs WHERE number = ? AND attempt = 2`, run.Number).Scan(new(int64)); err != nil || code != 200 {
		t.Fatalf("re-request: %d %s, %v", code, body, err)
	} else {
		var id int64
		_ = g.srv.db.QueryRow(`SELECT id FROM runs WHERE number = ? AND attempt = 2`, run.Number).Scan(&id)
		second, _ = g.srv.getRun(id)
		if body != fmt.Sprintf(`{"ok":true,"runs":[%d]}`, id) {
			t.Errorf("re-request: %s", body)
		}
	}
	// The second attempt is still going, so it isn't re-run again.
	if code, body := g.hook("check_suite", `{"action":"rerequested","check_suite":{"id":500},"repository":{"full_name":"gh/app"}}`,
		fakeWebhookSecret); code != 200 || body != `{"ok":true,"runs":[]}` {
		t.Errorf("re-request while running: %d %s", code, body)
	}
	if err := g.srv.cancelRun(ctx, second.ID); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the second attempt's check runs", func() bool {
		var n int
		_ = g.srv.db.QueryRow(`SELECT count(*) FROM github_checks WHERE run_id = ? AND state = 'completed/cancelled'`, second.ID).Scan(&n)
		return n == 2
	})
	if code, body := g.hook("check_suite", `{"action":"rerequested","check_suite":{"id":500},"repository":{"full_name":"gh/app"}}`,
		fakeWebhookSecret); code != 200 || !strings.HasPrefix(body, `{"ok":true,"runs":[`) || body == `{"ok":true,"runs":[]}` {
		t.Errorf("re-request a suite: %d %s", code, body)
	}
	if code, body := g.hook("check_run", `{"action":"completed","check_run":{"id":1001},"repository":{"full_name":"gh/app"}}`,
		fakeWebhookSecret); code != 200 || body != `{"ok":true,"runs":[]}` {
		t.Errorf("completed check_run event: %d %s", code, body)
	}

	// GitHub failing doesn't hold up the run, and a later change catches up.
	g.gh.mu.Lock()
	g.gh.checksFail = true
	g.gh.mu.Unlock()
	sha, run = push("third")
	for _, j := range jobsOf(run.ID) {
		if j.Status != protocol.StatusCompleted {
			if err := g.srv.completeJob(ctx, j.ID, protocol.ResultSuccess, "", nil, nil); err != nil {
				t.Fatal(err)
			}
		}
	}
	if run, _ = g.srv.getRun(run.ID); run.Status != protocol.StatusCompleted || run.Conclusion != protocol.ResultSuccess {
		t.Errorf("run while GitHub fails: %s %s", run.Status, run.Conclusion)
	}
	g.gh.mu.Lock()
	g.gh.checksFail = false
	g.gh.mu.Unlock()
	g.srv.reportChecks(run.ID)
	waitFor(t, "check runs after GitHub recovers", func() bool {
		var n int
		_ = g.srv.db.QueryRow(`SELECT count(*) FROM github_checks WHERE run_id = ? AND state LIKE 'completed/%'`, run.ID).Scan(&n)
		return n == 3
	})
	for name, c := range g.gh.latestChecks() {
		if c.Body.HeadSHA == sha && c.Body.Status != "completed" {
			t.Errorf("%s: %s after GitHub recovered", name, c.Body.Status)
		}
	}

	// Settings → GitHub says what the app and its installations lack.
	var settings struct {
		App struct {
			Access *githubAccess `json:"access"`
		} `json:"app"`
	}
	g.do(nil, "GET", "/api/github/app", "", &settings)
	if a := settings.App.Access; a == nil || fmt.Sprint(a.Missing) != "[check_run events check_suite events]" ||
		a.PermissionsURL != g.gh.ts.URL+"/settings/apps/kiwicd-test/permissions" || len(a.Installations) != 1 || !a.Installations[0].Checks {
		t.Errorf("access %+v", a)
	}
	g.gh.mu.Lock()
	g.gh.noChecks = true
	g.gh.mu.Unlock()
	g.srv.gh.forget()
	g.do(nil, "GET", "/api/github/app", "", &settings)
	if a := settings.App.Access; a == nil || fmt.Sprint(a.Missing) != "[checks: write check_run events check_suite events]" ||
		len(a.Installations) != 1 || a.Installations[0].Checks || a.Installations[0].Account != "gh" {
		t.Errorf("access without checks %+v", a)
	}
	// Without the permission nothing is reported, and kiwicd waits before
	// asking again.
	_, run = push("fourth")
	if err := g.srv.syncChecks(ctx, run.ID); err != nil {
		t.Errorf("sync without the permission: %v", err)
	}
	g.srv.gh.mu.Lock()
	_, waiting := g.srv.gh.noChecks["gh/app"]
	g.srv.gh.mu.Unlock()
	var reported int
	_ = g.srv.db.QueryRow(`SELECT count(*) FROM github_checks WHERE run_id = ?`, run.ID).Scan(&reported)
	if !waiting || reported != 0 {
		t.Errorf("without the permission: waiting %v, %d check runs", waiting, reported)
	}
}

func TestCheckTarget(t *testing.T) {
	srv, err := New(Config{DataDir: filepath.Join(t.TempDir(), "data"), AdminPassword: "pw", Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	add := func(name, kind, url string, report bool) *Repo {
		t.Helper()
		if _, err := srv.db.Exec(`INSERT INTO repos (name, kind, url, webhook_secret, created_at, report_checks) VALUES (?, ?, ?, '', 0, ?)`,
			name, kind, url, report); err != nil {
			t.Fatal(err)
		}
		r, err := srv.getRepo(name)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	app := add("t/app", "mirror", "https://github.com/Owner/App.git", false)
	reporting := add("t/reporting", "mirror", "git@github.com:owner/reporting.git", true)
	other := add("t/elsewhere", "mirror", "https://gitlab.com/owner/elsewhere.git", false)
	hosted := add("t/hosted", "hosted", "", false)
	deploy := add("t/deploy", "hosted", "", false)
	deployReporting := add("t/deploy-reporting", "hosted", "", true)

	sha1 := strings.Repeat("a", 40)
	sha2 := strings.Repeat("b", 40)
	pr := map[string]any{"pull_request": map[string]any{"head": map[string]any{"sha": sha2}}, "repository": map[string]any{"full_name": "t/app"}}
	for _, tc := range []struct {
		name  string
		repo  *Repo
		run   Run
		want  checkTarget
		wantN bool
	}{
		{"push", app, Run{Event: "push", Sha: sha1, WorkflowFile: ".github/workflows/ci.yml"}, checkTarget{"Owner/App", sha1, ""}, false},
		{"dispatch", app, Run{Event: "workflow_dispatch", Sha: sha1}, checkTarget{"Owner/App", sha1, ""}, false},
		{"pull request head", app, Run{Event: "pull_request", Sha: sha1, EventJSON: pr}, checkTarget{"Owner/App", sha2, ""}, false},
		{"uploaded workflow", app, Run{Event: "push", Sha: sha1, WorkflowFile: "kiwicd:deploy.yml"}, checkTarget{}, true},
		{"uploaded workflow, reported", reporting, Run{Event: "push", Sha: sha1, WorkflowFile: "kiwicd:deploy.yml"},
			checkTarget{"owner/reporting", sha1, ""}, false},
		{"not GitHub", other, Run{Event: "push", Sha: sha1}, checkTarget{}, true},
		{"hosted", hosted, Run{Event: "push", Sha: sha1}, checkTarget{}, true},
		{"not a commit", app, Run{Event: "push", Sha: "main"}, checkTarget{}, true},
		{"repository_push", deploy, Run{Event: "repository_push", Sha: sha2,
			EventJSON: map[string]any{"after": sha1, "repository": map[string]any{"full_name": "t/app"}}}, checkTarget{}, true},
		{"repository_push, reported", deployReporting, Run{Event: "repository_push", Sha: sha2,
			EventJSON: map[string]any{"after": sha1, "repository": map[string]any{"full_name": "t/app"}}},
			checkTarget{"Owner/App", sha1, "t/deploy-reporting: "}, false},
		{"repository_pull_request, reported", deployReporting, Run{Event: "repository_pull_request", Sha: sha1, EventJSON: pr},
			checkTarget{"Owner/App", sha2, "t/deploy-reporting: "}, false},
		{"repository_push from a hosted repository", deployReporting, Run{Event: "repository_push", Sha: sha2,
			EventJSON: map[string]any{"after": sha1, "repository": map[string]any{"full_name": "t/hosted"}}}, checkTarget{}, true},
	} {
		got, ok := srv.checkTarget(&tc.run, tc.repo)
		if ok == tc.wantN || (ok && got != tc.want) {
			t.Errorf("%s: %+v %v, want %+v %v", tc.name, got, ok, tc.want, !tc.wantN)
		}
	}
}

func TestJobCheckRun(t *testing.T) {
	db, err := openDB(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	srv := &Server{cfg: Config{PublicURL: "https://ci.example/"}, db: db}
	run := &Run{ID: 3, WorkflowName: "CI"}
	target := checkTarget{repo: "o/r", sha: strings.Repeat("a", 40), prefix: "o/deploy: "}
	start := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	j := &Job{ID: 9, Name: "test (linux)", Status: protocol.StatusCompleted, Conclusion: protocol.ResultCancelled,
		StartedAt: start.UnixMilli(), FinishedAt: start.Add(83 * time.Second).UnixMilli(),
		Steps: []protocol.StepState{{Name: "a | b", Status: protocol.StatusInProgress}}}
	c := srv.jobCheckRun(run, j, target)
	if c.Name != "o/deploy: CI / test (linux)" || c.DetailsURL != "https://ci.example/runs/3/jobs/9" || c.Status != "completed" ||
		c.Conclusion != "cancelled" || c.StartedAt != "2026-01-02T03:04:05Z" || c.CompletedAt != "2026-01-02T03:05:28Z" {
		t.Errorf("check run %+v", c)
	}
	if c.Output.Title != "Cancelled" || !strings.HasPrefix(c.Output.Summary, "**test (linux)** cancelled after 1m23s.") ||
		!strings.Contains(c.Output.Summary, `| a \| b | in progress |`) {
		t.Errorf("output %+v", c.Output)
	}
	srv.cfg.PublicURL = ""
	if c := srv.jobCheckRun(run, &Job{ID: 1, Name: "x", Status: protocol.StatusQueued}, target); c.DetailsURL != "" || c.Status != "queued" {
		t.Errorf("queued without a public URL: %+v", c)
	}
}

// TestCalledJobChecks checks that a job calling a reusable workflow shows
// on GitHub as the called jobs, as there, unless it runs none.
func TestCalledJobChecks(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	g := newGitHubTest(t)
	g.createApp()
	ctx := context.Background()
	work := filepath.Join(t.TempDir(), "work")
	origin := filepath.Join(g.gh.origins, "gh", "app.git")
	git := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("", "init", "-q", "--bare", origin)
	git("", "init", "-q", "-b", "main", work)
	_ = os.MkdirAll(filepath.Join(work, ".github", "workflows"), 0o755)
	files := map[string]string{
		"ci.yml": "name: CI\non: push\njobs:\n  call:\n    uses: ./.github/workflows/lib.yml\n" +
			"  off:\n    if: false\n    uses: ./.github/workflows/lib.yml\n",
		"lib.yml": "on: workflow_call\njobs:\n  a:\n    runs-on: self-hosted\n    steps:\n      - run: true\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(work, ".github", "workflows", name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git(work, "add", ".")
	git(work, "commit", "-q", "-m", "first")
	git(work, "push", "-q", origin, "HEAD:main")
	if resp := g.do(nil, "POST", "/api/repos", `{"name":"gh/app","url":"`+g.gh.ts.URL+`/gh/app.git"}`, nil); resp.StatusCode != 200 {
		t.Fatalf("create mirror: %d", resp.StatusCode)
	}
	repo, err := g.srv.getRepo("gh/app")
	if err != nil {
		t.Fatal(err)
	}
	git(work, "commit", "-q", "--allow-empty", "-m", "second")
	git(work, "push", "-q", origin, "HEAD:main")
	if err := g.srv.syncRepo(ctx, repo.ID, false); err != nil {
		t.Fatal(err)
	}
	var checks map[string]checkReq
	waitFor(t, "check runs", func() bool {
		checks = g.gh.latestChecks()
		a, off := checks["CI / call / a"], checks["CI / off"]
		return a.Body.state() == "queued/" && off.Body.state() == "completed/skipped"
	})
	if _, ok := checks["CI / call"]; ok || len(checks) != 2 {
		t.Errorf("check runs: %v", checks)
	}
}
