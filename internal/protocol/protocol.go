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

// Version is bumped on incompatible protocol changes.
const Version = 1

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
	// Unseal says the agent has a secrets service to open sealed secrets
	// with. Jobs that have some fail on agents without one.
	Unseal bool `json:"unseal,omitempty"`
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
	// Results of the run's other finished jobs, keyed by job ID.
	JobResults map[string]JobResult `json:"job_results"`

	Repository string         `json:"repository"`
	Sha        string         `json:"sha"`
	Ref        string         `json:"ref"`
	EventName  string         `json:"event_name"`
	Event      map[string]any `json:"event"`
	Actor      string         `json:"actor"`

	// Environment is the deployment environment the job uses, if any.
	Environment string `json:"environment,omitempty"`

	Secrets map[string]string `json:"secrets"`
	// SealedSecrets names the job's secrets that are sealed to the secrets
	// service's public key. The agent gets them from its secrets service
	// before the job starts (see UnsealRequest), and adds them to Secrets.
	SealedSecrets []string          `json:"sealed_secrets,omitempty"`
	Vars          map[string]string `json:"vars"`
	// Warnings from the server to show in the job's log, such as why it got
	// no secrets.
	Notices []string `json:"notices,omitempty"`
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
}

// The secrets service holds the private key that opens sealed secrets (see
// package sealed and cmd/kiwicd-secrets). An agent whose job has sealed
// secrets sends it the job's token (secrets.KIWICD_TOKEN) with the agent's
// own credentials for the service:
//
//	POST /v1/unseal
//	Authorization: Bearer SERVICE-TOKEN
//	{"job_token": "..."}
//
// The service asks the kiwicd server which job the token belongs to and for
// its sealed secrets (GET /api/secrets-service/job, with the job token as a
// bearer token), checks that the agent's credentials may open secrets for
// that repository and environment, and returns them opened.
type UnsealRequest struct {
	JobToken string `json:"job_token"`
}

type UnsealResponse struct {
	Secrets map[string]string `json:"secrets"`
}

// SealedJob is the server's answer to the secrets service: the running job
// a token belongs to and its sealed secrets.
type SealedJob struct {
	JobID       int64          `json:"job_id"`
	RunID       int64          `json:"run_id"`
	Repository  string         `json:"repository"`
	Ref         string         `json:"ref"`
	Environment string         `json:"environment,omitempty"`
	Workflow    string         `json:"workflow"`
	Event       string         `json:"event"`
	Runner      string         `json:"runner"`
	Secrets     []SealedSecret `json:"secrets"`
}

// SealedSecret is one sealed value and the scope it was sealed for.
type SealedSecret struct {
	Name string `json:"name"`
	// Repository is empty for global secrets, Environment for global and
	// repository ones.
	Repository  string `json:"repository,omitempty"`
	Environment string `json:"environment,omitempty"`
	Value       string `json:"value"`
}
