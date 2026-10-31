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
	"time"
)

const hourlyWorkflow = `name: Hourly
on:
  schedule:
    - cron: '0 * * * *'
  workflow_dispatch:
jobs:
  report:
    runs-on: self-hosted
    steps:
      - run: echo report
`

// scheduleTest is a server whose schedules run on a fake clock, with
// helpers to push to its repositories and call its API.
type scheduleTest struct {
	t    *testing.T
	tmp  string
	srv  *Server
	ts   *httptest.Server
	now  time.Time
	runs int // runs seen so far by newRuns
}

func newScheduleTest(t *testing.T) *scheduleTest {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	st := &scheduleTest{t: t, tmp: t.TempDir(), now: time.Date(2026, 11, 2, 12, 58, 0, 0, time.UTC)}
	st.start()
	return st
}

// start starts the server, again after restart.
func (st *scheduleTest) start() {
	srv, err := New(Config{DataDir: filepath.Join(st.tmp, "data"), AdminPassword: "pw", Version: "test"})
	if err != nil {
		st.t.Fatal(err)
	}
	srv.sched.now = func() time.Time { return st.now }
	st.srv = srv
	st.ts = httptest.NewServer(srv.Handler())
	st.t.Cleanup(st.ts.Close)
	st.t.Cleanup(func() { srv.db.Close() })
}

// restart stops the server and starts a new one on the same data.
func (st *scheduleTest) restart() {
	st.ts.Close()
	st.srv.db.Close()
	st.start()
}

func (st *scheduleTest) api(method, path, body string, out any) int {
	st.t.Helper()
	req, _ := http.NewRequest(method, st.ts.URL+path, strings.NewReader(body))
	req.SetBasicAuth("admin", "pw")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		st.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if out != nil {
		_ = json.Unmarshal(b, out)
	}
	return resp.StatusCode
}

