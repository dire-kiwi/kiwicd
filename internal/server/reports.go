package server

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/dire-kiwi/kiwicd/internal/protocol"
)

// A finished job's report: what its steps wrote to $GITHUB_STEP_SUMMARY and
// its annotations (::error, ::warning and ::notice workflow commands, and why
// it failed). The agent collects them; they are stored once, when the job
// completes, and shown on the run's summary page.

// jobReport is one job's report as the API returns it.
type jobReport struct {
	JobID       int64                  `json:"job_id"`
	Summaries   []protocol.StepSummary `json:"summaries"`
	Annotations []protocol.Annotation  `json:"annotations"`
}

// maxFileBytes is the longest annotation file path kept.
const maxFileBytes = 1 << 10

// limitReport enforces the protocol's limits on what an agent sent: jobs can
// write anything, and an agent shares its machine with them unless they are
// isolated.
func limitReport(summaries []protocol.StepSummary, annotations []protocol.Annotation) ([]protocol.StepSummary, []protocol.Annotation) {
	sums := []protocol.StepSummary{}
	total := 0
	for _, s := range summaries {
		if len(s.Markdown) > protocol.MaxStepSummaryBytes || strings.TrimSpace(s.Markdown) == "" {
			continue
		}
		if len(sums) == protocol.MaxJobSummaries || total+len(s.Markdown) > protocol.MaxJobSummaryBytes {
			break
		}
		total += len(s.Markdown)
		sums = append(sums, s)
	}
	anns := []protocol.Annotation{}
	// Room for the annotations that explain a failure, on top of the
	// workflow commands'.
	for _, a := range annotations {
		if len(anns) == 2*protocol.MaxAnnotations {
			break
		}
		switch a.Level {
		case protocol.AnnotationError, protocol.AnnotationWarning, protocol.AnnotationNotice:
		default:
			a.Level = protocol.AnnotationNotice
		}
		a.Message = truncateText(a.Message, protocol.MaxAnnotationBytes)
		a.Title = truncateText(a.Title, protocol.MaxAnnotationBytes)
		a.File = truncateText(a.File, maxFileBytes)
		a.Line, a.EndLine, a.Col, a.EndCol = max(a.Line, 0), max(a.EndLine, 0), max(a.Col, 0), max(a.EndCol, 0)
		a.Step, a.LogLine = max(a.Step, 0), max(a.LogLine, 0)
		anns = append(anns, a)
	}
	return sums, anns
}

// truncateText cuts s to at most n bytes without splitting a character.
func truncateText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n - len("…")
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

// saveJobReport stores a job's report, within limits. A job with neither
// summaries nor annotations gets no row.
func (s *Server) saveJobReport(jobID int64, summaries []protocol.StepSummary, annotations []protocol.Annotation) error {
	sums, anns := limitReport(summaries, annotations)
	if len(sums) == 0 && len(anns) == 0 {
		return nil
	}
	_, err := s.db.Exec(`INSERT OR REPLACE INTO job_reports (job_id, summaries, annotations) VALUES (?, ?, ?)`,
		jobID, mustJSON(sums), mustJSON(anns))
	return err
}

// jobReport returns a job's report, empty if it has none.
func (s *Server) jobReport(jobID int64) (*jobReport, error) {
	rep := &jobReport{JobID: jobID, Summaries: []protocol.StepSummary{}, Annotations: []protocol.Annotation{}}
	var sums, anns string
	err := s.db.QueryRow(`SELECT summaries, annotations FROM job_reports WHERE job_id = ?`, jobID).Scan(&sums, &anns)
	if errors.Is(err, sql.ErrNoRows) {
		return rep, nil
	}
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(sums), &rep.Summaries)
	_ = json.Unmarshal([]byte(anns), &rep.Annotations)
	return rep, nil
}

