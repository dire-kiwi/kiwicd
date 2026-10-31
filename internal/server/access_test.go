package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// accessTest is a server with repositories t/a and t/b, a run with a job in
// each, and people with each role.
type accessTest struct {
	t   *testing.T
	srv *Server
	ts  *httptest.Server
	// runs and jobs by repository
	run, job map[string]int64
}

func newAccessTest(t *testing.T, handler func(*Server) http.Handler) *accessTest {
	t.Helper()
	srv, err := New(Config{DataDir: filepath.Join(t.TempDir(), "data"), AdminPassword: "pw", Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.db.Close() })
	a := &accessTest{t: t, srv: srv, run: map[string]int64{}, job: map[string]int64{}}
	for _, name := range []string{"t/a", "t/b"} {
		res, err := srv.db.Exec(`INSERT INTO repos (name, kind, webhook_secret, created_at) VALUES (?, 'hosted', 'hook', 0)`, name)
		if err != nil {
			t.Fatal(err)
		}
		repoID, _ := res.LastInsertId()
		res, err = srv.db.Exec(`INSERT INTO runs (repo_id, number, workflow_file, workflow_name, workflow_yaml, title, event, event_json, ref, sha, actor, status, created_at)
			VALUES (?, 1, 'ci.yml', 'CI', '', 'test', 'push', '{}', 'refs/heads/main', 'abc', 'admin', 'completed', 0)`, repoID)
		if err != nil {
			t.Fatal(err)
		}
		a.run[name], _ = res.LastInsertId()
		res, err = srv.db.Exec(`INSERT INTO jobs (run_id, job_key, name, status, created_at) VALUES (?, 'build', 'build', 'completed', 0)`, a.run[name])
		if err != nil {
			t.Fatal(err)
		}
		a.job[name], _ = res.LastInsertId()
	}
	for _, g := range []RoleGrant{
		{Kind: "user", Name: "boss", Role: "admin"},
		{Kind: "user", Name: "maint", Role: "maintainer", Repos: []string{"t/a"}},
		{Kind: "user", Name: "owner", Role: "maintainer", Repos: []string{"t/*"}},
		{Kind: "user", Name: "view", Role: "viewer"},
	} {
		if err := srv.saveGrant(g); err != nil {
			t.Fatal(err)
		}
	}
	for _, login := range []string{"boss", "maint", "owner", "view", "nobody"} {
		if _, err := srv.db.Exec(`INSERT INTO sessions (token_hash, created_at, expires_at, github_login) VALUES (?, 0, ?, ?)`,
			hashToken("session-"+login), nowMS()+3_600_000, login); err != nil {
			t.Fatal(err)
		}
	}
	h := srv.Handler()
	if handler != nil {
		h = handler(srv)
	}
	a.ts = httptest.NewServer(h)
	t.Cleanup(a.ts.Close)
	return a
}

// auth is how a request authenticates: "" for not at all, "pw" for the
// admin password, "session-LOGIN" for a GitHub session, or an API token.
func (a *accessTest) do(auth, method, path, body string, out any) (int, string) {
	a.t.Helper()
	req, _ := http.NewRequest(method, a.ts.URL+path, strings.NewReader(body))
	switch {
	case strings.HasPrefix(auth, "session-"):
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: auth})
	case auth != "":
		req.Header.Set("Authorization", "Bearer "+auth)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		a.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if out != nil && resp.StatusCode == 200 {
		if err := json.Unmarshal(b, out); err != nil {
			a.t.Fatalf("%s %s: %v: %s", method, path, err, b)
		}
	}
	return resp.StatusCode, string(b)
}

// token makes an API token as auth with the scope in body and returns it.
func (a *accessTest) token(auth, body string) string {
	a.t.Helper()
	var out struct{ Token string }
	if code, b := a.do(auth, "POST", "/api/tokens", body, &out); code != 200 {
		a.t.Fatalf("token %s as %s: %d %s", body, auth, code, b)
	}
	return out.Token
}

// who is someone the test makes requests as, and what they may do.
type who struct {
	name, auth string
	signedIn   bool
	admin      bool
	maintains  []string // repositories they maintain
	sees       []string // the only repositories they see; nil for all
}

func (w who) canView(repo string) bool { return w.sees == nil || slices.Contains(w.sees, repo) }