func (st *scheduleTest) git(dir string, args ...string) {
	st.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
		"GIT_TERMINAL_PROMPT=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		st.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// commit writes files (removing those that are "") into a new commit of
// repo and pushes it to branch, returning its SHA.
func (st *scheduleTest) commit(repo, branch, msg string, files map[string]string) string {
	st.t.Helper()
	dir := filepath.Join(st.tmp, "src", repo)
	if _, err := os.Stat(dir); err != nil {
		_ = os.MkdirAll(dir, 0o755)
		st.git(dir, "init", "-q", "-b", "main")
	}
	if cur, _ := exec.Command("git", "-C", dir, "symbolic-ref", "--short", "HEAD").Output(); strings.TrimSpace(string(cur)) != branch {
		if exec.Command("git", "-C", dir, "checkout", "-q", branch).Run() != nil {
			st.git(dir, "checkout", "-q", "-b", branch)
		}
	}
	for name, content := range files {
		path := filepath.Join(dir, name)
		if content == "" {
			_ = os.Remove(path)
			continue
		}
		_ = os.MkdirAll(filepath.Dir(path), 0o755)
		_ = os.WriteFile(path, []byte(content), 0o644)
	}
	remote := strings.Replace(st.ts.URL, "http://", "http://admin:pw@", 1) + "/git/" + repo + ".git"
	st.git(dir, "add", "-A")
	st.git(dir, "commit", "-qm", msg, "--allow-empty")
	st.git(dir, "push", "-q", remote, "HEAD:"+branch)
	st.api("POST", "/api/repos/"+repo+"/sync", "", nil)
	out, _ := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	return strings.TrimSpace(string(out))
}

// tick advances the clock to at and runs the scheduler.
func (st *scheduleTest) tick(at string) time.Time {
	st.t.Helper()
	t, err := time.Parse(time.RFC3339, at)
	if err != nil {
		st.t.Fatal(err)
	}
	st.now = t
	return st.srv.runSchedules(context.Background(), t)
}

// newRuns returns the runs started since the last call, oldest first.
func (st *scheduleTest) newRuns() []*Run {
	st.t.Helper()
	var runs []*Run
	st.api("GET", "/api/runs?repo=t/app&limit=500", "", &runs)
	var out []*Run
	for i := len(runs) - 1 - st.runs; i >= 0; i-- {
		r, err := st.srv.getRun(runs[i].ID)
		if err != nil {
			st.t.Fatal(err)
		}
		out = append(out, r)
	}
	st.runs = len(runs)
	return out
}

func (st *scheduleTest) expectRuns(what string, files ...string) []*Run {
	st.t.Helper()
	runs := st.newRuns()
	var got []string
	for _, r := range runs {
		got = append(got, r.WorkflowFile)
	}
	if strings.Join(got, " ") != strings.Join(files, " ") {
		st.t.Fatalf("%s: runs %v, want %v", what, got, files)
	}
	return runs
}

func TestScheduledRuns(t *testing.T) {
	st := newScheduleTest(t)
	if code := st.api("POST", "/api/repos", `{"name":"t/app"}`, nil); code != 200 {
		t.Fatalf("create repo: %d", code)
	}
	sha := st.commit("t/app", "main", "Add the report", map[string]string{".github/workflows/hourly.yml": hourlyWorkflow})
	// Schedules on other branches don't run.
	st.commit("t/app", "feature", "Every five minutes", map[string]string{".github/workflows/often.yml": strings.Replace(hourlyWorkflow, "0 * * * *", "*/5 * * * *", 1)})
	st.commit("t/app", "main", "Back on main", nil)
	sha = st.commit("t/app", "main", "Latest", nil)

	if next := st.tick("2026-11-02T12:58:00Z"); !next.Equal(time.Date(2026, 11, 2, 13, 0, 0, 0, time.UTC)) {
		t.Fatalf("next slot = %v", next)
	}
	st.expectRuns("before the first slot")

	// The Workflows tab shows the schedule and when it next runs, only for
	// the default branch.
	var wfs []workflowInfo
	st.api("GET", "/api/repos/t/app/workflows", "", &wfs)
	if len(wfs) != 1 || len(wfs[0].Schedules) != 1 || wfs[0].Schedules[0].Cron != "0 * * * *" ||
		wfs[0].Schedules[0].NextRunAt != time.Date(2026, 11, 2, 13, 0, 0, 0, time.UTC).UnixMilli() {
		t.Fatalf("workflows: %+v", wfs)
	}
	wfs = nil
	st.api("GET", "/api/repos/t/app/workflows?ref=refs/heads/feature", "", &wfs)
	if len(wfs) != 2 || wfs[1].File != ".github/workflows/often.yml" || len(wfs[1].Schedules) != 1 || wfs[1].Schedules[0].NextRunAt != 0 {
		t.Fatalf("workflows on feature: %+v", wfs)
	}

	st.tick("2026-11-02T12:59:59Z")
	st.expectRuns("before the first slot")
	st.tick("2026-11-02T13:00:02Z")
	runs := st.expectRuns("at 13:00", ".github/workflows/hourly.yml")
	r := runs[0]
	if r.Event != "schedule" || r.EventJSON["schedule"] != "0 * * * *" || r.Ref != "refs/heads/main" || r.Sha != sha || !r.Protected ||
		r.WorkflowName != "Hourly" || r.Title != "Scheduled: Latest" {
		t.Fatalf("scheduled run: %+v", r)
	}
	if next := st.tick("2026-11-02T13:00:30Z"); !next.Equal(time.Date(2026, 11, 2, 14, 0, 0, 0, time.UTC)) {
		t.Fatalf("next slot after 13:00 = %v", next)
	}
	st.expectRuns("again at 13:00")

	// A restart doesn't run the slot again.
	st.restart()
	st.tick("2026-11-02T13:01:00Z")
	st.expectRuns("after a restart")
	// After an outage, only the latest missed slot runs.
	st.restart()
	st.tick("2026-11-02T18:20:00Z")
	st.expectRuns("after an outage", ".github/workflows/hourly.yml")
	st.tick("2026-11-02T18:40:00Z")
	st.expectRuns("after an outage, again")

	// An uploaded workflow on the default branch runs too, from when it's
	// uploaded; it can have a timezone. 07:00 in Auckland is 18:00 UTC.
	daily := strings.Replace(hourlyWorkflow, "- cron: '0 * * * *'", "- cron: '0 7 * * *'\n      timezone: Pacific/Auckland", 1)
	if code := st.api("PUT", "/api/repos/t/app/uploaded-workflows/daily.yml", daily, nil); code != 200 {
		t.Fatalf("upload: %d", code)
	}
	st.tick("2026-11-02T19:00:00Z")
	st.expectRuns("19:00", ".github/workflows/hourly.yml")
	wfs = nil
	st.api("GET", "/api/repos/t/app/workflows", "", &wfs)
	if len(wfs) != 2 || wfs[1].File != "kiwicd:daily.yml" || wfs[1].Schedules[0].Timezone != "Pacific/Auckland" ||
		wfs[1].Schedules[0].NextRunAt != time.Date(2026, 11, 3, 18, 0, 0, 0, time.UTC).UnixMilli() {
		t.Fatalf("workflows with the uploaded one: %+v", wfs)
	}
	st.tick("2026-11-03T18:00:00Z")
	st.expectRuns("18:00 the next day", ".github/workflows/hourly.yml", "kiwicd:daily.yml")

	// Invalid schedules can't be uploaded.
	for _, bad := range []string{"'@daily'", "'0 7 * *'", "'0 7 * * *'\n      timezone: Nowhere/Special"} {
		yaml := strings.Replace(hourlyWorkflow, "'0 * * * *'", bad, 1)
		if code := st.api("PUT", "/api/repos/t/app/uploaded-workflows/bad.yml", yaml, nil); code != 400 {
			t.Fatalf("upload with cron %s: %d", bad, code)
		}
	}
	// Deleting it stops it.
	st.api("DELETE", "/api/repos/t/app/uploaded-workflows/daily.yml", "", nil)
	st.tick("2026-11-04T18:00:00Z")
	st.expectRuns("after deleting the uploaded workflow", ".github/workflows/hourly.yml")

	// Removing the schedule from the default branch stops it; committing
	// it again counts from then.
	st.commit("t/app", "main", "Stop", map[string]string{".github/workflows/hourly.yml": strings.Replace(hourlyWorkflow, "  schedule:\n    - cron: '0 * * * *'\n", "", 1)})
	st.tick("2026-11-04T19:00:00Z")
	st.expectRuns("without a schedule")
	st.commit("t/app", "main", "Start", map[string]string{".github/workflows/hourly.yml": hourlyWorkflow})
	st.tick("2026-11-04T19:30:00Z")
	st.expectRuns("not at 19:00 when added at 19:30")
	st.tick("2026-11-04T20:00:00Z")
	st.expectRuns("20:00", ".github/workflows/hourly.yml")

	// Changing the default branch runs its schedules instead, at most every
	// five minutes even if they say every minute.
	if code := st.api("PATCH", "/api/repos/t/app", `{"default_branch":"feature"}`, nil); code != 200 {
		t.Fatalf("change the default branch: %d", code)
	}
	st.commit("t/app", "feature", "Every minute", map[string]string{".github/workflows/often.yml": strings.Replace(hourlyWorkflow, "0 * * * *", "* * * * *", 1)})
	st.tick("2026-11-04T20:00:30Z")
	st.tick("2026-11-04T20:01:00Z")
	runs = st.expectRuns("on feature", ".github/workflows/often.yml")
	if runs[0].Ref != "refs/heads/feature" || runs[0].EventJSON["schedule"] != "* * * * *" || !runs[0].Protected {
		t.Fatalf("run on the new default branch: %+v", runs[0])
	}
	for _, at := range []string{"2026-11-04T20:02:00Z", "2026-11-04T20:05:59Z"} {
		st.tick(at)
		st.expectRuns("within five minutes, at " + at)
	}
	st.tick("2026-11-04T20:06:00Z")
	st.expectRuns("five minutes later", ".github/workflows/often.yml")

	// A deleted repository's schedules go with it.
	if code := st.api("DELETE", "/api/repos/t/app", "", nil); code != 200 {
		t.Fatalf("delete: %d", code)
	}
	st.tick("2026-11-04T21:00:00Z")
	var n int
	_ = st.srv.db.QueryRow(`SELECT count(*) FROM schedules`).Scan(&n)
	if n != 0 || len(st.srv.sched.entries) != 0 {
		t.Fatalf("after deleting the repository: %d rows, entries %+v", n, st.srv.sched.entries)
	}
}
