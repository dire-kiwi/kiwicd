package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dire-kiwi/kiwicd/internal/protocol"
)

func TestParseProtectedLabels(t *testing.T) {
	pl, err := parseProtectedLabels([]string{"Trusted", "deploy"})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(pl.of([]string{"self-hosted", "trusted", "DEPLOY", "x"}), ","); got != "trusted,DEPLOY" {
		t.Errorf("of = %q", got)
	}
	for _, bad := range []string{"", " ", "a=b", "a,b"} {
		if _, err := parseProtectedLabels([]string{bad}); err == nil {
			t.Errorf("parsed %q", bad)
		}
	}
}

const protectedWorkflow = `name: CI
on: push
jobs:
  deploy:
    runs-on: [self-hosted, trusted]
    steps:
      - run: echo deploy
  build:
    runs-on: self-hosted
    steps:
      - run: echo build
`

// TestProtectedLabels: the trusted runner takes only jobs from protected
// refs, even ones any runner could take; jobs from other refs that ask for it
// fail at once, and so do queued ones whose ref stops being protected.
func TestProtectedLabels(t *testing.T) {
	srv, err := New(Config{DataDir: filepath.Join(t.TempDir(), "data"), AdminPassword: "pw", Version: "test",
		ProtectedLabels: []string{"trusted"}})
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
		if out != nil {
			_ = json.NewDecoder(resp.Body).Decode(out)
		}
		return resp.StatusCode
	}
	if code := api("POST", "/api/repos", `{"name":"t/app"}`, nil); code != 200 {
		t.Fatalf("create repo: %d", code)
	}
	if code := api("PATCH", "/api/repos/t/app", `{"protected_refs":["release/*"]}`, nil); code != 200 {
		t.Fatalf("protect release/*: %d", code)
	}
	newRun := func(ref string) (int64, map[string]*Job) {
		t.Helper()
		r, err := srv.getRepo("t/app")
		if err != nil {
			t.Fatal(err)
		}
		id, err := srv.createRun(ctx, runSpec{repo: r, file: ".github/workflows/ci.yml", yaml: protectedWorkflow, name: "CI", event: "push",
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
		return id, byKey
	}
	trusted := &Runner{ID: 1, Name: "trusted", Labels: normalizeLabels([]string{"trusted"}, "linux", "amd64")}
	plain := &Runner{ID: 2, Name: "plain", Labels: normalizeLabels(nil, "linux", "amd64")}
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

	featureID, feature := newRun("refs/heads/feature")
	if j := feature["deploy"]; j.Status != protocol.StatusCompleted || j.Conclusion != protocol.ResultFailure ||
		!strings.Contains(j.Error, `label "trusted" take only jobs from protected refs`) || !strings.Contains(j.Error, "refs/heads/feature") {
		t.Errorf("feature's deploy job: %+v", j)
	}
	// Its build job is queued, but not for the trusted runner, and the run
	// page says so.
	if id := assign(trusted); id != 0 {
		t.Fatalf("the trusted runner took job %d", id)
	}
	var detail struct {
		ProtectedLabels []string `json:"protected_labels"`
	}
	if api("GET", "/api/runs/"+fmt.Sprint(featureID), "", &detail); strings.Join(detail.ProtectedLabels, ",") != "trusted" {
		t.Errorf("feature run's protected_labels: %v", detail.ProtectedLabels)
	}

	mainID, main := newRun("refs/heads/main")
	got := map[int64]bool{assign(trusted): true, assign(trusted): true}
	if !got[main["deploy"].ID] || !got[main["build"].ID] {
		t.Errorf("the trusted runner took %v, want main's jobs %d and %d", got, main["deploy"].ID, main["build"].ID)
	}
	if api("GET", "/api/runs/"+fmt.Sprint(mainID), "", &detail); len(detail.ProtectedLabels) != 0 {
		t.Errorf("main run's protected_labels: %v", detail.ProtectedLabels)
	}
	if id := assign(plain); id != feature["build"].ID {
		t.Errorf("the plain runner took %d, want feature's build job %d", id, feature["build"].ID)
	}

	// A release run is queued, then release/* stops being protected: its
	// deploy job fails rather than wait for a runner that won't take it.
	_, release := newRun("refs/heads/release/1")
	if release["deploy"].Status != protocol.StatusQueued {
		t.Fatalf("release's deploy job: %+v", release["deploy"])
	}
	if code := api("PATCH", "/api/repos/t/app", `{"protected_refs":[]}`, nil); code != 200 {
		t.Fatalf("unprotect: %d", code)
	}
	if id := assign(trusted); id != 0 {
		t.Errorf("the trusted runner took job %d", id)
	}
	if j, _ := srv.getJob(release["deploy"].ID); j.Status != protocol.StatusCompleted || !strings.Contains(j.Error, "refs/heads/release/1 is not a protected ref") {
		t.Errorf("release's deploy job after unprotecting: %+v", j)
	}

	// The Runners page shows which runners take only protected refs' jobs.
	if _, err := srv.db.Exec(`INSERT INTO runners (name, labels, token_hash, created_at) VALUES ('trusted', ?, 'x', 0)`, mustJSON(trusted.Labels)); err != nil {
		t.Fatal(err)
	}
	var runners []Runner
	if api("GET", "/api/runners", "", &runners); len(runners) != 1 || strings.Join(runners[0].ProtectedLabels, ",") != "trusted" {
		t.Errorf("runners: %+v", runners)
	}
}
