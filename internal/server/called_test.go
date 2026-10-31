package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/dire-kiwi/kiwicd/internal/protocol"
)

const callerWorkflow = `name: CI
on: push
jobs:
  setup:
    runs-on: self-hosted
    steps:
      - run: echo setup
  build:
    needs: setup
    strategy:
      matrix:
        os: [linux, mac]
    uses: ./.github/workflows/build.yml
    with:
      os: ${{ matrix.os }}
      debug: true
    secrets:
      token: ${{ secrets.TOKEN }}
  shared:
    uses: t/shared/.github/workflows/lib.yml@main
    with:
      who: app
  remote:
    uses: gh/pub/.github/workflows/remote.yml@v1
    secrets: inherit
  report:
    needs: [build, shared, remote]
    runs-on: self-hosted
    steps:
      - run: echo report
`

const buildWorkflow = `on:
  workflow_call:
    inputs:
      os:
        type: string
        required: true
      debug:
        type: boolean
      level:
        type: number
        default: 3
    secrets:
      token:
        required: true
    outputs:
      artifact:
        value: ${{ jobs.compile.outputs.file }}-${{ inputs.os }}
jobs:
  compile:
    runs-on: self-hosted
    outputs:
      file: ${{ steps.x.outputs.file }}
    steps:
      - id: x
        run: echo "file=app" >> "$GITHUB_OUTPUT"
  test:
    needs: compile
    if: inputs.debug
    uses: ./.github/workflows/test.yml
    secrets: inherit
`

const testWorkflow = `on: workflow_call
jobs:
  unit:
    runs-on: self-hosted
    steps:
      - run: echo unit
`

const libWorkflow = `on:
  workflow_call:
    inputs:
      who:
        type: string
        required: true
    outputs:
      greeting:
        value: hello ${{ inputs.who }}
jobs:
  hello:
    runs-on: self-hosted
    steps:
      - run: echo "hello ${{ inputs.who }}"
`

const remoteWorkflow = `on: workflow_call
jobs:
  deploy:
    runs-on: self-hosted
    steps:
      - run: echo deploy
`

