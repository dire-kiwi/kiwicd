// Package e2e runs a real server and agent against a pushed repository.
package e2e

import (
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

	"github.com/dire-kiwi/kiwicd/internal/agent"
	"github.com/dire-kiwi/kiwicd/internal/server"
)

const workflow = `
name: CI
on: [push, workflow_dispatch]
jobs:
  build:
    runs-on: self-hosted
    outputs:
      v: ${{ steps.v.outputs.v }}
    steps:
      - uses: actions/checkout@v4
      - run: test -f hello.txt
      - id: v
        run: echo "v=$(cat hello.txt)" >> "$GITHUB_OUTPUT"
      - run: echo "secret=${{ secrets.TOKEN }}"
  test:
    needs: build
    runs-on: [self-hosted, linux]
    strategy:
      matrix:
        n: [1, 2]
    steps:
      - run: test "${{ needs.build.outputs.v }}" = "hi" && echo "matrix ${{ matrix.n }}"
  missing-label:
    if: false
    runs-on: nope
    steps:
      - run: exit 1
  after-failure:
    needs: test
    if: failure()
    runs-on: self-hosted
    steps:
      - run: exit 1
`

type runResp struct {
	Run struct {
		Status, Conclusion, Error string
	} `json:"run"`
	Jobs []struct {
		ID                int64
		Key, Name, Status string
		Conclusion        string
		Steps             []struct{ Name, Conclusion string }
	} `json:"jobs"`
}

func run(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v: %v\n%s", args, err, out)
	}
}

