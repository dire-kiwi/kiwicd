package server

import (
	"bytes"
	"context"
	"crypto"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// kiwicd can have one GitHub App. It does three things:
//
//   - Webhooks: GitHub sends the push and pull_request events of every
//     repository the app is installed on to /api/github/webhook, signed with
//     the app's webhook secret. Each goes to the mirrors of that repository,
//     as if it had come to a mirror's own webhook URL, so repositories need
//     no webhook of their own.
//   - Fetching: mirrors of repositories the app is installed on fetch over
//     HTTPS with an installation token, so private ones need no deploy key.
//   - Logins: people sign in with GitHub through the app's OAuth client
//     (see githublogin.go), if their login or an organization they're a
//     member of is allowed.
//
// The app is made with GitHub's manifest flow: the Settings page posts a
// manifest to GitHub, GitHub creates the app and sends the browser back to
// /api/github/app/callback with a code, which kiwicd exchanges for the app's
// credentials. An existing app can be entered by hand instead.

type githubApp struct {
	AppID         int64    `json:"app_id"`
	Slug          string   `json:"slug"`
	Name          string   `json:"name"`
	HTMLURL       string   `json:"html_url"`
	Owner         string   `json:"owner"`
	ClientID      string   `json:"client_id"`
	ClientSecret  string   `json:"-"`
	PrivateKey    string   `json:"-"`
	WebhookSecret string   `json:"-"`
	AllowedUsers  []string `json:"allowed_users"`
	AllowedOrgs   []string `json:"allowed_orgs"`
	CreatedAt     int64    `json:"created_at"`
}

var errNoGitHubApp = errors.New("no GitHub App is set up")

// githubApp returns the app with its secrets decrypted, or errNoGitHubApp.
func (s *Server) githubApp() (*githubApp, error) {
	var a githubApp
	var clientSecret, key, webhookSecret, users, orgs string
	err := s.db.QueryRow(`SELECT app_id, slug, name, html_url, owner, client_id, client_secret, private_key, webhook_secret,
		allowed_users, allowed_orgs, created_at FROM github_app WHERE id = 1`).Scan(&a.AppID, &a.Slug, &a.Name, &a.HTMLURL, &a.Owner,
		&a.ClientID, &clientSecret, &key, &webhookSecret, &users, &orgs, &a.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errNoGitHubApp
	}
	if err != nil {
		return nil, err
	}
	for _, f := range []struct {
		purpose, sealed string
		out             *string
	}{
		{purposeGitHubClientSecret, clientSecret, &a.ClientSecret},
		{purposeGitHubPrivateKey, key, &a.PrivateKey},
		{purposeGitHubWebhookSecret, webhookSecret, &a.WebhookSecret},
	} {
		if *f.out, err = s.box.open(f.purpose, f.sealed); err != nil {
			return nil, fmt.Errorf("GitHub App: %w", err)
		}
	}
	_ = json.Unmarshal([]byte(users), &a.AllowedUsers)
	_ = json.Unmarshal([]byte(orgs), &a.AllowedOrgs)
	if a.AllowedUsers == nil {
		a.AllowedUsers = []string{}
	}
	if a.AllowedOrgs == nil {
		a.AllowedOrgs = []string{}
	}
	return &a, nil
}

func (s *Server) saveGitHubApp(a *githubApp) error {
	_, err := s.db.Exec(`INSERT INTO github_app (id, app_id, slug, name, html_url, owner, client_id, client_secret, private_key,
		webhook_secret, allowed_users, allowed_orgs, created_at) VALUES (1, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.AppID, a.Slug, a.Name, a.HTMLURL, a.Owner, a.ClientID, s.box.seal(purposeGitHubClientSecret, a.ClientSecret),
		s.box.seal(purposeGitHubPrivateKey, a.PrivateKey), s.box.seal(purposeGitHubWebhookSecret, a.WebhookSecret),
		mustJSON(a.AllowedUsers), mustJSON(a.AllowedOrgs), nowMS())
	if err != nil && strings.Contains(err.Error(), "UNIQUE") {
		return errors.New("a GitHub App is already set up; remove it first")
	}
	s.gh.forget()
	return err
}

// ---------------------------------------------------------------- API client

// githubState holds what the GitHub App code keeps between requests.
type githubState struct {
	client *http.Client
	now    func() time.Time

	mu sync.Mutex
	// Installation IDs by "owner/name" or "org:name" (lower case); 0 when
	// the app isn't installed there.
	installs map[string]cachedInstall
	// Installation tokens by the same keys.
	tokens map[string]cachedToken
	// Manifest flows started in the UI, by state, until they expire.
	setups map[string]time.Time
}

type cachedInstall struct {
	id int64
	at time.Time
}

type cachedToken struct {
	token   string
	expires time.Time
}

// installCacheTime is how long kiwicd remembers where the app is installed;
// installation events forget it sooner.
const installCacheTime = 10 * time.Minute

func newGitHubState() githubState {
	return githubState{
		client:   &http.Client{Timeout: 30 * time.Second},
		now:      time.Now,
		installs: map[string]cachedInstall{},
		tokens:   map[string]cachedToken{},
		setups:   map[string]time.Time{},
	}
}

// forget drops cached installations and tokens, after the app or its
// installations change.
func (g *githubState) forget() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.installs = map[string]cachedInstall{}
	g.tokens = map[string]cachedToken{}
}

// githubError is an unexpected response from GitHub.
type githubError struct {
	Status  int
	Message string
}

func (e *githubError) Error() string {
	return fmt.Sprintf("GitHub: %d %s", e.Status, e.Message)
}

func isGitHubStatus(err error, status int) bool {
	var ge *githubError
	return errors.As(err, &ge) && ge.Status == status
}

// githubAPI calls GitHub's REST API at path (relative to GitHubAPIURL) with
// auth as the Authorization header, sending body and decoding the response
// into out, either of which may be nil.
func (s *Server) githubAPI(ctx context.Context, method, path, auth string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, s.cfg.GitHubAPIURL+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "kiwicd")
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return s.githubDo(req, out)
}

func (s *Server) githubDo(req *http.Request, out any) error {
	resp, err := s.gh.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		var e struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(b, &e)
		if e.Message == "" {
			e.Message = http.StatusText(resp.StatusCode)
		}
		return &githubError{resp.StatusCode, e.Message}
	}
	if out == nil || len(b) == 0 {
		return nil
	}
	return json.Unmarshal(b, out)
}

func parseRSAKey(pemKey string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(pemKey))
	if block == nil {
		return nil, errors.New("the private key isn't PEM")
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("private key: %w", err)
	}
	rk, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("the private key isn't an RSA key")
	}
	return rk, nil
}

// appJWT is the token the app authenticates to GitHub as itself with.
func appJWT(appID int64, pemKey string, now time.Time) (string, error) {
	key, err := parseRSAKey(pemKey)
	if err != nil {
		return "", err
	}
	enc := base64.RawURLEncoding
	claims, _ := json.Marshal(map[string]any{
		// A minute in the past, for clocks that differ a little.
		"iat": now.Add(-time.Minute).Unix(),
		"exp": now.Add(9 * time.Minute).Unix(),
		"iss": appID,
	})
	signed := enc.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`)) + "." + enc.EncodeToString(claims)
	sum := sha256.Sum256([]byte(signed))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return signed + "." + enc.EncodeToString(sig), nil
}

