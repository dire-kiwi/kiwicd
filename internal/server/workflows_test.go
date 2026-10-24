package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestUploadedWorkflows(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	tmp := t.TempDir()
	srv, err := New(Config{DataDir: filepath.Join(tmp, "data"), AdminPassword: "pw", Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	api := func(method, path, contentType, body string, out any) int {
		t.Helper()
		req, _ := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
		req.SetBasicAuth("admin", "pw")
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		if out != nil {
			_ = json.Unmarshal(b, out)
		}
		return resp.StatusCode
	}
	type run struct {
		ID           int64  `json:"id"`
		WorkflowFile string `json:"workflow_file"`
		WorkflowName string `json:"workflow_name"`
		Ref          string `json:"ref"`
	}
	listRuns := func() []run {
		t.Helper()
		var runs []run
		api("GET", "/api/runs?repo=t/r", "", "", &runs)
		return runs
	}
	runsOf := func(file string) int {
		n := 0
		for _, r := range listRuns() {
			if r.WorkflowFile == file {
				n++
			}
		}
		return n
	}

	api("POST", "/api/repos", "", `{"name":"t/r"}`, nil)
	base := "/api/repos/t/r/uploaded-workflows/"
	deploy := "name: Deploy\non:\n  push:\n    branches: [main]\n  workflow_dispatch:\njobs:\n  a:\n    runs-on: self-hosted\n    steps:\n      - run: echo private\n"

	// Raw YAML bodies and JSON bodies are both accepted; broken YAML and bad names are not.
	if code := api("PUT", base+"deploy.yml", "", deploy, nil); code != 200 {
		t.Fatalf("upload: %d", code)
	}
	if code := api("PUT", base+"other.yaml", "application/json", `{"content":"on: workflow_dispatch\njobs:\n  b:\n    runs-on: x\n    steps:\n      - run: true\n"}`, nil); code != 200 {
		t.Fatalf("upload json: %d", code)
	}
	for name, body := range map[string]string{"bad.yml": "on: [push\n", "empty.yml": "  \n", "nope.txt": deploy, ".hidden.yml": deploy} {
		if code := api("PUT", base+name, "", body, nil); code != 400 {
			t.Errorf("upload %s: %d, want 400", name, code)
		}
	}
	var got uploadedWorkflow
	if code := api("GET", base+"deploy.yml", "", "", &got); code != 200 || got.Content != deploy {
		t.Errorf("get: %d %+v", code, got)
	}

	// Listed before the repository has any commits.
	var wfs []workflowInfo
	api("GET", "/api/repos/t/r/workflows", "", "", &wfs)
	if len(wfs) != 2 || wfs[0].File != "kiwicd:deploy.yml" || !wfs[0].Uploaded || wfs[0].Name != "Deploy" || !wfs[0].Dispatch {
		t.Fatalf("workflows: %+v", wfs)
	}

	// A push without any workflow in the repository runs the uploaded one,
	// subject to its branch filter.
	src := filepath.Join(tmp, "src")
	gitT := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = src
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	_ = os.MkdirAll(src, 0o755)
	_ = os.WriteFile(filepath.Join(src, "f"), []byte("1"), 0o644)
	gitT("init", "-q", "-b", "main")
	gitT("add", "-A")
	gitT("commit", "-qm", "one")
	remote := strings.Replace(ts.URL, "http://", "http://admin:pw@", 1) + "/git/t/r.git"
	push := func(refspec string) {
		t.Helper()
		gitT("push", "-q", remote, refspec)
		// Pushes trigger a sync in the background; this one waits for it.
		api("POST", "/api/repos/t/r/sync", "", "", nil)
	}
	push("main")
	push("main:other")
	runs := listRuns()
	if len(runs) != 1 || runs[0].WorkflowFile != "kiwicd:deploy.yml" || runs[0].Ref != "refs/heads/main" || runs[0].WorkflowName != "Deploy" {
		t.Fatalf("runs after push: %+v", runs)
	}

	// Dispatch by bare file name falls back to the uploaded workflow.
	var disp map[string]int64
	if code := api("POST", "/api/repos/t/r/dispatch", "", `{"workflow":"deploy.yml","ref":"other"}`, &disp); code != 200 || disp["run_id"] == 0 {
		t.Fatalf("dispatch: %d %v", code, disp)
	}
	if r, err := srv.getRun(disp["run_id"]); err != nil || r.WorkflowFile != "kiwicd:deploy.yml" || r.Event != "workflow_dispatch" ||
		!strings.Contains(r.WorkflowYAML, "echo private") {
		t.Errorf("dispatched run: %+v %v", r, err)
	}

	// A committed workflow with the same file name stays separate.
	_ = os.MkdirAll(filepath.Join(src, ".github/workflows"), 0o755)
	_ = os.WriteFile(filepath.Join(src, ".github/workflows/deploy.yml"), []byte("on: push\njobs:\n  c:\n    runs-on: self-hosted\n    steps:\n      - run: true\n"), 0o644)
	gitT("add", "-A")
	gitT("commit", "-qm", "two")
	push("main")
	if runsOf("kiwicd:deploy.yml") != 3 || runsOf(".github/workflows/deploy.yml") != 1 {
		t.Errorf("runs after second push: %+v", listRuns())
	}

	// Deleting removes it from later pushes; deleting the repository removes the rest.
	if code := api("DELETE", base+"deploy.yml", "", "", nil); code != 200 {
		t.Errorf("delete: %d", code)
	}
	if code := api("DELETE", base+"deploy.yml", "", "", nil); code != 404 {
		t.Errorf("delete again: %d", code)
	}
	gitT("commit", "-qm", "three", "--allow-empty")
	push("main")
	if runsOf("kiwicd:deploy.yml") != 3 || runsOf(".github/workflows/deploy.yml") != 2 {
		t.Errorf("runs after deleting: %+v", listRuns())
	}
	wfs = nil
	api("GET", "/api/repos/t/r/workflows", "", "", &wfs)
	if len(wfs) != 2 || wfs[0].File != ".github/workflows/deploy.yml" || wfs[1].File != "kiwicd:other.yaml" {
		t.Errorf("workflows after delete: %+v", wfs)
	}
	api("DELETE", "/api/repos/t/r", "", "", nil)
	var n int
	_ = srv.db.QueryRow(`SELECT count(*) FROM uploaded_workflows`).Scan(&n)
	if n != 0 {
		t.Errorf("%d uploaded workflows left after deleting the repository", n)
	}
}
