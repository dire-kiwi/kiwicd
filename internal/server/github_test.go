package server

import (
	"crypto"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/cgi"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeGitHub is enough of GitHub's web, API and git hosting for the GitHub
// App: one app, installed on gh/app and the organization kiwi-org.
type fakeGitHub struct {
	t       *testing.T
	ts      *httptest.Server
	key     *rsa.PrivateKey
	pem     string
	origins string // bare repositories served over HTTP, as OWNER/NAME.git

	mu        sync.Mutex
	gitAuth   []string // Authorization headers of git requests
	tokenReqs []map[string]any
}

const (
	fakeAppID         = 42
	fakeClientID      = "Iv1.fake"
	fakeClientSecret  = "client-secret"
	fakeWebhookSecret = "hook-secret"
	fakeRepoToken     = "ghs_repo"
	fakeOrgToken      = "ghs_org"
)

func newFakeGitHub(t *testing.T) *fakeGitHub {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeGitHub{t: t, key: key, origins: t.TempDir(),
		pem: string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))}
	f.ts = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.ts.Close)
	return f
}

// checkJWT reports whether r is authenticated as the app.
func (f *fakeGitHub) checkJWT(r *http.Request) bool {
	jwt, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	parts := strings.Split(jwt, ".")
	if !ok || len(parts) != 3 {
		return false
	}
	sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if rsa.VerifyPKCS1v15(&f.key.PublicKey, crypto.SHA256, sum[:], sig) != nil {
		return false
	}
	var claims struct {
		Iss int64 `json:"iss"`
		Exp int64 `json:"exp"`
	}
	b, _ := base64.RawURLEncoding.DecodeString(parts[1])
	_ = json.Unmarshal(b, &claims)
	return claims.Iss == fakeAppID && claims.Exp > time.Now().Unix()
}

