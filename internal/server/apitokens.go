package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

// API tokens let scripts, the kiwicd CLI and its MCP server use the API
// without the admin password: they're sent like it, as a Bearer token or a
// Basic password, and act as whoever made them, limited by their scope (see
// access.go). They also work for git over /git/.
//
// Tokens go with the sessions of whoever made them: the admin's are revoked
// when the admin password changes, GitHub users' when they no longer have a
// role. GitHub organization membership is only checked when someone signs
// in, so a GitHub user's token lasts at most maxGitHubTokenDays; the admin's
// may never expire.

const (
	apiTokenPrefix     = "kiwicd_"
	maxGitHubTokenDays = 90
	// last_used_at is only written when it's this much out of date.
	tokenUseResolution = time.Minute
)

type APIToken struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	User       string `json:"user"`
	CreatedAt  int64  `json:"created_at"`
	ExpiresAt  int64  `json:"expires_at"` // 0: never
	LastUsedAt int64  `json:"last_used_at"`
	// The scope: at most this role ("" for its maker's), and only these
	// repositories (none for all).
	Role  string   `json:"role"`
	Repos []string `json:"repos"`
}

const tokenCols = `id, name, github_login, created_at, expires_at, last_used_at, role, repos`

func scanToken(row interface{ Scan(...any) error }) (*APIToken, error) {
	var t APIToken
	var repos string
	if err := row.Scan(&t.ID, &t.Name, &t.User, &t.CreatedAt, &t.ExpiresAt, &t.LastUsedAt, &t.Role, &repos); err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(repos), &t.Repos)
	if t.Repos == nil {
		t.Repos = []string{}
	}
	return &t, nil
}

// tokenPrincipal returns what the API token t may do, or nil if it isn't a
// current token or its maker has no role any more.
func (s *Server) tokenPrincipal(t string) *principal {
	tok, err := scanToken(s.db.QueryRow(`SELECT `+tokenCols+` FROM api_tokens WHERE token_hash = ?`, hashToken(t)))
	if err != nil || tok.ExpiresAt != 0 && tok.ExpiresAt <= nowMS() {
		return nil
	}
	if now := nowMS(); now-tok.LastUsedAt > tokenUseResolution.Milliseconds() {
		_, _ = s.db.Exec(`UPDATE api_tokens SET last_used_at = ? WHERE id = ?`, now, tok.ID)
	}
	var p *principal
	if tok.User == "" {
		tok.User = adminUser
		p = adminPrincipal()
	} else if p, err = s.githubPrincipal(tok.User); err != nil || p.role == roleNone {
		return nil
	}
	p.limit(tok)
	return p
}

// tokenOwner is the github_login of p's tokens.
func tokenOwner(p *principal) string {
	if p.github {
		return p.user
	}
	return ""
}

// handleListTokens lists the caller's tokens, or an admin's everyone's.
func (s *Server) handleListTokens(w http.ResponseWriter, r *http.Request) {
	p := requestPrincipal(r)
	q := `SELECT ` + tokenCols + ` FROM api_tokens WHERE (expires_at = 0 OR expires_at > ?)`
	args := []any{nowMS()}
	if !p.admin() {
		q += ` AND github_login = ?`
		args = append(args, tokenOwner(p))
	}
	rows, err := s.db.Query(q+` ORDER BY id`, args...)
	if err != nil {
		httpError(w, 500, "%v", err)
		return
	}
	defer rows.Close()
	out := []*APIToken{}
	for rows.Next() {
		t, err := scanToken(rows)
		if err != nil {
			httpError(w, 500, "%v", err)
			return
		}
		if t.User == "" {
			t.User = adminUser
		}
		out = append(out, t)
	}
	writeJSON(w, out)
}

// errScopedToken refuses token management to tokens with a scope, so that a
// token can't make itself one with more rights.
const errScopedToken = "API tokens with a scope can't make or revoke tokens"

