package agent

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/dire-kiwi/kiwicd/internal/protocol"
	"github.com/sirupsen/logrus"
)

// report collects what a job reports besides its log: step summaries and
// annotations. Its fields belong to the reporter and its mutex.
type report struct {
	// Log lines queued so far per step, to link annotations to theirs.
	lines map[int]int
	// The first error line of each step and the job's last one, to say why
	// a step or the job failed.
	firstError map[int]logRef
	lastError  *logRef

	summaries    []protocol.StepSummary
	summaryBytes int
	annotations  []protocol.Annotation
	// Workflow commands' annotations left out over MaxAnnotations.
	dropped int
}

type logRef struct {
	step, line int
	text       string
}

func newReport() report {
	return report{lines: map[int]int{}, firstError: map[int]logRef{}}
}

// queueLine queues a log line and returns its number among its step's
// lines, which is how the UI links to it.
func (r *reporter) queueLine(step int, ts int64, text string) int {
	r.pending = append(r.pending, protocol.LogLine{Step: step, TS: ts, Text: text})
	r.lines[step]++
	return r.lines[step]
}

// handleCommand handles act's entries for the workflow commands that make
// annotations and for a step's summary. It reports whether it did.
func (r *reporter) handleCommand(e *logrus.Entry, step int) bool {
	command, _ := e.Data["command"].(string)
	switch command {
	case "summary":
		// act logs the whole summary; it is shown on the run's page instead.
		content, _ := e.Data["content"].(string)
		r.addSummary(step, r.mask(e, content))
		return true
	case protocol.AnnotationError, protocol.AnnotationWarning, protocol.AnnotationNotice:
	default:
		return false
	}
	// act masks only the message, not the fields parsed from it.
	kv, _ := e.Data["kvPairs"].(map[string]string)
	arg, _ := e.Data["arg"].(string)
	arg = strings.TrimRight(r.mask(e, arg), "\r\n")
	// Like GitHub's log, show the message, not the command.
	first := 0
	for _, line := range strings.Split(arg, "\n") {
		n := r.queueLine(step, e.Time.UnixMilli(), "##["+command+"]"+line)
		if first == 0 {
			first = n
		}
	}
	a := protocol.Annotation{
		Level:   command,
		Message: arg,
		Title:   r.mask(e, kv["title"]),
		File:    r.mask(e, kv["file"]),
		Line:    atoi(kv["line"]),
		EndLine: atoi(kv["endLine"]),
		Col:     atoi(kv["col"]),
		EndCol:  atoi(kv["endColumn"]),
		Step:    step,
		LogLine: first,
	}
	if a.EndLine == 0 {
		a.EndLine = a.Line
	}
	r.annotate(a)
	return true
}

func atoi(s string) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// annotate adds an annotation from a workflow command, up to MaxAnnotations.
func (r *reporter) annotate(a protocol.Annotation) {
	if len(r.annotations) >= protocol.MaxAnnotations {
		r.dropped++
		return
	}
	a.Message = truncateUTF8(a.Message, protocol.MaxAnnotationBytes)
	a.Title = truncateUTF8(a.Title, protocol.MaxAnnotationBytes)
	r.annotations = append(r.annotations, a)
}

// addSummary keeps a step's summary. As on GitHub, one over 1 MiB is
// dropped with a warning rather than cut.
func (r *reporter) addSummary(step int, markdown string) {
	if strings.TrimSpace(markdown) == "" {
		return
	}
	var warning string
	switch {
	case len(markdown) > protocol.MaxStepSummaryBytes:
		warning = fmt.Sprintf("$GITHUB_STEP_SUMMARY upload aborted, supports content up to a size of %dk, got %dk.",
			protocol.MaxStepSummaryBytes>>10, len(markdown)>>10)
	case len(r.summaries) >= protocol.MaxJobSummaries || r.summaryBytes+len(markdown) > protocol.MaxJobSummaryBytes:
		warning = fmt.Sprintf("$GITHUB_STEP_SUMMARY upload aborted: a job's summaries are limited to %d steps and %d MiB in all.",
			protocol.MaxJobSummaries, protocol.MaxJobSummaryBytes>>20)
	}
	if warning != "" {
		n := r.queueLine(step, nowMS(), "##[warning]"+warning)
		r.annotate(protocol.Annotation{Level: protocol.AnnotationWarning, Message: warning, Step: step, LogLine: n})
		return
	}
	r.summaries = append(r.summaries, protocol.StepSummary{Step: step, Markdown: markdown})
	r.summaryBytes += len(markdown)
}

// trackError notes an error line (not a workflow command's), to explain a
// failure with it later.
func (r *reporter) trackError(step, line int, text string) {
	if _, ok := r.firstError[step]; !ok {
		r.firstError[step] = logRef{step: step, line: line, text: text}
	}
	r.lastError = &logRef{step: step, line: line, text: text}
}

var exitCodeError = regexp.MustCompile(`^exitcode '(\d+)': failure$`)

// failureMessage turns act's error for a step into GitHub's words.
func failureMessage(text string) string {
	if m := exitCodeError.FindStringSubmatch(text); m != nil {
		return "Process completed with exit code " + m[1] + "."
	}
	return truncateUTF8(text, protocol.MaxAnnotationBytes)
}

// finishReport adds the annotations that explain a failed or cancelled job:
// each failed step's first error, as GitHub annotates "Process completed
// with exit code 1.", or else the job's last error.
func (r *reporter) finishReport(result string) {
	if r.dropped > 0 {
		r.annotations = append(r.annotations, protocol.Annotation{Level: protocol.AnnotationNotice,
			Message: fmt.Sprintf("%d more annotations were left out, over the limit of %d; they are in the log.", r.dropped, protocol.MaxAnnotations)})
	}
	if result != protocol.ResultFailure && result != protocol.ResultCancelled {
		return
	}
	explained := false
	for i, st := range r.steps {
		if st.Conclusion != protocol.ResultFailure && st.Conclusion != protocol.ResultCancelled {
			continue
		}
		if ref, ok := r.firstError[i]; ok {
			r.annotations = append(r.annotations, protocol.Annotation{Level: protocol.AnnotationError,
				Message: failureMessage(ref.text), Step: i, LogLine: ref.line})
			explained = true
		}
	}
	if explained {
		return
	}
	a := protocol.Annotation{Level: protocol.AnnotationError, Message: "The job failed."}
	if result == protocol.ResultCancelled {
		a.Message = "The job was cancelled."
	}
	if ref := r.lastError; ref != nil {
		a.Message, a.Step, a.LogLine = failureMessage(ref.text), ref.step, ref.line
	}
	r.annotations = append(r.annotations, a)
}

// truncateUTF8 cuts s to at most n bytes without splitting a character.
func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n - len("…")
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}
