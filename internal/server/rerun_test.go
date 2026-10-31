package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/dire-kiwi/kiwicd/internal/protocol"
)

const rerunWorkflow = `name: CI
on: push
jobs:
  build:
    runs-on: self-hosted
    steps:
      - run: echo build
  test:
    needs: build
    runs-on: self-hosted
    strategy:
      fail-fast: false
      matrix:
        n: [1, 2, 3]
    steps:
      - run: echo test
  deploy:
    needs: test
    runs-on: self-hosted
    steps:
      - run: echo deploy
  lint:
    runs-on: self-hosted
    steps:
      - run: echo lint
`

// newRerunServer starts a server with a repository t/app, and returns a
// function that calls its API.
func newRerunServer(t *testing.T) (*Server, *httptest.Server, func(method, path, body string, out any) int) {
	t.Helper()
	srv, err := New(Config{DataDir: filepath.Join(t.TempDir(), "data"), AdminPassword: "pw", Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.db.Close() })
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	api := func(method, path, body string, out any) int {
		t.Helper()
		req, _ := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
		req.SetBasicAuth("admin", "pw")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if out != nil {
			_ = json.NewDecoder(resp.Body).Decode(out)
		}
		return resp.StatusCode
	}
	if code := api("POST", "/api/repos", `{"name":"t/app"}`, nil); code != 200 {
		t.Fatalf("create repo: %d", code)
	}
	return srv, ts, api
}

