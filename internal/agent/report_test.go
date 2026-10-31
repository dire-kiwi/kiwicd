package agent

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/dire-kiwi/kiwicd/internal/protocol"
	"github.com/nektos/act/pkg/model"
	"github.com/sirupsen/logrus"
)

// entry makes an act log entry for the step with ID step.
func entry(level logrus.Level, step, msg string, fields logrus.Fields) *logrus.Entry {
	data := logrus.Fields{"stage": "Main", "stepID": []string{step}, "step": step}
	for k, v := range fields {
		data[k] = v
	}
	return &logrus.Entry{Level: level, Message: msg, Data: data, Time: time.Now()}
}

func testReporter() *reporter {
	job := &model.Job{Steps: []*model.Step{{ID: "build", Run: "make"}, {ID: "after", Run: "true"}}}
	return newReporter(nil, 1, job, map[string]string{"TOKEN": "s3cret"}, nil)
}

func TestReporterAnnotationsAndSummaries(t *testing.T) {
	r := testReporter()
	r.handle(entry(logrus.InfoLevel, "build", "⭐ Run Main make", nil))
	r.handle(entry(logrus.InfoLevel, "build", "compiling\n", logrus.Fields{"raw_output": true}))
	// act masks the message only; kvPairs and arg arrive as the step wrote them.
	r.handle(entry(logrus.ErrorLevel, "build", "  ❗  ::error file=main.go,line=3,title=Build s3cret::bad s3cret", logrus.Fields{
		"command": "error", "kvPairs": map[string]string{"file": "main.go", "line": "3", "col": "5", "title": "Build s3cret"}, "arg": "bad s3cret\nsecond line"}))
	r.handle(entry(logrus.WarnLevel, "build", "  🚧  ::warning::careful", logrus.Fields{
		"command": "warning", "kvPairs": map[string]string{}, "arg": "careful"}))
	r.handle(entry(logrus.InfoLevel, "build", "  ⚙  Summary - # Report", logrus.Fields{"command": "summary", "content": "# Report\nusing s3cret\n"}))
	r.handle(entry(logrus.InfoLevel, "build", "  ❌  Failure - Main make", logrus.Fields{"stepResult": "failure"}))
	r.handle(entry(logrus.ErrorLevel, "build", "exitcode '2': failure", nil))
	r.handle(entry(logrus.ErrorLevel, "build", "another error", nil))
	done := r.finish(protocol.ResultFailure)

	var texts []string
	for _, l := range done.Logs {
		if l.Step == 1 {
			texts = append(texts, l.Text)
		}
	}
	wantLog := []string{"##[act]⭐ Run Main make", "compiling", "##[error]bad ***", "##[error]second line", "##[warning]careful",
		"##[act]❌  Failure - Main make", "##[error]exitcode '2': failure", "##[error]another error"}
	if strings.Join(texts, "\n") != strings.Join(wantLog, "\n") {
		t.Errorf("step log:\n%s\nwant:\n%s", strings.Join(texts, "\n"), strings.Join(wantLog, "\n"))
	}

	want := []protocol.Annotation{
		{Level: "error", Message: "bad ***\nsecond line", Title: "Build ***", File: "main.go", Line: 3, EndLine: 3, Col: 5, Step: 1, LogLine: 3},
		{Level: "warning", Message: "careful", Step: 1, LogLine: 5},
		{Level: "error", Message: "Process completed with exit code 2.", Step: 1, LogLine: 7},
	}
	if fmt.Sprint(done.Annotations) != fmt.Sprint(want) {
		t.Errorf("annotations:\n%+v\nwant:\n%+v", done.Annotations, want)
	}
	if len(done.Summaries) != 1 || done.Summaries[0] != (protocol.StepSummary{Step: 1, Markdown: "# Report\nusing ***\n"}) {
		t.Errorf("summaries: %+v", done.Summaries)
	}
}

func TestFailureMessage(t *testing.T) {
	for in, want := range map[string]string{
		"exitcode '2': failure": "Process completed with exit code 2.",
		"exit status 3":         "Process completed with exit code 3.",
		"Job 'x' failed":        "Job 'x' failed",
	} {
		if got := failureMessage(in); got != want {
			t.Errorf("failureMessage(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestReporterExplainsFailureWithoutStep(t *testing.T) {
	r := testReporter()
	r.logf(0, "##[error]Downloading source failed: boom")
	done := r.finish(protocol.ResultFailure)
	want := []protocol.Annotation{{Level: "error", Message: "Downloading source failed: boom", Step: 0, LogLine: 1}}
	if fmt.Sprint(done.Annotations) != fmt.Sprint(want) {
		t.Errorf("annotations: %+v", done.Annotations)
	}

	r = testReporter()
	if done := r.finish(protocol.ResultSuccess); len(done.Annotations) != 0 {
		t.Errorf("a successful job has annotations: %+v", done.Annotations)
	}
	r = testReporter()
	done = r.finish(protocol.ResultCancelled)
	if len(done.Annotations) != 1 || done.Annotations[0].Message != "The job was cancelled." {
		t.Errorf("cancelled: %+v", done.Annotations)
	}
}

func TestReporterLimits(t *testing.T) {
	r := testReporter()
	r.handle(entry(logrus.InfoLevel, "build", "⭐ Run Main make", nil))
	for i := 0; i < protocol.MaxAnnotations+5; i++ {
		r.handle(entry(logrus.InfoLevel, "build", "::notice::n", logrus.Fields{"command": "notice", "arg": fmt.Sprint(i)}))
	}
	big := strings.Repeat("x", protocol.MaxStepSummaryBytes+1)
	r.handle(entry(logrus.InfoLevel, "build", "Summary", logrus.Fields{"command": "summary", "content": big}))
	for i := 0; i < protocol.MaxJobSummaries+1; i++ {
		r.handle(entry(logrus.InfoLevel, "build", "Summary", logrus.Fields{"command": "summary", "content": "ok"}))
	}
	long := strings.Repeat("é", protocol.MaxAnnotationBytes)
	r.annotations = r.annotations[:0]
	r.handle(entry(logrus.InfoLevel, "build", "::notice::long", logrus.Fields{"command": "notice", "arg": long}))
	done := r.finish(protocol.ResultSuccess)

	if len(done.Summaries) != protocol.MaxJobSummaries {
		t.Errorf("%d summaries, want %d", len(done.Summaries), protocol.MaxJobSummaries)
	}
	var warnings int
	for _, l := range done.Logs {
		if strings.HasPrefix(l.Text, "##[warning]$GITHUB_STEP_SUMMARY upload aborted") {
			warnings++
		}
	}
	if warnings != 2 {
		t.Errorf("%d summary warnings in the log, want 2 (too big, too many)", warnings)
	}
	first := done.Annotations[0].Message
	if len(first) > protocol.MaxAnnotationBytes || !strings.HasSuffix(first, "…") || !strings.HasPrefix(first, "éé") {
		t.Errorf("long message kept as %d bytes: %q…", len(first), first[:10])
	}
	last := done.Annotations[len(done.Annotations)-1]
	if last.Level != "notice" || !strings.Contains(last.Message, "more annotations were left out") {
		t.Errorf("no note about dropped annotations: %+v", last)
	}
}
