// Package protocol defines the HTTP API between the kiwicd server and its agents.
//
// Agents work like GitHub's self-hosted runners: an agent registers once with a
// short-lived registration token and receives a long-lived runner token. It then
// long-polls for jobs, streams logs and step states while a job runs (the updates
// double as heartbeats and carry cancellation back), and reports the result.
//
// The server evaluates job-level expressions (if, runs-on, strategy.matrix) and
// sends one matrix combination per job. The agent runs the job with act.
package protocol

// Version is bumped on protocol changes. The server takes agents speaking
// MinVersion to Version.
const (
	Version    = 2
	MinVersion = 1
)

// CalledJobsVersion is the first version whose agents run jobs of reusable
// workflows (JobRequest.Inputs); older agents don't get them.
const CalledJobsVersion = 2

// Header carrying the runner token on every agent request.
const RunnerTokenHeader = "X-Kiwicd-Runner-Token"

type RegisterRequest struct {
	Token    string   `json:"token"`
	Name     string   `json:"name"`
	Labels   []string `json:"labels"`
	OS       string   `json:"os"`
	Arch     string   `json:"arch"`
	Version  string   `json:"version"`
	Protocol int      `json:"protocol"`
}

type RegisterResponse struct {
	RunnerID    int64    `json:"runner_id"`
	RunnerToken string   `json:"runner_token"`
	Name        string   `json:"name"`
	Labels      []string `json:"labels"`
}

type PollRequest struct {
	Version  string   `json:"version"`
	Protocol int      `json:"protocol"`
	Labels   []string `json:"labels"`
}

type PollResponse struct {
	Job *JobRequest `json:"job,omitempty"`
}

// JobResult is a finished job's result as seen through the needs context.
type JobResult struct {
	Result  string            `json:"result"`
	Outputs map[string]string `json:"outputs"`
}

// JobRequest is everything an agent needs to run one job.
type JobRequest struct {
	JobID       int64  `json:"job_id"`
	RunID       int64  `json:"run_id"`
	RunNumber   int64  `json:"run_number"`
	RunAttempt  int64  `json:"run_attempt"`
	JobKey      string `json:"job_key"`
	DisplayName string `json:"display_name"`

	WorkflowFile string `json:"workflow_file"`
	WorkflowYAML string `json:"workflow_yaml"`
	// Matrix is the single combination this job runs, or nil.
	Matrix map[string]any `json:"matrix,omitempty"`
	// Results of the workflow's other finished jobs, keyed by job ID.
	JobResults map[string]JobResult `json:"job_results"`
	// Inputs is the inputs context of a job in a reusable workflow, as its
	// caller's with: gave them; nil for the run's own workflow.
	Inputs map[string]any `json:"inputs"`

	Repository string         `json:"repository"`
	Sha        string         `json:"sha"`
	Ref        string         `json:"ref"`
	EventName  string         `json:"event_name"`
	Event      map[string]any `json:"event"`
	Actor      string         `json:"actor"`

	Secrets map[string]string `json:"secrets"`
	Vars    map[string]string `json:"vars"`
	// Warnings from the server to show in the job's log, such as why it got
	// no secrets.
	Notices []string `json:"notices,omitempty"`
	// StepDebug shows ::debug:: lines; RunnerDebug shows the runner's own
	// debug messages too (ACTIONS_STEP_DEBUG and ACTIONS_RUNNER_DEBUG).
	StepDebug   bool `json:"step_debug,omitempty"`
	RunnerDebug bool `json:"runner_debug,omitempty"`
	// Extra environment, mostly GITHUB_* variables.
	Env map[string]string `json:"env"`

	TimeoutMinutes int    `json:"timeout_minutes"`
	ServerURL      string `json:"server_url"`
	// Path (relative to the server URL) of the source archive (tar.gz).
	SourcePath string `json:"source_path"`
}

const (
	StatusPending    = "pending"
	StatusQueued     = "queued"
	StatusInProgress = "in_progress"
	StatusCompleted  = "completed"

	ResultSuccess   = "success"
	ResultFailure   = "failure"
	ResultCancelled = "cancelled"
	ResultSkipped   = "skipped"
)

type LogLine struct {
	// Index into the job's steps.
	Step int    `json:"step"`
	TS   int64  `json:"ts"` // unix ms
	Text string `json:"text"`
}

type StepState struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion,omitempty"`
	StartedAt  int64  `json:"started_at,omitempty"`
	FinishedAt int64  `json:"finished_at,omitempty"`
}

// JobUpdate is sent while a job runs, at least every few seconds.
type JobUpdate struct {
	Logs  []LogLine   `json:"logs"`
	Steps []StepState `json:"steps,omitempty"`
}

type JobUpdateResponse struct {
	Cancelled bool `json:"cancelled"`
}

type JobComplete struct {
	Result  string            `json:"result"`
	Outputs map[string]string `json:"outputs"`
	Logs    []LogLine         `json:"logs"`
	Steps   []StepState       `json:"steps"`
	// What steps wrote to $GITHUB_STEP_SUMMARY, in the order they wrote it.
	Summaries []StepSummary `json:"summaries,omitempty"`
	// The job's ::error, ::warning and ::notice workflow commands and why it
	// failed, in log order.
	Annotations []Annotation `json:"annotations,omitempty"`
}

// StepSummary is the Markdown a step wrote to $GITHUB_STEP_SUMMARY.
type StepSummary struct {
	Step     int    `json:"step"`
	Markdown string `json:"markdown"`
}

// Annotation levels; GitHub's check runs call the error level "failure".
const (
	AnnotationError   = "error"
	AnnotationWarning = "warning"
	AnnotationNotice  = "notice"
)

// Annotation is a message about a job's code or result: an ::error,
// ::warning or ::notice workflow command, or why the job failed.
type Annotation struct {
	Level   string `json:"level"`
	Message string `json:"message"`
	Title   string `json:"title,omitempty"`
	// Where in the repository the message is about, if anywhere.
	File    string `json:"file,omitempty"`
	Line    int    `json:"line,omitempty"`
	EndLine int    `json:"end_line,omitempty"`
	Col     int    `json:"col,omitempty"`
	EndCol  int    `json:"end_col,omitempty"`
	// Where it is in the job's log: the step's index and the line's number
	// (from 1) among the step's lines; 0 if it has none.
	Step    int `json:"step"`
	LogLine int `json:"log_line,omitempty"`
}

// Limits on what a job reports, as GitHub has them; the server enforces
// them too.
const (
	// MaxStepSummaryBytes is the most one step may write to
	// $GITHUB_STEP_SUMMARY; a larger summary is dropped, with a warning.
	MaxStepSummaryBytes = 1 << 20
	// MaxJobSummaries and MaxJobSummaryBytes cap a job's summaries.
	MaxJobSummaries    = 20
	MaxJobSummaryBytes = 8 << 20
	// MaxAnnotations caps a job's annotations from workflow commands; the
	// ones explaining a failure come on top of them.
	MaxAnnotations = 50
	// MaxAnnotationBytes is the longest message (or title) kept.
	MaxAnnotationBytes = 4 << 10
)
