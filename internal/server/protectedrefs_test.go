package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestRefProtected(t *testing.T) {
	repo := &Repo{DefaultBranch: "main", ProtectedRefs: []string{"release/*", "refs/tags/v*", "stable"}}
	for ref, want := range map[string]bool{
		"refs/heads/main":         true,
		"refs/heads/main2":        false,
		"refs/heads/release/1.0":  true,
		"refs/heads/release/1/x":  false, // * doesn't match /
		"refs/heads/stable":       true,
		"refs/heads/feature":      false,
		"refs/tags/v1.2":          true,
		"refs/tags/release/1.0":   false,
		"refs/heads/refs/tags/v1": false,
		"refs/pull/1/head":        false,
		"":                        false,
	} {
		if got := refProtected(repo, ref); got != want {
			t.Errorf("refProtected(%q) = %v, want %v", ref, got, want)
		}
	}
	if !refProtected(&Repo{DefaultBranch: "main", ProtectedRefs: []string{"**"}}, "refs/heads/a/b") {
		t.Error("** doesn't protect every branch")
	}

	got, err := cleanProtectedRefs([]string{" stable ", "release/*", "stable", "", "refs/tags/v*"})
	if err != nil || strings.Join(got, ",") != "refs/tags/v*,release/*,stable" {
		t.Errorf("cleanProtectedRefs: %v %v", got, err)
	}
	for _, bad := range []string{"!main", "refs/pull/*", "["} {
		if _, err := cleanProtectedRefs([]string{bad}); err == nil {
			t.Errorf("cleanProtectedRefs accepted %q", bad)
		}
	}
}

const secretsWorkflow = `on:
  push:
  workflow_dispatch:
jobs:
  a:
    runs-on: self-hosted
    steps:
      - run: echo "${{ secrets.TOKEN }}"
`

// TestSecretsOnlyForProtectedRefs checks that only runs from protected refs
// get secrets.
func TestSecretsOnlyForProtectedRefs(t *testing.T) {
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
	dir := filepath.Join(tmp, "src")
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
			"GIT_TERMINAL_PROMPT=0")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	remote := strings.Replace(ts.URL, "http://", "http://admin:pw@", 1) + "/git/t/r.git"
	// push pushes HEAD to branch and returns the run it started.
	push := func(branch string) *Run {
		t.Helper()
		git("commit", "-q", "--allow-empty", "-m", "to "+branch)
		git("push", "-q", "-f", remote, "HEAD:refs/heads/"+branch)
		api("POST", "/api/repos/t/r/sync", "", nil)
		var runs []*Run
		api("GET", "/api/runs?repo=t/r", "", &runs)
		if len(runs) == 0 || runs[0].Ref != "refs/heads/"+branch {
			t.Fatalf("no run for the push to %s: %+v", branch, runs)
		}
		return runs[0]
	}
	// secrets returns the secrets the run's first job gets, and its notices.
	secrets := func(run *Run) (map[string]string, []string) {
		t.Helper()
		jobs, err := srv.runJobs(run.ID)
		if err != nil || len(jobs) == 0 {
			t.Fatalf("jobs of run %d: %v %v", run.ID, jobs, err)
		}
		req, err := srv.buildJobRequest(jobs[0], ts.URL)
		if err != nil {
			t.Fatal(err)
		}
		return req.Secrets, req.Notices
	}

	if code := api("POST", "/api/repos", `{"name":"t/r"}`, nil); code != 200 {
		t.Fatalf("create repo: %d", code)
	}
	api("PUT", "/api/secrets?repo=t/r", `{"name":"TOKEN","value":"repo-secret"}`, nil)
	api("PUT", "/api/secrets", `{"name":"GLOBAL","value":"global-secret"}`, nil)
	_ = os.MkdirAll(filepath.Join(dir, ".github/workflows"), 0o755)
	git("init", "-q", "-b", "main")
	_ = os.WriteFile(filepath.Join(dir, ".github/workflows/ci.yml"), []byte(secretsWorkflow), 0o644)
	git("add", "-A")

	// The default branch gets the repository's and the global secrets.
	run := push("main")
	got, notices := secrets(run)
	if !run.Protected || got["TOKEN"] != "repo-secret" || got["GLOBAL"] != "global-secret" || got[jobTokenSecret] == "" || len(notices) != 0 {
		t.Errorf("default branch run: protected=%v secrets=%v notices=%v", run.Protected, got, notices)
	}

	// Another branch gets only KIWICD_TOKEN, and its log says why.
	run = push("feature")
	got, notices = secrets(run)
	if run.Protected || len(got) != 1 || got[jobTokenSecret] == "" || len(notices) != 1 || !strings.Contains(notices[0], "refs/heads/feature") {
		t.Errorf("feature branch run: protected=%v secrets=%v notices=%v", run.Protected, got, notices)
	}
	// So does a dispatch on it.
	var dispatched struct {
		RunID int64 `json:"run_id"`
	}
	if code := api("POST", "/api/repos/t/r/dispatch", `{"workflow":"ci.yml","ref":"feature"}`, &dispatched); code != 200 {
		t.Fatalf("dispatch: %d", code)
	}
	if d, _ := srv.getRun(dispatched.RunID); d.Protected {
		t.Error("a dispatch on feature is protected")
	} else if got, _ := secrets(d); len(got) != 1 {
		t.Errorf("a dispatch on feature got %v", got)
	}

	// Protecting the branch gives its new runs secrets; the repository lists
	// which of its branches are protected.
	var repo Repo
	if code := api("PATCH", "/api/repos/t/r", `{"protected_refs":["!x"]}`, nil); code != 400 {
		t.Errorf("negated pattern: %d", code)
	}
	if code := api("PATCH", "/api/repos/t/r", `{"protected_refs":["feat*"]}`, &repo); code != 200 ||
		strings.Join(repo.ProtectedRefs, ",") != "feat*" || strings.Join(repo.ProtectedBranches, ",") != "feature,main" {
		t.Fatalf("protect feat*: %d %+v %+v", code, repo.ProtectedRefs, repo.ProtectedBranches)
	}
	// Runs created before don't get them.
	if got, _ := secrets(run); len(got) != 1 {
		t.Errorf("an unprotected run got %v after protecting its branch", got)
	}
	run = push("feature")
	if got, _ := secrets(run); !run.Protected || got["TOKEN"] != "repo-secret" {
		t.Errorf("protected feature run: protected=%v secrets=%v", run.Protected, got)
	}
	// Unprotecting it takes them away from runs that haven't started yet.
	if code := api("PATCH", "/api/repos/t/r", `{"protected_refs":[]}`, &repo); code != 200 || len(repo.ProtectedRefs) != 0 {
		t.Fatalf("unprotect: %d %+v", code, repo.ProtectedRefs)
	}
	if got, _ := secrets(run); len(got) != 1 {
		t.Errorf("a run got %v after its branch was unprotected", got)
	}
	// A rerun is decided afresh.
	var rerun struct {
		RunID int64 `json:"run_id"`
	}
	if code := api("POST", "/api/runs/"+strconv.FormatInt(run.ID, 10)+"/rerun", "", &rerun); code != 200 {
		t.Fatalf("rerun: %d", code)
	}
	if r, _ := srv.getRun(rerun.RunID); r.Protected {
		t.Error("a rerun from an unprotected branch is protected")
	}
}