// TestReusableWorkflows runs a workflow that calls local, kiwicd and GitHub
// reusable workflows through the scheduler, with a fake agent.
func TestReusableWorkflows(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	tmp := t.TempDir()
	remoteSHA := strings.Repeat("c", 40)
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Header.Get("Authorization") != "":
			w.WriteHeader(401)
		case r.URL.Path == "/repos/gh/pub/commits/v1":
			_ = json.NewEncoder(w).Encode(map[string]string{"sha": remoteSHA})
		case r.URL.Path == "/repos/gh/pub/contents/.github/workflows/remote.yml" && r.URL.Query().Get("ref") == remoteSHA:
			_ = json.NewEncoder(w).Encode(map[string]string{"encoding": "base64", "content": base64.StdEncoding.EncodeToString([]byte(remoteWorkflow))})
		default:
			w.WriteHeader(404)
			_, _ = w.Write([]byte(`{"message":"Not Found"}`))
		}
	}))
	defer gh.Close()
	srv, err := New(Config{DataDir: filepath.Join(tmp, "data"), AdminPassword: "pw", Version: "test", GitHubAPIURL: gh.URL})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	ctx := context.Background()
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
	remote := strings.Replace(ts.URL, "http://", "http://admin:pw@", 1) + "/git/"
	// commit pushes files to a repository's branch and returns the commit.
	commit := func(repo, branch string, files map[string]string) string {
		t.Helper()
		dir := filepath.Join(tmp, "src", repo)
		git := func(args ...string) string {
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
		if _, err := os.Stat(dir); err != nil {
			_ = os.MkdirAll(dir, 0o755)
			git("init", "-q", "-b", "main")
		}
		for name, content := range files {
			_ = os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755)
			_ = os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644)
		}
		git("add", "-A")
		git("commit", "-q", "--allow-empty", "-m", "to "+branch)
		git("push", "-q", "-f", remote+repo+".git", "HEAD:refs/heads/"+branch)
		api("POST", "/api/repos/"+repo+"/sync", "", nil)
		return git("rev-parse", "HEAD")
	}
	for _, name := range []string{"t/app", "t/shared", "t/private"} {
		if code := api("POST", "/api/repos", `{"name":"`+name+`"}`, nil); code != 200 {
			t.Fatalf("create %s: %d", name, code)
		}
	}
	api("PATCH", "/api/repos/t/shared", `{"readers":["t/app"]}`, nil)
	api("PUT", "/api/secrets?repo=t/app", `{"name":"TOKEN","value":"repo-secret"}`, nil)
	api("PUT", "/api/secrets", `{"name":"GLOBAL","value":"global-secret"}`, nil)
	commit("t/shared", "main", map[string]string{".github/workflows/lib.yml": libWorkflow})
	commit("t/private", "main", map[string]string{".github/workflows/lib.yml": libWorkflow})

	runner := &Runner{ID: 1, Name: "r", Labels: normalizeLabels(nil, "linux", "amd64")}
	// take assigns every queued job, by name, as an agent speaking proto.
	take := func(proto int) map[string]*protocol.JobRequest {
		t.Helper()
		out := map[string]*protocol.JobRequest{}
		for {
			req, err := srv.assignJob(ctx, runner, proto, ts.URL)
			if err != nil {
				t.Fatal(err)
			}
			if req == nil {
				return out
			}
			out[req.DisplayName] = req
		}
	}
	names := func(m map[string]*protocol.JobRequest) string {
		var out []string
		for n := range m {
			out = append(out, n)
		}
		sort.Strings(out)
		return strings.Join(out, ", ")
	}
	complete := func(req *protocol.JobRequest, outputs map[string]string) {
		t.Helper()
		if err := srv.completeJob(ctx, req.JobID, protocol.ResultSuccess, "", outputs, nil); err != nil {
			t.Fatal(err)
		}
	}
	lastRun := func() *Run {
		t.Helper()
		var runs []*Run
		api("GET", "/api/runs?repo=t/app", "", &runs)
		if len(runs) == 0 {
			t.Fatal("no run")
		}
		run, _ := srv.getRun(runs[0].ID)
		return run
	}
	jobsByName := func(runID int64) map[string]*Job {
		t.Helper()
		jobs, err := srv.runJobs(runID)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]*Job{}
		for _, j := range jobs {
			out[j.Name] = j
		}
		return out
	}

	appFiles := map[string]string{
		".github/workflows/ci.yml":    callerWorkflow,
		".github/workflows/build.yml": buildWorkflow,
		".github/workflows/test.yml":  testWorkflow,
	}
	commit("t/app", "main", appFiles)
	run := lastRun()
	if run.Error != "" {
		t.Fatalf("run failed: %s", run.Error)
	}

	// Jobs without needs start: setup, and the jobs of the workflows that
	// shared and remote call. An old agent only gets setup.
	old := take(1)
	if got := names(old); got != "setup" {
		t.Fatalf("an old agent got %q", got)
	}
	reqs := take(protocol.Version)
	if got := names(reqs); got != "remote / deploy, shared / hello" {
		t.Fatalf("first jobs: %q", got)
	}
	hello := reqs["shared / hello"]
	if hello.JobKey != "hello" || hello.WorkflowYAML != libWorkflow || !reflect.DeepEqual(hello.Inputs, map[string]any{"who": "app"}) {
		t.Errorf("shared / hello: key %q inputs %v yaml %q", hello.JobKey, hello.Inputs, hello.WorkflowYAML)
	}
	// No secrets: block passes none; inherit passes them all.
	if len(hello.Secrets) != 1 || hello.Secrets[jobTokenSecret] == "" {
		t.Errorf("shared / hello's secrets: %v", hello.Secrets)
	}
	deploy := reqs["remote / deploy"]
	if deploy.WorkflowYAML != remoteWorkflow || deploy.Secrets["TOKEN"] != "repo-secret" || deploy.Secrets["GLOBAL"] != "global-secret" {
		t.Errorf("remote / deploy: secrets %v yaml %q", deploy.Secrets, deploy.WorkflowYAML)
	}
	complete(hello, nil)
	complete(deploy, nil)
	jobs := jobsByName(run.ID)
	if j := jobs["shared"]; j.Status != protocol.StatusCompleted || j.Conclusion != protocol.ResultSuccess || j.Outputs["greeting"] != "hello app" {
		t.Errorf("shared: %+v", j)
	}

	// setup done, build's matrix calls build.yml twice.
	complete(old["setup"], nil)
	jobs = jobsByName(run.ID)
	for _, leg := range []string{"linux", "mac"} {
		j := jobs["build ("+leg+")"]
		if j == nil || j.Uses != "./.github/workflows/build.yml" || j.Status != protocol.StatusInProgress ||
			!reflect.DeepEqual(j.Inputs, map[string]any{"os": leg, "debug": true, "level": 3.0}) {
			t.Errorf("build (%s): %+v", leg, j)
		}
	}
	reqs = take(protocol.Version)
	if got := names(reqs); got != "build (linux) / compile, build (mac) / compile" {
		t.Fatalf("build's jobs: %q", got)
	}
	compile := reqs["build (linux) / compile"]
	if compile.WorkflowYAML != buildWorkflow || compile.Inputs["os"] != "linux" || compile.Inputs["debug"] != true ||
		!reflect.DeepEqual(compile.Secrets, map[string]string{"token": "repo-secret", jobTokenSecret: compile.Secrets[jobTokenSecret]}) {
		t.Errorf("build (linux) / compile: inputs %v secrets %v", compile.Inputs, compile.Secrets)
	}
	complete(compile, map[string]string{"file": "app"})
	complete(reqs["build (mac) / compile"], map[string]string{"file": "app"})

	// The nested workflow's job sees its siblings and gets what build passed.
	reqs = take(protocol.Version)
	if got := names(reqs); got != "build (linux) / test / unit, build (mac) / test / unit" {
		t.Fatalf("test's jobs: %q", got)
	}
	unit := reqs["build (linux) / test / unit"]
	if unit.WorkflowYAML != testWorkflow || len(unit.Inputs) != 0 || unit.Inputs == nil || len(unit.JobResults) != 0 ||
		unit.Secrets["token"] != "repo-secret" || unit.Secrets["TOKEN"] != "" {
		t.Errorf("unit: inputs %v results %v secrets %v", unit.Inputs, unit.JobResults, unit.Secrets)
	}
	for _, r := range reqs {
		complete(r, nil)
	}
	jobs = jobsByName(run.ID)
	if j := jobs["build (linux)"]; j.Status != protocol.StatusCompleted || j.Conclusion != protocol.ResultSuccess || j.Outputs["artifact"] != "app-linux" {
		t.Errorf("build (linux): %+v", j)
	}

	// report needs the calling jobs and sees their outputs.
	reqs = take(protocol.Version)
	report := reqs["report"]
	if report == nil {
		t.Fatalf("after build: %q", names(reqs))
	}
	if r := report.JobResults["build"]; r.Result != protocol.ResultSuccess || !strings.HasPrefix(r.Outputs["artifact"], "app-") {
		t.Errorf("report's needs.build: %+v", r)
	}
	if r := report.JobResults["shared"]; r.Outputs["greeting"] != "hello app" {
		t.Errorf("report's needs.shared: %+v", r)
	}
	complete(report, nil)
	if run, _ = srv.getRun(run.ID); run.Status != protocol.StatusCompleted || run.Conclusion != protocol.ResultSuccess {
		t.Errorf("run: %s %s", run.Status, run.Conclusion)
	}

	// The run's plan has the called workflows' jobs under the calling ones.
	var detail struct {
		Plan []plannedJob `json:"plan"`
	}
	api("GET", fmt.Sprintf("/api/runs/%d", run.ID), "", &detail)
	planned := map[string]plannedJob{}
	for _, p := range detail.Plan {
		planned[p.Key] = p
	}
	if b := planned["build"]; b.Uses != "./.github/workflows/build.yml" || len(b.Jobs) != 2 || b.Jobs[1].Key != "test" ||
		len(b.Jobs[1].Jobs) != 1 || b.Jobs[1].Jobs[0].Key != "unit" {
		t.Errorf("build's plan: %+v", b)
	}

	// A re-run calls the same workflows.
	var rerun struct {
		RunID int64 `json:"run_id"`
	}
	api("POST", fmt.Sprintf("/api/runs/%d/rerun", run.ID), "", &rerun)
	if called, err := srv.calledWorkflows(rerun.RunID); err != nil || len(called) != 4 || called["remote"].Sha != remoteSHA {
		t.Errorf("re-run's called workflows: %v %v", called, err)
	}
	srv.cancelRun(ctx, rerun.RunID)

	// A run from a ref that isn't protected passes no secrets down.
	commit("t/app", "feature", nil)
	feature := lastRun()
	take(protocol.Version)
	for _, j := range jobsByName(feature.ID) {
		if j.Key == "setup" {
			_ = srv.completeJob(ctx, j.ID, protocol.ResultSuccess, "", nil, nil)
		}
	}
	reqs = take(protocol.Version)
	if c := reqs["build (linux) / compile"]; c == nil || c.Secrets["token"] != "" || len(c.Notices) != 1 {
		t.Errorf("feature's compile: %+v", c)
	}
	srv.cancelRun(ctx, feature.ID)

	// A failing matrix leg cancels the other one's called workflow (fail-fast).
	commit("t/app", "main", nil)
	ff := lastRun()
	for _, r := range take(protocol.Version) {
		complete(r, nil)
	}
	reqs = take(protocol.Version)
	if err := srv.completeJob(ctx, reqs["build (linux) / compile"].JobID, protocol.ResultFailure, "", nil, nil); err != nil {
		t.Fatal(err)
	}
	jobs = jobsByName(ff.ID)
	if j := jobs["build (linux)"]; j.Conclusion != protocol.ResultFailure {
		t.Errorf("build (linux) after its compile failed: %+v", j)
	}
	if j := jobs["build (mac) / compile"]; !j.Cancel {
		t.Errorf("build (mac) / compile isn't being cancelled: %+v", j)
	}
	complete(reqs["build (mac) / compile"], nil)
	jobs = jobsByName(ff.ID)
	if j := jobs["build (mac) / test"]; j == nil || j.Conclusion != protocol.ResultCancelled {
		t.Errorf("build (mac) / test: %+v", j)
	}
	if run, _ := srv.getRun(ff.ID); run.Status != protocol.StatusCompleted || run.Conclusion != protocol.ResultFailure {
		t.Errorf("fail-fast run: %s %s", run.Status, run.Conclusion)
	}
}

