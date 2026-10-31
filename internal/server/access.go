package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
)

// Everyone who signs in has a role:
//
//   - admin: everything, including the server's own settings: runners,
//     global secrets and variables, the GitHub App, adding and removing
//     repositories, roles, everyone's API tokens and the audit log.
//   - maintainer, of every repository or of some (OWNER/NAME or OWNER/*):
//     their secrets, variables, protected refs, readers, mirror settings and
//     uploaded workflows; starting, re-running and cancelling their runs;
//     pushing to them if they're hosted.
//   - viewer: repositories, runs and their logs, and runners.
//
// The admin password is the admin. GitHub logins get roles from grants to
// the login, to an organization's members or to a team's (Settings →
// GitHub); someone matched by several has the most they give, and someone
// matched by none can't sign in. Organization and team membership is checked
// when someone signs in, and kept in github_users until they next do.
// Roles themselves are looked up on every request, so changing a grant
// applies at once.
//
// An API token acts as its maker, limited by its scope: a role, which is no
// higher than its maker's when it's made and is lowered with the maker's
// afterwards, and optionally some repositories, the only ones it can see,
// and of which it is at most a maintainer. Tokens without a scope (all
// those made before scopes existed) have their maker's full rights.
//
// Every API route says which role it needs (see routes in server.go), and
// for routes about one repository, which one; authorize checks it before the
// handler runs. Lists show only what the caller can see.

type role int

const (
	roleNone role = iota
	roleViewer
	roleMaintainer
	roleAdmin
)

var roleNames = [...]string{"", "viewer", "maintainer", "admin"}

func (r role) String() string { return roleNames[r] }

func parseRole(s string) (role, error) {
	for i, n := range roleNames {
		if i > 0 && strings.EqualFold(s, n) {
			return role(i), nil
		}
	}
	return roleNone, fmt.Errorf("%q isn't a role: use viewer, maintainer or admin", s)
}

// principal is who made a request and what they may do.
type principal struct {
	user   string // adminUser, or a GitHub login
	github bool   // user is a GitHub login
	// role is the highest the principal has: admin, maintainer (of
	// every repository if maintainAll, otherwise of those maintain
	// matches) or viewer.
	role        role
	maintainAll bool
	maintain    []string
	// repos, if not nil, are the only repositories the principal can see.
	repos []string
	// token is the API token the request was made with, if any.
	token *APIToken
}

func adminPrincipal() *principal {
	return &principal{user: adminUser, role: roleAdmin, maintainAll: true}
}

func (p *principal) admin() bool { return p.role == roleAdmin }

func (p *principal) canView(repo string) bool {
	return p.repos == nil || containsFold(p.repos, repo)
}

func (p *principal) canMaintain(repo string) bool {
	return p.role >= roleMaintainer && p.canView(repo) && (p.maintainAll || matchRepoPatterns(p.maintain, repo))
}

// scoped reports whether the request carries an API token with a scope.
func (p *principal) scoped() bool {
	return p.token != nil && (p.token.Role != "" || len(p.token.Repos) > 0)
}

// grant adds what g gives.
func (p *principal) grant(g RoleGrant) {
	r, err := parseRole(g.Role)
	if err != nil {
		return
	}
	p.role = max(p.role, r)
	switch {
	case r == roleAdmin || r == roleMaintainer && len(g.Repos) == 0:
		p.maintainAll = true
	case r == roleMaintainer:
		p.maintain = append(p.maintain, g.Repos...)
	}
}

// limit applies an API token's scope.
func (p *principal) limit(t *APIToken) {
	p.token = t
	if t.Role != "" {
		r, err := parseRole(t.Role)
		if err != nil {
			r = roleViewer
		}
		p.role = min(p.role, r)
	}
	if len(t.Repos) > 0 {
		p.repos = t.Repos
		p.role = min(p.role, roleMaintainer)
	}
	if p.role < roleMaintainer {
		p.maintainAll, p.maintain = false, nil
	}
}

