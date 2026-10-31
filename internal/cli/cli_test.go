package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
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
name: Deploy
on:
  schedule:
    - cron: '0 0 1 1 *'
  workflow_dispatch:
    inputs:
      target:
        required: true
jobs:
  deploy:
    runs-on: self-hosted
    steps:
      - name: Say hello
        run: echo "hello ${{ inputs.target }} secret=${{ secrets.DEPLOY_KEY }} var=${{ vars.REGION }}"
  fail:
    needs: deploy
    if: inputs.target == 'broken'
    runs-on: self-hosted
    steps:
      - name: Break
        run: echo "about to fail" && exit 3
`

// testEnv is a server with an agent and a hosted repository t/app holding
// workflow, and the CLI's config in a temporary directory.
type testEnv struct {
	t   *testing.T
	url string
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	old := pollInterval
	pollInterval = 50 * time.Millisecond
	t.Cleanup(func() { pollInterval = old })
	tmp := t.TempDir()
	t.Setenv("KIWICD_CONFIG", filepath.Join(tmp, "cli.json"))
	t.Setenv("KIWICD_URL", "")
	t.Setenv("KIWICD_API_TOKEN", "")
	srv, err := server.New(server.Config{DataDir: filepath.Join(tmp, "data"), AdminPassword: "pw", RegistrationToken: "reg", Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	creds, err := agent.Register(ctx, ts.URL, "reg", "cli-test", nil, "test")
	if err != nil {
		t.Fatal(err)
	}
	a, err := agent.New(agent.Config{Creds: creds, WorkDir: filepath.Join(tmp, "work"), Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = a.Run(ctx) }()

	admin := &Client{URL: ts.URL, Token: "pw"}
	if err := admin.Do(ctx, "POST", "/api/repos", map[string]string{"name": "t/app"}, nil); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(tmp, "src")
	_ = os.MkdirAll(filepath.Join(src, ".github/workflows"), 0o755)
	_ = os.WriteFile(filepath.Join(src, ".github/workflows/deploy.yml"), []byte(workflow), 0o644)
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"}, {"add", "-A"}, {"commit", "-qm", "init"},
		{"push", "-q", strings.Replace(ts.URL, "http://", "http://admin:pw@", 1) + "/git/t/app.git", "main"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = src
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	// The push syncs in the background.
	deadline := time.Now().Add(10 * time.Second)
	for {
		if ws, err := admin.Workflows(ctx, "t/app", ""); err == nil && len(ws) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the pushed workflow never showed up")
		}
		time.Sleep(50 * time.Millisecond)
	}
	return &testEnv{t: t, url: ts.URL}
}

// kiwicd runs the command with stdin and returns its exit status and output.
func (e *testEnv) kiwicd(stdin string, args ...string) (int, string, string) {
	e.t.Helper()
	var out, errOut bytes.Buffer
	a := &App{Stdin: strings.NewReader(stdin), Stdout: &out, Stderr: &errOut, Version: "test", stdinFD: -1}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	code := a.Run(ctx, args)
	return code, out.String(), errOut.String()
}

// ok runs the command and fails the test unless it succeeds.
func (e *testEnv) ok(stdin string, args ...string) string {
	e.t.Helper()
	code, out, errOut := e.kiwicd(stdin, args...)
	if code != 0 {
		e.t.Fatalf("kiwicd %s: exit %d\n%s%s", strings.Join(args, " "), code, out, errOut)
	}
	return out
}

func TestCLI(t *testing.T) {
	e := newTestEnv(t)
	if code, _, errOut := e.kiwicd("", "run", "list"); code != 1 || !strings.Contains(errOut, "not logged in") {
		t.Fatalf("before login: %d %s", code, errOut)
	}
	if code, _, errOut := e.kiwicd("wrong\n", "login", "--url", e.url, "--password-stdin"); code != 1 || !strings.Contains(errOut, "login required") {
		t.Fatalf("wrong password: %d %s", code, errOut)
	}
	e.ok("pw\n", "login", "--url", e.url, "--password-stdin", "--name", "test")
	b, err := os.ReadFile(os.Getenv("KIWICD_CONFIG"))
	if err != nil || !strings.Contains(string(b), `"token": "kiwicd_`) || strings.Contains(string(b), `"pw"`) {
		t.Fatalf("saved config: %v %s", err, b)
	}
	if out := e.ok("", "whoami"); !strings.HasPrefix(out, "admin on "+e.url) {
		t.Fatalf("whoami: %s", out)
	}
	if out := e.ok("", "token", "list"); !strings.Contains(out, "test") || !strings.Contains(out, "never") {
		t.Fatalf("token list: %s", out)
	}

	// Secrets and variables.
	e.ok("deploy-s3cret\n", "secret", "set", "DEPLOY_KEY", "--repo", "t/app")
	e.ok("", "secret", "set", "GLOBAL_ONE", "--value", "x")
	e.ok("", "variable", "set", "REGION", "nz-north", "--repo", "t/app")
	if out := e.ok("", "secret", "list", "--repo", "t/app"); !strings.Contains(out, "DEPLOY_KEY") || strings.Contains(out, "s3cret") {
		t.Fatalf("secret list: %s", out)
	}
	if out := e.ok("", "secrets"); !strings.Contains(out, "GLOBAL_ONE") || strings.Contains(out, "DEPLOY_KEY") {
		t.Fatalf("global secrets: %s", out)
	}
	if out := e.ok("", "variable", "list", "--repo", "t/app"); !strings.Contains(out, "nz-north") {
		t.Fatalf("variable list: %s", out)
	}
	e.ok("", "secret", "delete", "GLOBAL_ONE")
	if code, _, errOut := e.kiwicd("", "secret", "delete", "GLOBAL_ONE"); code != 1 || !strings.Contains(errOut, "no global secret named GLOBAL_ONE") {
		t.Fatalf("deleting a missing secret: %d %s", code, errOut)
	}
	if code, _, errOut := e.kiwicd("", "secret", "set", "BAD-NAME", "--value", "x"); code != 1 || !strings.Contains(errOut, "names may contain") {
		t.Fatalf("bad name: %d %s", code, errOut)
	}

	// Repositories and workflows.
	if out := e.ok("", "repo", "list"); !strings.Contains(out, "t/app") || !strings.Contains(out, "hosted") {
		t.Fatalf("repo list: %s", out)
	}
	if out := e.ok("", "workflow", "list", "t/app"); !strings.Contains(out, ".github/workflows/deploy.yml") || !strings.Contains(out, "target*") ||
		!strings.Contains(out, "schedule (0 0 1 1 *)") {
		t.Fatalf("workflow list: %s", out)
	}

	// Starting and watching a run, flags after the arguments.
	if code, _, errOut := e.kiwicd("", "run", "start", "t/app", "deploy.yml"); code != 1 || !strings.Contains(errOut, "input target is required") {
		t.Fatalf("missing input: %d %s", code, errOut)
	}
	out := e.ok("", "run", "start", "t/app", "deploy.yml", "-i", "target=prod", "--watch")
	if !strings.Contains(out, "Started run 1") || !strings.Contains(out, "✓ success") || !strings.Contains(out, "Say hello") {
		t.Fatalf("run start --watch: %s", out)
	}
	if out := e.ok("", "run", "logs", "1"); !strings.Contains(out, "hello prod secret=*** var=nz-north") || !strings.Contains(out, "--- Say hello") {
		t.Fatalf("run logs: %s", out)
	}
	code, out, _ := e.kiwicd("", "run", "start", "t/app", "deploy.yml", "--input", "target=broken", "--watch")
	if code != 1 || !strings.Contains(out, "✗ failure") {
		t.Fatalf("failing run: %d %s", code, out)
	}
	if out := e.ok("", "run", "logs", "2", "--failed"); !strings.Contains(out, "about to fail") || strings.Contains(out, "hello broken") {
		t.Fatalf("failed logs: %s", out)
	}
	if out := e.ok("", "run", "logs", "2", "--job", "deploy", "--follow"); !strings.Contains(out, "hello broken") {
		t.Fatalf("logs of one job: %s", out)
	}
	if out := e.ok("", "runs", "--repo", "t/app"); !strings.Contains(out, "Deploy #2") || !strings.Contains(out, "Deploy #1") {
		t.Fatalf("run list: %s", out)
	}
	var d RunDetail
	if err := json.Unmarshal([]byte(e.ok("", "run", "view", "2", "--json")), &d); err != nil || d.Run.Conclusion != "failure" || len(d.Jobs) != 2 {
		t.Fatalf("run view --json: %v %+v", err, d)
	}
	if out := e.ok("", "run", "rerun", "1", "--watch"); !strings.Contains(out, "Started run 3") || !strings.Contains(out, "Deploy #1.2") {
		t.Fatalf("rerun: %s", out)
	}
	if out := e.ok("", "api", "runs/3"); !strings.Contains(out, `"attempt": 2`) {
		t.Fatalf("api: %s", out)
	}
	if out := e.ok("", "runner", "list"); !strings.Contains(out, "cli-test") {
		t.Fatalf("runner list: %s", out)
	}

	e.ok("", "logout")
	if code, _, _ := e.kiwicd("", "whoami"); code != 1 {
		t.Fatal("still logged in after logout")
	}
}

