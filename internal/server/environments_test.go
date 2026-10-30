package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dire-kiwi/kiwicd/internal/protocol"
	"github.com/dire-kiwi/kiwicd/internal/sealed"
)

type testAPI struct {
	t  *testing.T
	ts *httptest.Server
}

func (a testAPI) call(method, path, body string, out any) int {
	a.t.Helper()
	req, _ := http.NewRequest(method, a.ts.URL+path, strings.NewReader(body))
	req.SetBasicAuth("admin", "pw")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		a.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if out != nil {
		_ = json.Unmarshal(b, out)
	}
	return resp.StatusCode
}

func (a testAPI) must(method, path, body string, out any) {
	a.t.Helper()
	if code := a.call(method, path, body, out); code != 200 {
		a.t.Fatalf("%s %s: %d", method, path, code)
	}
}

func newEnvTestServer(t *testing.T, dir, publicKey string) (*Server, testAPI) {
	t.Helper()
	srv, err := New(Config{DataDir: dir, AdminPassword: "pw", Version: "test", SecretsPublicKey: publicKey})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(func() {
		ts.Close()
		srv.db.Close()
	})
	return srv, testAPI{t, ts}
}

func TestEnvironmentsAPI(t *testing.T) {
	_, api := newEnvTestServer(t, filepath.Join(t.TempDir(), "data"), "")
	api.must("POST", "/api/repos", `{"name":"t/r"}`, nil)
	var env Environment
	api.must("POST", "/api/repos/t/r/environments", `{"name":"Production","refs":[" main ","release/*","main"]}`, &env)
	if env.Name != "Production" || strings.Join(env.Refs, ",") != "main,release/*" {
		t.Errorf("created %+v", env)
	}
	for body, want := range map[string]int{
		`{"name":"production"}`:          409, // names ignore case
		`{"name":" "}`:                   400,
		`{"name":"a/b"}`:                 400,
		`{"name":"x","refs":["!main"]}`:  400,
		`{"name":"x","refs":["refs/x"]}`: 400,
	} {
		if code := api.call("POST", "/api/repos/t/r/environments", body, nil); code != want {
			t.Errorf("create %s: %d, want %d", body, code, want)
		}
	}
	api.must("PATCH", "/api/repos/t/r/environments/production", `{"refs":[]}`, &env)
	if len(env.Refs) != 0 {
		t.Errorf("refs after update: %v", env.Refs)
	}
	api.must("PUT", "/api/secrets?repo=t/r&environment=production", `{"name":"TOKEN","value":"x"}`, nil)
	api.must("PUT", "/api/variables?repo=t/r&environment=PRODUCTION", `{"name":"VAR","value":"v"}`, nil)
	if code := api.call("PUT", "/api/secrets?environment=production", `{"name":"TOKEN","value":"x"}`, nil); code != 400 {
		t.Errorf("environment without a repository: %d", code)
	}
	if code := api.call("GET", "/api/secrets?repo=t/r&environment=nope", "", nil); code != 404 {
		t.Errorf("unknown environment: %d", code)
	}
	var vars []struct{ Name, Value string }
	api.must("GET", "/api/variables?repo=t/r&environment=production", "", &vars)
	if len(vars) != 1 || vars[0].Value != "v" {
		t.Errorf("environment variables %+v", vars)
	}
	api.must("GET", "/api/variables?repo=t/r", "", &vars)
	if len(vars) != 0 {
		t.Errorf("the repository got the environment's variables: %+v", vars)
	}
	var envs []Environment
	api.must("GET", "/api/repos/t/r/environments", "", &envs)
	if len(envs) != 1 || envs[0].Secrets != 1 || envs[0].Variables != 1 {
		t.Errorf("environments %+v", envs)
	}
	api.must("DELETE", "/api/repos/t/r/environments/production", "", nil)
	api.must("POST", "/api/repos/t/r/environments", `{"name":"production"}`, nil)
	api.must("GET", "/api/variables?repo=t/r&environment=production", "", &vars)
	if len(vars) != 0 {
		t.Errorf("a deleted environment's variables came back: %+v", vars)
	}
}