// allowed reports whether p may make a request that needs role need, about
// repo ("" for one about no repository in particular, where maintainer
// means admin).
func (p *principal) allowed(need role, repo string) bool {
	switch need {
	case roleViewer:
		return repo == "" || p.canView(repo)
	case roleMaintainer:
		if repo == "" {
			return p.admin()
		}
		return p.canMaintain(repo)
	case roleAdmin:
		return p.admin()
	}
	return true
}

// access is what /api/me says the caller may do, for the UI and the CLI.
type access struct {
	Role        string    `json:"role"`
	Admin       bool      `json:"admin"`
	MaintainAll bool      `json:"maintain_all"`
	Maintain    []string  `json:"maintain"`
	Repos       []string  `json:"repos"` // null: all
	GitHub      bool      `json:"github"`
	Token       *APIToken `json:"token,omitempty"`
}

func (p *principal) access() access {
	a := access{Role: p.role.String(), Admin: p.admin(), MaintainAll: p.maintainAll, Maintain: p.maintain, Repos: p.repos,
		GitHub: p.github, Token: p.token}
	if a.Maintain == nil || a.MaintainAll {
		a.Maintain = []string{}
	}
	return a
}

type principalKey struct{}

// requestPrincipal is who made a request that authorize let through.
func requestPrincipal(r *http.Request) *principal {
	if p, ok := r.Context().Value(principalKey{}).(*principal); ok {
		return p
	}
	return adminPrincipal()
}

// requestUser is the name of who made a request that authorize let
// through.
func requestUser(r *http.Request) string { return requestPrincipal(r).user }

// ---------------------------------------------------------------- routes

// repoOf finds the repository a request is about.
type repoOf struct {
	what string // what the request names, for "no such …"
	// find returns the repository's name, "" if the request is about none
	// (such as global secrets), or false if what it names doesn't exist.
	find func(s *Server, r *http.Request) (string, bool)
}

var (
	// pathRepo: /api/repos/{owner}/{name}/...
	pathRepo = &repoOf{"repository", func(s *Server, r *http.Request) (string, bool) {
		repo, err := s.getRepo(r.PathValue("owner") + "/" + r.PathValue("name"))
		if err != nil {
			return "", false
		}
		return repo.Name, true
	}}
	// queryRepo: ?repo=OWNER/NAME, or none for global settings.
	queryRepo = &repoOf{"repository", func(s *Server, r *http.Request) (string, bool) {
		name := r.URL.Query().Get("repo")
		if name == "" {
			return "", true
		}
		repo, err := s.getRepo(name)
		if err != nil {
			return "", false
		}
		return repo.Name, true
	}}
	runRepo = &repoOf{"run", func(s *Server, r *http.Request) (string, bool) {
		return s.runRepoName(pathID(r))
	}}
	jobRepo = &repoOf{"job", func(s *Server, r *http.Request) (string, bool) {
		var name string
		err := s.db.QueryRow(`SELECT p.name FROM jobs j JOIN runs r ON r.id = j.run_id JOIN repos p ON p.id = r.repo_id WHERE j.id = ?`,
			pathID(r)).Scan(&name)
		return name, err == nil
	}}
	// eventsRepo: ?run=ID, or none for the stream of every run.
	eventsRepo = &repoOf{"run", func(s *Server, r *http.Request) (string, bool) {
		id := r.URL.Query().Get("run")
		if id == "" {
			return "", true
		}
		var n int64
		if _, err := fmt.Sscan(id, &n); err != nil {
			return "", false
		}
		return s.runRepoName(n)
	}}
)

func (s *Server) runRepoName(runID int64) (string, bool) {
	var name string
	err := s.db.QueryRow(`SELECT p.name FROM runs r JOIN repos p ON p.id = r.repo_id WHERE r.id = ?`, runID).Scan(&name)
	return name, err == nil
}

