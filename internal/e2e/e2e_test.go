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

const isolatedWorkflow = `
name: Isolated
on: push
jobs:
  isolated:
    runs-on: self-hosted
    steps:
      - uses: actions/checkout@v4
      - run: test "$(cat hello.txt)" = hi && touch hello.txt new.txt
      - name: No Docker socket, a home of its own
        run: |
          test ! -e /var/run/docker.sock
          test "$(id -un)" = runner
          touch "$HOME/.gitconfig"
          echo "isolated home=$HOME"
      - name: The runner's --job-options apply
        run: test "$(cat /sys/fs/cgroup/memory.max)" = 1073741824
      - name: Leave a process behind
        run: setsid sleep 600 >/dev/null 2>&1 < /dev/null &
      - uses: docker://debian:trixie-slim
        with:
          args: test -f hello.txt
      - name: The runner's --job-options apply to docker:// steps too
        uses: docker://debian:trixie-slim
        with:
          entrypoint: sh
          args: -c 'test "$(cat /sys/fs/cgroup/memory.max)" = 1073741824'
      - uses: ./action
  services:
    runs-on: self-hosted
    services:
      svc:
        image: kiwicd-e2e-job:test
        options: --health-cmd true --health-interval 1s
    steps:
      - run: getent hosts svc && echo "service reachable"
  privileged:
    runs-on: self-hosted
    container:
      image: debian:trixie-slim
      options: --privileged
    steps:
      - run: echo "privileged ran"
  host-volume:
    runs-on: self-hosted
    container:
      image: debian:trixie-slim
      volumes: ["/:/host"]
    steps:
      - run: echo "host volume ran"
`

const dockerAction = `
name: Docker action
runs:
  using: docker
  image: Dockerfile
`

// TestIsolatedJobs runs jobs with --job-image: each in a container of its
// own on a network of its own, without the Docker socket, refusing container
// options that reach the host, and leaving nothing behind.
func TestIsolatedJobs(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	if out, err := exec.Command("docker", "image", "inspect", "debian:trixie-slim").CombinedOutput(); err != nil {
		t.Skipf("needs Docker with debian:trixie-slim: %v %s", err, out)
	}
	const image = "kiwicd-e2e-job:test"
	build := exec.Command("docker", "build", "-q", "-t", image, "-")
	build.Stdin = strings.NewReader("FROM debian:trixie-slim\nRUN useradd -m -u 1000 runner\nUSER runner\nCMD [\"sleep\", \"infinity\"]\n")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("docker build: %v\n%s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "image", "rm", image).Run() })

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
	creds, err := agent.Register(ctx, ts.URL, "reg", "e2e-isolated", nil, "test")
	if err != nil {
		t.Fatal(err)
	}
	a, err := agent.New(agent.Config{Creds: creds, WorkDir: filepath.Join(tmp, "work"), Version: "test", JobImage: image,
		JobOptions: "--memory 1g --pids-limit 512", JobBridgePrefix: "kcdtst-"})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = a.Run(ctx) }()

	api("POST", "/api/repos", `{"name":"t/repo"}`, nil)
	src := filepath.Join(tmp, "src")
	files := map[string]string{
		".github/workflows/ci.yml": isolatedWorkflow,
		"hello.txt":                "hi\n",
		"action/action.yml":        dockerAction,
		// The action is built on the job's network: its RUN resolves the job
		// container's alias, which the default bridge can't. The nonce
		// keeps the layer cache from skipping the RUN.
		"action/nonce": fmt.Sprint(time.Now().UnixNano()),
		"action/Dockerfile": "FROM debian:trixie-slim\nCOPY nonce /nonce\nRUN getent hosts isolated\n" +
			"ENTRYPOINT [\"sh\", \"-c\", \"echo docker action ran\"]\n",
	}
	for name, content := range files {
		_ = os.MkdirAll(filepath.Dir(filepath.Join(src, name)), 0o755)
		_ = os.WriteFile(filepath.Join(src, name), []byte(content), 0o644)
	}
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
	for time.Now().Before(deadline.Add(3 * time.Minute)) {
		api("GET", fmt.Sprintf("/api/runs/%d", runs[0].ID), "", &r)
		if r.Run.Status == "completed" {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	logs := map[string]string{}
	got := map[string]string{}
	for _, j := range r.Jobs {
		got[j.Name] = j.Conclusion
		var lines []struct{ Text string }
		api("GET", fmt.Sprintf("/api/jobs/%d/logs", j.ID), "", &lines)
		for _, l := range lines {
			logs[j.Name] += l.Text + "\n"
		}
	}
	want := map[string]string{"isolated": "success", "services": "success", "privileged": "failure", "host-volume": "failure"}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("job %s: got %q want %q\n%s", k, got[k], v, logs[k])
		}
	}
	for _, s := range []string{"isolated home=/home/runner", "docker action ran"} {
		if !strings.Contains(logs["isolated"], s) {
			t.Errorf("isolated job's log lacks %q:\n%s", s, logs["isolated"])
		}
	}
	if !strings.Contains(logs["privileged"], `"--privileged" is not allowed`) || strings.Contains(logs["privileged"], "privileged ran") {
		t.Errorf("privileged job:\n%s", logs["privileged"])
	}
	if !strings.Contains(logs["host-volume"], `"/:/host" is not allowed`) || strings.Contains(logs["host-volume"], "host volume ran") {
		t.Errorf("host-volume job:\n%s", logs["host-volume"])
	}

	// The jobs' containers (and the process left running in one), volumes,
	// networks and action images are gone.
	prefix := fmt.Sprintf("kiwicd-r%d-j", creds.RunnerID)
	for _, args := range [][]string{
		{"ps", "-a", "--format", "{{.Names}}"},
		{"volume", "ls", "--format", "{{.Name}}"},
		{"network", "ls", "--format", "{{.Name}}"},
		{"image", "ls", "--format", "{{.Repository}}"},
	} {
		out, err := exec.Command("docker", args...).Output()
		if err != nil {
			t.Fatalf("docker %v: %v", args, err)
		}
		for _, name := range strings.Fields(string(out)) {
			if strings.HasPrefix(name, prefix) {
				t.Errorf("docker %s: %s is left", args[0], name)
			}
		}
	}
}

