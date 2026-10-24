package agent

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dire-kiwi/kiwicd/internal/protocol"
	"github.com/nektos/act/pkg/model"
	"github.com/nektos/act/pkg/runner"
	"github.com/sirupsen/logrus"
)

// reporter turns act's log entries into step states and log lines, and
// ships them to the server. act tags entries with stepID/stage/stepResult;
// raw_output entries are the commands' own output.
type reporter struct {
	c      *client
	jobID  int64
	cancel context.CancelFunc

	mu        sync.Mutex
	steps     []protocol.StepState
	stepIndex map[string]int // act step ID -> index in steps
	postIndex map[string]int
	complete  int // index of "Complete job", or -1
	lastStep  int
	pending   []protocol.LogLine
	dirty     bool
	cancelled bool
	// step that was running when the run was cancelled
	cancelledStep int
	secrets       []string
	named         map[int]bool // steps with an explicit name: (may contain expressions)
}

func newReporter(c *client, jobID int64, job *model.Job, secrets map[string]string, cancel context.CancelFunc) *reporter {
	r := &reporter{c: c, jobID: jobID, cancel: cancel, stepIndex: map[string]int{}, postIndex: map[string]int{}, complete: -1, named: map[int]bool{}}
	for _, v := range secrets {
		r.secrets = append(r.secrets, maskValues(v)...)
	}
	sortLongestFirst(r.secrets)
	r.steps = append(r.steps, protocol.StepState{Name: "Set up job", Status: protocol.StatusInProgress, StartedAt: nowMS()})
	for i, st := range job.Steps {
		if st == nil {
			continue
		}
		id := st.ID
		if id == "" {
			id = strconv.Itoa(i)
		}
		r.stepIndex[id] = len(r.steps)
		r.named[len(r.steps)] = st.Name != ""
		r.steps = append(r.steps, protocol.StepState{Name: stepTitle(st), Status: protocol.StatusPending})
	}
	r.dirty = true
	return r
}

func nowMS() int64 { return time.Now().UnixMilli() }

// stepTitle names a step the way GitHub does before it runs.
func stepTitle(st *model.Step) string {
	switch {
	case st.Name != "":
		return st.Name
	case st.Uses != "":
		return "Run " + st.Uses
	case st.Run != "":
		for _, line := range strings.Split(st.Run, "\n") {
			if line = strings.TrimSpace(line); line != "" {
				return "Run " + line
			}
		}
	}
	return st.String()
}

// maskValues returns what to mask for a secret: the value and, since act
// logs output a line at a time, each line of a multi-line value (an SSH key)
// as GitHub's runner does. Very short lines, such as a lone "}", are left.
func maskValues(v string) []string {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	out := []string{v}
	if strings.ContainsAny(v, "\r\n") {
		for _, line := range strings.FieldsFunc(v, func(c rune) bool { return c == '\n' || c == '\r' }) {
			if line = strings.TrimSpace(line); len(line) >= 4 {
				out = append(out, line)
			}
		}
	}
	return out
}

// sortLongestFirst orders masks so that a secret that starts with another
// one is replaced whole, not as "***" and the rest of it.
func sortLongestFirst(masks []string) {
	sort.SliceStable(masks, func(i, j int) bool { return len(masks[i]) > len(masks[j]) })
}

// maskSecrets hides the job's secrets in s.
func (r *reporter) maskSecrets(s string) string {
	for _, v := range r.secrets {
		s = strings.ReplaceAll(s, v, "***")
	}
	return s
}

// mask hides secrets and ::add-mask:: values in text act did not mask itself.
func (r *reporter) mask(e *logrus.Entry, s string) string {
	s = r.maskSecrets(s)
	if e.Context != nil {
		if masks := runner.Masks(e.Context); masks != nil {
			var extra []string
			for _, v := range *masks {
				extra = append(extra, maskValues(v)...)
			}
			sortLongestFirst(extra)
			for _, v := range extra {
				s = strings.ReplaceAll(s, v, "***")
			}
		}
	}
	return s
}