func (f *fakeGitHub) serve(w http.ResponseWriter, r *http.Request) {
	reply := func(code int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(v)
	}
	app := func() bool {
		if !f.checkJWT(r) {
			reply(401, map[string]string{"message": "bad JWT"})
			return false
		}
		return true
	}
	p := r.URL.Path
	switch {
	case strings.Contains(p, ".git/"):
		f.mu.Lock()
		f.gitAuth = append(f.gitAuth, r.Header.Get("Authorization"))
		f.mu.Unlock()
		want := "Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+fakeRepoToken))
		if r.Header.Get("Authorization") != want {
			w.Header().Set("WWW-Authenticate", `Basic realm="GitHub"`)
			http.Error(w, "auth", 401)
			return
		}
		gitPath, _ := exec.LookPath("git")
		(&cgi.Handler{Path: gitPath, Args: []string{"http-backend"},
			Env: []string{"GIT_PROJECT_ROOT=" + f.origins, "GIT_HTTP_EXPORT_ALL=1"}}).ServeHTTP(w, r)
	case r.Method == "POST" && strings.HasPrefix(p, "/app-manifests/"):
		if p != "/app-manifests/good-code/conversions" {
			reply(404, map[string]string{"message": "Not Found"})
			return
		}
		reply(201, map[string]any{"id": fakeAppID, "slug": "kiwicd-test", "name": "kiwicd test", "html_url": f.ts.URL + "/apps/kiwicd-test",
			"client_id": fakeClientID, "client_secret": fakeClientSecret, "pem": f.pem, "webhook_secret": fakeWebhookSecret,
			"owner": map[string]string{"login": "owner", "type": "User"}})
	case p == "/app":
		if app() {
			reply(200, map[string]any{"slug": "kiwicd-test", "name": "kiwicd test", "html_url": f.ts.URL + "/apps/kiwicd-test",
				"client_id": fakeClientID, "owner": map[string]string{"login": "owner"}})
		}
	case p == "/repos/gh/app/installation":
		if app() {
			reply(200, map[string]int{"id": 7})
		}
	case p == "/orgs/kiwi-org/installation":
		if app() {
			reply(200, map[string]int{"id": 8})
		}
	case strings.HasSuffix(p, "/installation"):
		if app() {
			reply(404, map[string]string{"message": "Not Found"})
		}
	case r.Method == "POST" && strings.HasPrefix(p, "/app/installations/"):
		if !app() {
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.tokenReqs = append(f.tokenReqs, body)
		f.mu.Unlock()
		token := fakeRepoToken
		if p == "/app/installations/8/access_tokens" {
			token = fakeOrgToken
		}
		reply(201, map[string]any{"token": token, "expires_at": time.Now().Add(time.Hour)})
	case p == "/app/installations":
		if app() {
			reply(200, []map[string]int{{"id": 7}})
		}
	case p == "/installation/repositories":
		reply(200, map[string]any{"repositories": []map[string]any{
			{"full_name": "gh/app", "clone_url": f.ts.URL + "/gh/app.git", "private": true, "default_branch": "main"}}})
	case strings.HasPrefix(p, "/orgs/kiwi-org/members/"):
		if r.Header.Get("Authorization") != "token "+fakeOrgToken {
			reply(401, map[string]string{"message": "bad token"})
		} else if strings.TrimPrefix(p, "/orgs/kiwi-org/members/") == "orgmember" {
			w.WriteHeader(204)
		} else {
			reply(404, map[string]string{"message": "Not Found"})
		}
	case p == "/login/oauth/access_token":
		_ = r.ParseForm()
		if r.Form.Get("client_id") != fakeClientID || r.Form.Get("client_secret") != fakeClientSecret {
			reply(200, map[string]string{"error": "incorrect_client_credentials"})
			return
		}
		// The code is the login, for the test.
		reply(200, map[string]string{"access_token": "gho_" + r.Form.Get("code"), "token_type": "bearer"})
	case p == "/user":
		login, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer gho_")
		if !ok {
			reply(401, map[string]string{"message": "Bad credentials"})
			return
		}
		reply(200, map[string]any{"login": login, "id": 1})
	default:
		reply(404, map[string]string{"message": "Not Found " + p})
	}
}

type githubTest struct {
	t   *testing.T
	srv *Server
	ts  *httptest.Server
	gh  *fakeGitHub
}

func newGitHubTest(t *testing.T) *githubTest {
	gh := newFakeGitHub(t)
	srv, err := New(Config{DataDir: filepath.Join(t.TempDir(), "data"), AdminPassword: "pw", Version: "test",
		GitHubURL: gh.ts.URL, GitHubAPIURL: gh.ts.URL})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	srv.cfg.PublicURL = ts.URL
	return &githubTest{t, srv, ts, gh}
}

// do sends a request as the admin unless client is given, and doesn't
// follow redirects.
func (g *githubTest) do(client *http.Client, method, path, body string, out any) *http.Response {
	g.t.Helper()
	req, _ := http.NewRequest(method, g.ts.URL+path, strings.NewReader(body))
	if client == nil {
		req.SetBasicAuth("admin", "pw")
		client = noRedirects(nil)
	}
	resp, err := client.Do(req)
	if err != nil {
		g.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if out != nil {
		if err := json.Unmarshal(b, out); err != nil {
			g.t.Fatalf("%s %s: %v: %s", method, path, err, b)
		}
	}
	return resp
}

func noRedirects(jar http.CookieJar) *http.Client {
	return &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// createApp sets up the app with the manifest flow.
func (g *githubTest) createApp() {
	g.t.Helper()
	var m struct{ Action, Manifest string }
	if resp := g.do(nil, "POST", "/api/github/app/manifest", `{"name":"kiwicd test"}`, &m); resp.StatusCode != 200 {
		g.t.Fatalf("manifest: %d", resp.StatusCode)
	}
	u, err := url.Parse(m.Action)
	if err != nil || u.Path != "/settings/apps/new" {
		g.t.Fatalf("action %q", m.Action)
	}
	var manifest struct {
		HookAttributes struct{ URL string } `json:"hook_attributes"`
		RedirectURL    string               `json:"redirect_url"`
		CallbackURLs   []string             `json:"callback_urls"`
		DefaultEvents  []string             `json:"default_events"`
	}
	_ = json.Unmarshal([]byte(m.Manifest), &manifest)
	if manifest.HookAttributes.URL != g.ts.URL+"/api/github/webhook" || manifest.RedirectURL != g.ts.URL+"/api/github/app/callback" ||
		len(manifest.CallbackURLs) != 1 || manifest.CallbackURLs[0] != g.ts.URL+"/api/auth/github/callback" {
		g.t.Fatalf("manifest %s", m.Manifest)
	}
	resp := g.do(nil, "GET", "/api/github/app/callback?code=good-code&state="+u.Query().Get("state"), "", nil)
	if resp.StatusCode != 303 || resp.Header.Get("Location") != "/settings?github=created" {
		g.t.Fatalf("callback: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
}

func TestGitHubAppManifest(t *testing.T) {
	g := newGitHubTest(t)
	// A callback this server didn't start is refused.
	resp := g.do(nil, "GET", "/api/github/app/callback?code=good-code&state=nope", "", nil)
	if loc := resp.Header.Get("Location"); resp.StatusCode != 303 || !strings.HasPrefix(loc, "/settings?github_error=") {
		t.Fatalf("callback with a bad state: %d %s", resp.StatusCode, loc)
	}
	// So is one without a login.
	if resp := g.do(http.DefaultClient, "GET", "/api/github/app/callback?code=good-code&state=x", "", nil); resp.StatusCode != 401 {
		t.Fatalf("anonymous callback: %d", resp.StatusCode)
	}
	var m struct{ Action string }
	g.do(nil, "POST", "/api/github/app/manifest", `{"org":"kiwi-org"}`, &m)
	if !strings.HasPrefix(m.Action, g.gh.ts.URL+"/organizations/kiwi-org/settings/apps/new?state=") {
		t.Errorf("organization action %q", m.Action)
	}

	g.createApp()
	var info struct {
		App *struct {
			AppID        int64    `json:"app_id"`
			Slug         string   `json:"slug"`
			ClientID     string   `json:"client_id"`
			AllowedUsers []string `json:"allowed_users"`
			InstallURL   string   `json:"install_url"`
			ClientSecret string   `json:"client_secret"`
		} `json:"app"`
		WebhookURL string `json:"webhook_url"`
	}
	g.do(nil, "GET", "/api/github/app", "", &info)
	if info.App == nil || info.App.AppID != fakeAppID || info.App.Slug != "kiwicd-test" || info.App.ClientID != fakeClientID ||
		info.App.InstallURL != g.gh.ts.URL+"/apps/kiwicd-test/installations/new" || info.WebhookURL != g.ts.URL+"/api/github/webhook" {
		t.Fatalf("app: %+v", info)
	}
	// The owner may sign in; secrets stay on the server.
	if len(info.App.AllowedUsers) != 1 || info.App.AllowedUsers[0] != "owner" || info.App.ClientSecret != "" {
		t.Errorf("app: %+v", info.App)
	}
	var stored string
	_ = g.srv.db.QueryRow(`SELECT private_key FROM github_app`).Scan(&stored)
	if !strings.HasPrefix(stored, sealedPrefix) {
		t.Errorf("private key stored as %.20q", stored)
	}
	// One app at a time.
	if resp := g.do(nil, "POST", "/api/github/app/manifest", `{}`, nil); resp.StatusCode != 409 {
		t.Errorf("second manifest: %d", resp.StatusCode)
	}
	if resp := g.do(nil, "DELETE", "/api/github/app", "", nil); resp.StatusCode != 200 {
		t.Fatalf("delete: %d", resp.StatusCode)
	}
	g.do(nil, "GET", "/api/github/app", "", &info)
	if info.App != nil {
		t.Errorf("app after delete: %+v", info.App)
	}

	// An existing app can be entered by hand; its key and client ID are checked.
	body := func(clientID string) string {
		b, _ := json.Marshal(map[string]any{"app_id": fakeAppID, "client_id": clientID, "client_secret": fakeClientSecret,
			"private_key": g.gh.pem, "webhook_secret": fakeWebhookSecret, "allowed_users": []string{"@Someone", "someone"}})
		return string(b)
	}
	if resp := g.do(nil, "PUT", "/api/github/app", body("Iv1.other"), nil); resp.StatusCode != 400 {
		t.Errorf("wrong client ID: %d", resp.StatusCode)
	}
	if resp := g.do(nil, "PUT", "/api/github/app", body(fakeClientID), &info); resp.StatusCode != 200 {
		t.Fatalf("put: %d", resp.StatusCode)
	}
	if info.App == nil || info.App.Slug != "kiwicd-test" || strings.Join(info.App.AllowedUsers, ",") != "Someone" {
		t.Errorf("app entered by hand: %+v", info.App)
	}
}

func TestGitHubLogin(t *testing.T) {
	g := newGitHubTest(t)
	var providers map[string]bool
	g.do(http.DefaultClient, "GET", "/api/auth/providers", "", &providers)
	if providers["github"] {
		t.Error("GitHub sign-in offered without an app")
	}
	g.createApp()
	g.do(nil, "PATCH", "/api/github/app", `{"allowed_orgs":["kiwi-org"]}`, nil)
	g.do(http.DefaultClient, "GET", "/api/auth/providers", "", &providers)
	if !providers["github"] {
		t.Error("GitHub sign-in not offered")
	}

	// signIn goes through GitHub's sign-in as login and returns the browser
	// and where kiwicd sent it.
	signIn := func(login string, tamper func(state string) string) (*http.Client, string) {
		t.Helper()
		jar, _ := cookiejar.New(nil)
		c := noRedirects(jar)
		resp := g.do(c, "GET", "/api/auth/github", "", nil)
		u, err := url.Parse(resp.Header.Get("Location"))
		if resp.StatusCode != 302 || err != nil || u.Path != "/login/oauth/authorize" {
			t.Fatalf("start: %d %s", resp.StatusCode, resp.Header.Get("Location"))
		}
		q := u.Query()
		if q.Get("client_id") != fakeClientID || q.Get("redirect_uri") != g.ts.URL+"/api/auth/github/callback" {
			t.Fatalf("authorize URL %s", u)
		}
		state := q.Get("state")
		if tamper != nil {
			state = tamper(state)
		}
		resp = g.do(c, "GET", "/api/auth/github/callback?"+url.Values{"code": {login}, "state": {state}}.Encode(), "", nil)
		if resp.StatusCode != 303 {
			t.Fatalf("callback: %d", resp.StatusCode)
		}
		return c, resp.Header.Get("Location")
	}
	me := func(c *http.Client) string {
		t.Helper()
		var m struct{ User string }
		if resp := g.do(c, "GET", "/api/me", "", &m); resp.StatusCode != 200 {
			return ""
		}
		return m.User
	}

	c, loc := signIn("owner", nil)
	if loc != "/" || me(c) != "owner" {
		t.Fatalf("owner: sent to %s, /api/me says %q", loc, me(c))
	}
	member, loc := signIn("orgmember", nil)
	if loc != "/" || me(member) != "orgmember" {
		t.Fatalf("organization member: sent to %s, /api/me says %q", loc, me(member))
	}
	stranger, loc := signIn("stranger", nil)
	if !strings.HasPrefix(loc, "/login?error=") || me(stranger) != "" {
		t.Errorf("stranger: sent to %s", loc)
	}
	// A callback with another browser's state is refused.
	forged, loc := signIn("owner", func(string) string { return "forged" })
	if !strings.HasPrefix(loc, "/login?error=") || me(forged) != "" {
		t.Errorf("forged state: sent to %s", loc)
	}

	if got := requestUser(httptest.NewRequest("GET", "/", nil)); got != adminUser {
		t.Errorf("requestUser without a user: %q", got)
	}

	// Adding someone keeps sessions; removing someone ends GitHub ones.
	g.do(nil, "PATCH", "/api/github/app", `{"allowed_users":["owner","another"]}`, nil)
	if me(c) != "owner" {
		t.Error("adding a user logged the owner out")
	}
	g.do(nil, "PATCH", "/api/github/app", `{"allowed_orgs":[]}`, nil)
	if me(c) != "" || me(member) != "" {
		t.Error("removing an organization kept GitHub sessions")
	}
	if me(nil) != adminUser {
		t.Error("the admin password stopped working")
	}
}

func TestGitHubWebhookAndFetch(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	defer func(d time.Duration) { pullRequestFetchRetry = d }(pullRequestFetchRetry)
	pullRequestFetchRetry = 10 * time.Millisecond
	g := newGitHubTest(t)
	g.createApp()

	// gh/app is a private repository the app is installed on.
	work := filepath.Join(t.TempDir(), "work")
	origin := filepath.Join(g.gh.origins, "gh", "app.git")
	run := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("", "init", "-q", "--bare", origin)
	run("", "init", "-q", "-b", "main", work)
	commit := func(msg string) string {
		run(work, "commit", "-q", "--allow-empty", "-m", msg)
		run(work, "push", "-q", origin, "HEAD:main")
		return run(work, "rev-parse", "HEAD")
	}
	first := commit("first")
	if resp := g.do(nil, "POST", "/api/repos", `{"name":"gh/app","url":"`+g.gh.ts.URL+`/gh/app.git"}`, nil); resp.StatusCode != 200 {
		t.Fatalf("create mirror: %d", resp.StatusCode)
	}
	var repo Repo
	g.do(nil, "GET", "/api/repos/gh/app", "", &repo)
	if repo.Refs["refs/heads/main"] != first {
		t.Fatalf("mirror refs %v, last error %q", repo.Refs, repo.LastSyncError)
	}
	g.gh.mu.Lock()
	req := g.gh.tokenReqs[0]
	g.gh.mu.Unlock()
	if fmt.Sprint(req["repositories"]) != "[app]" || fmt.Sprint(req["permissions"]) != "map[contents:read]" {
		t.Errorf("token request %v", req)
	}

	hook := func(event, body, secret string) (int, string) {
		t.Helper()
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write([]byte(body))
		req, _ := http.NewRequest("POST", g.ts.URL+"/api/github/webhook", strings.NewReader(body))
		req.Header.Set("X-GitHub-Event", event)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, strings.TrimSpace(string(b))
	}
	if code, _ := hook("push", `{"repository":{"full_name":"gh/app"}}`, "wrong"); code != 401 {
		t.Errorf("wrongly signed webhook: %d", code)
	}
	if code, body := hook("push", `{"repository":{"full_name":"gh/other"}}`, fakeWebhookSecret); code != 200 || body != `{"mirrors":[],"ok":true}` {
		t.Errorf("push to a repository without a mirror: %d %s", code, body)
	}
	if code, _ := hook("ping", `{"zen":"hi"}`, fakeWebhookSecret); code != 200 {
		t.Errorf("ping: %d", code)
	}

	// A push event syncs the mirror.
	second := commit("second")
	if code, body := hook("push", `{"ref":"refs/heads/main","repository":{"full_name":"GH/App"}}`, fakeWebhookSecret); code != 200 ||
		body != `{"mirrors":["gh/app"],"ok":true}` {
		t.Fatalf("push: %d %s", code, body)
	}
	waitFor(t, "the push to be fetched", func() bool {
		var r Repo
		g.do(nil, "GET", "/api/repos/gh/app", "", &r)
		return r.Refs["refs/heads/main"] == second
	})

	// A pull_request event fetches the pull request's head.
	run(work, "checkout", "-q", "-b", "feature")
	head := commit("feature")
	run(work, "push", "-q", origin, "HEAD:refs/pull/1/head")
	pr := fmt.Sprintf(`{"action":"opened","number":1,"pull_request":{"title":"T","head":{"ref":"feature","sha":%q,"repo":{"full_name":"gh/app"}},`+
		`"base":{"ref":"main","repo":{"full_name":"gh/app"}}},"sender":{"login":"someone"},"repository":{"full_name":"gh/app"}}`, head)
	if code, body := hook("pull_request", pr, fakeWebhookSecret); code != 200 || body != `{"mirrors":["gh/app"],"ok":true}` {
		t.Fatalf("pull_request: %d %s", code, body)
	}
	waitFor(t, "the pull request to be fetched", func() bool {
		sha, err := resolveRef(t.Context(), g.srv.repoPath("gh/app"), "refs/pull/1/head")
		return err == nil && sha == head
	})

	// A repository_dispatch event fetches the mirror and runs the default
	// branch's listeners at its new tip.
	_ = os.MkdirAll(filepath.Join(work, ".github", "workflows"), 0o755)
	_ = os.WriteFile(filepath.Join(work, ".github", "workflows", "d.yml"),
		[]byte("on:\n  repository_dispatch:\n    types: [deploy]\njobs:\n  a:\n    runs-on: x\n    steps: [{run: echo}]\n"), 0o644)
	run(work, "add", "-A")
	third := commit("dispatch listener")
	dispatch := `{"action":"deploy","branch":"main","client_payload":{"v":"2"},"repository":{"full_name":"gh/app"},"sender":{"login":"someone"}}`
	if code, body := hook("repository_dispatch", dispatch, fakeWebhookSecret); code != 200 || body != `{"mirrors":["gh/app"],"ok":true}` {
		t.Fatalf("repository_dispatch: %d %s", code, body)
	}
	var dispatched *Run
	waitFor(t, "the repository_dispatch run", func() bool {
		var runs []*Run
		g.do(nil, "GET", "/api/runs?repo=gh/app", "", &runs)
		for _, r := range runs {
			if r.Event == "repository_dispatch" {
				dispatched = r
			}
		}
		return dispatched != nil
	})
	if dispatched.Sha != third || dispatched.Actor != "someone" || dispatched.Title != "deploy" {
		t.Errorf("repository_dispatch run %+v", dispatched)
	}

	// Every fetch used the installation token.
	g.gh.mu.Lock()
	defer g.gh.mu.Unlock()
	for _, a := range g.gh.gitAuth {
		if a == "" {
			continue // git's first, unauthenticated try
		}
		if b, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(a, "Basic ")); string(b) != "x-access-token:"+fakeRepoToken {
			t.Errorf("git authorization %q", a)
		}
	}
}

func TestGitHubRepoName(t *testing.T) {
	s := &Server{cfg: Config{GitHubURL: "https://github.com"}}
	for _, c := range []struct {
		url, name string
		token     bool
	}{
		{"https://github.com/dire-kiwi/kiwicd.git", "dire-kiwi/kiwicd", true},
		{"https://GitHub.com/dire-kiwi/kiwicd", "dire-kiwi/kiwicd", true},
		{"https://github.com/dire-kiwi/kiwicd/", "dire-kiwi/kiwicd", true},
		{"https://x-access-token:t@github.com/dire-kiwi/kiwicd.git", "dire-kiwi/kiwicd", false},
		{"http://github.com/dire-kiwi/kiwicd.git", "dire-kiwi/kiwicd", false},
		{"ssh://git@github.com/dire-kiwi/kiwi.cd.git", "dire-kiwi/kiwi.cd", false},
		{"git@github.com:dire-kiwi/kiwicd.git", "dire-kiwi/kiwicd", false},
		{"github.com:dire-kiwi/kiwicd", "dire-kiwi/kiwicd", false},
		{"https://gitlab.com/dire-kiwi/kiwicd.git", "", false},
		{"https://github.com.evil.example/dire-kiwi/kiwicd.git", "", false},
		{"https://github.com/dire-kiwi/kiwicd/tree/main", "", false},
		{"/srv/git/kiwicd.git", "", false},
	} {
		name, token := s.githubRepoName(c.url)
		if name != c.name || token != c.token {
			t.Errorf("githubRepoName(%q) = %q, %v; want %q, %v", c.url, name, token, c.name, c.token)
		}
	}
}

func TestCleanAllowed(t *testing.T) {
	users, orgs, err := cleanAllowed([]string{" @b", "A", "a", ""}, []string{"kiwi-org"})
	if err != nil || strings.Join(users, ",") != "A,b" || strings.Join(orgs, ",") != "kiwi-org" {
		t.Errorf("cleanAllowed = %v, %v, %v", users, orgs, err)
	}
	if _, _, err := cleanAllowed([]string{"no/slashes"}, nil); err == nil {
		t.Error("accepted a login with a slash")
	}
}