const callerWorkflow = `
name: Caller
on: push
jobs:
  call:
    strategy:
      matrix:
        n: [1, 2]
    uses: ./.github/workflows/lib.yml
    with:
      greeting: hello ${{ matrix.n }}
      count: ${{ matrix.n }}
    secrets:
      key: ${{ secrets.TOKEN }}
  after:
    needs: call
    runs-on: self-hosted
    steps:
      - run: test "${{ needs.call.outputs.result }}" = "hi" && echo "outputs reached the caller"
`

const libWorkflow = `
on:
  workflow_call:
    inputs:
      greeting:
        type: string
        required: true
      count:
        type: number
      shout:
        type: boolean
        default: false
    secrets:
      key:
        required: true
    outputs:
      result:
        value: ${{ jobs.first.outputs.v }}
jobs:
  first:
    runs-on: self-hosted
    outputs:
      v: ${{ steps.v.outputs.v }}
    steps:
      - uses: actions/checkout@v4
      - id: v
        run: echo "v=$(cat hello.txt)" >> "$GITHUB_OUTPUT"
      - run: |
          echo "greeting=${{ inputs.greeting }} count=${{ inputs.count }} shout=${{ inputs.shout }} key=${{ secrets.key }} token=${{ secrets.TOKEN }}"
          test "${{ inputs.count }}" -ge 1
          test "${{ github.event_name }}" = push
  second:
    needs: first
    runs-on: self-hosted
    steps:
      - run: test "${{ needs.first.outputs.v }}" = hi && test "${{ inputs.shout }}" = false
`

// TestReusableWorkflow runs a workflow that calls another with a matrix,
// inputs, a secret and an output.
func TestReusableWorkflow(t *testing.T) {
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
	a, err := agent.New(agent.Config{Creds: creds, WorkDir: filepath.Join(tmp, "work"), Version: "test", Capacity: 2})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = a.Run(ctx) }()

	api("POST", "/api/repos", `{"name":"t/repo"}`, nil)
	api("PUT", "/api/secrets?repo=t/repo", `{"name":"TOKEN","value":"s3cret-value"}`, nil)
	src := filepath.Join(tmp, "src")
	files := map[string]string{".github/workflows/ci.yml": callerWorkflow, ".github/workflows/lib.yml": libWorkflow, "hello.txt": "hi\n"}
	for name, content := range files {
		_ = os.MkdirAll(filepath.Dir(filepath.Join(src, name)), 0o755)
		_ = os.WriteFile(filepath.Join(src, name), []byte(content), 0o644)
	}
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
	for time.Now().Before(deadline.Add(2 * time.Minute)) {
		api("GET", fmt.Sprintf("/api/runs/%d", runs[0].ID), "", &r)
		if r.Run.Status == "completed" {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	logs := map[string]string{}
	got := map[string]string{}
	for _, j := range r.Jobs {
		got[j.Name] = j.Conclusion
		var lines []struct{ Text string }
		api("GET", fmt.Sprintf("/api/jobs/%d/logs", j.ID), "", &lines)
		for _, l := range lines {
			logs[j.Name] += l.Text + "\n"
		}
	}
	for _, name := range []string{"call (1)", "call (2)", "call (1) / first", "call (1) / second", "call (2) / first", "call (2) / second", "after"} {
		if got[name] != "success" {
			t.Errorf("job %s: %q (all: %v)\n%s", name, got[name], got, logs[name])
		}
	}
	if r.Run.Conclusion != "success" {
		t.Errorf("run: %+v", r.Run)
	}
	if want := "greeting=hello 2 count=2 shout=false key=*** token="; !strings.Contains(logs["call (2) / first"], want) ||
		strings.Contains(logs["call (2) / first"], "s3cret-value") {
		t.Errorf("call (2) / first's log lacks %q:\n%s", want, logs["call (2) / first"])
	}
	if !strings.Contains(logs["after"], "outputs reached the caller") {
		t.Errorf("after's log:\n%s", logs["after"])
	}
}