// mcpSession talks to kiwicd mcp over pipes.
type mcpSession struct {
	t   *testing.T
	in  io.WriteCloser
	out *json.Decoder
	id  int
}

func (s *mcpSession) call(method string, params any) map[string]any {
	s.t.Helper()
	s.id++
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": s.id, "method": method, "params": params})
	if _, err := s.in.Write(append(b, '\n')); err != nil {
		s.t.Fatal(err)
	}
	var resp map[string]any
	if err := s.out.Decode(&resp); err != nil {
		s.t.Fatal(err)
	}
	if resp["id"] != float64(s.id) {
		s.t.Fatalf("response %v to request %d", resp, s.id)
	}
	return resp
}

// tool calls a tool and returns its text and whether it failed.
func (s *mcpSession) tool(name string, args map[string]any) (string, bool) {
	s.t.Helper()
	resp := s.call("tools/call", map[string]any{"name": name, "arguments": args})
	res, ok := resp["result"].(map[string]any)
	if !ok {
		s.t.Fatalf("%s: %v", name, resp)
	}
	content := res["content"].([]any)
	isErr, _ := res["isError"].(bool)
	return content[0].(map[string]any)["text"].(string), isErr
}

func startMCP(t *testing.T, e *testEnv, args ...string) *mcpSession {
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	a := &App{Stdin: inR, Stdout: outW, Stderr: io.Discard, Version: "test", stdinFD: -1}
	done := make(chan struct{})
	go func() {
		defer close(done)
		a.Run(context.Background(), append([]string{"mcp"}, args...))
		outW.Close()
	}()
	t.Cleanup(func() {
		inW.Close()
		<-done
	})
	return &mcpSession{t: t, in: inW, out: json.NewDecoder(outR)}
}