func (w who) canMaintain(repo string) bool { return w.admin || slices.Contains(w.maintains, repo) }

// need is what a request needs.
type need struct {
	admin    bool
	maintain string // maintainer of this repository
	view     string // able to see this repository
}

func (n need) allows(w who) bool {
	switch {
	case !w.signedIn:
		return false
	case n.admin:
		return w.admin && (n.view == "" || w.canView(n.view))
	case n.maintain != "":
		return w.canView(n.maintain) && w.canMaintain(n.maintain)
	case n.view != "":
		return w.canView(n.view)
	}
	return true
}

// TestRouteAccess checks every API route against everyone, with handlers
// that only say they were reached.
func TestRouteAccess(t *testing.T) {
	reached := "handler reached"
	a := newAccessTest(t, func(srv *Server) http.Handler {
		mux := http.NewServeMux()
		for _, rt := range srv.routes() {
			rt.handler = func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, reached) }
			mux.Handle(rt.pattern, srv.authorize(rt))
		}
		return mux
	})
	// Tokens are made by the real API.
	real := httptest.NewServer(a.srv.Handler())
	defer real.Close()
	tokenServer := &accessTest{t: t, srv: a.srv, ts: real}
	everyone := []who{
		{name: "anonymous"},
		{name: "GitHub user without a role", auth: "session-nobody"},
		{name: "admin password", auth: "pw", signedIn: true, admin: true, maintains: []string{"t/a", "t/b"}},
		{name: "GitHub admin", auth: "session-boss", signedIn: true, admin: true},
		{name: "maintainer of t/a", auth: "session-maint", signedIn: true, maintains: []string{"t/a"}},
		{name: "maintainer of t/*", auth: "session-owner", signedIn: true, maintains: []string{"t/a", "t/b"}},
		{name: "viewer", auth: "session-view", signedIn: true},
		{name: "admin's full token", auth: tokenServer.token("pw", `{"name":"full"}`), signedIn: true, admin: true},
		{name: "admin's maintainer token", auth: tokenServer.token("pw", `{"name":"m","role":"maintainer"}`), signedIn: true,
			maintains: []string{"t/a", "t/b"}},
		{name: "admin's read-only token", auth: tokenServer.token("pw", `{"name":"ro","role":"viewer"}`), signedIn: true},
		{name: "admin's token for t/a", auth: tokenServer.token("pw", `{"name":"a","repos":["t/a"]}`), signedIn: true,
			maintains: []string{"t/a"}, sees: []string{"t/a"}},
		{name: "admin's read-only token for t/b", auth: tokenServer.token("pw", `{"name":"b","role":"viewer","repos":["t/b"]}`),
			signedIn: true, sees: []string{"t/b"}},
		{name: "maintainer's full token", auth: tokenServer.token("session-maint", `{"name":"m"}`), signedIn: true,
			maintains: []string{"t/a"}},
		{name: "maintainer's token for t/b", auth: tokenServer.token("session-maint", `{"name":"mb","repos":["t/b"]}`),
			signedIn: true, sees: []string{"t/b"}},
		{name: "viewer's token", auth: tokenServer.token("session-view", `{"name":"v"}`), signedIn: true},
		{name: "wrong token", auth: apiTokenPrefix + "wrong"},
	}

	signedIn, admin := need{}, need{admin: true}
	view := func(repo string) need { return need{view: repo} }
	maintain := func(repo string) need { return need{maintain: repo} }
	id := itoa
	run, job := a.run["t/a"], a.job["t/a"]
	type request struct {
		method, path string
		need         need
	}
	cases := map[string][]request{
		"GET /api/me": {{"GET", "/api/me", signedIn}},

		"GET /api/github/app":           {{"GET", "/api/github/app", admin}},
		"PUT /api/github/app":           {{"PUT", "/api/github/app", admin}},
		"DELETE /api/github/app":        {{"DELETE", "/api/github/app", admin}},
		"POST /api/github/app/manifest": {{"POST", "/api/github/app/manifest", admin}},
		"GET /api/github/app/callback":  {{"GET", "/api/github/app/callback", admin}},
		"GET /api/github/repos":         {{"GET", "/api/github/repos", admin}},

		"GET /api/roles":    {{"GET", "/api/roles", admin}},
		"PUT /api/roles":    {{"PUT", "/api/roles", admin}},
		"DELETE /api/roles": {{"DELETE", "/api/roles?kind=user&name=view", admin}},
		"GET /api/users":    {{"GET", "/api/users", admin}},
		"GET /api/audit":    {{"GET", "/api/audit", admin}},

		"GET /api/repos":                      {{"GET", "/api/repos", signedIn}},
		"POST /api/repos":                     {{"POST", "/api/repos", admin}},
		"GET /api/repos/{owner}/{name}":       {{"GET", "/api/repos/t/a", view("t/a")}, {"GET", "/api/repos/t/b", view("t/b")}},
		"PATCH /api/repos/{owner}/{name}":     {{"PATCH", "/api/repos/t/a", maintain("t/a")}, {"PATCH", "/api/repos/t/b", maintain("t/b")}},
		"DELETE /api/repos/{owner}/{name}":    {{"DELETE", "/api/repos/t/a", need{admin: true, view: "t/a"}}},
		"POST /api/repos/{owner}/{name}/sync": {{"POST", "/api/repos/t/a/sync", maintain("t/a")}},
		"GET /api/repos/{owner}/{name}/workflows": {{"GET", "/api/repos/t/a/workflows", view("t/a")},
			{"GET", "/api/repos/t/b/workflows", view("t/b")}},
		"POST /api/repos/{owner}/{name}/dispatch": {{"POST", "/api/repos/t/a/dispatch", maintain("t/a")},
			{"POST", "/api/repos/t/b/dispatch", maintain("t/b")}},
		"POST /api/repos/{owner}/{name}/dispatches": {{"POST", "/api/repos/t/a/dispatches", maintain("t/a")},
			{"POST", "/api/repos/t/b/dispatches", maintain("t/b")}},
		"GET /api/repos/{owner}/{name}/uploaded-workflows/{file}":    {{"GET", "/api/repos/t/a/uploaded-workflows/x.yml", view("t/a")}},
		"PUT /api/repos/{owner}/{name}/uploaded-workflows/{file}":    {{"PUT", "/api/repos/t/a/uploaded-workflows/x.yml", maintain("t/a")}},
		"DELETE /api/repos/{owner}/{name}/uploaded-workflows/{file}": {{"DELETE", "/api/repos/t/b/uploaded-workflows/x.yml", maintain("t/b")}},

		"GET /api/runs":                        {{"GET", "/api/runs", signedIn}},
		"GET /api/runs/{id}":                   {{"GET", "/api/runs/" + id(run), view("t/a")}, {"GET", "/api/runs/" + id(a.run["t/b"]), view("t/b")}},
		"POST /api/runs/{id}/cancel":           {{"POST", "/api/runs/" + id(run) + "/cancel", maintain("t/a")}},
		"POST /api/runs/{id}/rerun":            {{"POST", "/api/runs/" + id(a.run["t/b"]) + "/rerun", maintain("t/b")}},
		"GET /api/jobs/{id}":                   {{"GET", "/api/jobs/" + id(job), view("t/a")}},
		"GET /api/jobs/{id}/logs":              {{"GET", "/api/jobs/" + id(a.job["t/b"]) + "/logs", view("t/b")}},
		"GET /api/jobs/{id}/events":            {{"GET", "/api/jobs/" + id(job) + "/events", view("t/a")}},
		"GET /api/events":                      {{"GET", "/api/events", signedIn}, {"GET", "/api/events?run=" + id(run), view("t/a")}},
		"GET /api/runners":                     {{"GET", "/api/runners", signedIn}},
		"DELETE /api/runners/{id}":             {{"DELETE", "/api/runners/1", admin}},
		"POST /api/runners/registration-token": {{"POST", "/api/runners/registration-token", admin}},

		"GET /api/tokens":         {{"GET", "/api/tokens", signedIn}},
		"POST /api/tokens":        {{"POST", "/api/tokens", signedIn}},
		"DELETE /api/tokens/{id}": {{"DELETE", "/api/tokens/current", signedIn}},

		"GET /api/secrets": {{"GET", "/api/secrets", admin}, {"GET", "/api/secrets?repo=t/a", maintain("t/a")},
			{"GET", "/api/secrets?repo=t/b", maintain("t/b")}},
		"PUT /api/secrets":      {{"PUT", "/api/secrets", admin}, {"PUT", "/api/secrets?repo=t/a", maintain("t/a")}},
		"DELETE /api/secrets":   {{"DELETE", "/api/secrets", admin}, {"DELETE", "/api/secrets?repo=t/b", maintain("t/b")}},
		"GET /api/variables":    {{"GET", "/api/variables", admin}, {"GET", "/api/variables?repo=t/a", maintain("t/a")}},
		"PUT /api/variables":    {{"PUT", "/api/variables", admin}, {"PUT", "/api/variables?repo=t/b", maintain("t/b")}},
		"DELETE /api/variables": {{"DELETE", "/api/variables", admin}, {"DELETE", "/api/variables?repo=t/a", maintain("t/a")}},
	}
	// Routes anyone may use check what they need themselves.
	public := []string{"GET /healthz", "POST /api/login", "POST /api/logout", "GET /api/auth/providers", "GET /api/auth/github",
		"GET /api/auth/github/callback", "POST /api/github/webhook", "POST /api/hooks/{owner}/{name}", "POST /api/runner/register",
		"POST /api/runner/poll", "POST /api/runner/jobs/{id}/update", "POST /api/runner/jobs/{id}/complete",
		"GET /api/runner/jobs/{id}/source", "/git/", "/"}

	for _, rt := range a.srv.routes() {
		if rt.need == roleNone {
			if !slices.Contains(public, rt.pattern) {
				t.Errorf("%s needs no sign-in", rt.pattern)
			}
			continue
		}
		reqs, ok := cases[rt.pattern]
		if !ok {
			t.Errorf("%s isn't tested", rt.pattern)
			continue
		}
		delete(cases, rt.pattern)
		for _, req := range reqs {
			for _, w := range everyone {
				code, body := a.do(w.auth, req.method, req.path, "", nil)
				allowed := code == 200 && body == reached
				if allowed != req.need.allows(w) {
					t.Errorf("%s %s as %s: %d %s", req.method, req.path, w.name, code, strings.TrimSpace(body))
					continue
				}
				if allowed {
					continue
				}
				// Not signed in: 401. Repositories one can't see don't exist.
				want := 403
				repo := req.need.view + req.need.maintain
				switch {
				case !w.signedIn:
					want = 401
				case repo != "" && !w.canView(repo):
					want = 404
				}
				if code != want {
					t.Errorf("%s %s as %s: %d, want %d: %s", req.method, req.path, w.name, code, want, strings.TrimSpace(body))
				}
			}
		}
	}
	for pattern := range cases {
		t.Errorf("tested %s isn't a route", pattern)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// TestAccessHandlers checks what handlers do with roles and scopes
// themselves: lists, tokens and hidden fields.
func TestAccessHandlers(t *testing.T) {
	a := newAccessTest(t, nil)

	// Lists show what one can see.
	tokenA := a.token("pw", `{"name":"a","repos":["t/a"]}`)
	var repos []Repo
	if a.do(tokenA, "GET", "/api/repos", "", &repos); len(repos) != 1 || repos[0].Name != "t/a" {
		t.Errorf("repositories with a token for t/a: %+v", repos)
	}
	var runs []Run
	if a.do(tokenA, "GET", "/api/runs", "", &runs); len(runs) != 1 || runs[0].Repo != "t/a" {
		t.Errorf("runs with a token for t/a: %+v", runs)
	}
	if a.do(tokenA, "GET", "/api/runs?repo=t/b", "", &runs); len(runs) != 0 {
		t.Errorf("t/b's runs with a token for t/a: %+v", runs)
	}
	if a.do("session-view", "GET", "/api/runs", "", &runs); len(runs) != 2 {
		t.Errorf("runs for a viewer: %+v", runs)
	}
	// Webhook secrets are for maintainers.
	var repo Repo
	if a.do("session-view", "GET", "/api/repos/t/a", "", &repo); repo.WebhookSecret != "" {
		t.Error("a viewer got the webhook secret")
	}
	if a.do("session-maint", "GET", "/api/repos/t/a", "", &repo); repo.WebhookSecret != "hook" {
		t.Error("a maintainer didn't get the webhook secret")
	}
	// A busy runner's job is hidden from those who can't see it.
	if _, err := a.srv.db.Exec(`INSERT INTO runners (id, name, labels, token_hash, created_at) VALUES (1, 'r', '[]', 'x', 0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := a.srv.db.Exec(`UPDATE jobs SET runner_id = 1, status = 'in_progress' WHERE id = ?`, a.job["t/b"]); err != nil {
		t.Fatal(err)
	}
	var runners []Runner
	if a.do(tokenA, "GET", "/api/runners", "", &runners); len(runners) != 1 || !runners[0].Busy || runners[0].JobName != "" {
		t.Errorf("runners with a token for t/a: %+v", runners)
	}
	if a.do("session-view", "GET", "/api/runners", "", &runners); len(runners) != 1 || runners[0].JobName != "build" {
		t.Errorf("runners for a viewer: %+v", runners)
	}

	// Only admins point a mirror elsewhere: it could fetch what a
	// maintainer can't see. Its other settings are the maintainers'.
	if _, err := a.srv.db.Exec(`INSERT INTO repos (name, kind, url, webhook_secret, created_at)
		VALUES ('t/m', 'mirror', 'https://u:p@git.example/m.git', 'hook', 0)`); err != nil {
		t.Fatal(err)
	}
	tokenM := a.token("pw", `{"name":"m","repos":["t/m"]}`)
	for _, c := range []struct {
		auth, body string
		want       int
	}{
		{"session-owner", `{"url":"/srv/kiwicd/repos/t/b.git"}`, 403},
		{tokenM, `{"url":"https://github.com/private/repo.git"}`, 403},
		{"session-owner", `{"url":"https://***@git.example/m.git","default_branch":"dev"}`, 200},
		{"session-owner", `{"url":" https://u:p@git.example/m.git "}`, 200},
		{"session-owner", `{"fork_pull_requests":true}`, 403},
		{"session-owner", `{"report_checks":true}`, 200},
		{"pw", `{"url":"https://git.example/other.git"}`, 200},
		{"pw", `{"fork_pull_requests":true}`, 200},
		{"session-owner", `{"fork_pull_requests":false}`, 200},
	} {
		if code, b := a.do(c.auth, "PATCH", "/api/repos/t/m", c.body, nil); code != c.want {
			t.Errorf("PATCH t/m %s as %.20s: %d, want %d: %s", c.body, c.auth, code, c.want, b)
		}
		if c.want == 200 && c.auth != "pw" && !strings.Contains(c.body, "url") {
			continue
		}
		if c.want == 200 && c.auth != "pw" {
			if m, _ := a.srv.getRepo("t/m"); m.URL != "https://u:p@git.example/m.git" {
				t.Errorf("t/m's URL after %s: %s", c.body, m.URL)
			}
		}
	}

	// /api/me says what one may do.
	var me struct {
		User   string
		Access access
	}
	if a.do(tokenA, "GET", "/api/me", "", &me); me.User != adminUser || me.Access.Role != "maintainer" || me.Access.Admin ||
		!slices.Equal(me.Access.Repos, []string{"t/a"}) || me.Access.Token == nil || me.Access.Token.Name != "a" {
		t.Errorf("me with a token for t/a: %+v", me)
	}
	if a.do("session-owner", "GET", "/api/me", "", &me); me.Access.Role != "maintainer" || me.Access.MaintainAll ||
		!slices.Equal(me.Access.Maintain, []string{"t/*"}) || me.Access.Repos != nil {
		t.Errorf("me for a maintainer of t/*: %+v", me)
	}

	// Tokens: no more than one's role, at most maintainer for some
	// repositories, which must exist.
	for _, c := range []struct{ auth, body string }{
		{"session-maint", `{"name":"x","role":"admin"}`},
		{"session-view", `{"name":"x","role":"maintainer"}`},
		{"pw", `{"name":"x","role":"admin","repos":["t/a"]}`},
		{"pw", `{"name":"x","repos":["t/none"]}`},
		{"pw", `{"name":"x","role":"owner"}`},
	} {
		if code, b := a.do(c.auth, "POST", "/api/tokens", c.body, nil); code != 400 {
			t.Errorf("token %s as %s: %d %s", c.body, c.auth, code, b)
		}
	}
	// Tokens with a scope can't make or revoke others.
	readOnly := a.token("session-maint", `{"name":"ro","role":"viewer"}`)
	if code, _ := a.do(readOnly, "POST", "/api/tokens", `{"name":"y"}`, nil); code != 403 {
		t.Errorf("a read-only token made a token: %d", code)
	}
	full := a.token("session-maint", `{"name":"full"}`)
	if code, _ := a.do(full, "POST", "/api/tokens", `{"name":"y","role":"viewer"}`, nil); code != 200 {
		t.Errorf("a full token couldn't make a token: %d", code)
	}
	// One's own tokens, or everyone's for an admin.
	var list []APIToken
	a.do("session-maint", "GET", "/api/tokens", "", &list)
	for _, tok := range list {
		if tok.User != "maint" {
			t.Errorf("a maintainer sees %+v", tok)
		}
	}
	if len(list) != 3 || list[0].Role != "viewer" || list[1].Role != "" {
		t.Errorf("a maintainer's tokens: %+v", list)
	}
	var all []APIToken
	if a.do("session-boss", "GET", "/api/tokens", "", &all); len(all) != 5 {
		t.Errorf("an admin sees %d tokens", len(all))
	}
	if code, _ := a.do("session-maint", "DELETE", "/api/tokens/"+itoa(all[0].ID), "", nil); code != 404 {
		t.Errorf("a maintainer revoked the admin's token: %d", code)
	}
	if code, _ := a.do(readOnly, "DELETE", "/api/tokens/"+itoa(list[1].ID), "", nil); code != 403 {
		t.Errorf("a read-only token revoked another: %d", code)
	}
	if code, _ := a.do(readOnly, "DELETE", "/api/tokens/current", "", nil); code != 200 {
		t.Errorf("a read-only token couldn't revoke itself: %d", code)
	}
	if code, _ := a.do("session-boss", "DELETE", "/api/tokens/"+itoa(list[1].ID), "", nil); code != 200 {
		t.Errorf("an admin couldn't revoke a maintainer's token: %d", code)
	}

	// A token never has more than its maker has now.
	maintToken := a.token("session-maint", `{"name":"m","role":"maintainer"}`)
	if code, _ := a.do(maintToken, "PUT", "/api/variables?repo=t/a", `{"name":"V","value":"1"}`, nil); code != 200 {
		t.Errorf("maintainer's token: %d", code)
	}
	if err := a.srv.saveGrant(RoleGrant{Kind: "user", Name: "maint", Role: "viewer", Repos: []string{}}); err != nil {
		t.Fatal(err)
	}
	if code, _ := a.do(maintToken, "PUT", "/api/variables?repo=t/a", `{"name":"V","value":"2"}`, nil); code != 403 {
		t.Errorf("token of a maintainer made viewer: %d", code)
	}

	// Changes are in the audit log, with who made them and how.
	type auditPage struct {
		Entries []auditEntry
		More    bool
	}
	var log auditPage
	a.do("pw", "GET", "/api/audit?repo=t/a", "", &log)
	if len(log.Entries) != 1 || log.Entries[0].Action != "variable.set" || log.Entries[0].Actor != "maint" ||
		!strings.HasPrefix(log.Entries[0].Token, "m (") || log.Entries[0].Target != "V" {
		t.Errorf("t/a's audit log: %+v", log.Entries)
	}
	log = auditPage{}
	a.do("pw", "GET", "/api/audit?limit=2", "", &log)
	if len(log.Entries) != 2 || !log.More || log.Entries[1].Action != "token.create" || log.Entries[1].Detail != "scope: maintainer" {
		t.Fatalf("a page of the audit log: %+v", log)
	}
	before := log.Entries[1].ID
	log = auditPage{}
	a.do("pw", "GET", "/api/audit?before="+itoa(before), "", &log)
	if len(log.Entries) == 0 || log.Entries[0].ID != before-1 || log.Entries[0].Action != "token.revoke" || log.Entries[0].Actor != "boss" {
		t.Errorf("an older page: %+v", log.Entries)
	}
}

// TestGitAccess checks fetching and pushing over /git/.
func TestGitAccess(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	a := newAccessTest(t, nil)
	if _, err := a.srv.db.Exec(`DELETE FROM repos`); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"t/a", "t/b"} {
		if code, b := a.do("pw", "POST", "/api/repos", `{"name":"`+name+`"}`, nil); code != 200 {
			t.Fatalf("create %s: %d %s", name, code, b)
		}
	}
	tokenB := a.token("pw", `{"name":"b","repos":["t/b"]}`)
	readOnly := a.token("pw", `{"name":"ro","role":"viewer"}`)
	for _, c := range []struct {
		auth, repo string
		push       bool
		want       int
	}{
		{"", "t/a", false, 401},
		{"session-view", "t/a", false, 200},
		{"session-view", "t/a", true, 403},
		{"session-maint", "t/a", true, 200},
		{"session-maint", "t/b", true, 403},
		{"session-owner", "t/b", true, 200},
		{tokenB, "t/b", true, 200},
		{tokenB, "t/a", false, 404},
		{readOnly, "t/a", false, 200},
		{readOnly, "t/a", true, 403},
		{"pw", "t/a", true, 200},
	} {
		service := "git-upload-pack"
		if c.push {
			service = "git-receive-pack"
		}
		auth := c.auth
		req, _ := http.NewRequest("GET", a.ts.URL+"/git/"+c.repo+".git/info/refs?service="+service, nil)
		switch {
		case strings.HasPrefix(auth, "session-"):
			// git sends no cookies; sessions stand in for GitHub users here.
			req.AddCookie(&http.Cookie{Name: sessionCookie, Value: auth})
		case auth != "":
			req.SetBasicAuth("x", auth)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != c.want {
			t.Errorf("%s %s as %.20s: %d, want %d", service, c.repo, c.auth, resp.StatusCode, c.want)
		}
	}
}

// TestAllowListMigration checks that the GitHub App's allow list from before
// roles becomes admin roles.
func TestAllowListMigration(t *testing.T) {
	data := filepath.Join(t.TempDir(), "data")
	srv, err := New(Config{DataDir: data, AdminPassword: "pw", Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srv.db.Exec(`INSERT INTO github_app (id, app_id, client_id, client_secret, private_key, webhook_secret, allowed_users, allowed_orgs, created_at)
		VALUES (1, 1, 'c', '', '', '', '["octocat"]', '["kiwi-org"]', 0)`); err != nil {
		t.Fatal(err)
	}
	for _, login := range []string{"octocat", "member"} {
		if _, err := srv.db.Exec(`INSERT INTO sessions (token_hash, created_at, expires_at, github_login) VALUES (?, 0, ?, ?)`,
			hashToken(login), nowMS()+60_000, login); err != nil {
			t.Fatal(err)
		}
	}
	srv.db.Close()
	if srv, err = New(Config{DataDir: data, AdminPassword: "pw", Version: "test"}); err != nil {
		t.Fatal(err)
	}
	defer srv.db.Close()
	grants, _ := srv.roleGrants()
	if len(grants) != 2 || grants[0].String() != "user octocat: admin" || grants[1].String() != "org kiwi-org: admin" {
		t.Errorf("grants: %+v", grants)
	}
	// The member came in through the only organization, and still has
	// access; octocat by login, not membership.
	for login, wantMember := range map[string]bool{"member": true, "octocat": false} {
		p, err := srv.githubPrincipal(login)
		if err != nil || !p.admin() {
			t.Errorf("%s after the migration: %+v, %v", login, p, err)
		}
		var ms string
		_ = srv.db.QueryRow(`SELECT memberships FROM github_users WHERE login = ?`, login).Scan(&ms)
		if (ms == `["org:kiwi-org"]`) != wantMember {
			t.Errorf("%s's memberships: %q", login, ms)
		}
	}
	var users string
	_ = srv.db.QueryRow(`SELECT allowed_users FROM github_app`).Scan(&users)
	if users != "[]" {
		t.Errorf("allow list kept: %s", users)
	}
}

func TestRepoPatterns(t *testing.T) {
	for _, c := range []struct {
		patterns []string
		repo     string
		want     bool
	}{
		{[]string{"t/a"}, "T/A", true},
		{[]string{"t/a"}, "t/ab", false},
		{[]string{"t/*"}, "t/anything", true},
		{[]string{"t/*"}, "tt/a", false},
		{nil, "t/a", false},
	} {
		if got := matchRepoPatterns(c.patterns, c.repo); got != c.want {
			t.Errorf("matchRepoPatterns(%v, %s) = %v", c.patterns, c.repo, got)
		}
	}
}