// authorize lets through requests from principals with the role rt needs.
// Repositories the principal can't see are reported as not found, like ones
// that don't exist.
func (s *Server) authorize(rt route) http.Handler {
	if rt.need == roleNone {
		return rt.handler
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, err := s.authenticate(r)
		if err != nil {
			httpError(w, http.StatusTooManyRequests, "%v", err)
			return
		}
		if p == nil {
			httpError(w, http.StatusUnauthorized, "login required")
			return
		}
		repo := ""
		if rt.repo != nil {
			name, ok := rt.repo.find(s, r)
			if !ok || name != "" && !p.canView(name) {
				httpError(w, http.StatusNotFound, "no such %s", rt.repo.what)
				return
			}
			repo = name
		}
		if !p.allowed(rt.need, repo) {
			httpError(w, http.StatusForbidden, "%s", forbidden(p, rt.need, repo))
			return
		}
		rt.handler(w, r.WithContext(context.WithValue(r.Context(), principalKey{}, p)))
	})
}

// forbidden says why p may not do something that needs role need.
func forbidden(p *principal, need role, repo string) string {
	who := "you are"
	if p.scoped() {
		who = "this API token is"
	}
	if need == roleMaintainer && repo != "" {
		return fmt.Sprintf("this needs a maintainer of %s, and %s %s", repo, who, describeRole(p))
	}
	return fmt.Sprintf("this needs an admin, and %s %s", who, describeRole(p))
}

func describeRole(p *principal) string {
	switch {
	case p.role == roleAdmin:
		return "an admin"
	case p.role == roleMaintainer && p.maintainAll:
		return "a maintainer"
	case p.role == roleMaintainer:
		return "a maintainer of " + strings.Join(p.maintain, ", ") + " only"
	}
	return "a viewer"
}

// ---------------------------------------------------------------- repository patterns

var repoPatternRE = regexp.MustCompile(`^[A-Za-z0-9_.-]+/(\*|[A-Za-z0-9_.-]+)$`)

// matchRepoPatterns reports whether repo is one of patterns: OWNER/NAME, or
// OWNER/* for every repository of OWNER.
func matchRepoPatterns(patterns []string, repo string) bool {
	owner, _, _ := strings.Cut(repo, "/")
	for _, p := range patterns {
		if o, ok := strings.CutSuffix(p, "/*"); ok && strings.EqualFold(o, owner) || strings.EqualFold(p, repo) {
			return true
		}
	}
	return false
}

func cleanRepoPatterns(in []string) ([]string, error) {
	out := []string{}
	for _, p := range in {
		p = strings.TrimSuffix(strings.TrimSpace(p), ".git")
		if p == "" || containsFold(out, p) {
			continue
		}
		if !repoPatternRE.MatchString(p) || strings.Contains(p, "..") {
			return nil, fmt.Errorf("%q must look like owner/repo or owner/*", p)
		}
		out = append(out, p)
	}
	sort.Strings(out)
	return out, nil
}

// ---------------------------------------------------------------- grants

// RoleGrant gives a role to a GitHub login (kind user), to the members of an
// organization (org) or to those of a team (team, named ORG/TEAM-SLUG).
type RoleGrant struct {
	Kind      string   `json:"kind"`
	Name      string   `json:"name"`
	Role      string   `json:"role"`
	Repos     []string `json:"repos"` // maintainers': OWNER/NAME or OWNER/*; empty for all
	UpdatedAt int64    `json:"updated_at"`
}

// key names the organization or team in github_users.memberships.
func (g RoleGrant) key() string { return g.Kind + ":" + strings.ToLower(g.Name) }

func (g RoleGrant) String() string {
	s := g.Kind + " " + g.Name + ": " + g.Role
	if len(g.Repos) > 0 {
		s += " of " + strings.Join(g.Repos, ", ")
	}
	return s
}

