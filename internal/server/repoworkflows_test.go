package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestRepoWorkflowsOff checks that a repository whose repo_workflows setting
// is off runs none of its .github/workflows/ files, listeners included, but
// still runs its uploaded workflows.
func TestRepoWorkflowsOff(t *testing.T) {
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
	commit := func(repo, msg string, files map[string]string) {
		t.Helper()
		dir := filepath.Join(tmp, "src", repo)
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
		if _, err := os.Stat(dir); err != nil {
			_ = os.MkdirAll(dir, 0o755)
			git("init", "-q", "-b", "main")
		}
		for name, content := range files {
			_ = os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755)
			_ = os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644)
		}
		git("add", "-A")
		git("commit", "-qm", msg, "--allow-empty")
		git("push", "-q", strings.Replace(ts.URL, "http://", "http://admin:pw@", 1)+"/git/"+repo+".git", "HEAD:main")
		api("POST", "/api/repos/"+repo+"/sync", "", nil)
	}
	const ci = "name: CI\non: push\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo test\n"

	for _, name := range []string{"t/app", "t/deploy"} {
		var repo Repo
		if code := api("POST", "/api/repos", `{"name":"`+name+`"}`, &repo); code != 200 || !repo.RepoWorkflows {
			t.Fatalf("create %s: %d %+v", name, code, repo)
		}
	}
	var repo Repo
	if code := api("PATCH", "/api/repos/t/app", `{"repo_workflows":false}`, &repo); code != 200 || repo.RepoWorkflows {
		t.Fatalf("turn off t/app's workflows: %d %+v", code, repo)
	}
	if code := api("PATCH", "/api/repos/t/deploy", `{"repo_workflows":false}`, nil); code != 200 {
		t.Fatalf("turn off t/deploy's workflows: %d", code)
	}
	commit("t/deploy", "listener", map[string]string{".github/workflows/deploy.yml": listenerWorkflow})
	commit("t/app", "ci", map[string]string{".github/workflows/ci.yml": ci})
	if runs := runsOf("t/app"); len(runs) != 0 {
		t.Errorf("t/app's own workflow ran: %+v", runs)
	}
	if runs := runsOf("t/deploy"); len(runs) != 0 {
		t.Errorf("t/deploy's listener ran: %+v", runs)
	}
	var workflows []workflowInfo
	if api("GET", "/api/repos/t/app/workflows", "", &workflows); len(workflows) != 0 {
		t.Errorf("workflows listed: %+v", workflows)
	}

	// Uploaded workflows still run.
	if code := api("PUT", "/api/repos/t/app/uploaded-workflows/up.yml", strings.Replace(ci, "CI", "Uploaded", 1), nil); code != 200 {
		t.Fatalf("upload: %d", code)
	}
	commit("t/app", "second", nil)
	if runs := runsOf("t/app"); len(runs) != 1 || runs[0].WorkflowFile != uploadedPrefix+"up.yml" {
		t.Errorf("runs after uploading: %+v", runs)
	}

	// Turned back on, the repository's own workflows run again.
	if code := api("PATCH", "/api/repos/t/app", `{"repo_workflows":true}`, nil); code != 200 {
		t.Fatalf("turn on: %d", code)
	}
	if code := api("PATCH", "/api/repos/t/deploy", `{"repo_workflows":true}`, nil); code != 200 {
		t.Fatalf("turn on t/deploy: %d", code)
	}
	commit("t/app", "third", nil)
	files := map[string]bool{}
	for _, r := range runsOf("t/app") {
		files[r.WorkflowFile] = true
	}
	if !files[".github/workflows/ci.yml"] {
		t.Errorf("t/app's own workflow didn't run once turned on: %v", files)
	}
	if runs := runsOf("t/deploy"); len(runs) != 1 || runs[0].Event != "repository_push" {
		t.Errorf("t/deploy's listener didn't run once turned on: %+v", runs)
	}
}
