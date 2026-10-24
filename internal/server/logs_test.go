package server

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/dire-kiwi/kiwicd/internal/protocol"
)

// newLogServer starts a server with one in-progress job and returns its id.
func newLogServer(t *testing.T, cfg Config) (*Server, *httptest.Server, int64) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	cfg.DataDir = filepath.Join(t.TempDir(), "data")
	cfg.AdminPassword = "pw"
	srv, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.db.Close() })
	for _, q := range []string{
		`INSERT INTO repos (id, name, kind, webhook_secret, created_at) VALUES (1, 't/r', 'hosted', 'x', 0)`,
		`INSERT INTO runs (id, repo_id, number, workflow_file, workflow_name, workflow_yaml, title, event, event_json, ref, sha, actor, status, created_at)
			VALUES (1, 1, 1, 'ci.yml', 'CI', '', 't', 'push', '{}', 'refs/heads/main', 'abc', 'me', 'in_progress', 0)`,
		`INSERT INTO jobs (id, run_id, job_key, name, status, created_at) VALUES (1, 1, 'a', 'a', 'in_progress', 0)`,
	} {
		if _, err := srv.db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return srv, ts, 1
}

func appendN(t *testing.T, srv *Server, jobID int64, n int, text func(i int) string) {
	t.Helper()
	for i := 0; i < n; {
		var batch []protocol.LogLine
		for ; i < n && len(batch) < 1000; i++ {
			batch = append(batch, protocol.LogLine{Step: 1, TS: int64(i), Text: text(i)})
		}
		if err := srv.appendLogs(jobID, batch); err != nil {
			t.Fatal(err)
		}
	}
}

func get(t *testing.T, ts *httptest.Server, path string) (*http.Response, []byte) {
	t.Helper()
	req, _ := http.NewRequest("GET", ts.URL+path, nil)
	req.SetBasicAuth("admin", "pw")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("GET %s: %d %s", path, resp.StatusCode, b)
	}
	return resp, b
}

var nextLink = regexp.MustCompile(`^<([^>]+)>; rel="next"$`)

// fetchAll follows the JSON API's next links and returns every line.
func fetchAll(t *testing.T, ts *httptest.Server, path string) (lines []logLine, pages int) {
	t.Helper()
	for path != "" {
		resp, b := get(t, ts, path)
		var page []logLine
		if err := json.Unmarshal(b, &page); err != nil {
			t.Fatal(err)
		}
		lines = append(lines, page...)
		pages++
		path = ""
		if m := nextLink.FindStringSubmatch(resp.Header.Get("Link")); m != nil {
			path = m[1]
		}
		if pages > 100 {
			t.Fatal("too many pages")
		}
	}
	return lines, pages
}

func TestLogPaging(t *testing.T) {
	srv, ts, job := newLogServer(t, Config{})
	const total = 2*logPageLines + 7
	appendN(t, srv, job, total, func(i int) string { return fmt.Sprintf("line %d", i) })

	lines, pages := fetchAll(t, ts, "/api/jobs/1/logs")
	if len(lines) != total || pages != 3 {
		t.Fatalf("got %d lines in %d pages, want %d in 3", len(lines), pages, total)
	}
	for i, l := range lines {
		if l.Seq != int64(i+1) || l.Text != fmt.Sprintf("line %d", i) {
			t.Fatalf("line %d: %+v", i, l)
		}
	}

	// ?limit= sets the page size (up to logPageLines) and ?after= the start.
	lines, pages = fetchAll(t, ts, "/api/jobs/1/logs?after=10000&limit=3")
	if len(lines) != 7 || pages != 3 || lines[0].Seq != 10001 {
		t.Fatalf("limit=3: %d lines in %d pages, first %+v", len(lines), pages, lines[0])
	}
	resp, b := get(t, ts, "/api/jobs/1/logs?limit=999999")
	var page []logLine
	_ = json.Unmarshal(b, &page)
	if len(page) != logPageLines || resp.Header.Get("Link") == "" {
		t.Fatalf("limit above the max: %d lines, link %q", len(page), resp.Header.Get("Link"))
	}

	// The text format streams the whole log.
	_, b = get(t, ts, "/api/jobs/1/logs?format=text")
	text := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
	if len(text) != total || !strings.HasSuffix(text[0], " line 0") || !strings.HasSuffix(text[total-1], fmt.Sprintf(" line %d", total-1)) {
		t.Fatalf("text: %d lines, first %q", len(text), text[0])
	}
}

