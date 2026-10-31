package server

import (
	"context"
	"strings"
	"testing"

	"github.com/dire-kiwi/kiwicd/internal/protocol"
)

// schedTest is a server with one hosted repository, t/x, for testing how runs
// and jobs are scheduled.
type schedTest struct {
	t    *testing.T
	srv  *Server
	repo *Repo
	ctx  context.Context
}

func newSchedTest(t *testing.T) *schedTest {
	srv, err := New(Config{DataDir: t.TempDir(), AdminPassword: "pw", Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srv.db.Exec(`INSERT INTO repos (name, kind, default_branch, webhook_secret, created_at) VALUES ('t/x', 'hosted', 'main', 's', 0)`); err != nil {
		t.Fatal(err)
	}
	repo, _ := srv.getRepo("t/x")
	return &schedTest{t: t, srv: srv, repo: repo, ctx: context.Background()}
}

func (st *schedTest) start(yaml string, inputs map[string]any) *Run {
	st.t.Helper()
	payload := map[string]any{}
	event := "push"
	if inputs != nil {
		payload["inputs"] = inputs
		event = "workflow_dispatch"
	}
	id, err := st.srv.createRun(st.ctx, runSpec{repo: st.repo, file: "w.yml", yaml: yaml, name: "w", event: event, payload: payload,
		ref: "refs/heads/main", sha: "abc", actor: "ann", title: "commit subject"})
	if err != nil {
		st.t.Fatal(err)
	}
	return st.run(id)
}

func (st *schedTest) run(id int64) *Run {
	st.t.Helper()
	run, err := st.srv.getRun(id)
	if err != nil {
		st.t.Fatal(err)
	}
	return run
}

// jobs returns a run's jobs by name.
func (st *schedTest) jobs(run *Run) map[string]*Job {
	st.t.Helper()
	jobs, err := st.srv.runJobs(run.ID)
	if err != nil {
		st.t.Fatal(err)
	}
	out := map[string]*Job{}
	for _, j := range jobs {
		out[j.Name] = j
	}
	return out
}

func (st *schedTest) complete(j *Job, result string) {
	st.t.Helper()
	if err := st.srv.completeJob(st.ctx, j.ID, result, "", nil, nil); err != nil {
		st.t.Fatal(err)
	}
}

func (st *schedTest) assign(rn *Runner) *protocol.JobRequest {
	st.t.Helper()
	req, err := st.srv.assignJob(st.ctx, rn, protocol.Version, "http://kiwicd.test")
	if err != nil {
		st.t.Fatal(err)
	}
	return req
}

func statusOf(j *Job) string {
	if j == nil {
		return "missing"
	}
	if j.Status == protocol.StatusCompleted {
		return j.Conclusion
	}
	return j.Status
}

func TestJobConcurrency(t *testing.T) {
	st := newSchedTest(t)
	const deploy = `on: push
jobs:
  build:
    runs-on: x
    steps: [{run: echo}]
  deploy:
    needs: build
    runs-on: x
    concurrency: deploy-${{ github.ref_name }}
    steps: [{run: echo}]
`
	a, b, c := st.start(deploy, nil), st.start(deploy, nil), st.start(deploy, nil)
	// The group is evaluated when the job is created, after its needs.
	st.complete(st.jobs(a)["build"], protocol.ResultSuccess)
	st.complete(st.jobs(b)["build"], protocol.ResultSuccess)
	if ja, jb := st.jobs(a)["deploy"], st.jobs(b)["deploy"]; statusOf(ja) != protocol.StatusQueued || ja.ConcurrencyGroup != "deploy-main" ||
		statusOf(jb) != protocol.StatusPending {
		t.Fatalf("a: %+v, b: %+v", ja, jb)
	}
	// c's job replaces b's as the one waiting, which fails nothing else.
	st.complete(st.jobs(c)["build"], protocol.ResultSuccess)
	if jb, jc := st.jobs(b)["deploy"], st.jobs(c)["deploy"]; statusOf(jb) != protocol.ResultCancelled || statusOf(jc) != protocol.StatusPending {
		t.Fatalf("b: %+v, c: %+v", jb, jc)
	}
	if b = st.run(b.ID); b.Conclusion != protocol.ResultCancelled {
		t.Errorf("b's run: %+v", b)
	}
	if a = st.run(a.ID); a.Status == protocol.StatusCompleted || a.Cancel {
		t.Errorf("a's run: %+v", a)
	}
	// A run without the group's jobs doesn't wait: run groups are apart.
	other := st.start("on: push\nconcurrency: deploy-main\njobs:\n  a: {runs-on: x, steps: [{run: echo}]}\n", nil)
	if statusOf(st.jobs(other)["a"]) != protocol.StatusQueued {
		t.Error("a run in a workflow group of the same name waited")
	}
	// When a's job finishes, c's starts.
	st.complete(st.jobs(a)["deploy"], protocol.ResultSuccess)
	if jc := st.jobs(c)["deploy"]; statusOf(jc) != protocol.StatusQueued {
		t.Fatalf("c after a: %+v", jc)
	}

	// cancel-in-progress cancels the queued job and asks a running one to
	// stop, then waits for it.
	const cancelling = `on: push
jobs:
  deploy:
    runs-on: x
    concurrency:
      group: deploy-${{ github.ref_name }}
      cancel-in-progress: true
    steps: [{run: echo}]
`
	d := st.start(cancelling, nil)
	if jc, jd := st.jobs(c)["deploy"], st.jobs(d)["deploy"]; statusOf(jc) != protocol.ResultCancelled || statusOf(jd) != protocol.StatusQueued {
		t.Fatalf("c: %+v, d: %+v", jc, jd)
	}
	rn := &Runner{ID: 1, Name: "r", Labels: []string{"x"}}
	for req := st.assign(rn); req != nil; req = st.assign(rn) {
		if req.JobID == st.jobs(d)["deploy"].ID {
			break
		}
	}
	if jd := st.jobs(d)["deploy"]; statusOf(jd) != protocol.StatusInProgress {
		t.Fatalf("d: %+v", jd)
	}
	e := st.start(cancelling, nil)
	if jd, je := st.jobs(d)["deploy"], st.jobs(e)["deploy"]; !jd.Cancel || statusOf(je) != protocol.StatusPending {
		t.Fatalf("d: %+v, e: %+v", jd, je)
	}
	st.complete(st.jobs(d)["deploy"], protocol.ResultCancelled)
	if je := st.jobs(e)["deploy"]; statusOf(je) != protocol.StatusQueued {
		t.Errorf("e after d stopped: %+v", je)
	}
	// Cancelling a run cancels its waiting job.
	f := st.start(cancelling, nil)
	g := st.start(strings.Replace(cancelling, "cancel-in-progress: true", "cancel-in-progress: false", 1), nil)
	if statusOf(st.jobs(f)["deploy"]) != protocol.StatusQueued || statusOf(st.jobs(g)["deploy"]) != protocol.StatusPending {
		t.Fatalf("f: %+v, g: %+v", st.jobs(f)["deploy"], st.jobs(g)["deploy"])
	}
	if err := st.srv.cancelRun(st.ctx, g.ID); err != nil {
		t.Fatal(err)
	}
	if g = st.run(g.ID); g.Conclusion != protocol.ResultCancelled {
		t.Errorf("g: %+v", g)
	}

	// Matrix jobs with a group each run side by side.
	m := st.start(`on: push
jobs:
  test:
    runs-on: x
    strategy:
      matrix:
        os: [a, b]
    concurrency: test-${{ matrix.os }}
    steps: [{run: echo}]
`, nil)
	for name, j := range st.jobs(m) {
		if statusOf(j) != protocol.StatusQueued {
			t.Errorf("%s: %+v", name, j)
		}
	}
	// A group that can't be evaluated fails the job.
	bad := st.start("on: push\njobs:\n  a:\n    runs-on: x\n    concurrency: ${{ fromJSON('x') }}\n    steps: [{run: echo}]\n", nil)
	if j := st.jobs(bad)["a"]; statusOf(j) != protocol.ResultFailure || !strings.Contains(j.Error, "concurrency") {
		t.Errorf("bad group: %+v", j)
	}
}

func TestContinueOnError(t *testing.T) {
	st := newSchedTest(t)
	run := st.start(`on: push
jobs:
  test:
    runs-on: x
    continue-on-error: ${{ matrix.experimental }}
    strategy:
      matrix:
        v: [1, 2]
        experimental: [false]
        include:
          - v: 3
            experimental: true
    steps: [{run: echo}]
  report:
    needs: test
    runs-on: x
    steps: [{run: echo}]
`, nil)
	jobs := st.jobs(run)
	if !jobs["test (true, 3)"].ContinueOnError || jobs["test (false, 1)"].ContinueOnError {
		t.Fatalf("continue-on-error: %+v", jobs)
	}
	// The experimental job's failure doesn't cancel the others (fail-fast).
	st.complete(jobs["test (true, 3)"], protocol.ResultFailure)
	if j := st.jobs(run)["test (false, 1)"]; statusOf(j) != protocol.StatusQueued {
		t.Fatalf("fail-fast cancelled %+v", j)
	}
	st.complete(jobs["test (false, 1)"], protocol.ResultSuccess)
	st.complete(jobs["test (false, 2)"], protocol.ResultSuccess)
	report := st.jobs(run)["report"]
	if statusOf(report) != protocol.StatusQueued {
		t.Fatalf("report: %+v", report)
	}
	// Dependents see it as successful.
	req := st.assign(&Runner{ID: 1, Name: "r", Labels: []string{"x"}})
	if req == nil || req.JobID != report.ID || req.JobResults["test"].Result != protocol.ResultSuccess {
		t.Fatalf("job request: %+v", req)
	}
	st.complete(report, protocol.ResultSuccess)
	if run = st.run(run.ID); run.Conclusion != protocol.ResultSuccess {
		t.Errorf("run: %+v", run)
	}

	// A failure without it fails the run and cancels the rest of the matrix.
	run = st.start(`on: push
jobs:
  test:
    runs-on: x
    continue-on-error: ${{ matrix.v == 2 }}
    strategy:
      matrix:
        v: [1, 2]
    steps: [{run: echo}]
`, nil)
	jobs = st.jobs(run)
	st.complete(jobs["test (1)"], protocol.ResultFailure)
	if run = st.run(run.ID); run.Conclusion != protocol.ResultFailure || statusOf(st.jobs(run)["test (2)"]) != protocol.ResultCancelled {
		t.Errorf("run: %+v, jobs: %+v", run, st.jobs(run))
	}
}

func TestMaxParallel(t *testing.T) {
	st := newSchedTest(t)
	run := st.start(`on: push
jobs:
  test:
    runs-on: x
    strategy:
      max-parallel: 2
      matrix:
        v: [1, 2, 3]
    steps: [{run: echo}]
`, nil)
	rn := &Runner{ID: 1, Name: "r", Labels: []string{"x"}}
	first, second := st.assign(rn), st.assign(rn)
	if first == nil || second == nil {
		t.Fatal("the first two didn't start")
	}
	if third := st.assign(rn); third != nil {
		t.Fatalf("a third job started: %s", third.DisplayName)
	}
	// Other jobs aren't held up.
	other := st.start("on: push\njobs:\n  a: {runs-on: x, steps: [{run: echo}]}\n", nil)
	if req := st.assign(rn); req == nil || req.RunID != other.ID {
		t.Fatalf("the other run's job: %+v", req)
	}
	for _, j := range st.jobs(run) {
		if j.ID == first.JobID {
			st.complete(j, protocol.ResultSuccess)
		}
	}
	if third := st.assign(rn); third == nil || third.DisplayName != "test (3)" {
		t.Fatalf("the third job: %+v", third)
	}
	bad := st.start("on: push\njobs:\n  a:\n    runs-on: x\n    strategy: {max-parallel: '${{ vars.N }}'}\n    steps: [{run: echo}]\n", nil)
	if j := st.jobs(bad)["a"]; statusOf(j) != protocol.ResultFailure || !strings.Contains(j.Error, "max-parallel") {
		t.Errorf("bad max-parallel: %+v", j)
	}
}

func TestRunNameAndPermissions(t *testing.T) {
	st := newSchedTest(t)
	const src = `on:
  push:
  workflow_dispatch:
    inputs:
      target: {default: prod}
run-name: ${{ inputs.target && format('Deploy to {0}', inputs.target) || github.event_name }} by ${{ github.actor }}
permissions:
  contents: read
jobs:
  a:
    runs-on: x
    steps: [{run: echo}]
  b:
    runs-on: x
    permissions: write-all
    steps: [{run: echo}]
`
	run := st.start(src, map[string]any{"target": "staging"})
	if run.Title != "Deploy to staging by ann" {
		t.Errorf("title: %q", run.Title)
	}
	if run.Permissions == nil || run.Permissions.Level("contents") != "read" || run.Permissions.Level("packages") != "none" {
		t.Errorf("run permissions: %+v", run.Permissions)
	}
	jobs := st.jobs(run)
	if p := jobs["a"].Permissions; p == nil || p.Level("contents") != "read" {
		t.Errorf("a's permissions: %+v", p)
	}
	if p := jobs["b"].Permissions; p == nil || p.All != "write" {
		t.Errorf("b's permissions: %+v", p)
	}
	rn := &Runner{ID: 1, Name: "r", Labels: []string{"x"}}
	for req := st.assign(rn); req != nil; req = st.assign(rn) {
		if req.JobKey == "b" && (req.Permissions == nil || req.Permissions.All != "write") {
			t.Errorf("b's job request: %+v", req.Permissions)
		}
	}
	if run := st.start(src, nil); run.Title != "push by ann" {
		t.Errorf("title of a push: %q", run.Title)
	}
	// Without run-name or permissions.
	if run := st.start("on: push\njobs:\n  a: {runs-on: x, steps: [{run: echo}]}\n", nil); run.Title != "commit subject" || run.Permissions != nil ||
		st.jobs(run)["a"].Permissions != nil {
		t.Errorf("defaults: %+v", run)
	}
	if bad := st.start("on: push\nrun-name: ${{ fromJSON('x') }}\njobs:\n  a: {runs-on: x, steps: [{run: echo}]}\n", nil); bad.Conclusion != protocol.ResultFailure ||
		!strings.Contains(bad.Error, "run-name") {
		t.Errorf("bad run-name: %+v", bad)
	}
}

// An invalid workflow gets a failed run showing the error, as on GitHub, when
// its triggers would run it or can't be read.
func TestInvalidWorkflowRuns(t *testing.T) {
	st := newSchedTest(t)
	for file, content := range map[string]string{
		// Uploaded workflows are checked on upload; these were stored before
		// a check that now fails, or as YAML that no longer parses.
		"perms.yml":    "on: push\npermissions: everything\njobs:\n  a: {runs-on: x, steps: [{run: echo}]}\n",
		"broken.yml":   "on: push\njobs: [\n",
		"dispatch.yml": "on: workflow_dispatch\npermissions: everything\njobs:\n  a: {runs-on: x, steps: [{run: echo}]}\n",
		"release.yml":  "on:\n  push:\n    branches: [release]\njobs:\n  a: {runs-on: x}\n  b: {needs: c, runs-on: x}\n",
	} {
		if _, err := st.srv.db.Exec(`INSERT INTO uploaded_workflows (repo_id, file, content, updated_at) VALUES (?, ?, ?, 0)`, st.repo.ID, file, content); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.srv.runPushWorkflows(st.ctx, st.repo, "refs/heads/main", "", nil, map[string]any{}, commitInfo{Subject: "s", Author: "ann"}); err != nil {
		t.Fatal(err)
	}
	rows, err := st.srv.db.Query(`SELECT workflow_file, conclusion, error FROM runs ORDER BY workflow_file`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := map[string]string{}
	for rows.Next() {
		var file, conclusion, errText string
		_ = rows.Scan(&file, &conclusion, &errText)
		if conclusion != protocol.ResultFailure || !strings.HasPrefix(errText, "Invalid workflow file: ") {
			t.Errorf("%s: %s %q", file, conclusion, errText)
		}
		got[file] = errText
	}
	if len(got) != 2 || !strings.Contains(got["kiwicd:perms.yml"], "permissions") || got["kiwicd:broken.yml"] == "" {
		t.Errorf("runs: %v", got)
	}
}

// A pull request's workflow can't hold up or cancel other runs' jobs by naming
// their group, as for workflow groups.
func TestJobConcurrencyPullRequestsApart(t *testing.T) {
	st := newSchedTest(t)
	const deploy = "on: [push, pull_request]\njobs:\n  a:\n    runs-on: x\n    concurrency: {group: deploy, cancel-in-progress: true}\n    steps: [{run: echo}]\n"
	push := st.start(deploy, nil)
	id, err := st.srv.createRun(st.ctx, runSpec{repo: st.repo, file: "w.yml", yaml: deploy, name: "w", event: "pull_request",
		payload: map[string]any{"number": 1.0}, ref: "refs/pull/1/merge", sha: "abc", actor: "fork"})
	if err != nil {
		t.Fatal(err)
	}
	pr := st.run(id)
	if j := st.jobs(push)["a"]; statusOf(j) != protocol.StatusQueued {
		t.Errorf("the pull request cancelled the push's job: %+v", j)
	}
	if j := st.jobs(pr)["a"]; statusOf(j) != protocol.StatusQueued {
		t.Errorf("the pull request's job: %+v", j)
	}
	// Each still has a group of its own kind.
	pr2 := st.start(deploy, nil)
	if statusOf(st.jobs(push)["a"]) != protocol.ResultCancelled || statusOf(st.jobs(pr)["a"]) != protocol.StatusQueued ||
		statusOf(st.jobs(pr2)["a"]) != protocol.StatusQueued {
		t.Errorf("push: %+v, pr: %+v, second push: %+v", st.jobs(push)["a"], st.jobs(pr)["a"], st.jobs(pr2)["a"])
	}
}