// logf adds an agent message to a step (0 = Set up job).
func (r *reporter) logf(step int, format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	text := r.maskSecrets(fmt.Sprintf(format, args...))
	for _, line := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		r.pending = append(r.pending, protocol.LogLine{Step: step, TS: nowMS(), Text: line})
	}
}

func (r *reporter) completeIndex() int {
	if r.complete < 0 {
		r.complete = len(r.steps)
		r.steps = append(r.steps, protocol.StepState{Name: "Complete job", Status: protocol.StatusInProgress, StartedAt: nowMS()})
		r.dirty = true
	}
	return r.complete
}

func (r *reporter) finishStep(i int, conclusion string) {
	st := &r.steps[i]
	if st.Status == protocol.StatusCompleted {
		return
	}
	if st.StartedAt == 0 {
		st.StartedAt = nowMS()
	}
	st.Status = protocol.StatusCompleted
	if r.cancelled && i == r.cancelledStep && conclusion == protocol.ResultFailure {
		conclusion = protocol.ResultCancelled
	}
	st.Conclusion = conclusion
	st.FinishedAt = nowMS()
	r.dirty = true
}

func (r *reporter) startStep(i int) {
	st := &r.steps[i]
	if st.Status == protocol.StatusPending {
		st.Status = protocol.StatusInProgress
		st.StartedAt = nowMS()
		r.dirty = true
	}
	// Starting a step ends set-up.
	if i > 0 && r.steps[0].Status != protocol.StatusCompleted {
		r.finishStep(0, protocol.ResultSuccess)
	}
}

// handle is called (via the formatter) for every act log entry, after
// act has masked secrets in the message.
func (r *reporter) handle(e *logrus.Entry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	stage, _ := e.Data["stage"].(string)
	stepIDs, _ := e.Data["stepID"].([]string)
	stepName, _ := e.Data["step"].(string)
	raw := e.Data["raw_output"] == true
	result, hasResult := e.Data["stepResult"]
	resultStr := ""
	if hasResult {
		resultStr = fmt.Sprint(result)
	}

	idx := -1
	if len(stepIDs) > 0 {
		if stage == "Post" {
			key := stepIDs[0]
			i, ok := r.postIndex[key]
			if !ok {
				i = len(r.steps)
				name := key
				if i, ok := r.stepIndex[key]; ok {
					name = strings.TrimPrefix(r.steps[i].Name, "Run ")
				}
				r.steps = append(r.steps, protocol.StepState{Name: "Post " + name, Status: protocol.StatusPending})
				r.postIndex[key] = i
				r.dirty = true
			}
			idx = i
		} else if i, ok := r.stepIndex[stepIDs[0]]; ok {
			idx = i
			if stage == "Main" && stepName != "" && len(stepIDs) == 1 && r.named[i] {
				if name := r.mask(e, stepName); r.steps[i].Name != name {
					r.steps[i].Name = name
					r.dirty = true
				}
			}
		}
	}
	if idx < 0 {
		switch {
		case r.complete >= 0 || strings.Contains(e.Message, "Complete job"):
			idx = r.completeIndex()
		default:
			idx = r.lastStep
		}
	}

	// Step state transitions. Results from composite inner steps
	// (len(stepIDs) > 1) don't finish the outer step.
	if len(stepIDs) <= 1 {
		if hasResult {
			switch {
			case len(stepIDs) == 0 && idx == 0:
				r.finishStep(0, resultStr)
			case len(stepIDs) == 0 && idx == r.complete:
				r.finishStep(idx, resultStr)
			case len(stepIDs) == 1 && (stage == "Main" || stage == "Post"):
				r.finishStep(idx, resultStr)
			}
		} else if len(stepIDs) == 1 && (stage == "Main" || stage == "Post") {
			r.startStep(idx)
		}
	}
	if idx > r.lastStep && idx != r.complete {
		r.lastStep = idx
	}

	if e.Level == logrus.DebugLevel {
		return
	}
	msg := e.Message
	if raw {
		msg = strings.TrimRight(msg, "\r\n")
	} else {
		msg = strings.TrimRight(strings.TrimSpace(msg), "\r\n")
		if msg == "" {
			return
		}
		switch e.Level {
		case logrus.ErrorLevel, logrus.FatalLevel, logrus.PanicLevel:
			msg = "##[error]" + msg
		case logrus.WarnLevel:
			msg = "##[warning]" + msg
		default:
			msg = "##[act]" + msg
		}
	}
	for _, line := range strings.Split(msg, "\n") {
		r.pending = append(r.pending, protocol.LogLine{Step: idx, TS: e.Time.UnixMilli(), Text: line})
	}
}