// handleRunReports returns the reports of a run's jobs that have one, in the
// order the jobs were created.
func (s *Server) handleRunReports(w http.ResponseWriter, r *http.Request) {
	run, err := s.getRun(pathID(r))
	if err != nil {
		httpError(w, 404, "no such run")
		return
	}
	rows, err := s.db.Query(`SELECT r.job_id, r.summaries, r.annotations FROM job_reports r JOIN jobs j ON j.id = r.job_id
		WHERE j.run_id = ? ORDER BY j.id`, run.ID)
	if err != nil {
		httpError(w, 500, "%v", err)
		return
	}
	// Summaries can be large: read them all before writing anything, so a
	// slow client doesn't hold the database connection.
	out := []jobReport{}
	for rows.Next() {
		var rep jobReport
		var sums, anns string
		if err := rows.Scan(&rep.JobID, &sums, &anns); err != nil {
			rows.Close()
			httpError(w, 500, "%v", err)
			return
		}
		_ = json.Unmarshal([]byte(sums), &rep.Summaries)
		_ = json.Unmarshal([]byte(anns), &rep.Annotations)
		out = append(out, rep)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		httpError(w, 500, "%v", err)
		return
	}
	writeJSON(w, out)
}

// Check runs: GitHub's API for showing a run on a commit or pull request.
// checkRunOutput is the hook for reporting a job there: it turns the job's
// report into a check run's "output", within GitHub's limits.

// GitHub's limits on a check run's output.
const (
	checkSummaryChars  = 65535
	checkAnnotationMax = 50 // per request; more need further PATCHes
)

type checkRunOutput struct {
	Title       string               `json:"title"`
	Summary     string               `json:"summary"`
	Annotations []checkRunAnnotation `json:"annotations,omitempty"`
}

type checkRunAnnotation struct {
	Path            string `json:"path"`
	StartLine       int    `json:"start_line"`
	EndLine         int    `json:"end_line"`
	StartColumn     int    `json:"start_column,omitempty"`
	EndColumn       int    `json:"end_column,omitempty"`
	AnnotationLevel string `json:"annotation_level"` // notice, warning or failure
	Message         string `json:"message"`
	Title           string `json:"title,omitempty"`
}

// jobCheckRunOutput makes a job's check run output: its step summaries one
// after the other, and its annotations. GitHub needs a file and line for
// each annotation; like GitHub Actions, the ones without a file are put on
// .github, line 1.
func jobCheckRunOutput(job *Job, rep *jobReport) checkRunOutput {
	out := checkRunOutput{Title: job.Name}
	switch n := len(rep.Annotations); n {
	case 0:
	case 1:
		out.Title = job.Name + ": 1 annotation"
	default:
		out.Title = fmt.Sprintf("%s: %d annotations", job.Name, n)
	}
	var parts []string
	for _, s := range rep.Summaries {
		parts = append(parts, strings.TrimRight(s.Markdown, "\n"))
	}
	summary := strings.Join(parts, "\n\n")
	if utf8.RuneCountInString(summary) > checkSummaryChars {
		const note = "\n\n*The summary was cut to fit GitHub's limit; see the run in kiwicd for all of it.*"
		r := []rune(summary)
		summary = string(r[:checkSummaryChars-utf8.RuneCountInString(note)]) + note
	}
	out.Summary = summary
	for _, a := range rep.Annotations {
		if len(out.Annotations) == checkAnnotationMax {
			break
		}
		ca := checkRunAnnotation{Path: a.File, StartLine: a.Line, EndLine: a.EndLine, AnnotationLevel: a.Level, Message: a.Message, Title: a.Title}
		if ca.AnnotationLevel == protocol.AnnotationError {
			ca.AnnotationLevel = "failure"
		}
		if ca.Path == "" {
			ca.Path, ca.StartLine, ca.EndLine = ".github", 1, 1
		}
		if ca.StartLine == 0 {
			ca.StartLine = 1
		}
		if ca.EndLine < ca.StartLine {
			ca.EndLine = ca.StartLine
		}
		// Columns are only allowed within a line.
		if ca.StartLine == ca.EndLine && a.Col > 0 {
			ca.StartColumn, ca.EndColumn = a.Col, max(a.EndCol, a.Col)
		}
		out.Annotations = append(out.Annotations, ca)
	}
	return out
}