func TestMCP(t *testing.T) {
	e := newTestEnv(t)
	e.ok("pw\n", "login", "--url", e.url, "--password-stdin")
	s := startMCP(t, e)

	init := s.call("initialize", map[string]any{"protocolVersion": "2025-03-26", "capabilities": map[string]any{},
		"clientInfo": map[string]any{"name": "test", "version": "1"}})
	if res := init["result"].(map[string]any); res["protocolVersion"] != "2025-03-26" || res["serverInfo"].(map[string]any)["name"] != "kiwicd" {
		t.Fatalf("initialize: %v", init)
	}
	// A notification gets no response; the next response is the ping's.
	_, _ = s.in.Write([]byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n"))
	if ping := s.call("ping", nil); ping["result"] == nil || ping["error"] != nil {
		t.Fatalf("ping: %v", ping)
	}
	if resp := s.call("nope", nil); resp["error"].(map[string]any)["code"] != float64(-32601) {
		t.Fatalf("unknown method: %v", resp)
	}
	list := s.call("tools/list", nil)["result"].(map[string]any)["tools"].([]any)
	names := map[string]bool{}
	for _, tool := range list {
		names[tool.(map[string]any)["name"].(string)] = true
	}
	for _, n := range []string{"list_runs", "get_run", "get_job_logs", "start_run", "set_secret", "wait_for_run"} {
		if !names[n] {
			t.Errorf("no tool %s in %v", n, names)
		}
	}

	if text, isErr := s.tool("set_secret", map[string]any{"repo": "t/app", "name": "DEPLOY_KEY", "value": "mcp-s3cret"}); isErr {
		t.Fatalf("set_secret: %s", text)
	}
	if text, _ := s.tool("list_secrets", map[string]any{"repo": "t/app"}); !strings.Contains(text, "DEPLOY_KEY") || strings.Contains(text, "mcp-s3cret") {
		t.Fatalf("list_secrets: %s", text)
	}
	if text, isErr := s.tool("start_run", map[string]any{"repo": "t/app", "workflow": "deploy.yml"}); !isErr || !strings.Contains(text, "required") {
		t.Fatalf("start_run without its input: %v %s", isErr, text)
	}
	text, isErr := s.tool("start_run", map[string]any{"repo": "t/app", "workflow": "deploy.yml", "inputs": map[string]any{"target": "broken"}})
	var started struct {
		RunID int64 `json:"run_id"`
	}
	if err := json.Unmarshal([]byte(text), &started); isErr || err != nil || started.RunID == 0 {
		t.Fatalf("start_run: %s", text)
	}
	text, _ = s.tool("wait_for_run", map[string]any{"run_id": started.RunID, "timeout_seconds": 30})
	var run struct {
		State string
		Jobs  []struct {
			ID    int64
			Name  string
			State string
		}
	}
	if err := json.Unmarshal([]byte(text), &run); err != nil || run.State != "failure" || len(run.Jobs) != 2 {
		t.Fatalf("wait_for_run: %v %s", err, text)
	}
	var failed int64
	for _, j := range run.Jobs {
		if j.State == "failure" {
			failed = j.ID
		}
	}
	text, _ = s.tool("get_job_logs", map[string]any{"job_id": failed})
	if !strings.Contains(text, "--- step 1: Break") || !strings.Contains(text, "about to fail") || !strings.Contains(text, ": failure") {
		t.Fatalf("get_job_logs: %s", text)
	}
	if text, _ := s.tool("get_job_logs", map[string]any{"job_id": run.Jobs[0].ID, "tail": 1}); !strings.Contains(text, "earlier lines left out") {
		t.Fatalf("get_job_logs tail 1: %s", text)
	}
	if text, _ := s.tool("list_runs", map[string]any{"repo": "t/app"}); !strings.Contains(text, fmt.Sprintf(`"id": %d`, started.RunID)) {
		t.Fatalf("list_runs: %s", text)
	}
	if text, isErr := s.tool("get_run", map[string]any{"run_id": 999}); !isErr || !strings.Contains(text, "no such run") {
		t.Fatalf("get_run of a missing run: %v %s", isErr, text)
	}

	// --read-only leaves out the tools that change things.
	ro := startMCP(t, e, "--read-only")
	for _, tool := range ro.call("tools/list", nil)["result"].(map[string]any)["tools"].([]any) {
		m := tool.(map[string]any)
		if m["annotations"].(map[string]any)["readOnlyHint"] != true {
			t.Errorf("read-only server lists %v", m["name"])
		}
	}
	if resp := ro.call("tools/call", map[string]any{"name": "set_secret", "arguments": map[string]any{}}); resp["error"] == nil {
		t.Fatalf("read-only set_secret: %v", resp)
	}
}