// TestRerunSomeJobs: re-running failed jobs runs the failed matrix
// combination and what needs it, keeping the other jobs' results, outputs
// and logs; re-running one job runs it and what needs it.
func TestRerunSomeJobs(t *testing.T) {
	srv, ts, api := newRerunServer(t)
	ctx := context.Background()
	repo, err := srv.getRepo("t/app")
	if err != nil {
		t.Fatal(err)
	}
	runID, err := srv.createRun(ctx, runSpec{repo: repo, file: ".github/workflows/ci.yml", yaml: rerunWorkflow, name: "CI", event: "push",
		payload: map[string]any{}, ref: "refs/heads/main", sha: strings.Repeat("a", 40), actor: "t"})
	if err != nil {
		t.Fatal(err)
	}
	rn := &Runner{ID: 1, Name: "r", Labels: normalizeLabels(nil, "linux", "amd64")}
	// work runs every queued job, giving it a line of log and the result
	// result picks, and returns the requests the runner got.
	work := func(result func(*Job) string) map[string]*protocol.JobRequest {
		t.Helper()
		got := map[string]*protocol.JobRequest{}
		for {
			req, err := srv.assignJob(ctx, rn, ts.URL)
			if err != nil {
				t.Fatal(err)
			}
			if req == nil {
				return got
			}
			j, _ := srv.getJob(req.JobID)
			got[j.Name] = req
			if err := srv.appendLogs(j.ID, []protocol.LogLine{{Step: 1, TS: 1, Text: fmt.Sprintf("ran %s in run %d", j.Name, j.RunID)}}); err != nil {
				t.Fatal(err)
			}
			if err := srv.completeJob(ctx, j.ID, result(j), "", map[string]string{"by": fmt.Sprint(j.RunID)}, nil); err != nil {
				t.Fatal(err)
			}
		}
	}
	jobsOf := func(runID int64) map[string]*Job {
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
	names := func(m map[string]*protocol.JobRequest) string {
		var out []string
		for k := range m {
			out = append(out, k)
		}
		sort.Strings(out)
		return strings.Join(out, ",")
	}
	rerun := func(id int64, body string) (int64, int) {
		t.Helper()
		var res struct {
			RunID int64 `json:"run_id"`
		}
		code := api("POST", fmt.Sprintf("/api/runs/%d/rerun", id), body, &res)
		return res.RunID, code
	}

	// Some jobs can't be re-run while the run is going.
	if _, code := rerun(runID, `{"failed":true}`); code != http.StatusConflict {
		t.Errorf("re-running failed jobs of a running run: %d", code)
	}
	ran := work(func(j *Job) string {
		if j.Name == "test (2)" {
			return protocol.ResultFailure
		}
		return protocol.ResultSuccess
	})
	if names(ran) != "build,lint,test (1),test (2),test (3)" {
		t.Fatalf("first attempt ran %s", names(ran))
	}
	first := jobsOf(runID)
	if first["deploy"].Conclusion != protocol.ResultSkipped {
		t.Fatalf("deploy: %+v", first["deploy"])
	}

	second, code := rerun(runID, `{"failed":true}`)
	if code != 200 {
		t.Fatalf("re-run failed jobs: %d", code)
	}
	ran = work(func(*Job) string { return protocol.ResultSuccess })
	if names(ran) != "deploy,test (2)" {
		t.Errorf("re-running failed jobs ran %s", names(ran))
	}
	if r := ran["deploy"]; r == nil || r.JobResults["build"].Outputs["by"] != fmt.Sprint(runID) || r.JobResults["test"].Result != protocol.ResultSuccess {
		t.Errorf("deploy's needs: %+v", r)
	}
	run, _ := srv.getRun(second)
	if run.Attempt != 2 || run.Status != protocol.StatusCompleted || run.Conclusion != protocol.ResultSuccess {
		t.Errorf("second attempt: %+v", run)
	}
	again := jobsOf(second)
	for _, name := range []string{"build", "lint", "test (1)", "test (3)"} {
		j := again[name]
		if j == nil || j.CopiedFrom != first[name].ID || j.CopiedAttempt != 1 || j.Conclusion != protocol.ResultSuccess || j.Outputs["by"] != fmt.Sprint(runID) {
			t.Errorf("kept %s: %+v", name, j)
			continue
		}
		lines, _, err := srv.logPage(j.ID, 0, 0)
		if err != nil || len(lines) != 1 || lines[0].Text != fmt.Sprintf("ran %s in run %d", name, runID) {
			t.Errorf("kept %s's log: %+v %v", name, lines, err)
		}
	}
	if j := again["test (2)"]; j.CopiedFrom != 0 || j.Outputs["by"] != fmt.Sprint(second) {
		t.Errorf("re-run test (2): %+v", j)
	}

	// Nothing failed now.
	if _, code := rerun(second, `{"failed":true}`); code != http.StatusBadRequest {
		t.Errorf("re-running failed jobs of a successful run: %d", code)
	}

	// Re-running build runs everything that needs it; lint is kept, as it
	// ran in the first attempt.
	third, code := rerun(second, fmt.Sprintf(`{"job_id":%d}`, again["build"].ID))
	if code != 200 {
		t.Fatalf("re-run build: %d", code)
	}
	ran = work(func(*Job) string { return protocol.ResultSuccess })
	if names(ran) != "build,deploy,test (1),test (2),test (3)" {
		t.Errorf("re-running build ran %s", names(ran))
	}
	if j := jobsOf(third)["lint"]; j.CopiedFrom != first["lint"].ID || j.CopiedAttempt != 1 {
		t.Errorf("lint in the third attempt: %+v", j)
	}

	// Re-running one matrix combination keeps the others.
	fourth, code := rerun(third, fmt.Sprintf(`{"job_id":%d}`, jobsOf(third)["test (3)"].ID))
	if code != 200 {
		t.Fatalf("re-run test (3): %d", code)
	}
	if ran = work(func(*Job) string { return protocol.ResultSuccess }); names(ran) != "deploy,test (3)" {
		t.Errorf("re-running test (3) ran %s", names(ran))
	}
	if j := jobsOf(fourth)["test (1)"]; j.CopiedAttempt != 3 {
		t.Errorf("test (1) in the fourth attempt: %+v", j)
	}

	// A job from another run.
	if _, code := rerun(fourth, fmt.Sprintf(`{"job_id":%d}`, first["build"].ID)); code != http.StatusBadRequest {
		t.Errorf("re-running another run's job: %d", code)
	}
}

// TestDebugLogging: a re-run with debug logging, or the ACTIONS_STEP_DEBUG
// variable, turns it on for the jobs the agent gets.
func TestDebugLogging(t *testing.T) {
	srv, ts, api := newRerunServer(t)
	ctx := context.Background()
	repo, err := srv.getRepo("t/app")
	if err != nil {
		t.Fatal(err)
	}
	rn := &Runner{ID: 1, Name: "r", Labels: normalizeLabels(nil, "linux", "amd64")}
	runOne := func(id int64) *protocol.JobRequest {
		t.Helper()
		req, err := srv.assignJob(ctx, rn, ts.URL)
		if err != nil || req == nil {
			t.Fatalf("assign: %v %v", req, err)
		}
		if err := srv.completeJob(ctx, req.JobID, protocol.ResultSuccess, "", nil, nil); err != nil {
			t.Fatal(err)
		}
		return req
	}
	const workflow = "on: push\njobs:\n  a:\n    runs-on: self-hosted\n    steps:\n      - run: echo\n"
	id, err := srv.createRun(ctx, runSpec{repo: repo, file: "ci.yml", yaml: workflow, name: "CI", event: "push",
		payload: map[string]any{}, ref: "refs/heads/main", sha: strings.Repeat("a", 40), actor: "t"})
	if err != nil {
		t.Fatal(err)
	}
	if req := runOne(id); req.StepDebug || req.RunnerDebug || req.Env["RUNNER_DEBUG"] != "" {
		t.Errorf("debug logging without asking for it: %v %v %q", req.StepDebug, req.RunnerDebug, req.Env["RUNNER_DEBUG"])
	}

	var res struct {
		RunID int64 `json:"run_id"`
	}
	if code := api("POST", fmt.Sprintf("/api/runs/%d/rerun", id), `{"debug":true}`, &res); code != 200 {
		t.Fatalf("re-run with debug logging: %d", code)
	}
	if run, _ := srv.getRun(res.RunID); !run.Debug {
		t.Errorf("re-run: %+v", run)
	}
	if req := runOne(res.RunID); !req.StepDebug || !req.RunnerDebug || req.Env["RUNNER_DEBUG"] != "1" {
		t.Errorf("re-run with debug logging: %v %v %q", req.StepDebug, req.RunnerDebug, req.Env["RUNNER_DEBUG"])
	}

	if code := api("PUT", "/api/variables?repo=t/app", `{"name":"ACTIONS_STEP_DEBUG","value":"True"}`, nil); code != 200 {
		t.Fatalf("set variable: %d", code)
	}
	if code := api("POST", fmt.Sprintf("/api/runs/%d/rerun", id), `{}`, &res); code != 200 {
		t.Fatalf("re-run: %d", code)
	}
	if req := runOne(res.RunID); !req.StepDebug || req.RunnerDebug || req.Env["RUNNER_DEBUG"] != "1" {
		t.Errorf("with ACTIONS_STEP_DEBUG: %v %v %q", req.StepDebug, req.RunnerDebug, req.Env["RUNNER_DEBUG"])
	}
}