// take returns the pending logs (at most max) and, if changed, the steps.
func (r *reporter) take(max int) ([]protocol.LogLine, []protocol.StepState) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := len(r.pending)
	if n > max {
		n = max
	}
	logs := append([]protocol.LogLine(nil), r.pending[:n]...)
	r.pending = r.pending[n:]
	var steps []protocol.StepState
	if r.dirty {
		steps = append([]protocol.StepState(nil), r.steps...)
		r.dirty = false
	}
	return logs, steps
}

func (r *reporter) markDirty() {
	r.mu.Lock()
	r.dirty = true
	r.mu.Unlock()
}

// flushLoop ships updates every second until ctx is done.
func (r *reporter) flushLoop(ctx context.Context) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	lastSent := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		logs, steps := r.take(2000)
		if len(logs) == 0 && steps == nil && time.Since(lastSent) < 10*time.Second {
			continue
		}
		resp, err := r.c.update(ctx, r.jobID, protocol.JobUpdate{Logs: logs, Steps: steps})
		if err != nil {
			// Put the batch back; it is retried next tick.
			r.mu.Lock()
			r.pending = append(logs, r.pending...)
			if steps != nil {
				r.dirty = true
			}
			r.mu.Unlock()
			continue
		}
		lastSent = time.Now()
		if resp.Cancelled && !r.cancelled {
			r.mu.Lock()
			r.cancelled = true
			r.cancelledStep = r.lastStep
			r.pending = append(r.pending, protocol.LogLine{Step: r.lastStep, TS: nowMS(), Text: "##[error]The run was cancelled."})
			r.mu.Unlock()
			r.cancel()
		}
	}
}

// finish closes open steps and returns everything left to send.
func (r *reporter) finish(result string) ([]protocol.LogLine, []protocol.StepState) {
	r.mu.Lock()
	idx := r.completeIndex()
	for i := range r.steps {
		st := &r.steps[i]
		switch st.Status {
		case protocol.StatusInProgress:
			c := result
			if i == idx {
				c = protocol.ResultSuccess
			}
			r.finishStep(i, c)
		case protocol.StatusPending:
			r.finishStep(i, protocol.ResultSkipped)
			st.StartedAt, st.FinishedAt = 0, 0
		}
	}
	logs := r.pending
	r.pending = nil
	var steps []protocol.StepState
	hasLogs := map[int]bool{}
	for _, l := range logs {
		hasLogs[l.Step] = true
	}
	for i, st := range r.steps {
		// Hide post steps that did nothing, but keep indexes stable.
		if strings.HasPrefix(st.Name, "Post ") && st.Conclusion == protocol.ResultSkipped && !hasLogs[i] {
			st.Name = ""
		}
		steps = append(steps, st)
	}
	r.mu.Unlock()
	return logs, steps
}

// formatter hands entries to the reporter. act wraps it in its masking
// formatter, so messages arrive with secrets already replaced.
type formatter struct{ r *reporter }

func (f *formatter) Format(e *logrus.Entry) ([]byte, error) {
	f.r.handle(e)
	return nil, nil
}
