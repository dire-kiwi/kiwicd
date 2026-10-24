package server

import (
	"bufio"
	"context"
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

	"github.com/dire-kiwi/kiwicd/internal/protocol"
)

const archiveTestWorkflow = `
on: push
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - run: echo hi
`

type archiveTest struct {
	t   *testing.T
	srv *Server
	ts  *httptest.Server
}

func newArchiveTest(t *testing.T) *archiveTest {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	srv, err := New(Config{DataDir: filepath.Join(t.TempDir(), "data"), AdminPassword: "pw", Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	if _, err := srv.db.Exec(`INSERT INTO repos (name, kind, webhook_secret, created_at) VALUES ('t/r', 'hosted', 'x', 0)`); err != nil {
		t.Fatal(err)
	}
	return &archiveTest{t: t, srv: srv, ts: ts}
}

// startJob creates a run with one job, marks it in progress and gives it n
// log lines.
func (a *archiveTest) startJob(n int) int64 {
	a.t.Helper()
	repo, err := a.srv.getRepo("t/r")
	if err != nil {
		a.t.Fatal(err)
	}
	runID, err := a.srv.createRun(context.Background(), runSpec{repo: repo, file: "ci.yml", yaml: archiveTestWorkflow, name: "CI",
		event: "push", payload: map[string]any{}, ref: "refs/heads/main", sha: "abc", actor: "admin"})
	if err != nil {
		a.t.Fatal(err)
	}
	jobs, err := a.srv.runJobs(runID)
	if err != nil || len(jobs) != 1 {
		a.t.Fatalf("jobs: %v %v", jobs, err)
	}
	id := jobs[0].ID
	if _, err := a.srv.db.Exec(`UPDATE jobs SET status = ? WHERE id = ?`, protocol.StatusInProgress, id); err != nil {
		a.t.Fatal(err)
	}
	var lines []protocol.LogLine
	for i := 1; i <= n; i++ {
		lines = append(lines, protocol.LogLine{Step: i % 3, TS: int64(1_700_000_000_000 + i), Text: fmt.Sprintf("line %d <&>", i)})
	}
	if err := a.srv.appendLogs(id, lines); err != nil {
		a.t.Fatal(err)
	}
	return id
}

func (a *archiveTest) rows(jobID int64) int {
	a.t.Helper()
	var n int
	if err := a.srv.db.QueryRow(`SELECT count(*) FROM log_lines WHERE job_id = ?`, jobID).Scan(&n); err != nil {
		a.t.Fatal(err)
	}
	return n
}

func (a *archiveTest) get(path string) (*http.Response, []byte) {
	a.t.Helper()
	req, _ := http.NewRequest("GET", a.ts.URL+path, nil)
	req.SetBasicAuth("admin", "pw")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		a.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

// logPage fetches one page of logs from path and returns it with the next
// page's path, if any.
func (a *archiveTest) logPage(path string) ([]logLine, string) {
	a.t.Helper()
	resp, b := a.get(path)
	if resp.StatusCode != 200 {
		a.t.Fatalf("logs: %d %s", resp.StatusCode, b)
	}
	var out []logLine
	if err := json.Unmarshal(b, &out); err != nil {
		a.t.Fatal(err)
	}
	next, _, _ := strings.Cut(strings.TrimPrefix(resp.Header.Get("Link"), "<"), ">")
	return out, next
}

// logs fetches a job's log after seq after, following the pages.
func (a *archiveTest) logs(jobID, after int64) []logLine {
	a.t.Helper()
	var out []logLine
	for path := fmt.Sprintf("/api/jobs/%d/logs?after=%d", jobID, after); path != ""; {
		var page []logLine
		page, path = a.logPage(path)
		out = append(out, page...)
	}
	return out
}

// sseBacklog reads the job events stream up to its first "job" event and
// returns the log lines sent before it.
func (a *archiveTest) sseBacklog(jobID, after int64) []logLine {
	a.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf("%s/api/jobs/%d/events?after=%d", a.ts.URL, jobID, after), nil)
	req.SetBasicAuth("admin", "pw")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		a.t.Fatal(err)
	}
	defer resp.Body.Close()
	out := []logLine{}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(nil, 1<<20)
	typ := ""
	for sc.Scan() {
		line := sc.Text()
		if t, ok := strings.CutPrefix(line, "event: "); ok {
			typ = t
		} else if d, ok := strings.CutPrefix(line, "data: "); ok {
			if typ == "job" {
				return out
			}
			var batch []logLine
			if err := json.Unmarshal([]byte(d), &batch); err != nil {
				a.t.Fatal(err)
			}
			out = append(out, batch...)
		}
	}
	a.t.Fatalf("event stream ended before the job event: %v", sc.Err())
	return nil
}

func checkSeqs(t *testing.T, what string, lines []logLine, from, to int64) {
	t.Helper()
	if int64(len(lines)) != to-from+1 {
		t.Fatalf("%s: got %d lines, want seq %d..%d", what, len(lines), from, to)
	}
	for i, l := range lines {
		want := from + int64(i)
		if l.Seq != want || l.Text != fmt.Sprintf("line %d <&>", want) || l.Step != int(want%3) || l.TS != 1_700_000_000_000+want {
			t.Fatalf("%s: line %d = %+v", what, i, l)
		}
	}
}

func TestArchiveOnCompletion(t *testing.T) {
	a := newArchiveTest(t)
	const n = logPageLines + 7 // more than one page, so two gzip members
	id := a.startJob(n)

	// While running, lines come from the database.
	checkSeqs(t, "live logs", a.logs(id, 0), 1, n)
	checkSeqs(t, "live backlog", a.sseBacklog(id, n-5), n-4, n)

	if err := a.srv.completeJob(context.Background(), id, protocol.ResultSuccess, "", nil, nil); err != nil {
		t.Fatal(err)
	}
	if got := a.rows(id); got != 0 {
		t.Fatalf("%d log rows left after archiving", got)
	}
	path := a.srv.logArchivePath(id)
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	if tmps, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".*.tmp")); len(tmps) > 0 {
		t.Fatalf("temp files left: %v", tmps)
	}

	var archive string
	_ = a.srv.db.QueryRow(`SELECT log_archive FROM jobs WHERE id = ?`, id).Scan(&archive)
	var index archiveIndex
	if err := json.Unmarshal([]byte(archive), &index); err != nil || len(index) != 2 || index[1][0] != logPageLines+1 {
		t.Fatalf("archive index %q: %v", archive, err)
	}

	checkSeqs(t, "archived logs", a.logs(id, 0), 1, n)
	checkSeqs(t, "archived logs after", a.logs(id, 100), 101, n)
	checkSeqs(t, "archived logs in the second member", a.logs(id, logPageLines+2), logPageLines+3, n)
	if got := a.logs(id, n); len(got) != 0 {
		t.Fatalf("after=last: got %d lines", len(got))
	}
	// A page across the members' boundary, with a link to the next one.
	page, next := a.logPage(fmt.Sprintf("/api/jobs/%d/logs?after=%d&limit=10", id, logPageLines-5))
	checkSeqs(t, "page across members", page, logPageLines-4, logPageLines+5)
	if want := fmt.Sprintf("/api/jobs/%d/logs?after=%d&limit=10", id, logPageLines+5); next != want {
		t.Fatalf("next page %q, want %q", next, want)
	}
	page, next = a.logPage(fmt.Sprintf("/api/jobs/%d/logs?after=%d&limit=10", id, n-10))
	checkSeqs(t, "last page", page, n-9, n)
	if next != "" {
		t.Fatalf("last page links to %q", next)
	}
	checkSeqs(t, "archived backlog", a.sseBacklog(id, 0), 1, n)
	checkSeqs(t, "archived backlog after", a.sseBacklog(id, n-3), n-2, n)

	resp, text := a.get(fmt.Sprintf("/api/jobs/%d/logs?format=text&after=%d", id, n-2))
	want := fmt.Sprintf("%s line %d <&>\n%s line %d <&>\n",
		time.UnixMilli(1_700_000_000_000+n-1).UTC().Format(time.RFC3339), n-1,
		time.UnixMilli(1_700_000_000_000+n).UTC().Format(time.RFC3339), n)
	if resp.StatusCode != 200 || string(text) != want {
		t.Fatalf("text logs: %d %q, want %q", resp.StatusCode, text, want)
	}
	if _, text := a.get(fmt.Sprintf("/api/jobs/%d/logs?format=text", id)); strings.Count(string(text), "\n") != n {
		t.Fatalf("whole text log has %d lines, want %d", strings.Count(string(text), "\n"), n)
	}

	// The log is final: late lines (e.g. from a reaped job's agent) are dropped.
	if err := a.srv.appendLogs(id, []protocol.LogLine{{Text: "late"}}); err != nil {
		t.Fatal(err)
	}
	if got := a.rows(id); got != 0 {
		t.Fatalf("late line was stored")
	}
	checkSeqs(t, "logs after late line", a.logs(id, 0), 1, n)
}

