package server

import (
	"archive/zip"
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/dire-kiwi/kiwicd/internal/protocol"
)

const (
	// logPageLines is the most log lines one read returns.
	logPageLines = 5000
	// logPageBytes stops a page early once its text reaches this size, so a
	// page stays small even when its lines are long.
	logPageBytes = 4 << 20
	// maxLogLineBytes is the longest log line stored; longer ones are cut.
	maxLogLineBytes = 64 << 10

	defaultMaxLogLines = 1_000_000
	defaultMaxLogBytes = 100 << 20
)

type logLine struct {
	Seq  int64  `json:"seq"`
	Step int    `json:"step"`
	TS   int64  `json:"ts"`
	Text string `json:"text"`
}

// readLogs calls fn, in order, for up to limit lines of a job's log that come
// after seq after (limit <= 0 or above logPageLines means logPageLines). It
// stops early after about logPageBytes of text and reports whether more lines
// follow; read the next page with after set to the last seq fn saw. Every log
// read goes through here so none of them is unbounded. Archived logs are read
// from their file (see logarchive.go). fn runs while the rows are open,
// holding the only database connection: it must not touch the database or
// block.
func (s *Server) readLogs(jobID, after int64, limit int, fn func(logLine)) (more bool, err error) {
	if limit <= 0 || limit > logPageLines {
		limit = logPageLines
	}
	// In one transaction, so the rows can't be half deleted by archiving
	// between the two queries.
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var archive string
	if err := tx.QueryRow(`SELECT log_archive FROM jobs WHERE id = ?`, jobID).Scan(&archive); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	if archive != "" {
		_ = tx.Rollback()
		return s.readLogArchive(jobID, archive, after, limit, fn)
	}
	rows, err := tx.Query(`SELECT seq, step, ts, text FROM log_lines WHERE job_id = ? AND seq > ? ORDER BY seq LIMIT ?`, jobID, after, limit+1)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	n, size := 0, 0
	for rows.Next() {
		if n == limit || size >= logPageBytes {
			return true, nil
		}
		var l logLine
		if err := rows.Scan(&l.Seq, &l.Step, &l.TS, &l.Text); err != nil {
			return false, err
		}
		fn(l)
		n++
		size += len(l.Text)
	}
	return false, rows.Err()
}

// logPage returns one page of a job's log (see readLogs).
func (s *Server) logPage(jobID, after int64, limit int) ([]logLine, bool, error) {
	out := []logLine{}
	more, err := s.readLogs(jobID, after, limit, func(l logLine) { out = append(out, l) })
	return out, more, err
}

// handleJobLogs returns one page of a job's log as JSON, starting after
// ?after= and at most ?limit= lines long. When more lines follow, a Link
// header (rel="next") points at the next page. ?format=text streams the
// whole log as plain text instead, and with ?download=1 as a file to save.
func (s *Server) handleJobLogs(w http.ResponseWriter, r *http.Request) {
	id := pathID(r)
	q := r.URL.Query()
	after, _ := strconv.ParseInt(q.Get("after"), 10, 64)
	if q.Get("format") == "text" {
		var name string
		if q.Get("download") != "" {
			job, err := s.getJob(id)
			if err != nil {
				httpError(w, 404, "no such job")
				return
			}
			run, err := s.getRun(job.RunID)
			if err != nil {
				httpError(w, 404, "no such run")
				return
			}
			name = fmt.Sprintf("%s-%s.log", runFileName(run), fileNamePart(job.Name))
		}
		started := false
		err := s.copyTextLogs(w, id, after, func() {
			started = true
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			if name != "" {
				w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
			}
		})
		if err != nil && !started {
			httpError(w, 500, "%v", err)
		}
		return
	}
	limit, _ := strconv.Atoi(q.Get("limit"))
	lines, more, err := s.logPage(id, after, limit)
	if err != nil {
		httpError(w, 500, "%v", err)
		return
	}
	if more {
		q.Set("after", strconv.FormatInt(lines[len(lines)-1].Seq, 10))
		next := url.URL{Path: r.URL.Path, RawQuery: q.Encode()}
		w.Header().Set("Link", fmt.Sprintf(`<%s>; rel="next"`, next.String()))
	}
	writeJSON(w, lines)
}

// copyTextLogs writes a job's log after seq after to w as text, a page at a
// time, calling start before writing anything. Each page is formatted into
// a buffer and written after its rows are closed, so a slow client doesn't
// hold the database connection.
func (s *Server) copyTextLogs(w io.Writer, jobID, after int64, start func()) error {
	var buf bytes.Buffer
	for page := 0; ; page++ {
		buf.Reset()
		more, err := s.readLogs(jobID, after, 0, func(l logLine) {
			buf.WriteString(time.UnixMilli(l.TS).UTC().Format(time.RFC3339))
			buf.WriteByte(' ')
			buf.WriteString(l.Text)
			buf.WriteByte('\n')
			after = l.Seq
		})
		if err != nil {
			return err
		}
		if page == 0 && start != nil {
			start()
		}
		if _, err := w.Write(buf.Bytes()); err != nil || !more {
			return err
		}
	}
}

