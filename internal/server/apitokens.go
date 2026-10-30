package server

import (
	"database/sql"
	"net/http"
	"strings"
	"time"
)

// API tokens let scripts, the kiwicd CLI and its MCP server use the API
// without the admin password: they're sent like it, as a Bearer token or a
// Basic password, and act as whoever made them. They also work for git over
// /git/.
//
// Tokens go with the sessions of whoever made them: the admin's are revoked
// when the admin password changes, GitHub users' when sign-in access is
// removed. GitHub organization membership is only checked when someone signs
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
}

// tokenUser returns who the API token t acts as, or "" if it isn't a current
// token.
func (s *Server) tokenUser(t string) string {
	if !strings.HasPrefix(t, apiTokenPrefix) {
		return ""
	}
	var id, exp, used int64
	var login string
	err := s.db.QueryRow(`SELECT id, github_login, expires_at, last_used_at FROM api_tokens WHERE token_hash = ?`, hashToken(t)).
		Scan(&id, &login, &exp, &used)
	if err != nil || exp != 0 && exp <= nowMS() {
		return ""
	}
	if now := nowMS(); now-used > tokenUseResolution.Milliseconds() {
		_, _ = s.db.Exec(`UPDATE api_tokens SET last_used_at = ? WHERE id = ?`, now, id)
	}
	if login == "" {
		return adminUser
	}
	return login
}

func (s *Server) handleListTokens(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.Query(`SELECT id, name, github_login, created_at, expires_at, last_used_at FROM api_tokens
		WHERE expires_at = 0 OR expires_at > ? ORDER BY id`, nowMS())
	if err != nil {
		httpError(w, 500, "%v", err)
		return
	}
	defer rows.Close()
	out := []APIToken{}
	for rows.Next() {
		var t APIToken
		if err := rows.Scan(&t.ID, &t.Name, &t.User, &t.CreatedAt, &t.ExpiresAt, &t.LastUsedAt); err != nil {
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

// handleCreateToken makes a token for whoever asks. expires_in_days 0 means
// never for the admin and maxGitHubTokenDays for GitHub users.
func (s *Server) handleCreateToken(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name          string `json:"name"`
		ExpiresInDays int    `json:"expires_in_days"`
	}
	if err := readJSON(r, &req); err != nil {
		httpError(w, 400, "bad request: %v", err)
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
	user := requestUser(r)
	login := ""
	if user != adminUser {
		login = user
		if req.ExpiresInDays == 0 || req.ExpiresInDays > maxGitHubTokenDays {
			req.ExpiresInDays = maxGitHubTokenDays
		}
	}
	t := APIToken{Name: req.Name, User: user, CreatedAt: nowMS()}
	if req.ExpiresInDays > 0 {
		t.ExpiresAt = time.Now().AddDate(0, 0, req.ExpiresInDays).UnixMilli()
	}
	token := apiTokenPrefix + randomToken(24)
	res, err := s.db.Exec(`INSERT INTO api_tokens (name, token_hash, github_login, created_at, expires_at) VALUES (?, ?, ?, ?, ?)`,
		t.Name, hashToken(token), login, t.CreatedAt, t.ExpiresAt)
	if err != nil {
		httpError(w, 500, "%v", err)
		return
	}
	t.ID, _ = res.LastInsertId()
	s.log.Info("API token created", "id", t.ID, "name", t.Name, "by", user)
	writeJSON(w, map[string]any{"token": token, "info": t})
}

// handleDeleteToken revokes a token. DELETE /api/tokens/current revokes the
// one the request was made with, for logging out.
func (s *Server) handleDeleteToken(w http.ResponseWriter, r *http.Request) {
	var res sql.Result
	var err error
	if r.PathValue("id") == "current" {
		t, _ := requestPassword(r)
		if !strings.HasPrefix(t, apiTokenPrefix) {
			httpError(w, 400, "this request wasn't made with an API token")
			return
		}
		res, err = s.db.Exec(`DELETE FROM api_tokens WHERE token_hash = ?`, hashToken(t))
	} else {
		res, err = s.db.Exec(`DELETE FROM api_tokens WHERE id = ?`, pathID(r))
	}
	if err != nil {
		httpError(w, 500, "%v", err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		httpError(w, 404, "no such token")
		return
	}
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
	}
	return nil
}
