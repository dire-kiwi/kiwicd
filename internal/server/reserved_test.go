package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dire-kiwi/kiwicd/internal/protocol"
)

func TestParseReservedLabels(t *testing.T) {
	rl, err := parseReservedLabels([]string{"Deploy=t/deploy", "release=t/app@refs/tags/v*,t/app@release/*", "any=t/x@*"})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		label, repo, ref string
		want             bool
	}{
		{"deploy", "t/deploy", "refs/heads/main", true},
		{"DEPLOY", "T/Deploy", "refs/heads/main", true},
		{"deploy", "t/deploy", "refs/heads/feature", false},
		{"deploy", "t/deploy", "refs/tags/main", false},
		{"deploy", "t/other", "refs/heads/main", false},
		{"release", "t/app", "refs/tags/v1.2", true},
		{"release", "t/app", "refs/heads/release/1", true},
		{"release", "t/app", "refs/heads/release/1/x", false},
		{"release", "t/app", "refs/heads/main", false},
		{"any", "t/x", "refs/heads/whatever", true},
		{"any", "t/x", "refs/tags/v1", false},
		{"self-hosted", "t/other", "refs/heads/dev", true},
	} {
		if got := rl.allows(c.label, c.repo, c.ref, "main"); got != c.want {
			t.Errorf("allows(%s, %s, %s) = %v", c.label, c.repo, c.ref, got)
		}
	}
	for _, bad := range []string{"deploy", "deploy=", "=t/x", "deploy=nope", "deploy=a/b/c", "deploy=t/x@", "deploy=t/x@[", "deploy=t/x,,t/y"} {
		if _, err := parseReservedLabels([]string{bad}); err == nil {
			t.Errorf("parsed %q", bad)
		}
	}
}

const reservedWorkflow = `name: CI
on: push
jobs:
  deploy:
    runs-on: [self-hosted, deploy]
    steps:
      - run: echo deploy
  build:
    runs-on: self-hosted
    steps:
      - run: echo build
`

// TestReservedLabels: the deploy runner only takes t/deploy's default
// branch's jobs, even ones any runner could take, and other runs' deploy
// jobs fail at once.
func TestReservedLabels(t *testing.T) {
	srv, err := New(Config{DataDir: filepath.Join(t.TempDir(), "data"), AdminPassword: "pw", Version: "test",
		ReservedLabels: []string{"deploy=t/deploy"}})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	ctx := context.Background()
	for _, name := range []string{"t/deploy", "t/other"} {
		req, _ := http.NewRequest("POST", ts.URL+"/api/repos", strings.NewReader(`{"name":"`+name+`"}`))
		req.SetBasicAuth("admin", "pw")
		resp, err := http.DefaultClient.Do(req)
		if err != nil || resp.StatusCode != 200 {
			t.Fatalf("create %s: %v %v", name, err, resp)
		}
		resp.Body.Close()
	}
	newRun := func(repo, ref string) map[string]*Job {
		t.Helper()
		r, err := srv.getRepo(repo)
		if err != nil {
			t.Fatal(err)
		}
		id, err := srv.createRun(ctx, runSpec{repo: r, file: ".github/workflows/ci.yml", yaml: reservedWorkflow, name: "CI", event: "push",
			payload: map[string]any{}, ref: ref, sha: strings.Repeat("a", 40), actor: "t"})
		if err != nil {
			t.Fatal(err)
		}
		jobs, err := srv.runJobs(id)
		if err != nil {
			t.Fatal(err)
		}
		byKey := map[string]*Job{}
		for _, j := range jobs {
			byKey[j.Key] = j
		}
		return byKey
	}
	deployRunner := &Runner{ID: 1, Name: "deploy", Labels: normalizeLabels([]string{"deploy"}, "linux", "amd64")}
	plainRunner := &Runner{ID: 2, Name: "plain", Labels: normalizeLabels(nil, "linux", "amd64")}
	assign := func(rn *Runner) int64 {
		t.Helper()
		req, err := srv.assignJob(ctx, rn, ts.URL)
		if err != nil {
			t.Fatal(err)
		}
		if req == nil {
			return 0
		}
		return req.JobID
	}

	other := newRun("t/other", "refs/heads/main")
	branch := newRun("t/deploy", "refs/heads/feature")
	for name, jobs := range map[string]map[string]*Job{"t/other": other, "t/deploy@feature": branch} {
		if j := jobs["deploy"]; j.Status != protocol.StatusCompleted || j.Conclusion != protocol.ResultFailure ||
			!strings.Contains(j.Error, `"deploy" is reserved`) {
			t.Errorf("%s's deploy job: %+v", name, j)
		}
	}
	// Those runs' build jobs are queued, but not for the deploy runner.
	if id := assign(deployRunner); id != 0 {
		t.Fatalf("the deploy runner took job %d", id)
	}
	main := newRun("t/deploy", "refs/heads/main")
	got := map[int64]bool{assign(deployRunner): true, assign(deployRunner): true}
	if !got[main["deploy"].ID] || !got[main["build"].ID] {
		t.Errorf("the deploy runner took %v, want t/deploy@main's jobs %d and %d", got, main["deploy"].ID, main["build"].ID)
	}
	if id := assign(deployRunner); id != 0 {
		t.Errorf("the deploy runner took job %d", id)
	}
	got = map[int64]bool{assign(plainRunner): true, assign(plainRunner): true}
	if !got[other["build"].ID] || !got[branch["build"].ID] {
		t.Errorf("the plain runner took %v, want the build jobs %d and %d", got, other["build"].ID, branch["build"].ID)
	}

	// The Runners page shows who may use a runner's reserved labels.
	if _, err := srv.db.Exec(`INSERT INTO runners (name, labels, token_hash, created_at) VALUES ('deploy', ?, 'x', 0)`, mustJSON(deployRunner.Labels)); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest("GET", ts.URL+"/api/runners", nil)
	req.SetBasicAuth("admin", "pw")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var runners []Runner
	if err := json.NewDecoder(resp.Body).Decode(&runners); err != nil || len(runners) != 1 ||
		strings.Join(runners[0].Reserved["deploy"], ",") != "t/deploy" {
		t.Errorf("runners: %+v %v", runners, err)
	}
}