func (s *Server) appAuth(a *githubApp) (string, error) {
	jwt, err := appJWT(a.AppID, a.PrivateKey, s.gh.now())
	if err != nil {
		return "", err
	}
	return "Bearer " + jwt, nil
}

// installation returns the ID of the app's installation on a repository
// ("owner/name") or an organization ("org:name"), or 0 if it has none.
func (s *Server) installation(ctx context.Context, a *githubApp, key string) (int64, error) {
	key = strings.ToLower(key)
	s.gh.mu.Lock()
	c, ok := s.gh.installs[key]
	s.gh.mu.Unlock()
	if ok && s.gh.now().Sub(c.at) < installCacheTime {
		return c.id, nil
	}
	auth, err := s.appAuth(a)
	if err != nil {
		return 0, err
	}
	path := "/repos/" + key + "/installation"
	if org, ok := strings.CutPrefix(key, "org:"); ok {
		path = "/orgs/" + url.PathEscape(org) + "/installation"
	}
	var inst struct {
		ID int64 `json:"id"`
	}
	if err := s.githubAPI(ctx, "GET", path, auth, nil, &inst); err != nil && !isGitHubStatus(err, 404) {
		return 0, err
	}
	s.gh.mu.Lock()
	s.gh.installs[key] = cachedInstall{inst.ID, s.gh.now()}
	s.gh.mu.Unlock()
	return inst.ID, nil
}

