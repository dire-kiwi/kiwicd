package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// triggerTest is a server with hosted repositories that tests push to.
type triggerTest struct {
	t   *testing.T
	tmp string
	srv *Server
	ts  *httptest.Server
}

func newTriggerTest(t *testing.T, repos ...string) *triggerTest {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	tmp := t.TempDir()
	srv, err := New(Config{DataDir: filepath.Join(tmp, "data"), AdminPassword: "pw", Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	tt := &triggerTest{t: t, tmp: tmp, srv: srv, ts: httptest.NewServer(srv.Handler())}
	t.Cleanup(tt.ts.Close)
	for _, name := range repos {
		if code := tt.api("POST", "/api/repos", `{"name":"`+name+`"}`, nil); code != 200 {
			t.Fatalf("create %s: %d", name, code)
		}
	}
	return tt
}

func (tt *triggerTest) api(method, path, body string, out any) int {
	tt.t.Helper()
	req, _ := http.NewRequest(method, tt.ts.URL+path, strings.NewReader(body))
	req.SetBasicAuth("admin", "pw")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		tt.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if out != nil {
		_ = json.Unmarshal(b, out)
	}
	return resp.StatusCode
}

// git runs git in repo's work tree, which it creates the first time.
func (tt *triggerTest) git(repo string, args ...string) string {
	tt.t.Helper()
	dir := filepath.Join(tt.tmp, "src", repo)
	if _, err := os.Stat(dir); err != nil {
		_ = os.MkdirAll(dir, 0o755)
		tt.git(repo, "init", "-q", "-b", "main")
	}
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
		"GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		tt.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// push pushes refspecs of repo's work tree to kiwicd, and then syncs the
// repository (a push does that too, but in the background).
func (tt *triggerTest) push(repo string, refspec ...string) {
	tt.t.Helper()
	remote := strings.Replace(tt.ts.URL, "http://", "http://admin:pw@", 1) + "/git/" + repo + ".git"
	tt.git(repo, append([]string{"push", "-q", remote}, refspec...)...)
	tt.api("POST", "/api/repos/"+repo+"/sync", "", nil)
}

// commit commits files on the work tree's current branch and pushes it to
// branch.
func (tt *triggerTest) commit(repo, branch, msg string, files map[string]string) string {
	tt.t.Helper()
	tt.git(repo, "status") // creates the work tree
	dir := filepath.Join(tt.tmp, "src", repo)
	for name, content := range files {
		_ = os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755)
		_ = os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644)
	}
	tt.git(repo, "add", "-A")
	tt.git(repo, "commit", "-qm", msg, "--allow-empty")
	tt.push(repo, "HEAD:"+branch)
	return tt.git(repo, "rev-parse", "HEAD")
}

func (tt *triggerTest) runs(repo string) []*Run {
	tt.t.Helper()
	var runs []*Run
	tt.api("GET", "/api/runs?repo="+repo, "", &runs)
	return runs
}

// runsOf returns repo's runs for event, oldest first, with what the API
// leaves out.
func (tt *triggerTest) runsOf(repo, event string) []*Run {
	tt.t.Helper()
	var out []*Run
	for _, r := range tt.runs(repo) {
		if r.Event != event {
			continue
		}
		full, err := tt.srv.getRun(r.ID)
		if err != nil {
			tt.t.Fatal(err)
		}
		out = append([]*Run{full}, out...)
	}
	return out
}

// settle waits until the server has handled the workflow_run activity so
// far.
func (tt *triggerTest) settle() {
	tt.t.Helper()
	waitFor(tt.t, "workflow_run events", func() bool { return len(tt.srv.wrQueue) == 0 })
	time.Sleep(50 * time.Millisecond) // the one the worker took
}

func field(m map[string]any, path ...string) any {
	var v any = m
	for _, k := range path {
		mm, _ := v.(map[string]any)
		v = mm[k]
	}
	return v
}

const queuedJob = "jobs:\n  a:\n    runs-on: self-hosted\n    steps: [{run: echo}]\n"

// skippedJob finishes as soon as it starts: its only job is skipped.
const skippedJob = "jobs:\n  a:\n    if: false\n    runs-on: self-hosted\n    steps: [{run: echo}]\n"

func TestWorkflowRun(t *testing.T) {
	tt := newTriggerTest(t, "t/app", "t/loop")
	sha := tt.commit("t/app", "main", "Add workflows", map[string]string{
		".github/workflows/ci.yml":     "name: CI\non: push\n" + skippedJob,
		".github/workflows/deploy.yml": "name: Deploy\non:\n  workflow_run:\n    workflows: [CI]\n    types: [completed]\n    branches: [main]\n" + queuedJob,
		".github/workflows/watch.yml":  "on:\n  workflow_run:\n    workflows: [CI]\n    types: [requested]\n" + queuedJob,
	})
	tt.settle()
	ci := tt.runsOf("t/app", "push")
	runs := tt.runsOf("t/app", "workflow_run")
	if len(ci) != 1 || len(runs) != 2 {
		t.Fatalf("push runs %+v, workflow_run runs %+v", ci, runs)
	}
	watch, deploy := runs[0], runs[1]
	if watch.WorkflowFile != ".github/workflows/watch.yml" || field(watch.EventJSON, "action") != "requested" ||
		field(watch.EventJSON, "workflow_run", "status") != "queued" || field(watch.EventJSON, "workflow_run", "conclusion") != nil {
		t.Errorf("requested run %+v", watch)
	}
	if deploy.WorkflowName != "Deploy" || deploy.Ref != "refs/heads/main" || deploy.Sha != sha || !deploy.Protected ||
		field(deploy.EventJSON, "action") != "completed" || field(deploy.EventJSON, "workflow_run", "conclusion") != "success" ||
		field(deploy.EventJSON, "workflow_run", "id") != float64(ci[0].ID) || field(deploy.EventJSON, "workflow_run", "head_branch") != "main" ||
		field(deploy.EventJSON, "workflow_run", "head_sha") != sha || field(deploy.EventJSON, "workflow", "name") != "CI" {
		t.Errorf("completed run %+v", deploy)
	}
	if deploy.TriggerDepth != 1 || deploy.Title != ci[0].Title {
		t.Errorf("depth %d, title %q", deploy.TriggerDepth, deploy.Title)
	}
	var wfs []workflowInfo
	tt.api("GET", "/api/repos/t/app/workflows", "", &wfs)
	if len(wfs) != 3 || wfs[1].File != ".github/workflows/deploy.yml" ||
		wfs[1].EventDetails["workflow_run"] != "workflows: CI · types: completed · branches: main" {
		t.Errorf("workflows %+v", wfs)
	}

	// CI on another branch: watch runs, from main, but deploy's branches
	// leave it out.
	tt.git("t/app", "checkout", "-q", "-b", "dev")
	devSHA := tt.commit("t/app", "dev", "On dev", nil)
	tt.settle()
	runs = tt.runsOf("t/app", "workflow_run")
	if len(runs) != 3 || runs[2].WorkflowFile != ".github/workflows/watch.yml" || runs[2].Ref != "refs/heads/main" || runs[2].Sha != sha ||
		field(runs[2].EventJSON, "workflow_run", "head_branch") != "dev" || field(runs[2].EventJSON, "workflow_run", "head_sha") != devSHA {
		t.Errorf("workflow_run runs after a push to dev: %+v", runs)
	}

	// A workflow following itself stops after three levels, as on GitHub.
	tt.commit("t/loop", "main", "Loop", map[string]string{
		".github/workflows/loop.yml": "name: Loop\non:\n  workflow_dispatch:\n  workflow_run:\n    workflows: [Loop]\n    types: [completed]\n" + skippedJob,
	})
	if code := tt.api("POST", "/api/repos/t/loop/dispatch", `{"workflow":"loop.yml"}`, nil); code != 200 {
		t.Fatalf("dispatch: %d", code)
	}
	waitFor(t, "the loop", func() bool { return len(tt.runsOf("t/loop", "workflow_run")) == 3 })
	tt.settle()
	if runs := tt.runsOf("t/loop", "workflow_run"); len(runs) != 3 || runs[2].TriggerDepth != 3 {
		t.Errorf("loop runs: %+v", runs)
	}

	// A re-run keeps its depth.
	var rerun struct {
		RunID int64 `json:"run_id"`
	}
	loop := tt.runsOf("t/loop", "workflow_run")
	tt.api("POST", fmt.Sprintf("/api/runs/%d/rerun", loop[2].ID), "", &rerun)
	tt.settle()
	if r, _ := tt.srv.getRun(rerun.RunID); r == nil || r.TriggerDepth != 3 || len(tt.runsOf("t/loop", "workflow_run")) != 4 {
		t.Errorf("re-run %+v; runs %d", r, len(tt.runsOf("t/loop", "workflow_run")))
	}
}

func TestRepositoryDispatch(t *testing.T) {
	tt := newTriggerTest(t, "t/app", "t/empty")
	sha := tt.commit("t/app", "main", "Add workflows", map[string]string{
		".github/workflows/deploy.yml": "name: Deploy\non:\n  repository_dispatch:\n    types: [deploy, rollback]\n" + queuedJob,
		".github/workflows/any.yml":    "on: repository_dispatch\n" + queuedJob,
		".github/workflows/push.yml":   "on: push\n" + queuedJob,
	})
	// The listeners on another branch don't count.
	tt.git("t/app", "checkout", "-q", "-b", "dev")
	tt.commit("t/app", "dev", "Change", map[string]string{".github/workflows/dev.yml": "on: repository_dispatch\n" + queuedJob})

	var out struct {
		RunIDs []int64 `json:"run_ids"`
	}
	if code := tt.api("POST", "/api/repos/t/app/dispatches", `{"event_type":"deploy","client_payload":{"env":"prod","n":2}}`, &out); code != 200 ||
		len(out.RunIDs) != 2 {
		t.Fatalf("dispatch: %d %+v", code, out)
	}
	runs := tt.runsOf("t/app", "repository_dispatch")
	if len(runs) != 2 {
		t.Fatalf("runs %+v", runs)
	}
	for _, r := range runs {
		if r.Ref != "refs/heads/main" || r.Sha != sha || r.Title != "deploy" || r.Actor != "admin" || field(r.EventJSON, "action") != "deploy" ||
			field(r.EventJSON, "client_payload", "env") != "prod" || field(r.EventJSON, "branch") != "main" {
			t.Errorf("run %+v", r)
		}
	}
	// Only the workflow without types takes other event types.
	if code := tt.api("POST", "/api/repos/t/app/dispatches", `{"event_type":"build"}`, &out); code != 200 || len(out.RunIDs) != 1 {
		t.Errorf("dispatch build: %d %+v", code, out)
	}
	if r, _ := tt.srv.getRun(out.RunIDs[0]); r == nil || r.WorkflowFile != ".github/workflows/any.yml" || field(r.EventJSON, "client_payload") == nil {
		t.Errorf("build run %+v", r)
	}
	if code := tt.api("POST", "/api/repos/t/empty/dispatches", `{"event_type":"deploy"}`, &out); code != 200 || len(out.RunIDs) != 0 {
		t.Errorf("dispatch to a repository without commits: %d %+v", code, out)
	}
	for _, body := range []string{`{}`, `{"event_type":" "}`, `{"event_type":"x","client_payload":[1]}`, `{"event_type":"` + strings.Repeat("x", 101) + `"}`} {
		if code := tt.api("POST", "/api/repos/t/app/dispatches", body, nil); code != 400 {
			t.Errorf("%s: %d", body, code)
		}
	}
	if code := tt.api("POST", "/api/repos/t/nope/dispatches", `{"event_type":"x"}`, nil); code != 404 {
		t.Errorf("unknown repository: %d", code)
	}
}

func TestCreateAndDelete(t *testing.T) {
	tt := newTriggerTest(t, "t/app")
	refs := "name: Refs\non: [create, delete]\n" + queuedJob
	main := tt.commit("t/app", "main", "Add workflows", map[string]string{".github/workflows/refs.yml": refs})
	// The first push to a hosted repository creates its branch, too.
	if runs := tt.runsOf("t/app", "create"); len(runs) != 1 || field(runs[0].EventJSON, "ref") != "main" {
		t.Fatalf("create runs after the first push: %+v", runs)
	}

	// A new branch runs its own commit's workflows, at its ref.
	tt.git("t/app", "checkout", "-q", "-b", "feature")
	feature := tt.commit("t/app", "feature", "Feature", map[string]string{
		".github/workflows/refs.yml": "name: Feature refs\non: [create, delete]\n" + queuedJob})
	runs := tt.runsOf("t/app", "create")
	if len(runs) != 2 {
		t.Fatalf("create runs %+v", runs)
	}
	created := runs[1]
	if created.Ref != "refs/heads/feature" || created.Sha != feature || created.WorkflowName != "Feature refs" || created.Protected ||
		field(created.EventJSON, "ref") != "feature" || field(created.EventJSON, "ref_type") != "branch" ||
		field(created.EventJSON, "master_branch") != "main" || created.Title != "Created branch feature" {
		t.Errorf("create run %+v", created)
	}
	// Pushing to it again doesn't create it.
	tt.commit("t/app", "feature", "More", nil)
	if n := len(tt.runsOf("t/app", "create")); n != 2 {
		t.Errorf("%d create runs after another push", n)
	}

	// Tags too.
	tt.git("t/app", "tag", "v1", main)
	tt.push("t/app", "refs/tags/v1")
	if runs := tt.runsOf("t/app", "create"); len(runs) != 3 || runs[2].Ref != "refs/tags/v1" || field(runs[2].EventJSON, "ref_type") != "tag" {
		t.Errorf("create runs after a tag %+v", runs)
	}

	// Deleting runs the default branch's workflows.
	tt.push("t/app", ":refs/heads/feature")
	tt.push("t/app", ":refs/tags/v1")
	runs = tt.runsOf("t/app", "delete")
	if len(runs) != 2 {
		t.Fatalf("delete runs %+v", runs)
	}
	if runs[0].Ref != "refs/heads/main" || runs[0].Sha != main || runs[0].WorkflowName != "Refs" || !runs[0].Protected ||
		field(runs[0].EventJSON, "ref") != "feature" || field(runs[0].EventJSON, "ref_type") != "branch" || runs[0].Title != "Deleted branch feature" {
		t.Errorf("delete run %+v", runs[0])
	}
	if field(runs[1].EventJSON, "ref") != "v1" || field(runs[1].EventJSON, "ref_type") != "tag" {
		t.Errorf("delete run %+v", runs[1])
	}
	var repo Repo
	tt.api("GET", "/api/repos/t/app", "", &repo)
	if _, ok := repo.Refs["refs/heads/feature"]; ok {
		t.Errorf("refs %v", repo.Refs)
	}
}

func TestRunBranch(t *testing.T) {
	for _, c := range []struct {
		run  Run
		want string
	}{
		{Run{Event: "push", Ref: "refs/heads/release/1"}, "release/1"},
		{Run{Event: "push", Ref: "refs/tags/v1"}, "v1"},
		{Run{Event: "pull_request", Ref: "refs/pull/3/merge",
			EventJSON: map[string]any{"pull_request": map[string]any{"head": map[string]any{"ref": "feature"}}}}, "feature"},
		{Run{Event: "pull_request", Ref: "refs/pull/3/merge"}, "refs/pull/3/merge"},
	} {
		if got := runBranch(&c.run); got != c.want {
			t.Errorf("%+v: %q, want %q", c.run, got, c.want)
		}
	}
}