func TestPushRunsWorkflow(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	tmp := t.TempDir()
	srv, err := server.New(server.Config{DataDir: filepath.Join(tmp, "data"), AdminPassword: "pw", RegistrationToken: "reg", Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	api := func(method, path, body string, out any) {
		t.Helper()
		req, _ := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
		req.SetBasicAuth("admin", "pw")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode >= 300 {
			t.Fatalf("%s %s: %d %s", method, path, resp.StatusCode, b)
		}
		if out != nil {
			if err := json.Unmarshal(b, out); err != nil {
				t.Fatal(err)
			}
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	creds, err := agent.Register(ctx, ts.URL, "reg", "e2e", nil, "test")
	if err != nil {
		t.Fatal(err)
	}
	a, err := agent.New(agent.Config{Creds: creds, WorkDir: filepath.Join(tmp, "work"), Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = a.Run(ctx) }()

	api("POST", "/api/repos", `{"name":"t/repo"}`, nil)
	api("PUT", "/api/secrets?repo=t/repo", `{"name":"TOKEN","value":"s3cret-value"}`, nil)

	src := filepath.Join(tmp, "src")
	_ = os.MkdirAll(filepath.Join(src, ".github/workflows"), 0o755)
	_ = os.WriteFile(filepath.Join(src, ".github/workflows/ci.yml"), []byte(workflow), 0o644)
	_ = os.WriteFile(filepath.Join(src, "hello.txt"), []byte("hi\n"), 0o644)
	run(t, src, "git", "init", "-q", "-b", "main")
	run(t, src, "git", "add", "-A")
	run(t, src, "git", "commit", "-qm", "init")
	u := strings.Replace(ts.URL, "http://", "http://admin:pw@", 1)
	run(t, src, "git", "push", "-q", u+"/git/t/repo.git", "main")

	var runs []struct{ ID int64 }
	deadline := time.Now().Add(30 * time.Second)
	for len(runs) == 0 && time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
		api("GET", "/api/runs?repo=t/repo", "", &runs)
	}
	if len(runs) != 1 {
		t.Fatalf("expected one run, got %d", len(runs))
	}
	var r runResp
	for time.Now().Before(deadline.Add(60 * time.Second)) {
		api("GET", fmt.Sprintf("/api/runs/%d", runs[0].ID), "", &r)
		if r.Run.Status == "completed" {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	if r.Run.Conclusion != "success" {
		t.Fatalf("run %+v", r)
	}
	got := map[string]string{}
	for _, j := range r.Jobs {
		got[j.Name] = j.Conclusion
	}
	want := map[string]string{"build": "success", "test (1)": "success", "test (2)": "success", "missing-label": "skipped", "after-failure": "skipped"}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("job %s: got %q want %q (all: %v)", k, got[k], v, got)
		}
	}
	// Secrets are masked in logs.
	var logs []struct{ Text string }
	api("GET", fmt.Sprintf("/api/jobs/%d/logs", r.Jobs[0].ID), "", &logs)
	all := ""
	for _, l := range logs {
		all += l.Text + "\n"
	}
	if strings.Contains(all, "s3cret-value") || !strings.Contains(all, "secret=***") {
		t.Errorf("secret not masked:\n%s", all)
	}
}

const listener = `
name: Deploy app
on:
  repository_push:
    repositories: [t/app]
    branches: [main]
jobs:
  deploy:
    runs-on: self-hosted
    steps:
      - uses: actions/checkout@v4
      - run: test -f deploy.sh
      - name: Clone the pushed commit
        env:
          TOKEN: ${{ secrets.KIWICD_TOKEN }}
        run: |
          git -c http.extraHeader="Authorization: Bearer $TOKEN" clone --quiet "$GITHUB_SERVER_URL/git/${{ github.event.repository.full_name }}.git" app
          git -C app checkout --quiet "${{ github.event.after }}"
          test "$(cat app/app.txt)" = v1
          echo "token=$TOKEN deploy=${{ secrets.DEPLOY_KEY }} repo=$GITHUB_REPOSITORY"
`

// TestRepositoryPush deploys a repository from another one that holds the
// workflow and its secrets.
func TestRepositoryPush(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	tmp := t.TempDir()
	srv, err := server.New(server.Config{DataDir: filepath.Join(tmp, "data"), AdminPassword: "pw", RegistrationToken: "reg", Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	api := func(method, path, body string, out any) {
		t.Helper()
		req, _ := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
		req.SetBasicAuth("admin", "pw")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode >= 300 {
			t.Fatalf("%s %s: %d %s", method, path, resp.StatusCode, b)
		}
		if out != nil {
			if err := json.Unmarshal(b, out); err != nil {
				t.Fatal(err)
			}
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	creds, err := agent.Register(ctx, ts.URL, "reg", "e2e", nil, "test")
	if err != nil {
		t.Fatal(err)
	}
	a, err := agent.New(agent.Config{Creds: creds, WorkDir: filepath.Join(tmp, "work"), Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = a.Run(ctx) }()

	api("POST", "/api/repos", `{"name":"t/app"}`, nil)
	api("POST", "/api/repos", `{"name":"t/deploy"}`, nil)
	api("PUT", "/api/secrets?repo=t/deploy", `{"name":"DEPLOY_KEY","value":"deploy-s3cret"}`, nil)
	api("PATCH", "/api/repos/t/app", `{"readers":["t/deploy"]}`, nil)
	u := strings.Replace(ts.URL, "http://", "http://admin:pw@", 1)
	push := func(repo string, files map[string]string) {
		t.Helper()
		src := filepath.Join(tmp, "src", repo)
		for name, content := range files {
			_ = os.MkdirAll(filepath.Dir(filepath.Join(src, name)), 0o755)
			_ = os.WriteFile(filepath.Join(src, name), []byte(content), 0o644)
		}
		run(t, src, "git", "init", "-q", "-b", "main")
		run(t, src, "git", "add", "-A")
		run(t, src, "git", "commit", "-qm", "init")
		run(t, src, "git", "push", "-q", u+"/git/"+repo+".git", "main")
	}
	push("t/deploy", map[string]string{".github/workflows/deploy.yml": listener, "deploy.sh": "true\n"})
	push("t/app", map[string]string{"app.txt": "v1"})

	var runs []struct{ ID int64 }
	deadline := time.Now().Add(30 * time.Second)
	for len(runs) == 0 && time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
		api("GET", "/api/runs?repo=t/deploy", "", &runs)
	}
	if len(runs) != 1 {
		t.Fatalf("expected one run of t/deploy, got %d", len(runs))
	}
	var r runResp
	for time.Now().Before(deadline.Add(60 * time.Second)) {
		api("GET", fmt.Sprintf("/api/runs/%d", runs[0].ID), "", &r)
		if r.Run.Status == "completed" {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	var logs []struct{ Text string }
	if len(r.Jobs) > 0 {
		api("GET", fmt.Sprintf("/api/jobs/%d/logs", r.Jobs[0].ID), "", &logs)
	}
	all := ""
	for _, l := range logs {
		all += l.Text + "\n"
	}
	if r.Run.Conclusion != "success" {
		t.Fatalf("run %+v\n%s", r, all)
	}
	if !strings.Contains(all, "token=*** deploy=*** repo=t/deploy") || strings.Contains(all, "deploy-s3cret") {
		t.Errorf("secrets not masked, or wrong repository:\n%s", all)
	}
	var appRuns []struct{ ID int64 }
	api("GET", "/api/runs?repo=t/app", "", &appRuns)
	if len(appRuns) != 0 {
		t.Errorf("t/app has %d runs", len(appRuns))
	}
}
