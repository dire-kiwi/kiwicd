package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dire-kiwi/kiwicd/internal/protocol"
)

const previewWorkflow = `name: Preview
on:
  repository_pull_request:
    repositories: [t/app]
    types: [opened, synchronize, reopened, closed]
concurrency:
  group: pr-${{ github.event.number }}
  cancel-in-progress: true
jobs:
  preview:
    runs-on: self-hosted
    steps:
      - run: echo preview
`

// Takes pull requests from forks too.
const forkWorkflow = `on:
  repository_pull_request:
    repositories: [t/app]
    types: [opened]
    forks: true
jobs:
  check:
    runs-on: self-hosted
    steps:
      - run: echo check
`

func TestRepositoryPullRequest(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	defer func(d time.Duration) { pullRequestFetchRetry = d }(pullRequestFetchRetry)
	pullRequestFetchRetry = 10 * time.Millisecond
	tmp := t.TempDir()
	srv, err := New(Config{DataDir: filepath.Join(tmp, "data"), AdminPassword: "pw", Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	api := func(method, path, body string, out any) int {
		t.Helper()
		req, _ := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
		req.SetBasicAuth("admin", "pw")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		if out != nil {
			_ = json.Unmarshal(b, out)
		}
		return resp.StatusCode
	}
	git := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
			"GIT_TERMINAL_PROMPT=0")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}

	// t/app mirrors a repository with a main branch; pull requests live in
	// its refs/pull/N/head, as on GitHub.
	origin := filepath.Join(tmp, "origin")
	git(tmp, "init", "-q", "-b", "main", origin)
	git(origin, "commit", "-q", "--allow-empty", "-m", "init")
	newHead := func(number int, msg string) string {
		git(origin, "commit", "-q", "--allow-empty", "-m", msg)
		sha := git(origin, "rev-parse", "HEAD")
		git(origin, "update-ref", fmt.Sprintf("refs/pull/%d/head", number), sha)
		return sha
	}
	if code := api("POST", "/api/repos", `{"name":"t/app","url":"`+origin+`"}`, nil); code != 200 {
		t.Fatalf("create mirror: %d", code)
	}
	// t/deploy listens.
	if code := api("POST", "/api/repos", `{"name":"t/deploy"}`, nil); code != 200 {
		t.Fatalf("create t/deploy: %d", code)
	}
	deploy := filepath.Join(tmp, "deploy")
	git(tmp, "init", "-q", "-b", "main", deploy)
	_ = os.MkdirAll(filepath.Join(deploy, ".github/workflows"), 0o755)
	_ = os.WriteFile(filepath.Join(deploy, ".github/workflows/preview.yml"), []byte(previewWorkflow), 0o644)
	_ = os.WriteFile(filepath.Join(deploy, ".github/workflows/forks.yml"), []byte(forkWorkflow), 0o644)
	git(deploy, "add", "-A")
	git(deploy, "commit", "-qm", "listeners")
	git(deploy, "push", "-q", strings.Replace(ts.URL, "http://", "http://admin:pw@", 1)+"/git/t/deploy.git", "HEAD:main")
	api("POST", "/api/repos/t/deploy/sync", "", nil)

	var app, hosted Repo
	api("GET", "/api/repos/t/app", "", &app)
	api("GET", "/api/repos/t/deploy", "", &hosted)
	hook := func(repo *Repo, event, body string) int {
		t.Helper()
		mac := hmac.New(sha256.New, []byte(repo.WebhookSecret))
		mac.Write([]byte(body))
		req, _ := http.NewRequest("POST", ts.URL+"/api/hooks/"+repo.Name, strings.NewReader(body))
		req.Header.Set("X-GitHub-Event", event)
		req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	prEvent := func(action string, number int, sha, headRepo string) string {
		head := `null`
		if headRepo != "" {
			head = `{"full_name":"` + headRepo + `"}`
		}
		return fmt.Sprintf(`{"action":%q,"number":%d,"pull_request":{"number":%d,"title":"Add a thing","merged":false,`+
			`"head":{"ref":"feature","sha":%q,"repo":%s},"base":{"ref":"main","repo":{"full_name":"gh/app"}}},`+
			`"sender":{"login":"someone"},"repository":{"full_name":"gh/app"}}`, action, number, number, sha, head)
	}
	// runsFor waits for the webhook worker to create n runs of t/deploy.
	runsFor := func(n int) []*Run {
		t.Helper()
		var runs []*Run
		for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
			api("GET", "/api/runs?repo=t/deploy", "", &runs)
			if len(runs) >= n {
				break
			}
		}
		if len(runs) != n {
			t.Fatalf("t/deploy has %d runs, want %d: %+v", len(runs), n, runs)
		}
		for i, r := range runs {
			if runs[i], err = srv.getRun(r.ID); err != nil {
				t.Fatal(err)
			}
		}
		return runs // newest first
	}

	// Pull request events need a valid signature, a mirror and a pull request.
	sha1 := newHead(1, "First")
	if code := hook(&app, "pull_request", strings.Replace(prEvent("opened", 1, sha1, "gh/app"), "}", "", 1)); code != 400 {
		t.Errorf("bad payload: %d", code)
	}
	if code := hook(&hosted, "pull_request", prEvent("opened", 1, sha1, "gh/app")); code != 400 {
		t.Errorf("pull request on a hosted repository: %d", code)
	}
	wrong := app
	wrong.WebhookSecret = "nope"
	if code := hook(&wrong, "pull_request", prEvent("opened", 1, sha1, "gh/app")); code != 401 {
		t.Errorf("bad signature: %d", code)
	}

	// Opening a pull request runs both listeners, from t/deploy's main
	// branch, and fetches its head into the mirror.
	if code := hook(&app, "pull_request", prEvent("opened", 1, sha1, "gh/app")); code != 200 {
		t.Fatalf("webhook: %d", code)
	}
	runs := runsFor(2)
	var opened *Run
	for _, r := range runs {
		if r.WorkflowFile == ".github/workflows/preview.yml" {
			opened = r
		}
	}
	if opened == nil {
		t.Fatalf("no preview run: %+v", runs)
	}
	// The webhook's worker has handled the event once both runs have jobs.
	for _, r := range runs {
		waitFor(t, "the run's jobs", func() bool { jobs, _ := srv.runJobs(r.ID); return len(jobs) > 0 })
	}
	opened, _ = srv.getRun(opened.ID)
	ev := opened.EventJSON
	pr, _ := ev["pull_request"].(map[string]any)
	head, _ := pr["head"].(map[string]any)
	repo, _ := ev["repository"].(map[string]any)
	if opened.Event != "repository_pull_request" || opened.Ref != "refs/heads/main" || opened.Actor != "someone" ||
		opened.Title != "t/app #1 opened: Add a thing" || ev["action"] != "opened" || ev["number"] != 1.0 ||
		head["sha"] != sha1 || head["ref"] != "feature" || repo["full_name"] != "t/app" || opened.ConcurrencyGroup != "pr-1" {
		t.Fatalf("run: %+v", opened)
	}
	if got, err := resolveRef(context.Background(), srv.repoPath("t/app"), "refs/pull/1/head"); err != nil || got != sha1 {
		t.Errorf("mirror's refs/pull/1/head = %q %v, want %s", got, err, sha1)
	}
	// Later events are handled directly rather than through the worker, so
	// that they're done when the checks run.
	deliver := func(body string) {
		t.Helper()
		if err := srv.onPullRequest(context.Background(), app.ID, []byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	// The mirror's branches didn't change, so nothing else ran.
	var appRuns []*Run
	if api("GET", "/api/runs?repo=t/app", "", &appRuns); len(appRuns) != 0 {
		t.Errorf("t/app has runs: %+v", appRuns)
	}

	// The job's token fetches t/app once t/app lets t/deploy read it.
	if got := srv.jobReadableRepos(opened); len(got) != 1 {
		t.Errorf("t/deploy reads %v before t/app lets it", got)
	}
	if code := api("PATCH", "/api/repos/t/app", `{"readers":["t/deploy"]}`, nil); code != 200 {
		t.Fatalf("set readers: %d", code)
	}
	if got := srv.jobReadableRepos(opened); len(got) != 2 {
		t.Errorf("t/deploy reads %v", got)
	}
	jobs, _ := srv.runJobs(opened.ID)
	if len(jobs) != 1 || jobs[0].Status != protocol.StatusQueued {
		t.Fatalf("jobs of the first preview: %+v", jobs)
	}
	jobReq, err := srv.buildJobRequest(jobs[0], ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srv.db.Exec(`UPDATE jobs SET status = ? WHERE id = ?`, protocol.StatusInProgress, jobs[0].ID); err != nil {
		t.Fatal(err)
	}
	clone := filepath.Join(tmp, "clone")
	git(tmp, "init", "-q", clone)
	git(clone, "-c", "http.extraHeader=Authorization: Bearer "+jobReq.Secrets["KIWICD_TOKEN"], "fetch", "-q", ts.URL+"/git/t/app.git", "refs/pull/1/head")
	if got := git(clone, "rev-parse", "FETCH_HEAD"); got != sha1 {
		t.Errorf("fetched %s, want %s", got, sha1)
	}

	// A pull request from a fork, or from a deleted one, only runs the
	// listener that takes forks.
	fork := newHead(2, "From a fork")
	deliver(prEvent("opened", 2, fork, "someone/app"))
	runs = runsFor(3)
	if runs[0].WorkflowFile != ".github/workflows/forks.yml" {
		t.Errorf("a fork's pull request ran %s", runs[0].WorkflowFile)
	}
	deliver(prEvent("opened", 3, newHead(3, "Deleted fork"), ""))
	runs = runsFor(4)
	if runs[0].WorkflowFile != ".github/workflows/forks.yml" {
		t.Errorf("a deleted fork's pull request ran %s", runs[0].WorkflowFile)
	}

	// The preview's job, running since the fetch above, holds pr-1. A new
	// push to the pull request cancels it and waits for it to stop.
	deliver(prEvent("synchronize", 1, newHead(1, "Second"), "gh/app"))
	sync := runsFor(5)[0]
	if opened, _ = srv.getRun(opened.ID); !opened.Cancel || opened.Status == protocol.StatusCompleted {
		t.Errorf("the first preview wasn't cancelled: %+v", opened)
	}
	if jobs, _ := srv.runJobs(sync.ID); len(jobs) != 0 || sync.Status != protocol.StatusQueued {
		t.Errorf("the second preview didn't wait: %+v %+v", sync, jobs)
	}
	// Closing it before the first stops cancels the waiting one too.
	deliver(prEvent("closed", 1, git(origin, "rev-parse", "HEAD"), "gh/app"))
	closed := runsFor(6)[0]
	if sync, _ = srv.getRun(sync.ID); sync.Conclusion != protocol.ResultCancelled {
		t.Errorf("the waiting preview wasn't cancelled: %+v", sync)
	}
	if jobs, _ := srv.runJobs(closed.ID); len(jobs) != 0 || closed.EventJSON["action"] != "closed" {
		t.Errorf("the close didn't wait: %+v %+v", closed, jobs)
	}
	// Once the first preview's job stops, the close runs.
	if err := srv.completeJob(context.Background(), jobs[0].ID, protocol.ResultCancelled, "", nil, nil); err != nil {
		t.Fatal(err)
	}
	if jobs, _ := srv.runJobs(closed.ID); len(jobs) != 1 || jobs[0].Status != protocol.StatusQueued {
		t.Errorf("the close didn't start: %+v", jobs)
	}
	// Other pull requests' groups don't wait for pr-1.
	deliver(prEvent("synchronize", 4, newHead(4, "Another"), "gh/app"))
	if jobs, _ := srv.runJobs(runsFor(7)[0].ID); len(jobs) != 1 {
		t.Errorf("pr-4 waited: %+v", jobs)
	}

	// GitHub's form-encoded webhooks work too.
	body := "payload=" + url.QueryEscape(prEvent("reopened", 1, sha1, "gh/app"))
	mac := hmac.New(sha256.New, []byte(app.WebhookSecret))
	mac.Write([]byte(body))
	req, _ := http.NewRequest("POST", ts.URL+"/api/hooks/t/app", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-GitHub-Event", "pull_request")
	req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || runsFor(8)[0].EventJSON["action"] != "reopened" {
		t.Errorf("form-encoded webhook: %d", resp.StatusCode)
	}
}

// Without cancel-in-progress a new run waits for the running one, and only
// the newest waits.
func TestConcurrencyQueue(t *testing.T) {
	srv, err := New(Config{DataDir: t.TempDir(), AdminPassword: "pw", Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srv.db.Exec(`INSERT INTO repos (name, kind, default_branch, webhook_secret, created_at) VALUES ('t/x', 'hosted', 'main', 's', 0)`); err != nil {
		t.Fatal(err)
	}
	repo, _ := srv.getRepo("t/x")
	ctx := context.Background()
	start := func(yaml string) *Run {
		t.Helper()
		id, err := srv.createRun(ctx, runSpec{repo: repo, file: "w.yml", yaml: yaml, name: "w", event: "push", ref: "refs/heads/main", sha: "abc"})
		if err != nil {
			t.Fatal(err)
		}
		run, _ := srv.getRun(id)
		return run
	}
	jobsOf := func(run *Run) []*Job {
		jobs, _ := srv.runJobs(run.ID)
		return jobs
	}
	const deploy = "on: push\nconcurrency: deploy-${{ github.ref_name }}\njobs:\n  a: {runs-on: x, steps: [{run: echo}]}\n"
	a, b := start(deploy), start(deploy)
	if a.ConcurrencyGroup != "deploy-main" || len(jobsOf(a)) != 1 || len(jobsOf(b)) != 0 {
		t.Fatalf("a: %+v %v, b: %v", a, jobsOf(a), jobsOf(b))
	}
	c := start(deploy)
	if a, _ = srv.getRun(a.ID); a.Cancel {
		t.Error("the running run was cancelled")
	}
	if b, _ = srv.getRun(b.ID); b.Conclusion != protocol.ResultCancelled || len(jobsOf(c)) != 0 {
		t.Errorf("b: %+v, c: %v", b, jobsOf(c))
	}
	// Other groups and runs without one don't wait.
	if other := start("on: push\njobs:\n  a: {runs-on: x, steps: [{run: echo}]}\n"); len(jobsOf(other)) != 1 {
		t.Error("a run without a group waited")
	}
	// GitHub's usual group gets each push run a group of its own, from its
	// run ID, so they don't cancel each other.
	const perRun = "on: push\nconcurrency:\n  group: ${{ github.workflow }}-${{ github.head_ref || github.run_id }}\n" +
		"  cancel-in-progress: true\njobs:\n  a: {runs-on: x, steps: [{run: echo}]}\n"
	r1, r2 := start(perRun), start(perRun)
	if r1, _ = srv.getRun(r1.ID); r1.Cancel || r1.ConcurrencyGroup != fmt.Sprintf("w-%d", r1.ID) || len(jobsOf(r1)) != 1 ||
		r2.ConcurrencyGroup != fmt.Sprintf("w-%d", r2.ID) || len(jobsOf(r2)) != 1 {
		t.Errorf("runs grouped by run ID: %+v %+v", r1, r2)
	}
	// So does run_number.
	if r := start("on: push\nconcurrency: n-${{ github.run_number }}\njobs:\n  a: {runs-on: x, steps: [{run: echo}]}\n"); r.ConcurrencyGroup != fmt.Sprintf("n-%d", r.Number) {
		t.Errorf("group by run number: %q, number %d", r.ConcurrencyGroup, r.Number)
	}
	if err := srv.completeJob(ctx, jobsOf(a)[0].ID, protocol.ResultSuccess, "", nil, nil); err != nil {
		t.Fatal(err)
	}
	if len(jobsOf(c)) != 1 {
		t.Error("c didn't start after a")
	}
	// A group that can't be evaluated fails the run.
	if bad := start("on: push\nconcurrency: ${{ fromJSON('x') }}\njobs:\n  a: {runs-on: x, steps: [{run: echo}]}\n"); bad.Conclusion != protocol.ResultFailure ||
		!strings.Contains(bad.Error, "concurrency") {
		t.Errorf("bad group: %+v", bad)
	}
}