func TestArchivePendingLogs(t *testing.T) {
	a := newArchiveTest(t)
	id := a.startJob(10)
	empty := a.startJob(0)
	running := a.startJob(3)
	// Completed without archiving, as if the server stopped first.
	if _, err := a.srv.db.Exec(`UPDATE jobs SET status = ? WHERE id IN (?, ?)`, protocol.StatusCompleted, id, empty); err != nil {
		t.Fatal(err)
	}
	// A stale archive from a crash after the rename is replaced.
	path := a.srv.logArchivePath(id)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("junk"), 0o644); err != nil {
		t.Fatal(err)
	}
	checkSeqs(t, "unarchived logs", a.logs(id, 0), 1, 10)

	a.srv.archivePendingLogs()
	if a.rows(id) != 0 {
		t.Fatal("completed job was not archived")
	}
	checkSeqs(t, "archived logs", a.logs(id, 4), 5, 10)
	if a.rows(running) != 3 {
		t.Fatal("running job was archived")
	}
	if _, err := os.Stat(a.srv.logArchivePath(empty)); !os.IsNotExist(err) {
		t.Fatalf("archive written for a job without logs: %v", err)
	}
	if got := a.logs(empty, 0); len(got) != 0 {
		t.Fatalf("empty job has %d lines", len(got))
	}

	// Rows left behind when the server stopped while deleting them, after
	// recording the archive, are ignored and then deleted.
	if _, err := a.srv.db.Exec(`INSERT INTO log_lines (job_id, seq, step, ts, text) VALUES (?, 1, 0, 0, 'stale')`, id); err != nil {
		t.Fatal(err)
	}
	checkSeqs(t, "archived logs with leftover rows", a.logs(id, 0), 1, 10)
	a.srv.archivePendingLogs()
	if a.rows(id) != 0 {
		t.Fatal("leftover rows were not deleted")
	}
	checkSeqs(t, "archived logs", a.logs(id, 0), 1, 10)
}

func TestDeleteRepoRemovesLogArchives(t *testing.T) {
	a := newArchiveTest(t)
	var ids []int64
	for range 2 {
		id := a.startJob(5)
		if err := a.srv.completeJob(context.Background(), id, protocol.ResultSuccess, "", nil, nil); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(a.srv.logArchivePath(id)); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	req, _ := http.NewRequest("DELETE", a.ts.URL+"/api/repos/t/r", nil)
	req.SetBasicAuth("admin", "pw")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("delete repo: %d", resp.StatusCode)
	}
	for _, id := range ids {
		if _, err := os.Stat(a.srv.logArchivePath(id)); !os.IsNotExist(err) {
			t.Fatalf("archive of job %d not removed: %v", id, err)
		}
	}
	var n int
	_ = a.srv.db.QueryRow(`SELECT count(*) FROM jobs`).Scan(&n)
	if n != 0 {
		t.Fatalf("%d jobs left", n)
	}
}