// installationToken returns a token for the app's installation on key (see
// installation) with permissions, or "" if the app isn't installed there.
// Tokens are kept until shortly before they expire.
func (s *Server) installationToken(ctx context.Context, a *githubApp, key string, permissions map[string]string) (string, error) {
	key = strings.ToLower(key)
	s.gh.mu.Lock()
	t, ok := s.gh.tokens[key]
	s.gh.mu.Unlock()
	if ok && s.gh.now().Before(t.expires.Add(-5*time.Minute)) {
		return t.token, nil
	}
	id, err := s.installation(ctx, a, key)
	if err != nil || id == 0 {
		return "", err
	}
	auth, err := s.appAuth(a)
	if err != nil {
		return "", err
	}
	body := map[string]any{"permissions": permissions}
	if _, name, ok := strings.Cut(key, "/"); ok {
		body["repositories"] = []string{name} // only this repository
	}
	var tok struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := s.githubAPI(ctx, "POST", fmt.Sprintf("/app/installations/%d/access_tokens", id), auth, body, &tok); err != nil {
		if isGitHubStatus(err, 404) || isGitHubStatus(err, 422) {
			// Uninstalled, or no longer given this repository.
			s.gh.forget()
		}
		return "", err
	}
	s.gh.mu.Lock()
	s.gh.tokens[key] = cachedToken{tok.Token, tok.ExpiresAt}
	s.gh.mu.Unlock()
	return tok.Token, nil
}

// ---------------------------------------------------------------- repositories

var githubPathRE = regexp.MustCompile(`^/?([A-Za-z0-9_.-]+)/([A-Za-z0-9_.-]+?)(\.git)?/?$`)

// githubRepoName returns "owner/name" if rawURL is a repository on GitHub
// (https://github.com/owner/name.git, ssh://git@github.com/owner/name.git
// or git@github.com:owner/name.git). token reports whether kiwicd may
// fetch it with an installation token: an HTTPS URL without credentials of
// its own.
func (s *Server) githubRepoName(rawURL string) (name string, token bool) {
	gh, err := url.Parse(s.cfg.GitHubURL)
	if err != nil {
		return "", false
	}
	host, path := "", ""
	u, err := url.Parse(rawURL)
	switch {
	case err == nil && u.Scheme != "" && u.Host != "":
		host, path = u.Hostname(), u.Path
		token = u.Scheme == gh.Scheme && u.User == nil && u.Port() == gh.Port()
	default:
		// scp-like: [user@]host:owner/name.git
		before, after, ok := strings.Cut(rawURL, ":")
		if !ok || strings.Contains(before, "/") {
			return "", false
		}
		host, path = before[strings.LastIndex(before, "@")+1:], after
	}
	if !strings.EqualFold(host, gh.Hostname()) {
		return "", false
	}
	m := githubPathRE.FindStringSubmatch(path)
	if m == nil {
		return "", false
	}
	return m[1] + "/" + m[2], token
}

// githubGitEnv returns environment variables that make git fetch a mirror of
// a GitHub repository the app is installed on with an installation token.
// Without the app, or for other repositories, it returns nil and git fetches
// as before.
func (s *Server) githubGitEnv(ctx context.Context, repo *Repo) []string {
	name, ok := s.githubRepoName(repo.URL)
	if !ok {
		return nil
	}
	a, err := s.githubApp()
	if err != nil {
		if !errors.Is(err, errNoGitHubApp) {
			s.log.Warn("GitHub App", "err", err)
		}
		return nil
	}
	token, err := s.installationToken(ctx, a, name, map[string]string{"contents": "read"})
	if err != nil {
		s.log.Warn("GitHub App installation token", "repo", name, "err", err)
		return nil
	}
	if token == "" {
		return nil
	}
	basic := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
	return []string{
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=http." + s.cfg.GitHubURL + "/.extraHeader",
		"GIT_CONFIG_VALUE_0=Authorization: Basic " + basic,
	}
}