// TestSealedSecrets checks sealing to the secrets service's public key:
// values sent sealed, resealing what the server could read, and what jobs
// and the secrets service get.
func TestSealedSecrets(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	srv, api := newEnvTestServer(t, dir, "")
	api.must("POST", "/api/repos", `{"name":"t/r"}`, nil)
	api.must("POST", "/api/repos/t/r/environments", `{"name":"prod","refs":["main"]}`, nil)
	api.must("PUT", "/api/secrets?repo=t/r", `{"name":"A","value":"repo-a"}`, nil)
	api.must("PUT", "/api/secrets?repo=t/r&environment=prod", `{"name":"B","value":"env-b"}`, nil)
	api.must("PUT", "/api/secrets", `{"name":"G","value":"global-g"}`, nil)
	if code := api.call("PUT", "/api/secrets?repo=t/r", `{"name":"C","encrypted_value":"pk1:x:y"}`, nil); code != 400 {
		t.Errorf("encrypted_value without a public key: %d", code)
	}
	if code := api.call("POST", "/api/secrets/reseal", "", nil); code != 400 {
		t.Errorf("reseal without a public key: %d", code)
	}
	ts := api.ts
	ts.Close()
	srv.db.Close()

	key, _ := sealed.GenerateKey()
	srv, api = newEnvTestServer(t, dir, key.Public().String())
	var info secretsKeyInfo
	api.must("GET", "/api/secrets/key", "", &info)
	if info.KeyID != key.Public().ID() || info.ServerSealed != 3 {
		t.Errorf("before resealing: %+v", info)
	}
	other, _ := sealed.GenerateKey()
	wrong, _ := other.Public().Seal(sealed.Scope{Repository: "t/r", Name: "C"}, "c")
	if code := api.call("PUT", "/api/secrets?repo=t/r", fmt.Sprintf(`{"name":"C","encrypted_value":%q}`, wrong), nil); code != 400 {
		t.Errorf("a value sealed to another key: %d", code)
	}
	var res map[string]int
	api.must("POST", "/api/secrets/reseal", "", &res)
	if res["resealed"] != 3 {
		t.Errorf("resealed %v", res)
	}
	api.must("GET", "/api/secrets/key", "", &info)
	if info.ServerSealed != 0 {
		t.Errorf("after resealing: %+v", info)
	}
	var list []struct {
		Name, Value string
		KeyID       string `json:"key_id"`
	}
	api.must("GET", "/api/secrets?repo=t/r", "", &list)
	if len(list) != 1 || list[0].KeyID != key.Public().ID() || list[0].Value != "" {
		t.Errorf("listed %+v", list)
	}

	// A run from main with a job in prod, and one from a branch.
	repo, _ := srv.getRepo("t/r")
	job := func(ref string) *Job {
		t.Helper()
		res, err := srv.db.Exec(`INSERT INTO runs (repo_id, number, workflow_file, workflow_name, workflow_yaml, title, event, event_json, ref, sha, actor, status, created_at)
			VALUES (?, 1, 'w.yml', 'w', 'jobs: {}', 't', 'push', '{}', ?, 'abc', 'a', ?, 0)`, repo.ID, ref, protocol.StatusInProgress)
		if err != nil {
			t.Fatal(err)
		}
		runID, _ := res.LastInsertId()
		res, err = srv.db.Exec(`INSERT INTO jobs (run_id, job_key, name, status, created_at, environment) VALUES (?, 'deploy', 'deploy', ?, 0, 'PROD')`,
			runID, protocol.StatusInProgress)
		if err != nil {
			t.Fatal(err)
		}
		id, _ := res.LastInsertId()
		j, _ := srv.getJob(id)
		return j
	}
	j := job("refs/heads/main")
	if _, err := srv.buildJobRequest(j, &Runner{Name: "r"}, ts.URL, false); err == nil || !strings.Contains(err.Error(), "A, B, G") {
		t.Errorf("an agent without a secrets service got sealed secrets: %v", err)
	}
	req, err := srv.buildJobRequest(j, &Runner{Name: "r"}, ts.URL, true)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(req.SealedSecrets, ",") != "A,B,G" || len(req.Secrets) != 1 || req.Secrets[jobTokenSecret] == "" {
		t.Errorf("job request: sealed %v, secrets %v", req.SealedSecrets, req.Secrets)
	}

	// The secrets service asks with the job's token.
	ask := func(token string, out any) int {
		r, _ := http.NewRequest("GET", api.ts.URL+"/api/secrets-service/job", nil)
		r.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		_ = json.NewDecoder(resp.Body).Decode(out)
		return resp.StatusCode
	}
	var sj protocol.SealedJob
	if code := ask(req.Secrets[jobTokenSecret], &sj); code != 200 || sj.Environment != "prod" || len(sj.Secrets) != 3 {
		t.Fatalf("secrets service: %d %+v", code, sj)
	}
	for _, s := range sj.Secrets {
		v, err := sealed.Open([]*sealed.PrivateKey{key}, sealed.Scope{Repository: s.Repository, Environment: s.Environment, Name: s.Name}, s.Value)
		want := map[string]string{"A": "repo-a", "B": "env-b", "G": "global-g"}[s.Name]
		if err != nil || v != want {
			t.Errorf("secret %+v opened to %q, %v", s, v, err)
		}
	}
	if code := ask("nope", &sj); code != 401 {
		t.Errorf("unknown token: %d", code)
	}

	// A branch may not use prod; the service hears the same.
	j = job("refs/heads/feature")
	if _, err := srv.buildJobRequest(j, &Runner{Name: "r"}, ts.URL, true); err == nil || !strings.Contains(err.Error(), "may not use this environment") {
		t.Errorf("a branch used prod: %v", err)
	}
	api.must("PATCH", "/api/repos/t/r", `{"protected_refs":["feature"]}`, nil)
	token, _ := srv.issueJobToken(j.ID)
	if code := ask(token, &sj); code != 403 {
		t.Errorf("secrets service for a branch job in prod: %d", code)
	}
}
