package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestAPITokens(t *testing.T) {
	data := filepath.Join(t.TempDir(), "data")
	srv, err := New(Config{DataDir: data, AdminPassword: "pw", Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	do := func(method, path, auth, body string) (int, []byte) {
		t.Helper()
		req, _ := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
		if auth != "" {
			req.Header.Set("Authorization", "Bearer "+auth)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, b
	}

	code, b := do("POST", "/api/tokens", "pw", `{"name":"cli"}`)
	if code != 200 {
		t.Fatalf("create: %d %s", code, b)
	}
	var created struct {
		Token string   `json:"token"`
		Info  APIToken `json:"info"`
	}
	_ = json.Unmarshal(b, &created)
	if !strings.HasPrefix(created.Token, apiTokenPrefix) || created.Info.ExpiresAt != 0 || created.Info.User != adminUser {
		t.Fatalf("created %+v", created)
	}
	if code, b := do("GET", "/api/me", created.Token, ""); code != 200 || !strings.Contains(string(b), adminUser) {
		t.Fatalf("me with token: %d %s", code, b)
	}
	if code, _ := do("GET", "/api/me", created.Token+"x", ""); code != 401 {
		t.Fatalf("wrong token: %d", code)
	}
	code, b = do("GET", "/api/tokens", created.Token, "")
	var list []APIToken
	_ = json.Unmarshal(b, &list)
	if code != 200 || len(list) != 1 || list[0].Name != "cli" || list[0].LastUsedAt == 0 {
		t.Fatalf("list: %d %s", code, b)
	}
	if code, _ := do("POST", "/api/tokens", "pw", `{"name":""}`); code != 400 {
		t.Fatalf("nameless token: %d", code)
	}

	// GitHub users' tokens expire within maxGitHubTokenDays.
	if err := srv.saveGrant(RoleGrant{Kind: "user", Name: "octocat", Role: "viewer"}); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.db.Exec(`INSERT INTO sessions (token_hash, created_at, expires_at, github_login) VALUES (?, 0, ?, 'octocat')`,
		hashToken("sess"), nowMS()+60_000); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest("POST", ts.URL+"/api/tokens", strings.NewReader(`{"name":"gh"}`))
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: "sess"})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var gh struct {
		Token string   `json:"token"`
		Info  APIToken `json:"info"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&gh)
	resp.Body.Close()
	if gh.Info.User != "octocat" || gh.Info.ExpiresAt == 0 || gh.Info.ExpiresAt > nowMS()+maxGitHubTokenDays*86_400_000+60_000 {
		t.Fatalf("GitHub user's token: %+v", gh.Info)
	}
	if code, b := do("GET", "/api/me", gh.Token, ""); code != 200 || !strings.Contains(string(b), "octocat") {
		t.Fatalf("me with GitHub user's token: %d %s", code, b)
	}
	srv.endGitHubSessions("test")
	if code, _ := do("GET", "/api/me", gh.Token, ""); code != 401 {
		t.Fatalf("GitHub user's token after sign-in access was removed: %d", code)
	}

	// Logging out revokes the token used.
	if code, b := do("DELETE", "/api/tokens/current", created.Token, ""); code != 200 {
		t.Fatalf("revoke current: %d %s", code, b)
	}
	if code, _ := do("GET", "/api/me", created.Token, ""); code != 401 {
		t.Fatalf("revoked token: %d", code)
	}
	if code, _ := do("DELETE", "/api/tokens/current", "pw", ""); code != 400 {
		t.Fatalf("revoke current with the password: %d", code)
	}

	// Changing the admin password revokes the admin's tokens.
	_, b = do("POST", "/api/tokens", "pw", `{"name":"other","expires_in_days":30}`)
	_ = json.Unmarshal(b, &created)
	if created.Info.ExpiresAt == 0 {
		t.Fatal("expires_in_days ignored")
	}
	ts.Close()
	srv.db.Close()
	srv, err = New(Config{DataDir: data, AdminPassword: "new", Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.db.Close()
	if p := srv.tokenPrincipal(created.Token); p != nil {
		t.Fatalf("token survived a password change: %+v", p)
	}
}