// handleRunLogs sends the logs of all of a run's jobs as a zip file, one
// text file per job.
func (s *Server) handleRunLogs(w http.ResponseWriter, r *http.Request) {
	run, err := s.getRun(pathID(r))
	if err != nil {
		httpError(w, 404, "no such run")
		return
	}
	jobs, err := s.runJobs(run.ID)
	if err != nil {
		httpError(w, 500, "%v", err)
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": runFileName(run) + "-logs.zip"}))
	zw := zip.NewWriter(w)
	for i, j := range jobs {
		modified := time.UnixMilli(max(j.FinishedAt, j.StartedAt, j.CreatedAt))
		f, err := zw.CreateHeader(&zip.FileHeader{Name: fmt.Sprintf("%d_%s.txt", i+1, fileNamePart(j.Name)), Method: zip.Deflate, Modified: modified})
		if err == nil {
			err = s.copyTextLogs(f, j.ID, 0, nil)
		}
		if err != nil {
			// The response has begun: leave the zip unfinished, so it
			// doesn't look complete.
			s.log.Warn("run logs zip", "run", run.ID, "job", j.ID, "err", err)
			return
		}
	}
	_ = zw.Close()
}

// runFileName names files downloaded from a run: owner-name-run-12, with
// -attempt-2 for re-runs.
func runFileName(run *Run) string {
	name := fmt.Sprintf("%s-run-%d", fileNamePart(strings.ReplaceAll(run.Repo, "/", "-")), run.Number)
	if run.Attempt > 1 {
		name += fmt.Sprintf("-attempt-%d", run.Attempt)
	}
	return name
}

// fileNamePart makes s safe in a file name: path separators, characters
// Windows refuses and control characters become _.
func fileNamePart(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || strings.ContainsRune(`/\:*?"<>|`, r) {
			return '_'
		}
		return r
	}, strings.TrimSpace(s))
	if s == "" || s == "." || s == ".." {
		return "_"
	}
	return s
}

// truncateLogLine cuts text to maxLogLineBytes, on a UTF-8 boundary, and
// marks the cut.
func truncateLogLine(text string) string {
	if len(text) <= maxLogLineBytes {
		return text
	}
	cut := maxLogLineBytes
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut] + fmt.Sprintf(" … [line truncated: %d bytes]", len(text))
}

// logCap returns the notice to store when a job whose log has lines lines
// and size bytes can't take a line of n more bytes, or "" if it can.
func (s *Server) logCap(lines, size int64, n int) string {
	if s.cfg.MaxLogLines > 0 && lines >= s.cfg.MaxLogLines {
		return fmt.Sprintf("##[warning]The log was truncated: it reached the limit of %d lines. Later output is not stored.", s.cfg.MaxLogLines)
	}
	if s.cfg.MaxLogBytes > 0 && size+int64(n) > s.cfg.MaxLogBytes {
		return fmt.Sprintf("##[warning]The log was truncated: it reached the limit of %d bytes. Later output is not stored.", s.cfg.MaxLogBytes)
	}
	return ""
}

// appendLogs stores log lines and publishes them to live viewers. Long lines
// are cut; once a job's log reaches its line or byte limit, one notice is
// stored and later lines are dropped (the job carries on). Lines for a job
// that has already completed (e.g. from an agent after the job was reaped)
// are dropped too: its log is final and may have been archived.
func (s *Server) appendLogs(jobID int64, lines []protocol.LogLine) error {
	if len(lines) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var seq, size int64
	var truncated bool
	var status string
	if err := tx.QueryRow(`SELECT log_seq, log_bytes, log_truncated, status FROM jobs WHERE id = ?`, jobID).Scan(&seq, &size, &truncated, &status); err != nil {
		return err
	}
	if truncated || status == protocol.StatusCompleted {
		return nil
	}
	out := make([]logLine, 0, len(lines))
	stmt, err := tx.Prepare(`INSERT INTO log_lines (job_id, seq, step, ts, text) VALUES (?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, l := range lines {
		text := truncateLogLine(l.Text)
		if notice := s.logCap(seq, size, len(text)); notice != "" {
			text, truncated = notice, true
		} else {
			size += int64(len(text))
		}
		seq++
		if _, err := stmt.Exec(jobID, seq, l.Step, l.TS, text); err != nil {
			return err
		}
		out = append(out, logLine{Seq: seq, Step: l.Step, TS: l.TS, Text: text})
		if truncated {
			break
		}
	}
	if _, err := tx.Exec(`UPDATE jobs SET log_seq = ?, log_bytes = ?, log_truncated = ? WHERE id = ?`, seq, size, truncated, jobID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.hub.publish(fmt.Sprintf("job:%d", jobID), "logs", out)
	return nil
}