// mirrorsOf returns the mirrors of the GitHub repository fullName.
func (s *Server) mirrorsOf(fullName string) ([]*Repo, error) {
	rows, err := s.db.Query(`SELECT ` + repoCols + ` FROM repos WHERE kind = 'mirror' ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Repo
	for rows.Next() {
		repo, err := scanRepo(rows)
		if err != nil {
			return nil, err
		}
		if name, _ := s.githubRepoName(repo.URL); name != "" && strings.EqualFold(name, fullName) {
			out = append(out, repo)
		}
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------- webhook

// handleGitHubWebhook takes the app's webhook deliveries. Push events sync
// the repository's mirrors and pull_request events go to them as through
// their own webhook URL (see handleWebhook). Repositories without a mirror
// are ignored.
func (s *Server) handleGitHubWebhook(w http.ResponseWriter, r *http.Request) {
	a, err := s.githubApp()
	if errors.Is(err, errNoGitHubApp) {
		httpError(w, 404, "%v", err)
		return
	}
	if err != nil {
		httpError(w, 500, "%v", err)
		return
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 25<<20))
	if !validSignature(a.WebhookSecret, body, r.Header.Get("X-Hub-Signature-256")) {
		httpError(w, 401, "bad webhook signature")
		return
	}
	body = webhookJSON(r, body)
	event := r.Header.Get("X-GitHub-Event")
	switch event {
	case "installation", "installation_repositories":
		s.gh.forget()
		writeJSON(w, map[string]bool{"ok": true})
		return
	case "push", "pull_request":
	default:
		writeJSON(w, map[string]any{"ok": true, "ignored": event})
		return
	}
	var ev struct {
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
	}
	if err := json.Unmarshal(body, &ev); err != nil || ev.Repository.FullName == "" {
		httpError(w, 400, "bad %s event", event)
		return
	}
	mirrors, err := s.mirrorsOf(ev.Repository.FullName)
	if err != nil {
		httpError(w, 500, "%v", err)
		return
	}
	names := []string{}
	for _, repo := range mirrors {
		names = append(names, repo.Name)
	}
	if event == "pull_request" {
		if _, _, err := parsePullRequestEvent(body); err != nil {
			httpError(w, 400, "bad pull_request event: %v", err)
			return
		}
		for _, repo := range mirrors {
			if !s.queuePullRequest(queuedPullRequest{repoID: repo.ID, body: body}) {
				httpError(w, 503, "too many pull request events waiting; redeliver this one later")
				return
			}
		}
	} else {
		for _, repo := range mirrors {
			go func() {
				if err := s.syncRepo(context.Background(), repo.ID, false); err != nil {
					s.log.Warn("webhook sync", "repo", repo.Name, "err", err)
				}
			}()
		}
	}
	writeJSON(w, map[string]any{"ok": true, "mirrors": names})
}

// validSignature checks a webhook's X-Hub-Signature-256 header.
func validSignature(secret string, body []byte, header string) bool {
	if secret == "" || header == "" {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hmac.Equal([]byte(header), []byte("sha256="+hex.EncodeToString(mac.Sum(nil))))
}

// webhookJSON returns a webhook's JSON payload: GitHub sends
// application/x-www-form-urlencoded webhooks as payload=JSON.
func webhookJSON(r *http.Request, body []byte) []byte {
	if ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); ct == "application/x-www-form-urlencoded" {
		form, _ := url.ParseQuery(string(body))
		return []byte(form.Get("payload"))
	}
	return body
}

// ---------------------------------------------------------------- settings API

type githubAppInfo struct {
	*githubApp
	InstallURL string `json:"install_url,omitempty"`
}

func (s *Server) handleGetGitHubApp(w http.ResponseWriter, r *http.Request) {
	base := s.publicURL(r)
	out := map[string]any{
		"app":          nil,
		"webhook_url":  base + "/api/github/webhook",
		"callback_url": base + "/api/auth/github/callback",
		"github_url":   s.cfg.GitHubURL,
	}
	a, err := s.githubApp()
	switch {
	case errors.Is(err, errNoGitHubApp):
	case err != nil:
		httpError(w, 500, "%v", err)
		return
	default:
		info := githubAppInfo{githubApp: a}
		if a.HTMLURL != "" {
			info.InstallURL = a.HTMLURL + "/installations/new"
		}
		out["app"] = info
	}
	writeJSON(w, out)
}

// handlePutGitHubApp sets up an existing app from its credentials.
func (s *Server) handlePutGitHubApp(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AppID         int64    `json:"app_id"`
		ClientID      string   `json:"client_id"`
		ClientSecret  string   `json:"client_secret"`
		PrivateKey    string   `json:"private_key"`
		WebhookSecret string   `json:"webhook_secret"`
		AllowedUsers  []string `json:"allowed_users"`
		AllowedOrgs   []string `json:"allowed_orgs"`
	}
	if err := readJSON(r, &req); err != nil {
		httpError(w, 400, "bad request: %v", err)
		return
	}
	a := &githubApp{AppID: req.AppID, ClientID: strings.TrimSpace(req.ClientID), ClientSecret: strings.TrimSpace(req.ClientSecret),
		PrivateKey: strings.TrimSpace(req.PrivateKey) + "\n", WebhookSecret: req.WebhookSecret}
	if a.AppID <= 0 || a.ClientID == "" || a.ClientSecret == "" || a.WebhookSecret == "" {
		httpError(w, 400, "app_id, client_id, client_secret, private_key and webhook_secret are required")
		return
	}
	var err error
	if a.AllowedUsers, a.AllowedOrgs, err = cleanAllowed(req.AllowedUsers, req.AllowedOrgs); err != nil {
		httpError(w, 400, "%v", err)
		return
	}
	// Checks the key and fills in the app's name and owner.
	auth, err := s.appAuth(a)
	if err != nil {
		httpError(w, 400, "%v", err)
		return
	}
	var info struct {
		Slug    string `json:"slug"`
		Name    string `json:"name"`
		HTMLURL string `json:"html_url"`
		Owner   struct {
			Login string `json:"login"`
		} `json:"owner"`
		ClientID string `json:"client_id"`
	}
	if err := s.githubAPI(r.Context(), "GET", "/app", auth, nil, &info); err != nil {
		httpError(w, 400, "checking the app with GitHub: %v", err)
		return
	}
	if info.ClientID != "" && info.ClientID != a.ClientID {
		httpError(w, 400, "the client ID isn't app %d's (%s)", a.AppID, info.ClientID)
		return
	}
	a.Slug, a.Name, a.HTMLURL, a.Owner = info.Slug, info.Name, info.HTMLURL, info.Owner.Login
	if err := s.saveGitHubApp(a); err != nil {
		httpError(w, 409, "%v", err)
		return
	}
	s.log.Info("GitHub App set up", "app", a.Slug, "by", requestUser(r))
	s.handleGetGitHubApp(w, r)
}

// handleUpdateGitHubApp changes who may sign in with GitHub. Removing
// someone logs out everyone signed in with GitHub, since a session doesn't
// say which entry let it in.
func (s *Server) handleUpdateGitHubApp(w http.ResponseWriter, r *http.Request) {
	a, err := s.githubApp()
	if err != nil {
		httpError(w, 404, "%v", err)
		return
	}
	var req struct {
		AllowedUsers *[]string `json:"allowed_users"`
		AllowedOrgs  *[]string `json:"allowed_orgs"`
	}
	if err := readJSON(r, &req); err != nil {
		httpError(w, 400, "bad request: %v", err)
		return
	}
	users, orgs := a.AllowedUsers, a.AllowedOrgs
	if req.AllowedUsers != nil {
		users = *req.AllowedUsers
	}
	if req.AllowedOrgs != nil {
		orgs = *req.AllowedOrgs
	}
	if users, orgs, err = cleanAllowed(users, orgs); err != nil {
		httpError(w, 400, "%v", err)
		return
	}
	removed := !containsAllFold(users, a.AllowedUsers) || !containsAllFold(orgs, a.AllowedOrgs)
	if _, err := s.db.Exec(`UPDATE github_app SET allowed_users = ?, allowed_orgs = ? WHERE id = 1`, mustJSON(users), mustJSON(orgs)); err != nil {
		httpError(w, 500, "%v", err)
		return
	}
	if removed {
		s.endGitHubSessions("sign-in access was removed")
	}
	s.handleGetGitHubApp(w, r)
}

// handleDeleteGitHubApp forgets the app. It stays on GitHub, where it can be
// deleted under its advanced settings.
func (s *Server) handleDeleteGitHubApp(w http.ResponseWriter, r *http.Request) {
	if _, err := s.db.Exec(`DELETE FROM github_app`); err != nil {
		httpError(w, 500, "%v", err)
		return
	}
	s.gh.forget()
	s.endGitHubSessions("the GitHub App was removed")
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) endGitHubSessions(why string) {
	res, err := s.db.Exec(`DELETE FROM sessions WHERE github_login != ''`)
	if err != nil {
		s.log.Error("ending GitHub sessions", "err", err)
		return
	}
	if n, _ := res.RowsAffected(); n > 0 {
		s.log.Info("logged out everyone signed in with GitHub: "+why, "sessions", n)
	}
	if err := s.deleteTokens(true, why); err != nil {
		s.log.Error("revoking GitHub users' API tokens", "err", err)
	}
}

var githubLoginRE = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})$`)

// cleanAllowed checks lists of GitHub logins and organizations.
func cleanAllowed(users, orgs []string) ([]string, []string, error) {
	clean := func(what string, in []string) ([]string, error) {
		out := []string{}
		for _, v := range in {
			v = strings.TrimPrefix(strings.TrimSpace(v), "@")
			if v == "" || containsFold(out, v) {
				continue
			}
			if !githubLoginRE.MatchString(v) {
				return nil, fmt.Errorf("%q isn't a GitHub %s", v, what)
			}
			out = append(out, v)
		}
		sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i]) < strings.ToLower(out[j]) })
		return out, nil
	}
	u, err := clean("login", users)
	if err != nil {
		return nil, nil, err
	}
	o, err := clean("organization", orgs)
	return u, o, err
}

