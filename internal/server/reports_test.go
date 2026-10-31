package server

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/dire-kiwi/kiwicd/internal/protocol"
)

func TestLimitReport(t *testing.T) {
	var sums []protocol.StepSummary
	sums = append(sums, protocol.StepSummary{Step: 1, Markdown: strings.Repeat("x", protocol.MaxStepSummaryBytes+1)})
	sums = append(sums, protocol.StepSummary{Step: 1, Markdown: " \n"})
	for i := 0; i < protocol.MaxJobSummaries+3; i++ {
		sums = append(sums, protocol.StepSummary{Step: 2, Markdown: fmt.Sprint(i)})
	}
	var anns []protocol.Annotation
	anns = append(anns, protocol.Annotation{Level: "fatal", Message: strings.Repeat("é", protocol.MaxAnnotationBytes), Line: -3, File: strings.Repeat("f", 5000)})
	for i := 0; i < 3*protocol.MaxAnnotations; i++ {
		anns = append(anns, protocol.Annotation{Level: "warning", Message: "w"})
	}
	gotSums, gotAnns := limitReport(sums, anns)
	if len(gotSums) != protocol.MaxJobSummaries || gotSums[0].Markdown != "0" {
		t.Errorf("summaries: %d, first %+v", len(gotSums), gotSums[0])
	}
	if len(gotAnns) != 2*protocol.MaxAnnotations {
		t.Errorf("%d annotations", len(gotAnns))
	}
	a := gotAnns[0]
	if a.Level != "notice" || a.Line != 0 || len(a.Message) > protocol.MaxAnnotationBytes || !utf8.ValidString(a.Message) || len(a.File) > maxFileBytes {
		t.Errorf("first annotation not limited: level %q line %d message %d bytes file %d bytes", a.Level, a.Line, len(a.Message), len(a.File))
	}

	big := protocol.StepSummary{Step: 1, Markdown: strings.Repeat("y", protocol.MaxStepSummaryBytes)}
	var many []protocol.StepSummary
	for range protocol.MaxJobSummaries {
		many = append(many, big)
	}
	if got, _ := limitReport(many, nil); len(got) != protocol.MaxJobSummaryBytes/protocol.MaxStepSummaryBytes {
		t.Errorf("kept %d MiB summaries", len(got))
	}
}

func TestReportCheckOutput(t *testing.T) {
	rep := &jobReport{
		Summaries: []protocol.StepSummary{{Step: 1, Markdown: "# One\n"}, {Step: 3, Markdown: "two"}},
		Annotations: []protocol.Annotation{
			{Level: "error", Message: "bad", File: "main.go", Line: 3, EndLine: 3, Col: 5},
			{Level: "warning", Message: "spans", File: "a.go", Line: 1, EndLine: 4, Col: 2},
			{Level: "error", Message: "Process completed with exit code 1.", Step: 2, LogLine: 7},
		},
	}
	out := &checkOutput{Title: "Failed", Summary: "**build** failed."}
	reportCheckOutput(out, rep)
	if out.Summary != "**build** failed." || out.Text != "# One\n\ntwo" {
		t.Errorf("summary %q text %q", out.Summary, out.Text)
	}
	want := []checkAnnotation{
		{Path: "main.go", StartLine: 3, EndLine: 3, StartColumn: 5, EndColumn: 5, AnnotationLevel: "failure", Message: "bad"},
		{Path: "a.go", StartLine: 1, EndLine: 4, AnnotationLevel: "warning", Message: "spans"},
		{Path: ".github", StartLine: 1, EndLine: 1, AnnotationLevel: "failure", Message: "Process completed with exit code 1."},
	}
	if fmt.Sprint(out.Annotations) != fmt.Sprint(want) {
		t.Errorf("annotations:\n%+v\nwant\n%+v", out.Annotations, want)
	}

	rep = &jobReport{Summaries: []protocol.StepSummary{{Markdown: strings.Repeat("ü", maxSummary)}}}
	for range 2 * maxCheckAnnotations {
		rep.Annotations = append(rep.Annotations, protocol.Annotation{Level: "notice", Message: "n"})
	}
	out = &checkOutput{}
	reportCheckOutput(out, rep)
	if len(out.Text) > maxSummary+200 || !utf8.ValidString(out.Text) || !strings.Contains(out.Text, "GitHub's limit") {
		t.Errorf("long summary: %d bytes", len(out.Text))
	}
	if len(out.Annotations) != maxCheckAnnotations {
		t.Errorf("%d annotations", len(out.Annotations))
	}
}