var teamRE = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})/[A-Za-z0-9_.-]+$`)

func cleanGrant(g RoleGrant) (RoleGrant, error) {
	g.Name = strings.TrimPrefix(strings.TrimSpace(g.Name), "@")
	switch g.Kind {
	case "user", "org":
		if !githubLoginRE.MatchString(g.Name) {
			return g, fmt.Errorf("%q isn't a GitHub %s", g.Name, map[string]string{"user": "login", "org": "organization"}[g.Kind])
		}
	case "team":
		if !teamRE.MatchString(g.Name) {
			return g, fmt.Errorf("%q isn't a team: use ORG/TEAM, with the team's slug", g.Name)
		}
	default:
		return g, errors.New("kind must be user, org or team")
	}
	r, err := parseRole(g.Role)
	if err != nil {
		return g, err
	}
	g.Role = r.String()
	if g.Repos, err = cleanRepoPatterns(g.Repos); err != nil {
		return g, err
	}
	if r != roleMaintainer && len(g.Repos) > 0 {
		return g, errors.New("only maintainers are limited to repositories")
	}
	return g, nil
}

func (s *Server) roleGrants() ([]RoleGrant, error) {
	rows, err := s.db.Query(`SELECT kind, name, role, repos, updated_at FROM role_grants ORDER BY kind = 'team', kind = 'org', name COLLATE NOCASE`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RoleGrant{}
	for rows.Next() {
		var g RoleGrant
		var repos string
		if err := rows.Scan(&g.Kind, &g.Name, &g.Role, &repos, &g.UpdatedAt); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(repos), &g.Repos)
		if g.Repos == nil {
			g.Repos = []string{}
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

func (s *Server) saveGrant(g RoleGrant) error {
	_, err := s.db.Exec(`INSERT INTO role_grants (kind, name, role, repos, updated_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (kind, name) DO UPDATE SET name = excluded.name, role = excluded.role, repos = excluded.repos, updated_at = excluded.updated_at`,
		g.Kind, g.Name, g.Role, mustJSON(g.Repos), nowMS())
	return err
}

// githubPrincipal is what the GitHub user login may do; its role is
// roleNone if nothing, or err is set.
func (s *Server) githubPrincipal(login string) (*principal, error) {
	grants, err := s.roleGrants()
	if err != nil {
		return nil, err
	}
	var ms string
	var memberships []string
	if err := s.db.QueryRow(`SELECT memberships FROM github_users WHERE login = ?`, login).Scan(&ms); err == nil {
		_ = json.Unmarshal([]byte(ms), &memberships)
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	p := &principal{user: login, github: true}
	for _, g := range grants {
		if g.Kind == "user" && strings.EqualFold(g.Name, login) || g.Kind != "user" && containsFold(memberships, g.key()) {
			p.grant(g)
		}
	}
	return p, nil
}

// checkMemberships finds the grants that apply to login, asking GitHub
// about organizations and teams, records the organizations and teams it's a
// member of, and reports whether any grant applies.
func (s *Server) checkMemberships(ctx context.Context, a *githubApp, login string) (bool, error) {
	grants, err := s.roleGrants()
	if err != nil {
		return false, err
	}
	allowed := false
	memberships := []string{}
	tokens := map[string]string{}
	for _, g := range grants {
		if g.Kind == "user" {
			allowed = allowed || strings.EqualFold(g.Name, login)
			continue
		}
		org, team, _ := strings.Cut(g.Name, "/")
		token, ok := tokens[strings.ToLower(org)]
		if !ok {
			token, err = s.installationToken(ctx, a, "org:"+org, map[string]string{"members": "read"})
			if err != nil || token == "" {
				s.log.Warn("can't check members of an organization with a role: is the GitHub App installed on it, with members permission?",
					"org", org, "err", err)
			}
			tokens[strings.ToLower(org)] = token
		}
		if token == "" {
			continue
		}
		member := false
		if g.Kind == "org" {
			err = s.githubAPI(ctx, "GET", "/orgs/"+url.PathEscape(org)+"/members/"+url.PathEscape(login), "token "+token, nil, nil)
			member = err == nil
		} else {
			var m struct {
				State string `json:"state"`
			}
			err = s.githubAPI(ctx, "GET", "/orgs/"+url.PathEscape(org)+"/teams/"+url.PathEscape(team)+"/memberships/"+url.PathEscape(login),
				"token "+token, nil, &m)
			member = err == nil && m.State == "active"
		}
		if err != nil && !isGitHubStatus(err, 404) {
			s.log.Warn("checking membership", g.Kind, g.Name, "login", login, "err", err)
		}
		if member {
			memberships = append(memberships, g.key())
			allowed = true
		}
	}
	if !allowed {
		// Forget memberships they've lost, but don't list strangers.
		_, err = s.db.Exec(`UPDATE github_users SET memberships = '[]' WHERE login = ?`, login)
		return false, err
	}
	_, err = s.db.Exec(`INSERT INTO github_users (login, memberships, signed_in_at) VALUES (?, ?, ?)
		ON CONFLICT (login) DO UPDATE SET login = excluded.login, memberships = excluded.memberships, signed_in_at = excluded.signed_in_at`,
		login, mustJSON(memberships), nowMS())
	return true, err
}

// revokeLostAccess signs out GitHub users that no grant applies to any more
// and revokes their API tokens.
func (s *Server) revokeLostAccess(why string) {
	rows, err := s.db.Query(`SELECT github_login FROM sessions WHERE github_login != '' UNION SELECT github_login FROM api_tokens WHERE github_login != ''`)
	if err != nil {
		s.log.Error("revoking access", "err", err)
		return
	}
	var logins []string
	for rows.Next() {
		var l string
		_ = rows.Scan(&l)
		logins = append(logins, l)
	}
	rows.Close()
	for _, login := range logins {
		p, err := s.githubPrincipal(login)
		if err != nil || p.role != roleNone {
			continue
		}
		_, _ = s.db.Exec(`DELETE FROM sessions WHERE github_login = ?`, login)
		res, err := s.db.Exec(`DELETE FROM api_tokens WHERE github_login = ?`, login)
		if err != nil {
			continue
		}
		n, _ := res.RowsAffected()
		s.log.Info("signed out a GitHub user without a role: "+why, "login", login, "tokens", n)
		s.audit(nil, "access.revoke", "", login, fmt.Sprintf("signed out, %d API tokens revoked: %s", n, why))
	}
}

// migrateAllowList turns the GitHub App's sign-in allow list, from before
// roles, into admin grants, which is what it gave. People let in by an
// organization keep their access if it's the only one; with several, which
// one let them in isn't known, so they sign in again.
func (s *Server) migrateAllowList() error {
	var users, orgs string
	err := s.db.QueryRow(`SELECT allowed_users, allowed_orgs FROM github_app WHERE id = 1`).Scan(&users, &orgs)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var u, o []string
	_ = json.Unmarshal([]byte(users), &u)
	_ = json.Unmarshal([]byte(orgs), &o)
	if len(u) == 0 && len(o) == 0 {
		return nil
	}
	for _, g := range append(grantsOf("user", u), grantsOf("org", o)...) {
		if _, err := s.db.Exec(`INSERT INTO role_grants (kind, name, role, repos, updated_at) VALUES (?, ?, 'admin', '[]', ?)
			ON CONFLICT DO NOTHING`, g.Kind, g.Name, nowMS()); err != nil {
			return err
		}
	}
	if len(o) == 1 {
		// Everyone signed in who isn't an allowed user came in as a member.
		rows, err := s.db.Query(`SELECT github_login FROM sessions WHERE github_login != '' UNION SELECT github_login FROM api_tokens WHERE github_login != ''`)
		if err != nil {
			return err
		}
		var members []string
		for rows.Next() {
			var l string
			_ = rows.Scan(&l)
			if !containsFold(u, l) {
				members = append(members, l)
			}
		}
		rows.Close()
		membership := mustJSON([]string{RoleGrant{Kind: "org", Name: o[0]}.key()})
		for _, l := range members {
			if _, err := s.db.Exec(`INSERT INTO github_users (login, memberships, signed_in_at) VALUES (?, ?, 0) ON CONFLICT DO NOTHING`,
				l, membership); err != nil {
				return err
			}
		}
	}
	_, err = s.db.Exec(`UPDATE github_app SET allowed_users = '[]', allowed_orgs = '[]' WHERE id = 1`)
	if err == nil {
		s.log.Info("made the GitHub sign-in allow list into admin roles", "users", len(u), "orgs", len(o))
	}
	return err
}

func grantsOf(kind string, names []string) []RoleGrant {
	var out []RoleGrant
	for _, n := range names {
		out = append(out, RoleGrant{Kind: kind, Name: n, Role: "admin"})
	}
	return out
}

// ---------------------------------------------------------------- API

func (s *Server) handleListRoles(w http.ResponseWriter, r *http.Request) {
	grants, err := s.roleGrants()
	if err != nil {
		httpError(w, 500, "%v", err)
		return
	}
	writeJSON(w, grants)
}

// handlePutRole gives a login, organization or team a role, or changes the
// one it has.
func (s *Server) handlePutRole(w http.ResponseWriter, r *http.Request) {
	var g RoleGrant
	if err := readJSON(r, &g); err != nil {
		httpError(w, 400, "bad request: %v", err)
		return
	}
	g, err := cleanGrant(g)
	if err != nil {
		httpError(w, 400, "%v", err)
		return
	}
	if err := s.saveGrant(g); err != nil {
		httpError(w, 500, "%v", err)
		return
	}
	s.audit(r, "role.set", "", g.Kind+" "+g.Name, g.String())
	s.revokeLostAccess("their role was changed")
	writeJSON(w, g)
}

// handleDeleteRole takes a role away: DELETE /api/roles?kind=org&name=my-org.
func (s *Server) handleDeleteRole(w http.ResponseWriter, r *http.Request) {
	kind, name := r.URL.Query().Get("kind"), r.URL.Query().Get("name")
	res, err := s.db.Exec(`DELETE FROM role_grants WHERE kind = ? AND name = ?`, kind, name)
	if err != nil {
		httpError(w, 500, "%v", err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		httpError(w, 404, "%s %s has no role", kind, name)
		return
	}
	s.audit(r, "role.delete", "", kind+" "+name, "")
	s.revokeLostAccess("their role was removed")
	writeJSON(w, map[string]bool{"ok": true})
}

// githubUser is someone who has signed in with GitHub, with the role they
// have now.
type githubUser struct {
	Login       string   `json:"login"`
	Memberships []string `json:"memberships"`
	SignedInAt  int64    `json:"signed_in_at"`
	Role        string   `json:"role"` // "" for none
	MaintainAll bool     `json:"maintain_all"`
	Maintain    []string `json:"maintain"`
}

func (s *Server) handleListUsers(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.Query(`SELECT login, memberships, signed_in_at FROM github_users ORDER BY login`)
	if err != nil {
		httpError(w, 500, "%v", err)
		return
	}
	users := []*githubUser{}
	for rows.Next() {
		var u githubUser
		var ms string
		if err := rows.Scan(&u.Login, &ms, &u.SignedInAt); err != nil {
			rows.Close()
			httpError(w, 500, "%v", err)
			return
		}
		_ = json.Unmarshal([]byte(ms), &u.Memberships)
		users = append(users, &u)
	}
	rows.Close()
	for _, u := range users {
		if p, err := s.githubPrincipal(u.Login); err == nil {
			a := p.access()
			u.Role, u.MaintainAll, u.Maintain = a.Role, a.MaintainAll, a.Maintain
		}
		if u.Memberships == nil {
			u.Memberships = []string{}
		}
		if u.Maintain == nil {
			u.Maintain = []string{}
		}
	}
	writeJSON(w, users)
}