// containsAllFold reports whether list has every entry of want.
func containsAllFold(list, want []string) bool {
	for _, v := range want {
		if !containsFold(list, v) {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------- manifest flow

// setupTimeout is how long a manifest flow started in the UI may take.
const setupTimeout = time.Hour

// handleGitHubAppManifest starts creating the app: it returns the manifest
// and the GitHub URL the UI posts it to, for a personal account or an
// organization.
func (s *Server) handleGitHubAppManifest(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
		Org  string `json:"org"`
	}
	if err := readJSON(r, &req); err != nil {
		httpError(w, 400, "bad request: %v", err)
		return
	}
	if _, err := s.githubApp(); err == nil {
		httpError(w, 409, "a GitHub App is already set up; remove it first")
		return
	}
	req.Org = strings.TrimSpace(req.Org)
	if req.Org != "" && !githubLoginRE.MatchString(req.Org) {
		httpError(w, 400, "%q isn't a GitHub organization", req.Org)
		return
	}
	if req.Name = strings.TrimSpace(req.Name); req.Name == "" {
		req.Name = "kiwicd"
	}
	state := randomToken(16)
	now := s.gh.now()
	s.gh.mu.Lock()
	for k, at := range s.gh.setups {
		if now.Sub(at) > setupTimeout {
			delete(s.gh.setups, k)
		}
	}
	s.gh.setups[state] = now
	s.gh.mu.Unlock()

	base := s.publicURL(r)
	manifest := map[string]any{
		"name":            req.Name,
		"url":             base,
		"hook_attributes": map[string]any{"url": base + "/api/github/webhook", "active": true},
		"redirect_url":    base + "/api/github/app/callback",
		"callback_urls":   []string{base + "/api/auth/github/callback"},
		"setup_url":       base + "/settings",
		"public":          false,
		"default_permissions": map[string]string{
			"contents":      "read", // fetching mirrors, push events
			"metadata":      "read",
			"pull_requests": "read", // pull_request events
			"members":       "read", // organizations whose members may sign in
		},
		"default_events": []string{"push", "pull_request"},
	}
	action := s.cfg.GitHubURL + "/settings/apps/new"
	if req.Org != "" {
		action = s.cfg.GitHubURL + "/organizations/" + url.PathEscape(req.Org) + "/settings/apps/new"
	}
	writeJSON(w, map[string]string{"action": action + "?state=" + state, "manifest": mustJSON(manifest)})
}

// handleGitHubAppCallback finishes creating the app: GitHub sends the browser
// here with a code for the app's credentials.
func (s *Server) handleGitHubAppCallback(w http.ResponseWriter, r *http.Request) {
	state, code := r.URL.Query().Get("state"), r.URL.Query().Get("code")
	s.gh.mu.Lock()
	at, ok := s.gh.setups[state]
	delete(s.gh.setups, state)
	s.gh.mu.Unlock()
	if !ok || s.gh.now().Sub(at) > setupTimeout || code == "" {
		settingsRedirect(w, r, "github_error", "The GitHub App setup expired or didn't start here; try again.")
		return
	}
	var conv struct {
		ID            int64  `json:"id"`
		Slug          string `json:"slug"`
		Name          string `json:"name"`
		HTMLURL       string `json:"html_url"`
		ClientID      string `json:"client_id"`
		ClientSecret  string `json:"client_secret"`
		PEM           string `json:"pem"`
		WebhookSecret string `json:"webhook_secret"`
		Owner         struct {
			Login string `json:"login"`
			Type  string `json:"type"`
		} `json:"owner"`
	}
	if err := s.githubAPI(r.Context(), "POST", "/app-manifests/"+url.PathEscape(code)+"/conversions", "", nil, &conv); err != nil {
		settingsRedirect(w, r, "github_error", "Getting the new app's credentials from GitHub failed: "+err.Error())
		return
	}
	a := &githubApp{AppID: conv.ID, Slug: conv.Slug, Name: conv.Name, HTMLURL: conv.HTMLURL, Owner: conv.Owner.Login,
		ClientID: conv.ClientID, ClientSecret: conv.ClientSecret, PrivateKey: conv.PEM, WebhookSecret: conv.WebhookSecret,
		AllowedUsers: []string{}, AllowedOrgs: []string{}}
	// Whoever owns the app may sign in with it.
	if conv.Owner.Type == "Organization" {
		a.AllowedOrgs = []string{conv.Owner.Login}
	} else if conv.Owner.Login != "" {
		a.AllowedUsers = []string{conv.Owner.Login}
	}
	if err := s.saveGitHubApp(a); err != nil {
		settingsRedirect(w, r, "github_error", err.Error())
		return
	}
	s.log.Info("GitHub App created", "app", a.Slug, "owner", a.Owner, "by", requestUser(r))
	settingsRedirect(w, r, "github", "created")
}

func settingsRedirect(w http.ResponseWriter, r *http.Request, key, value string) {
	http.Redirect(w, r, "/settings?"+url.Values{key: {value}}.Encode(), http.StatusSeeOther)
}

// handleListGitHubRepos lists the repositories the app is installed on, for
// adding mirrors of them.
func (s *Server) handleListGitHubRepos(w http.ResponseWriter, r *http.Request) {
	a, err := s.githubApp()
	if err != nil {
		httpError(w, 404, "%v", err)
		return
	}
	auth, err := s.appAuth(a)
	if err != nil {
		httpError(w, 500, "%v", err)
		return
	}
	ctx := r.Context()
	var installs []struct {
		ID int64 `json:"id"`
	}
	if err := s.githubAPI(ctx, "GET", "/app/installations?per_page=100", auth, nil, &installs); err != nil {
		httpError(w, 502, "%v", err)
		return
	}
	type ghRepo struct {
		FullName      string `json:"full_name"`
		CloneURL      string `json:"clone_url"`
		Private       bool   `json:"private"`
		DefaultBranch string `json:"default_branch"`
	}
	out := []ghRepo{}
	for _, inst := range installs {
		var tok struct {
			Token string `json:"token"`
		}
		path := fmt.Sprintf("/app/installations/%d/access_tokens", inst.ID)
		if err := s.githubAPI(ctx, "POST", path, auth, map[string]any{"permissions": map[string]string{"metadata": "read"}}, &tok); err != nil {
			httpError(w, 502, "%v", err)
			return
		}
		for page := 1; page <= 10; page++ {
			var res struct {
				Repositories []ghRepo `json:"repositories"`
			}
			if err := s.githubAPI(ctx, "GET", "/installation/repositories?per_page=100&page="+strconv.Itoa(page), "token "+tok.Token, nil, &res); err != nil {
				httpError(w, 502, "%v", err)
				return
			}
			out = append(out, res.Repositories...)
			if len(res.Repositories) < 100 {
				break
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].FullName) < strings.ToLower(out[j].FullName) })
	writeJSON(w, out)
}
