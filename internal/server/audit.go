package server

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// The audit log records who changed access and settings: secrets,
// variables, repositories' settings (protected refs and readers among
// them), uploaded workflows, runners, roles, API tokens and the GitHub App.
// Values of secrets are never in it. Entries are kept for good; there are
// few of them.

// systemActor is the actor of changes kiwicd makes itself, such as revoking
// tokens when the admin password changes.
const systemActor = "kiwicd"

type auditEntry struct {
	ID     int64  `json:"id"`
	At     int64  `json:"at"`
	Actor  string `json:"actor"`
	Token  string `json:"token,omitempty"` // the API token's name, if one was used
	Client string `json:"client,omitempty"`
	Action string `json:"action"`
	Repo   string `json:"repo,omitempty"`
	Target string `json:"target,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// audit records that r's principal did action, to target in repo (each ""
// if it doesn't apply). r nil means kiwicd did it.
func (s *Server) audit(r *http.Request, action, repo, target, detail string) {
	e := auditEntry{Actor: systemActor, Action: action, Repo: repo, Target: target, Detail: detail}
	if r != nil {
		p := requestPrincipal(r)
		e.Actor, e.Client = p.user, s.clientIP(r)
		if p.token != nil {
			e.Token = fmt.Sprintf("%s (%d)", p.token.Name, p.token.ID)
		}
	}
	if _, err := s.db.Exec(`INSERT INTO audit_log (at, actor, token, client, action, repo, target, detail) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		nowMS(), e.Actor, e.Token, e.Client, e.Action, e.Repo, e.Target, e.Detail); err != nil {
		s.log.Error("audit log", "action", action, "err", err)
	}
}

// handleAuditLog lists entries newest first: ?repo= for one repository's,
// ?before=ID for older pages, ?limit= (at most 500) per page.
func (s *Server) handleAuditLog(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	where, args := []string{"1"}, []any{}
	if repo := q.Get("repo"); repo != "" {
		where, args = append(where, "repo = ?"), append(args, repo)
	}
	if before, err := strconv.ParseInt(q.Get("before"), 10, 64); err == nil && before > 0 {
		where, args = append(where, "id < ?"), append(args, before)
	}
	limit := 100
	if l, err := strconv.Atoi(q.Get("limit")); err == nil && l > 0 && l <= 500 {
		limit = l
	}
	rows, err := s.db.Query(`SELECT id, at, actor, token, client, action, repo, target, detail FROM audit_log WHERE `+
		strings.Join(where, " AND ")+fmt.Sprintf(` ORDER BY id DESC LIMIT %d`, limit+1), args...)
	if err != nil {
		httpError(w, 500, "%v", err)
		return
	}
	defer rows.Close()
	entries := []auditEntry{}
	for rows.Next() {
		var e auditEntry
		if err := rows.Scan(&e.ID, &e.At, &e.Actor, &e.Token, &e.Client, &e.Action, &e.Repo, &e.Target, &e.Detail); err != nil {
			httpError(w, 500, "%v", err)
			return
		}
		entries = append(entries, e)
	}
	more := len(entries) > limit
	if more {
		entries = entries[:limit]
	}
	writeJSON(w, map[string]any{"entries": entries, "more": more})
}
