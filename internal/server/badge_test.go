package server

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestBadge(t *testing.T) {
	srv, ts, api := newRerunServer(t)
	ctx := context.Background()
	repo, err := srv.getRepo("t/app")
	if err != nil {
		t.Fatal(err)
	}
	badge := func(path string, auth bool) (int, string) {
		t.Helper()
		req, _ := http.NewRequest("GET", ts.URL+path, nil)
		if auth {
			req.SetBasicAuth("admin", "pw")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode == 200 && resp.Header.Get("Content-Type") != "image/svg+xml" {
			t.Errorf("%s: Content-Type %q", path, resp.Header.Get("Content-Type"))
		}
		return resp.StatusCode, string(b)
	}
	says := func(path string, want ...string) {
		t.Helper()
		code, body := badge(path, true)
		for _, w := range want {
			if code != 200 || !strings.Contains(body, ">"+w+"</text>") {
				t.Errorf("%s: %d, want %q in %s", path, code, w, body)
			}
		}
	}
	newRun := func(ref, status, conclusion string) {
		t.Helper()
		id, err := srv.createRun(ctx, runSpec{repo: repo, file: ".github/workflows/ci.yml", yaml: "on: push\njobs: {}\n", name: "CI <tests>", event: "push",
			payload: map[string]any{}, ref: ref, sha: strings.Repeat("a", 40), actor: "t"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := srv.db.Exec(`UPDATE runs SET status = ?, conclusion = ? WHERE id = ?`, status, conclusion, id); err != nil {
			t.Fatal(err)
		}
	}

	// Signing in is needed, and without it a repository that doesn't exist
	// looks the same as one that does.
	for _, path := range []string{"/api/repos/t/app/badge/ci.yml.svg", "/api/repos/t/nope/badge/ci.yml.svg"} {
		if code, body := badge(path, false); code != 401 || strings.Contains(body, "svg") {
			t.Errorf("%s without signing in: %d %s", path, code, body)
		}
	}
	if code, _ := badge("/api/repos/t/nope/badge/ci.yml.svg", true); code != 404 {
		t.Errorf("missing repository: %d", code)
	}
	says("/api/repos/t/app/badge/ci.yml.svg", "ci", "no runs")

	newRun("refs/heads/main", "completed", "success")
	says("/api/repos/t/app/badge/ci.yml.svg", "CI &lt;tests&gt;", "passing")
	newRun("refs/heads/feature", "completed", "failure")
	says("/api/repos/t/app/badge/ci.yml.svg", "passing")
	says("/api/repos/t/app/badge/ci.yml.svg?branch=feature", "failing")
	says("/api/repos/t/app/badge/.github/workflows/ci.yml.svg", "passing")
	says("/api/repos/t/app/badge/ci.yml.svg?event=workflow_dispatch", "no runs")
	// A run in progress doesn't hide the last result.
	newRun("refs/heads/main", "in_progress", "")
	says("/api/repos/t/app/badge/ci.yml.svg", "passing")
	says("/api/repos/t/app/badge/ci.yml.svg?branch=other", "no runs")
	newRun("refs/heads/other", "queued", "")
	says("/api/repos/t/app/badge/ci.yml.svg?branch=other", "running")

	if code := api("PATCH", "/api/repos/t/app", `{"public_badges":true}`, nil); code != 200 {
		t.Fatalf("make badges public: %d", code)
	}
	if code, body := badge("/api/repos/t/app/badge/ci.yml.svg", false); code != 200 || !strings.Contains(body, ">passing</text>") {
		t.Errorf("public badge without signing in: %d %s", code, body)
	}
	if code, _ := badge("/api/repos/t/nope/badge/ci.yml.svg", false); code != 401 {
		t.Errorf("missing repository without signing in: %d", code)
	}
}

// TestBadgeAccess: someone who may not see a repository gets the answer for
// one that doesn't exist, unless its badges are public.
func TestBadgeAccess(t *testing.T) {
	a := newAccessTest(t, nil)
	onlyB := a.token("pw", `{"name":"b","role":"viewer","repos":["t/b"]}`)
	for _, c := range []struct {
		auth string
		code int
	}{{"", 401}, {"session-nobody", 401}, {"session-view", 200}, {onlyB, 404}, {"pw", 200}} {
		if code, body := a.do(c.auth, "GET", "/api/repos/t/a/badge/ci.yml.svg", "", nil); code != c.code {
			t.Errorf("t/a's badge as %q: %d %s, want %d", c.auth, code, body, c.code)
		}
	}
	if code, _ := a.do("pw", "PATCH", "/api/repos/t/a", `{"public_badges":true}`, nil); code != 200 {
		t.Fatalf("make t/a's badges public: %d", code)
	}
	for _, auth := range []string{"", onlyB} {
		if code, body := a.do(auth, "GET", "/api/repos/t/a/badge/ci.yml.svg", "", nil); code != 200 || !strings.Contains(body, "<svg") {
			t.Errorf("t/a's public badge as %q: %d %s", auth, code, body)
		}
	}
}
