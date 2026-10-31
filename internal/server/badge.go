package server

import (
	"database/sql"
	"errors"
	"fmt"
	"html"
	"net/http"
	"strings"

	"github.com/dire-kiwi/kiwicd/internal/protocol"
)

// handleBadge serves a workflow's status badge, an SVG image:
// GET /api/repos/OWNER/NAME/badge/WORKFLOW.svg, with WORKFLOW a file in
// .github/workflows (ci.yml) or its whole name (kiwicd:deploy.yml). It shows
// the latest finished run on the repository's default branch, or ?branch=
// (a branch or a full ref), and of ?event= if given.
//
// Badges of repositories with public_badges need no sign-in, so a README
// elsewhere can show them; that shows the workflow's name and how its runs
// end to anyone. Others need it, and to anyone who may not see the
// repository answer as if it didn't exist.
func (s *Server) handleBadge(w http.ResponseWriter, r *http.Request) {
	repo, err := s.getRepo(r.PathValue("owner") + "/" + r.PathValue("name"))
	if err != nil || !repo.PublicBadges {
		p, aerr := s.authenticate(r)
		if aerr != nil {
			httpError(w, http.StatusTooManyRequests, "%v", aerr)
			return
		}
		if p == nil {
			httpError(w, http.StatusUnauthorized, "login required")
			return
		}
		if err != nil || !p.canView(repo.Name) {
			httpError(w, http.StatusNotFound, "no such repository")
			return
		}
	}
	file, ok := strings.CutSuffix(r.PathValue("file"), ".svg")
	if !ok || file == "" {
		httpError(w, http.StatusNotFound, "badges are WORKFLOW.svg")
		return
	}
	q := r.URL.Query()
	branch := q.Get("branch")
	if branch == "" {
		branch = repo.DefaultBranch
	}
	where := `repo_id = ? AND (workflow_file = ? OR workflow_file = '.github/workflows/' || ?) AND (ref = ? OR ref = 'refs/heads/' || ?)`
	args := []any{repo.ID, file, file, branch, branch}
	if event := q.Get("event"); event != "" {
		where += ` AND event = ?`
		args = append(args, event)
	}
	// The latest run names the workflow; the latest finished one gives the
	// status, unless none has finished.
	var name, status, conclusion string
	err = s.db.QueryRow(`SELECT workflow_name, status, conclusion FROM runs WHERE `+where+` ORDER BY id DESC LIMIT 1`, args...).Scan(&name, &status, &conclusion)
	if errors.Is(err, sql.ErrNoRows) {
		name = strings.TrimSuffix(strings.TrimSuffix(file, ".yml"), ".yaml")
	} else if err != nil {
		httpError(w, 500, "%v", err)
		return
	} else if status != protocol.StatusCompleted {
		err := s.db.QueryRow(`SELECT conclusion FROM runs WHERE `+where+` AND status = ? ORDER BY id DESC LIMIT 1`,
			append(args, protocol.StatusCompleted)...).Scan(&conclusion)
		if err == nil {
			status = protocol.StatusCompleted
		} else if !errors.Is(err, sql.ErrNoRows) {
			httpError(w, 500, "%v", err)
			return
		}
	}
	message, color := badgeStatus(status, conclusion)
	w.Header().Set("Content-Type", "image/svg+xml")
	if repo.PublicBadges {
		w.Header().Set("Cache-Control", "no-cache")
	} else {
		w.Header().Set("Cache-Control", "private, no-cache")
	}
	_, _ = w.Write([]byte(badgeSVG(name, message, color)))
}

// badgeStatus is what a badge says for a run's status and conclusion
// (status "" for no run), and its colour.
func badgeStatus(status, conclusion string) (message, color string) {
	switch {
	case status == "":
		return "no runs", "#9f9f9f"
	case status != protocol.StatusCompleted:
		return "running", "#dbab09"
	case conclusion == protocol.ResultSuccess:
		return "passing", "#2ea44f"
	case conclusion == protocol.ResultFailure:
		return "failing", "#cb2431"
	default:
		return conclusion, "#9f9f9f"
	}
}

// badgeSVG draws a badge in the usual flat style: label on grey, message on
// color.
func badgeSVG(label, message, color string) string {
	const pad = 6
	lw, mw := textWidth(label)+2*pad, textWidth(message)+2*pad
	w := lw + mw
	l, m := html.EscapeString(label), html.EscapeString(message)
	return fmt.Sprintf(`<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="20" role="img" aria-label="%s: %s">`+
		`<title>%s: %s</title>`+
		`<linearGradient id="s" x2="0" y2="100%%"><stop offset="0" stop-color="#bbb" stop-opacity=".1"/><stop offset="1" stop-opacity=".1"/></linearGradient>`+
		`<clipPath id="r"><rect width="%d" height="20" rx="3" fill="#fff"/></clipPath>`+
		`<g clip-path="url(#r)"><rect width="%d" height="20" fill="#555"/><rect x="%d" width="%d" height="20" fill="%s"/><rect width="%d" height="20" fill="url(#s)"/></g>`+
		`<g fill="#fff" text-anchor="middle" font-family="Verdana,Geneva,DejaVu Sans,sans-serif" font-size="11">`+
		`<text x="%.1f" y="15" fill="#010101" fill-opacity=".3">%s</text><text x="%.1f" y="14">%s</text>`+
		`<text x="%.1f" y="15" fill="#010101" fill-opacity=".3">%s</text><text x="%.1f" y="14">%s</text></g></svg>`,
		w, l, m, l, m, w, lw, lw, mw, color, w,
		float64(lw)/2, l, float64(lw)/2, l, float64(lw)+float64(mw)/2, m, float64(lw)+float64(mw)/2, m)
}

// textWidth guesses how wide s is in 11px Verdana, in pixels.
func textWidth(s string) int {
	w := 0.0
	for _, c := range s {
		switch {
		case strings.ContainsRune("il.,:;'|!", c):
			w += 3.5
		case strings.ContainsRune("fjrt ()[]-/", c):
			w += 4.5
		case strings.ContainsRune("mwMW", c):
			w += 10.5
		case c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
			w += 7.5
		default:
			w += 6.8
		}
	}
	return int(w + 0.5)
}
