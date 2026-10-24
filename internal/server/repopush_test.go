package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dire-kiwi/kiwicd/internal/protocol"
)

const listenerWorkflow = `name: Deploy app
on:
  repository_push:
    repositories: [t/app]
    branches: [main]
  workflow_dispatch:
jobs:
  deploy:
    runs-on: self-hosted
    steps:
      - run: echo deploy
`

func TestRepositoryPush(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
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
	runsOf := func(repo string) []*Run {
		t.Helper()
		var runs []*Run
		api("GET", "/api/runs?repo="+repo, "", &runs)
		return runs
	}
	gitIn := func(dir string, args ...string) error {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
			"GIT_TERMINAL_PROMPT=0")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Logf("git %v: %v\n%s", args, err, out)
		}
		return err
	}
	// commit writes files into a fresh commit of repo's work tree and pushes it.
	commit := func(repo, branch, msg string, files map[string]string) string {
		t.Helper()
		dir := filepath.Join(tmp, "src", repo)
		if _, err := os.Stat(dir); err != nil {
			_ = os.MkdirAll(dir, 0o755)
			if gitIn(dir, "init", "-q", "-b", "main") != nil {
				t.Fatal("git init")
			}
		}
		for name, content := range files {
			_ = os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755)
			_ = os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644)
		}
		remote := strings.Replace(ts.URL, "http://", "http://admin:pw@", 1) + "/git/" + repo + ".git"
		if gitIn(dir, "add", "-A") != nil || gitIn(dir, "commit", "-qm", msg, "--allow-empty") != nil ||
			gitIn(dir, "push", "-q", remote, "HEAD:"+branch) != nil {
			t.Fatal("commit and push")
		}
		api("POST", "/api/repos/"+repo+"/sync", "", nil)
		out, _ := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
		return strings.TrimSpace(string(out))
	}

	for _, name := range []string{"t/app", "t/deploy", "t/other"} {
		if code := api("POST", "/api/repos", `{"name":"`+name+`"}`, nil); code != 200 {
			t.Fatalf("create %s: %d", name, code)
		}
	}
	deploySHA := commit("t/deploy", "main", "listener", map[string]string{
		".github/workflows/deploy.yml": listenerWorkflow,
		".github/workflows/broken.yml": "on:\n  repository_push:\n    repositories: [t/app]\njobs: [\n",
		// Names t/app only as part of longer names, so pushes to t/app don't report it.
		".github/workflows/broken-other.yml": "on:\n  repository_push:\n    repositories: [t/app-legacy, xt/app]\njobs: [\n",
	})
	// A listener that's only on another branch of t/other is ignored.
	commit("t/other", "main", "init", map[string]string{"README": "x"})
	commit("t/other", "feature", "listener on a branch", map[string]string{".github/workflows/l.yml": listenerWorkflow})
	// Pushing a listener doesn't run it, but reports it if it's broken.
	if runs := runsOf("t/deploy"); len(runs) != 2 || runs[0].Event != "push" || runs[1].Event != "push" {
		t.Fatalf("runs after pushing the listener: %+v", runs)
	}

	appSHA := commit("t/app", "main", "Fix the thing", map[string]string{"app.txt": "v1"})
	commit("t/app", "dev", "Not deployed", map[string]string{"app.txt": "v2"})
	if runs := runsOf("t/app"); len(runs) != 0 {
		t.Errorf("t/app has runs: %+v", runs)
	}
	if runs := runsOf("t/other"); len(runs) != 0 {
		t.Errorf("a listener on a branch ran: %+v", runs)
	}
	runs := runsOf("t/deploy")
	var run, broken *Run
	for _, r := range runs {
		switch {
		case r.Event != "repository_push":
		case r.WorkflowFile == ".github/workflows/deploy.yml":
			run = r
		case r.WorkflowFile == ".github/workflows/broken.yml":
			broken = r
		}
	}
	// The broken one is reported for both pushes to t/app, as broken push
	// workflows are; the one for other repositories isn't.
	if len(runs) != 5 || run == nil || broken == nil {
		t.Fatalf("runs of t/deploy: %+v", runs)
	}
	if broken.Conclusion != protocol.ResultFailure || !strings.Contains(broken.Error, "Invalid workflow") {
		t.Errorf("broken listener: %+v", broken)
	}
	run, err = srv.getRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	repo, _ := run.EventJSON["repository"].(map[string]any)
	if run.Event != "repository_push" || run.Ref != "refs/heads/main" || run.Sha != deploySHA || run.WorkflowName != "Deploy app" ||
		run.Title != "t/app: Fix the thing" || run.EventJSON["after"] != appSHA || run.EventJSON["ref"] != "refs/heads/main" ||
		repo["full_name"] != "t/app" {
		t.Fatalf("run: %+v", run)
	}

	// The job's token fetches its own repository, and the one it listens to
	// once that lists it as a reader, only while the job runs, and never pushes.
	jobs, err := srv.runJobs(run.ID)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("jobs: %v %v", jobs, err)
	}
	req, err := srv.buildJobRequest(jobs[0], ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	token := req.Secrets["KIWICD_TOKEN"]
	if token == "" {
		t.Fatal("no KIWICD_TOKEN")
	}
	fetch := func(repo, auth string) error {
		t.Helper()
		return gitIn(tmp, "-c", "http.extraHeader=Authorization: "+auth, "ls-remote", ts.URL+"/git/"+repo+".git")
	}
	bearer := "Bearer " + token
	if fetch("t/app", bearer) == nil {
		t.Error("a queued job's token fetched")
	}
	if _, err := srv.db.Exec(`UPDATE jobs SET status = ? WHERE id = ?`, protocol.StatusInProgress, jobs[0].ID); err != nil {
		t.Fatal(err)
	}
	if fetch("t/deploy", bearer) != nil {
		t.Error("the token can't fetch its own repository")
	}
	if fetch("t/app", bearer) == nil {
		t.Error("the token fetched t/app before t/app let t/deploy read it")
	}
	// Readers are checked: owner/name each; duplicates and the repository itself are dropped.
	if code := api("PATCH", "/api/repos/t/app", `{"readers":["not a repo"]}`, nil); code != 400 {
		t.Errorf("bad reader: %d", code)
	}
	var app Repo
	if code := api("PATCH", "/api/repos/t/app", `{"readers":["t/deploy"," T/Deploy ","t/app","t/x.git",""]}`, &app); code != 200 ||
		strings.Join(app.Readers, ",") != "t/deploy,t/x" {
		t.Fatalf("set readers: %d %+v", code, app.Readers)
	}
	// Reading t/other needs both its consent and the workflow listening to it.
	if code := api("PATCH", "/api/repos/t/other", `{"readers":["t/deploy"]}`, nil); code != 200 {
		t.Fatalf("set readers of t/other: %d", code)
	}
	if fetch("t/app", bearer) != nil {
		t.Error("the token can't fetch t/app")
	}
	basic := strings.Replace(ts.URL, "http://", "http://x-access-token:"+token+"@", 1)
	if gitIn(tmp, "ls-remote", basic+"/git/t/app.git") != nil {
		t.Error("the token doesn't work as a basic auth password")
	}
	if fetch("t/other", bearer) == nil || fetch("t/app", "Bearer nope") == nil {
		t.Error("fetched without access")
	}
	appDir := filepath.Join(tmp, "src", "t/app")
	if gitIn(appDir, "push", "-q", basic+"/git/t/app.git", "HEAD:refs/heads/pwned") == nil {
		t.Error("the token pushed")
	}
	if err := srv.completeJob(context.Background(), jobs[0].ID, protocol.ResultSuccess, "", nil, nil); err != nil {
		t.Fatal(err)
	}
	if fetch("t/app", bearer) == nil {
		t.Error("a finished job's token fetched")
	}

	// Push runs only read their own repository, whatever their workflow says,
	// and so do runs from refs that aren't protected.
	deployRun := func(event, ref string) *Run {
		return &Run{Repo: "t/deploy", RepoID: run.RepoID, Event: event, Ref: ref, Protected: ref == "refs/heads/main", WorkflowYAML: listenerWorkflow}
	}
	for event, want := range map[string]int{"push": 1, "workflow_dispatch": 2, "repository_push": 2} {
		if got := srv.jobReadableRepos(deployRun(event, "refs/heads/main")); len(got) != want {
			t.Errorf("%s run reads %v", event, got)
		}
	}
	if got := srv.jobReadableRepos(deployRun("workflow_dispatch", "refs/heads/feature")); len(got) != 1 {
		t.Errorf("a dispatch from an unprotected branch reads %v", got)
	}
	// Other listeners don't get t/app's consent to t/deploy.
	other, _ := srv.getRepo("t/other")
	if got := srv.jobReadableRepos(&Run{Repo: "t/other", RepoID: other.ID, Event: "repository_push", Ref: "refs/heads/main", Protected: true,
		WorkflowYAML: listenerWorkflow}); len(got) != 1 {
		t.Errorf("t/other's listener reads %v", got)
	}
	// Removing the reader takes the access away.
	if code := api("PATCH", "/api/repos/t/app", `{"readers":[]}`, &app); code != 200 || len(app.Readers) != 0 {
		t.Fatalf("clear readers: %d %+v", code, app.Readers)
	}
	if got := srv.jobReadableRepos(deployRun("repository_push", "refs/heads/main")); len(got) != 1 {
		t.Errorf("t/deploy reads %v after t/app removed it", got)
	}

	// A new commit on the listener's default branch replaces the cached
	// workflows: the listener now deploys t/app's dev branch.
	countDeploys := func() int {
		n := 0
		for _, r := range runsOf("t/deploy") {
			if r.Event == "repository_push" && r.WorkflowFile == ".github/workflows/deploy.yml" {
				n++
			}
		}
		return n
	}
	before := countDeploys()
	commit("t/deploy", "main", "deploy dev", map[string]string{
		".github/workflows/deploy.yml": strings.Replace(listenerWorkflow, "branches: [main]", "branches: [dev]", 1),
	})
	commit("t/app", "dev", "Deployed now", map[string]string{"app.txt": "v3"})
	if got := countDeploys(); got != before+1 {
		t.Errorf("deploys after changing the listener: %d, want %d", got, before+1)
	}
}

// The server works with a relative data directory, though git commands run in
// other directories.
func TestRelativeDataDir(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	tmp := t.TempDir()
	t.Chdir(tmp)
	srv, err := New(Config{DataDir: "data", AdminPassword: "pw", Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	req, _ := http.NewRequest("POST", ts.URL+"/api/repos", strings.NewReader(`{"name":"t/build"}`))
	req.SetBasicAuth("admin", "pw")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("create repository: %d", resp.StatusCode)
	}

	src := filepath.Join(tmp, "src")
	remote := strings.Replace(ts.URL, "http://", "http://admin:pw@", 1) + "/git/t/build.git"
	for _, args := range [][]string{
		{"init", "-q", "-b", "main", src},
		{"-C", src, "commit", "-q", "--allow-empty", "-m", "init"},
		{"-C", src, "push", "-q", remote, "HEAD:main"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
			"GIT_TERMINAL_PROMPT=0")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	want, _ := exec.Command("git", "-C", src, "rev-parse", "HEAD").Output()
	if sha, err := resolveRef(context.Background(), srv.repoPath("t/build"), "refs/heads/main"); err != nil ||
		sha != strings.TrimSpace(string(want)) {
		t.Errorf("pushed main: %q %v, want %s", sha, err, want)
	}
}
