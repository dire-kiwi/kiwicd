package secretsservice

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dire-kiwi/kiwicd/internal/protocol"
	"github.com/dire-kiwi/kiwicd/internal/sealed"
)

func TestAllows(t *testing.T) {
	c := Client{Allow: []string{"o/*", "o/app@prod*", "x/y@*"}}
	for target, want := range map[[2]string]bool{
		{"o/app", ""}:           true,
		{"O/App", ""}:           true,
		{"o/app", "prod"}:       true,
		{"o/app", "production"}: true,
		{"o/app", "staging"}:    false,
		{"o/other", "prod"}:     false,
		{"x/y", ""}:             false, // only its environments
		{"x/y", "any"}:          true,
		{"o/a/b", ""}:           false,
	} {
		if got := c.allows(target[0], target[1]); got != want {
			t.Errorf("allows(%q, %q) = %v", target[0], target[1], got)
		}
	}
	for _, bad := range []Client{
		{Name: "a", TokenSHA256: "nothex", Allow: []string{"o/*"}},
		{Name: "a", TokenSHA256: HashToken("t")},
		{Name: "a", TokenSHA256: HashToken("t"), Allow: []string{"app"}},
		{Name: "a", TokenSHA256: HashToken("t"), Allow: []string{"o/app@"}},
		{TokenSHA256: HashToken("t"), Allow: []string{"o/*"}},
	} {
		if CheckClients([]Client{bad}) == nil {
			t.Errorf("CheckClients accepted %+v", bad)
		}
	}
}

func TestUnseal(t *testing.T) {
	key, _ := sealed.GenerateKey()
	seal := func(repo, env, name, v string) protocol.SealedSecret {
		s, err := key.Public().Seal(sealed.Scope{Repository: repo, Environment: env, Name: name}, v)
		if err != nil {
			t.Fatal(err)
		}
		return protocol.SealedSecret{Name: name, Repository: repo, Environment: env, Value: s}
	}
	// A fake kiwicd server: each job token is a job.
	jobs := map[string]protocol.SealedJob{
		"ok": {Repository: "o/app", Environment: "prod", Secrets: []protocol.SealedSecret{
			seal("", "", "G", "g"), seal("o/app", "", "A", "a"), seal("o/app", "prod", "P", "p")}},
		"staging": {Repository: "o/app", Environment: "staging", Secrets: []protocol.SealedSecret{seal("o/app", "staging", "S", "s")}},
		// The server claims another repository's secret is this job's.
		"moved": {Repository: "o/app", Secrets: []protocol.SealedSecret{{Name: "A", Repository: "o/app", Value: seal("o/other", "", "A", "a").Value}}},
		"other": {Repository: "o/app", Secrets: []protocol.SealedSecret{seal("o/other", "", "A", "a")}},
		"none":  {Repository: "z/z", Secrets: []protocol.SealedSecret{}},
	}
	kiwicd := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		job, ok := jobs[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]
		if !ok || r.URL.Path != "/api/secrets-service/job" {
			http.Error(w, `{"error":"no"}`, 401)
			return
		}
		_ = json.NewEncoder(w).Encode(job)
	}))
	defer kiwicd.Close()
	svc, err := New(Config{Server: kiwicd.URL, Keys: []*sealed.PrivateKey{key}, Clients: []Client{
		{Name: "c", TokenSHA256: HashToken("client"), Allow: []string{"o/*", "o/app@prod"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(svc.Handler())
	defer ts.Close()
	unseal := func(client, job string) (int, map[string]string) {
		req, _ := http.NewRequest("POST", ts.URL+"/v1/unseal", strings.NewReader(`{"job_token":"`+job+`"}`))
		req.Header.Set("Authorization", "Bearer "+client)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out protocol.UnsealResponse
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out.Secrets
	}
	if code, got := unseal("client", "ok"); code != 200 || got["G"] != "g" || got["A"] != "a" || got["P"] != "p" {
		t.Errorf("ok: %d %v", code, got)
	}
	for _, c := range []struct {
		client, job string
		code        int
	}{
		{"wrong", "ok", 401},
		{"client", "unknown", 403},
		{"client", "staging", 403},
		{"client", "moved", 422},
		{"client", "other", 502},
		{"client", "none", 200}, // nothing to open, so nothing to allow
	} {
		if code, got := unseal(c.client, c.job); code != c.code || (code != 200 && len(got) > 0) {
			t.Errorf("%s/%s: %d %v, want %d", c.client, c.job, code, got, c.code)
		}
	}
}
