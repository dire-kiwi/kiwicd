package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dire-kiwi/kiwicd/internal/agent"
	"github.com/dire-kiwi/kiwicd/internal/sealed"
	"github.com/dire-kiwi/kiwicd/internal/secretsservice"
	"github.com/dire-kiwi/kiwicd/internal/server"
)

const environments = `
name: Deploy
on: push
jobs:
  build:
    runs-on: self-hosted
    steps:
      - run: |
          test "${{ secrets.TOKEN }}" = repo-token
          test "${{ secrets.GLOBAL }}" = global-value
          test "${{ vars.VAR }}" = repo-var
          echo "token=${{ secrets.TOKEN }}"
  deploy:
    needs: build
    runs-on: self-hosted
    environment:
      name: production
      url: https://app.example.com/${{ github.ref_name }}
    steps:
      - run: |
          test "${{ secrets.TOKEN }}" = prod-token
          test "${{ secrets.GLOBAL }}" = global-value
          test "${{ vars.VAR }}" = prod-var
          echo "token=${{ secrets.TOKEN }}"
  staging:
    runs-on: self-hosted
    environment: staging
    steps:
      - run: exit 0
`

// TestSealedEnvironmentSecrets runs a workflow whose secrets the server
// can't read: they're sealed to a secrets service's key, and the agent gets
// them from the service. One job deploys to an environment with secrets and
// variables of its own; another names one its ref may not use.
func TestSealedEnvironmentSecrets(t *testing.T) {
	tmp := t.TempDir()
	key, err := sealed.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	srv, err := server.New(server.Config{DataDir: filepath.Join(tmp, "data"), AdminPassword: "pw", RegistrationToken: "reg", Version: "test",
		SecretsPublicKey: key.Public().String()})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	svc, err := secretsservice.New(secretsservice.Config{Server: ts.URL, Keys: []*sealed.PrivateKey{key}, Clients: []secretsservice.Client{
		{Name: "runners", TokenSHA256: secretsservice.HashToken("svc-token"), Allow: []string{"t/*", "t/repo@production"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	ss := httptest.NewServer(svc.Handler())
	defer ss.Close()

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
	a, err := agent.New(agent.Config{Creds: creds, WorkDir: filepath.Join(tmp, "work"), Version: "test", SecretsURL: ss.URL, SecretsToken: "svc-token"})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = a.Run(ctx) }()

	api("POST", "/api/repos", `{"name":"t/repo"}`, nil)
	api("POST", "/api/repos/t/repo/environments", `{"name":"production","refs":["main"]}`, nil)
	api("POST", "/api/repos/t/repo/environments", `{"name":"staging","refs":["release/*"]}`, nil)
	// The server seals values it is sent; the UI and kiwicd-secrets seal
	// them before they're sent.
	api("PUT", "/api/secrets", `{"name":"GLOBAL","value":"global-value"}`, nil)
	api("PUT", "/api/secrets?repo=t/repo", `{"name":"TOKEN","value":"repo-token"}`, nil)
	prod, err := key.Public().Seal(sealed.Scope{Repository: "t/repo", Environment: "production", Name: "TOKEN"}, "prod-token")
	if err != nil {
		t.Fatal(err)
	}
	api("PUT", "/api/secrets?repo=t/repo&environment=production", fmt.Sprintf(`{"name":"TOKEN","encrypted_value":%q}`, prod), nil)
	api("PUT", "/api/variables?repo=t/repo", `{"name":"VAR","value":"repo-var"}`, nil)
	api("PUT", "/api/variables?repo=t/repo&environment=production", `{"name":"VAR","value":"prod-var"}`, nil)

	var info struct {
		KeyID        string `json:"key_id"`
		ServerSealed int    `json:"server_sealed"`
	}
	api("GET", "/api/secrets/key", "", &info)
	if info.KeyID != key.Public().ID() || info.ServerSealed != 0 {
		t.Errorf("secrets key info %+v", info)
	}
	db, _ := os.ReadFile(filepath.Join(tmp, "data", "kiwicd.db"))
	if strings.Contains(string(db), "repo-token") || strings.Contains(string(db), "global-value") {
		t.Error("the database holds a secret in plain text")
	}

	src := filepath.Join(tmp, "src")
	_ = os.MkdirAll(filepath.Join(src, ".github/workflows"), 0o755)
	_ = os.WriteFile(filepath.Join(src, ".github/workflows/deploy.yml"), []byte(environments), 0o644)
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
	var r struct {
		Run  struct{ Status, Conclusion string } `json:"run"`
		Jobs []struct {
			ID                              int64
			Name, Status, Conclusion, Error string
			Environment                     string `json:"environment"`
			EnvironmentURL                  string `json:"environment_url"`
		} `json:"jobs"`
	}
	for time.Now().Before(deadline.Add(60 * time.Second)) {
		api("GET", fmt.Sprintf("/api/runs/%d", runs[0].ID), "", &r)
		if r.Run.Status == "completed" {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	jobs := map[string]int{}
	for i, j := range r.Jobs {
		jobs[j.Name] = i
	}
	logs := func(name string) string {
		var lines []struct{ Text string }
		api("GET", fmt.Sprintf("/api/jobs/%d/logs", r.Jobs[jobs[name]].ID), "", &lines)
		var b strings.Builder
		for _, l := range lines {
			b.WriteString(l.Text + "\n")
		}
		return b.String()
	}
	for _, name := range []string{"build", "deploy"} {
		j := r.Jobs[jobs[name]]
		if j.Conclusion != "success" {
			t.Errorf("job %s: %s %s\n%s", name, j.Conclusion, j.Error, logs(name))
		} else if l := logs(name); strings.Contains(l, "-token") || !strings.Contains(l, "token=***") {
			t.Errorf("job %s: secret not masked:\n%s", name, l)
		}
	}
	if d := r.Jobs[jobs["deploy"]]; d.Environment != "production" || d.EnvironmentURL != "https://app.example.com/main" {
		t.Errorf("deploy job's environment: %q %q", d.Environment, d.EnvironmentURL)
	}
	if s := r.Jobs[jobs["staging"]]; s.Conclusion != "failure" || !strings.Contains(s.Error, "may not use this environment") {
		t.Errorf("staging job: %s %q", s.Conclusion, s.Error)
	}
}