// TestReusableWorkflowErrors checks the calls a run refuses when it starts,
// and inputs of the wrong type when the calling job does.
func TestReusableWorkflowErrors(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	tmp := t.TempDir()
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
		_, _ = w.Write([]byte(`{"message":"Not Found"}`))
	}))
	defer gh.Close()
	srv, err := New(Config{DataDir: filepath.Join(tmp, "data"), AdminPassword: "pw", Version: "test", GitHubAPIURL: gh.URL})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	call := func(path string) string {
		return fmt.Sprintf("on:\n  workflow_call:\njobs:\n  next:\n    uses: ./.github/workflows/%s\n", path)
	}
	leaf := "on:\n  workflow_call:\n    inputs:\n      flag:\n        type: boolean\n      name:\n        type: string\n        required: true\njobs:\n  a:\n    runs-on: self-hosted\n    steps:\n      - run: true\n"
	files := map[string]string{
		"loop.yml":  call("loop.yml"),
		"d1.yml":    call("d2.yml"),
		"d2.yml":    call("d3.yml"),
		"leaf.yml":  leaf,
		"plain.yml": "on: push\njobs:\n  a:\n    runs-on: self-hosted\n    steps:\n      - run: true\n",
	}
	files["d3.yml"] = call("leaf.yml") + "    with:\n      name: x\n"
	dir := filepath.Join(tmp, "src")
	_ = os.MkdirAll(filepath.Join(dir, ".github/workflows"), 0o755)
	for name, content := range files {
		_ = os.WriteFile(filepath.Join(dir, ".github/workflows", name), []byte(content), 0o644)
	}
	if _, err := srv.db.Exec(`INSERT INTO repos (name, kind, webhook_secret, created_at) VALUES ('t/app', 'hosted', 'x', 0), ('t/private', 'hosted', 'x', 0)`); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"t/app", "t/private"} {
		if err := srv.initHostedRepo(ctx, name); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"add", "-A"}, {"commit", "-qm", "x"},
		{"push", "-q", srv.repoPath("t/app"), "HEAD:refs/heads/main"}, {"push", "-q", srv.repoPath("t/private"), "HEAD:refs/heads/main"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	sha, err := resolveRef(ctx, srv.repoPath("t/app"), "main")
	if err != nil {
		t.Fatal(err)
	}
	repo, err := srv.getRepo("t/app")
	if err != nil {
		t.Fatal(err)
	}
	start := func(jobs string) *Run {
		t.Helper()
		id, err := srv.createRun(ctx, runSpec{repo: repo, file: ".github/workflows/ci.yml", yaml: "on: push\njobs:\n" + jobs, name: "CI",
			event: "push", payload: map[string]any{}, ref: "refs/heads/main", sha: sha, actor: "t"})
		if err != nil {
			t.Fatal(err)
		}
		run, err := srv.getRun(id)
		if err != nil {
			t.Fatal(err)
		}
		return run
	}
	for _, tc := range []struct{ jobs, want string }{
		{"  a:\n    uses: ./.github/workflows/loop.yml\n", "job a / next: ./.github/workflows/loop.yml: workflows call each other in a loop"},
		{"  a:\n    uses: ./.github/workflows/d1.yml\n", "job a / next / next / next: ./.github/workflows/leaf.yml: reusable workflows can only be nested 4 levels deep"},
		{"  a:\n    uses: ./.github/workflows/plain.yml\n", "it isn't a reusable workflow"},
		{"  a:\n    uses: ./.github/workflows/leaf.yml\n", `the called workflow's input "name" is required`},
		{"  a:\n    uses: ./.github/workflows/leaf.yml\n    with:\n      name: x\n      other: y\n", `the called workflow has no input "other"`},
		{"  a:\n    uses: ./.github/workflows/leaf.yml\n    with:\n      name: x\n    secrets:\n      s: y\n", `the called workflow has no secret "s"`},
		{"  a:\n    uses: ./.github/workflows/missing.yml\n", "t/app has no .github/workflows/missing.yml"},
		{"  a:\n    uses: ./.github/workflows/leaf.yml@main\n", "a local workflow is"},
		{"  a:\n    uses: t/private/.github/workflows/leaf.yml@main\n", "t/private doesn't list t/app as a reader"},
		{"  a:\n    uses: t/app/.github/workflows/leaf.yml@nope\n", "t/app has no branch, tag or commit nope"},
		{"  a:\n    uses: gh/x/.github/workflows/leaf.yml@v1\n", "not found on GitHub"},
	} {
		run := start(tc.jobs)
		if run.Status != protocol.StatusCompleted || !strings.Contains(run.Error, tc.want) {
			t.Errorf("%s: %s %q, want an error with %q", tc.jobs, run.Status, run.Error, tc.want)
		}
	}

	// Three levels below the run's workflow are fine.
	if run := start("  a:\n    uses: ./.github/workflows/d2.yml\n"); run.Error != "" {
		t.Errorf("d2: %s", run.Error)
	}
	// The run's own repository at another ref is fine too.
	if run := start("  a:\n    uses: t/app/.github/workflows/leaf.yml@main\n    with:\n      name: x\n"); run.Error != "" {
		t.Errorf("t/app@main: %s", run.Error)
	}
	// Inputs of the wrong type fail the calling job.
	run := start("  a:\n    uses: ./.github/workflows/leaf.yml\n    with:\n      name: x\n      flag: yes please\n")
	jobs, _ := srv.runJobs(run.ID)
	if len(jobs) != 1 || jobs[0].Conclusion != protocol.ResultFailure || !strings.Contains(jobs[0].Error, "with.flag: expected a boolean") {
		t.Errorf("wrong type: %+v", jobs)
	}
	if run, _ = srv.getRun(run.ID); run.Conclusion != protocol.ResultFailure {
		t.Errorf("wrong type run: %s %s", run.Status, run.Conclusion)
	}
}