// handleCreateToken makes a token for whoever asks. expires_in_days 0 means
// never for the admin and maxGitHubTokenDays for GitHub users. role and
// repos limit what it may do (see access.go).
func (s *Server) handleCreateToken(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name          string   `json:"name"`
		ExpiresInDays int      `json:"expires_in_days"`
		Role          string   `json:"role"`
		Repos         []string `json:"repos"`
	}
	if err := readJSON(r, &req); err != nil {
		httpError(w, 400, "bad request: %v", err)
		return
	}
	p := requestPrincipal(r)
	if p.scoped() {
		httpError(w, 403, errScopedToken)
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" || len(req.Name) > 100 {
		httpError(w, 400, "a token needs a name of at most 100 characters")
		return
	}
	if req.ExpiresInDays < 0 || req.ExpiresInDays > 3650 {
		httpError(w, 400, "expires_in_days must be between 0 (never) and 3650")
		return
	}
	if req.Role != "" {
		want, err := parseRole(req.Role)
		if err != nil {
			httpError(w, 400, "%v", err)
			return
		}
		if want > p.role {
			httpError(w, 400, "a token's role can't be higher than yours (%s)", p.role)
			return
		}
		req.Role = want.String()
	}
	repos := []string{}
	for _, name := range req.Repos {
		name = strings.TrimSuffix(strings.TrimSpace(name), ".git")
		if name == "" {
			continue
		}
		repo, err := s.getRepo(name)
		if err != nil {
			httpError(w, 400, "no such repository %s", name)
			return
		}
		if !containsFold(repos, repo.Name) {
			repos = append(repos, repo.Name)
		}
	}
	sort.Strings(repos)
	if len(repos) > 0 && req.Role == roleAdmin.String() {
		httpError(w, 400, "a token limited to repositories can be at most a maintainer")
		return
	}
	user := requestUser(r)
	login := tokenOwner(p)
	if p.github && (req.ExpiresInDays == 0 || req.ExpiresInDays > maxGitHubTokenDays) {
		req.ExpiresInDays = maxGitHubTokenDays
	}
	t := APIToken{Name: req.Name, User: user, CreatedAt: nowMS(), Role: req.Role, Repos: repos}
	if req.ExpiresInDays > 0 {
		t.ExpiresAt = time.Now().AddDate(0, 0, req.ExpiresInDays).UnixMilli()
	}
	token := apiTokenPrefix + randomToken(24)
	res, err := s.db.Exec(`INSERT INTO api_tokens (name, token_hash, github_login, created_at, expires_at, role, repos) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		t.Name, hashToken(token), login, t.CreatedAt, t.ExpiresAt, t.Role, mustJSON(t.Repos))
	if err != nil {
		httpError(w, 500, "%v", err)
		return
	}
	t.ID, _ = res.LastInsertId()
	s.log.Info("API token created", "id", t.ID, "name", t.Name, "by", user)
	s.audit(r, "token.create", "", fmt.Sprintf("%s (%d)", t.Name, t.ID), "scope: "+t.scope())
	writeJSON(w, map[string]any{"token": token, "info": t})
}

// scope describes what a token may do, for the audit log.
func (t *APIToken) scope() string {
	s := "full"
	if t.Role != "" {
		s = t.Role
	}
	if len(t.Repos) > 0 {
		s += " of " + strings.Join(t.Repos, ", ")
	}
	return s
}

// handleDeleteToken revokes one of the caller's tokens, or an admin's
// anyone's. DELETE /api/tokens/current revokes the one the request was made
// with, for logging out.
func (s *Server) handleDeleteToken(w http.ResponseWriter, r *http.Request) {
	p := requestPrincipal(r)
	var t *APIToken
	var err error
	if r.PathValue("id") == "current" {
		if p.token == nil {
			httpError(w, 400, "this request wasn't made with an API token")
			return
		}
		t = p.token
	} else {
		if p.scoped() {
			httpError(w, 403, errScopedToken)
			return
		}
		t, err = scanToken(s.db.QueryRow(`SELECT `+tokenCols+` FROM api_tokens WHERE id = ?`, pathID(r)))
		if err != nil || !p.admin() && t.User != tokenOwner(p) {
			httpError(w, 404, "no such token")
			return
		}
	}
	res, err := s.db.Exec(`DELETE FROM api_tokens WHERE id = ?`, t.ID)
	if err != nil {
		httpError(w, 500, "%v", err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		httpError(w, 404, "no such token")
		return
	}
	owner := t.User
	if owner == "" {
		owner = adminUser
	}
	s.audit(r, "token.revoke", "", fmt.Sprintf("%s (%d)", t.Name, t.ID), "made by "+owner)
	writeJSON(w, map[string]bool{"ok": true})
}

// deleteTokens revokes the admin's tokens (github false) or GitHub users'.
func (s *Server) deleteTokens(github bool, why string) error {
	q := `DELETE FROM api_tokens WHERE github_login = ''`
	if github {
		q = `DELETE FROM api_tokens WHERE github_login != ''`
	}
	res, err := s.db.Exec(q)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		s.log.Info("revoked API tokens: "+why, "tokens", n)
		s.audit(nil, "token.revoke", "", fmt.Sprintf("%d tokens", n), why)
	}
	return nil
}