func TestLogPageByteBudget(t *testing.T) {
	srv, ts, job := newLogServer(t, Config{})
	long := strings.Repeat("x", maxLogLineBytes-10)
	n := 2*logPageBytes/len(long) + 3
	appendN(t, srv, job, n, func(int) string { return long })

	resp, b := get(t, ts, "/api/jobs/1/logs")
	var page []logLine
	_ = json.Unmarshal(b, &page)
	if len(page) == n || len(page)*len(long) > logPageBytes+len(long) || resp.Header.Get("Link") == "" {
		t.Fatalf("first page: %d of %d lines, link %q", len(page), n, resp.Header.Get("Link"))
	}
	lines, _ := fetchAll(t, ts, "/api/jobs/1/logs")
	if len(lines) != n {
		t.Fatalf("got %d lines, want %d", len(lines), n)
	}
	_, b = get(t, ts, "/api/jobs/1/logs?format=text")
	if got := strings.Count(string(b), "\n"); got != n {
		t.Fatalf("text: %d lines, want %d", got, n)
	}
}

func TestJobEventsBacklogPaged(t *testing.T) {
	srv, ts, job := newLogServer(t, Config{})
	const total = logPageLines + 50
	appendN(t, srv, job, total, func(i int) string { return fmt.Sprintf("line %d", i) })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", ts.URL+"/api/jobs/1/events?after=20", nil)
	req.SetBasicAuth("admin", "pw")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(nil, 64<<20)
	var typ string
	var chunks, got int
	last := int64(20)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "event: ") {
			typ = strings.TrimPrefix(line, "event: ")
			continue
		}
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		if typ == "job" {
			break
		}
		var lines []logLine
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &lines); err != nil {
			t.Fatal(err)
		}
		for _, l := range lines {
			if l.Seq != last+1 {
				t.Fatalf("seq %d after %d", l.Seq, last)
			}
			last = l.Seq
		}
		chunks++
		got += len(lines)
	}
	if got != total-20 || chunks != 2 {
		t.Fatalf("backlog: %d lines in %d chunks, want %d in 2", got, chunks, total-20)
	}
}

func TestLogLineTruncation(t *testing.T) {
	srv, ts, job := newLogServer(t, Config{})
	// A multi-byte rune straddles the cut.
	long := strings.Repeat("a", maxLogLineBytes-1) + "é" + strings.Repeat("b", 100_000)
	appendN(t, srv, job, 2, func(i int) string { return []string{long, "short"}[i] })

	lines, _ := fetchAll(t, ts, "/api/jobs/1/logs")
	if len(lines) != 2 || lines[1].Text != "short" {
		t.Fatalf("lines: %d", len(lines))
	}
	text := lines[0].Text
	want := fmt.Sprintf(" … [line truncated: %d bytes]", len(long))
	if !strings.HasSuffix(text, want) || !utf8.ValidString(text) {
		t.Fatalf("truncated line ends %q", text[len(text)-60:])
	}
	if kept := strings.TrimSuffix(text, want); kept != strings.Repeat("a", maxLogLineBytes-1) {
		t.Fatalf("kept %d bytes", len(kept))
	}
}

func TestLogCap(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cfg    Config
		stored int
	}{
		{"lines", Config{MaxLogLines: 10}, 10},
		{"bytes", Config{MaxLogBytes: 33}, 5}, // "line N" is 6 bytes
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, ts, job := newLogServer(t, tc.cfg)
			appendN(t, srv, job, 7, func(i int) string { return fmt.Sprintf("line %d", i) })
			appendN(t, srv, job, 7, func(i int) string { return fmt.Sprintf("line %d", 7+i) })
			appendN(t, srv, job, 3, func(i int) string { return "more" })

			lines, _ := fetchAll(t, ts, "/api/jobs/1/logs")
			if len(lines) != tc.stored+1 {
				t.Fatalf("stored %d lines, want %d plus a notice", len(lines), tc.stored)
			}
			for i, l := range lines[:tc.stored] {
				if l.Text != fmt.Sprintf("line %d", i) {
					t.Fatalf("line %d: %q", i, l.Text)
				}
			}
			if notice := lines[tc.stored].Text; !strings.HasPrefix(notice, "##[warning]The log was truncated") {
				t.Fatalf("notice: %q", notice)
			}
			// The job keeps running.
			j, err := srv.getJob(job)
			if err != nil || j.Status != protocol.StatusInProgress || j.LogSeq != int64(tc.stored+1) {
				t.Fatalf("job: %+v %v", j, err)
			}
		})
	}

	srv, ts, job := newLogServer(t, Config{MaxLogLines: -1, MaxLogBytes: -1})
	appendN(t, srv, job, 3, func(i int) string { return "x" })
	if lines, _ := fetchAll(t, ts, "/api/jobs/1/logs"); len(lines) != 3 {
		t.Fatalf("unlimited: %d lines", len(lines))
	}
}
