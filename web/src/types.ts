export type Status = "queued" | "in_progress" | "completed" | "pending";
export type Conclusion = "" | "success" | "failure" | "cancelled" | "skipped";

export type Role = "admin" | "maintainer" | "viewer";

/** What the signed-in user (or the API token used) may do; see the server's access.go. */
export interface Access {
  role: Role;
  admin: boolean;
  /** Maintainer of every repository. */
  maintain_all: boolean;
  /** Otherwise of these: "owner/name" or "owner/*". */
  maintain: string[];
  /** The only repositories visible; null for all. */
  repos: string[] | null;
  /** Signed in with GitHub. */
  github: boolean;
  token?: APIToken;
}

export interface Me {
  user: string;
  version: string;
  public_url: string;
  access: Access;
}

export interface Repo {
  id: number;
  name: string;
  kind: "hosted" | "mirror";
  url?: string;
  default_branch: string;
  refs: Record<string, string> | null;
  last_sync_at: number;
  last_sync_error: string;
  created_at: number;
  /** Last successful sync; 0 until a mirror has been fetched once. */
  synced_at: number;
  clone_url: string;
  webhook_url: string;
  webhook_secret?: string;
  /** Mirrors: the deploy key's authorized_keys line, if one is set. */
  ssh_public_key?: string;
  /** Mirrors: configured known_hosts; empty means trust on first connection. */
  ssh_known_hosts?: string;
  /** Mirrors: host keys in use, as "host type SHA256:fingerprint". */
  ssh_host_keys?: string[];
  /** Repositories whose repository_push, repository_pull_request and dispatched jobs may fetch this one with KIWICD_TOKEN. */
  readers?: string[];
  /** Ref patterns besides the default branch whose runs get secrets: "release/*", "refs/tags/v*". */
  protected_refs?: string[];
  /** Status badges can be fetched without signing in. */
  public_badges?: boolean;
  /** Whether its own .github/workflows/ files run; off for mirrors whose workflows run on GitHub Actions. Uploaded ones always do. */
  repo_workflows?: boolean;
  /** Branches whose runs get secrets: the default branch and those matching protected_refs. */
  protected_branches?: string[];
  /** Whether runs of its repository_push/repository_pull_request listeners and uploaded workflows are reported to GitHub as check runs. */
  report_checks?: boolean;
  /** Mirrors: whether pull requests from forks run the repository's pull_request workflows. */
  fork_pull_requests?: boolean;
}

export interface WorkflowInput {
  name: string;
  description: string;
  required: boolean;
  default: string;
  type: string;
  options?: string[];
}

export interface WorkflowSchedule {
  cron: string;
  /** IANA time zone of the cron fields; UTC when absent. */
  timezone?: string;
  /** When it next runs; absent off the default branch, whose workflows are the ones that run on schedule. */
  next_run_at?: number;
  error?: string;
}

export interface Workflow {
  file: string;
  name: string;
  events: string[] | null;
  /** The events' filters, e.g. "workflows: CI · types: completed", by event. */
  event_details?: Record<string, string>;
  dispatch: boolean;
  inputs: WorkflowInput[] | null;
  schedules?: WorkflowSchedule[];
  error?: string;
  /** Stored in kiwicd rather than the repository; file is "kiwicd:NAME". */
  uploaded?: boolean;
}

export interface UploadedWorkflow {
  file: string;
  content: string;
  updated_at: number;
}

export interface Run {
  id: number;
  repo_id: number;
  repo: string;
  number: number;
  attempt: number;
  workflow_file: string;
  workflow_name: string;
  workflow_yaml?: string;
  event_payload?: unknown;
  title: string;
  event: string;
  ref: string;
  sha: string;
  actor: string;
  status: Status;
  conclusion: Conclusion;
  error: string;
  cancel_requested: boolean;
  /** Its ref was protected, so its jobs get secrets. */
  protected: boolean;
  /** A re-run with debug logging. */
  debug?: boolean;
  /** The workflow's concurrency group, of which one run of the repository runs at a time. */
  concurrency_group?: string;
  /** The workflow's permissions:, or null when not set. */
  permissions: Permissions | null;
  created_at: number;
  started_at: number;
  finished_at: number;
}

/** Values the runs list can be filtered by, from the latest runs. */
export interface RunFilterValues {
  workflows: { file: string; name: string }[];
  refs: string[];
  events: string[];
  actors: string[];
}

/** A permissions: setting: read-all/write-all set all, a mapping sets scopes (the rest get none). */
export interface Permissions {
  all?: "read" | "write";
  scopes?: Record<string, string>;
}

export interface Step {
  name: string;
  status: "pending" | "in_progress" | "completed";
  conclusion: Conclusion;
  started_at: number;
  finished_at: number;
}

export interface Job {
  id: number;
  run_id: number;
  key: string;
  name: string;
  /** The calling job's row, for a job of a reusable workflow; 0 otherwise. */
  parent_id?: number;
  /** The workflow a calling job calls, and the inputs it gives it. */
  uses?: string;
  inputs?: Record<string, unknown> | null;
  matrix: Record<string, unknown> | null;
  runs_on: string[] | null;
  status: Status;
  conclusion: Conclusion;
  error: string;
  runner_id: number;
  runner_name: string;
  outputs: Record<string, string> | null;
  steps: Step[] | null;
  cancel_requested: boolean;
  /** Job-level concurrency group; a pending job waits for the group's running one. */
  concurrency_group?: string;
  /** Its failure doesn't fail the run. */
  continue_on_error: boolean;
  /** strategy.max-parallel of its matrix. */
  max_parallel?: number;
  /** Its permissions (its own or the workflow's), or null when not set. */
  permissions: Permissions | null;
  log_seq: number;
  /** Kept by a re-run from an earlier attempt: the job that ran, and its attempt. */
  copied_from?: number;
  copied_attempt?: number;
  created_at: number;
  started_at: number;
  finished_at: number;
}

export interface PlanEntry {
  key: string;
  name: string;
  needs: string[] | null;
  /** For a job that calls a reusable workflow: what it calls, and its jobs. */
  uses?: string;
  jobs?: PlanEntry[];
}

/** A reusable workflow the run calls, as read when the run was created. */
export interface CalledWorkflow {
  /** The calling jobs' keys from the run's workflow down: build/deploy. */
  path: string;
  uses: string;
  source: "kiwicd" | "github";
  repo: string;
  file: string;
  sha: string;
  yaml: string;
}

export interface RunDetail {
  run: Run;
  jobs: Job[] | null;
  plan: PlanEntry[] | null;
  workflows?: CalledWorkflow[] | null;
  // Labels whose runners won't take this run's jobs, as its ref isn't protected.
  protected_labels?: string[];
}

export interface LogLine {
  seq: number;
  step: number;
  ts: number;
  text: string;
}

export interface Runner {
  id: number;
  name: string;
  labels: string[] | null;
  os: string;
  arch: string;
  version: string;
  created_at: number;
  last_seen_at: number;
  online: boolean;
  busy: boolean;
  job_id?: number;
  run_id?: number;
  job_name?: string;
  // Reserved labels: who may use them (owner/repo, or owner/repo@ref).
  reserved?: Record<string, string[]>;
  // The runner's labels that keep it for jobs from protected refs.
  protected_labels?: string[];
}

export interface RegistrationToken {
  token: string;
  expires_at: number;
  url: string;
}

export interface Setting {
  name: string;
  value?: string;
  updated_at: number;
}

export interface APIToken {
  id: number;
  name: string;
  user: string;
  created_at: number;
  expires_at: number; // 0: never
  last_used_at: number;
  /** At most this role; "" for its maker's. */
  role: Role | "";
  /** Only these repositories; empty for all. */
  repos: string[];
}

/** A role given to a GitHub login, an organization's members or a team's ("org/team-slug"). */
export interface RoleGrant {
  kind: "user" | "org" | "team";
  name: string;
  role: Role;
  /** Maintainers: "owner/name" or "owner/*"; empty for all repositories. */
  repos: string[];
  updated_at: number;
}

/** Someone who has signed in with GitHub, and the role they have now. */
export interface GitHubUser {
  login: string;
  /** Organizations and teams with roles they were members of when they signed in: "org:NAME", "team:ORG/TEAM". */
  memberships: string[];
  signed_in_at: number;
  role: Role | "";
  maintain_all: boolean;
  maintain: string[];
}

export interface AuditEntry {
  id: number;
  at: number;
  actor: string;
  token?: string;
  client?: string;
  action: string;
  repo?: string;
  target?: string;
  detail?: string;
}

export interface GitHubApp {
  app_id: number;
  slug: string;
  name: string;
  html_url: string;
  /** The account or organization that owns the app. */
  owner: string;
  client_id: string;
  created_at: number;
  install_url?: string;
  /** What the app and its installations lack for reporting check runs, from GitHub. */
  access?: GitHubAccess;
  /** Why GitHub couldn't be asked for access. */
  access_error?: string;
}

export interface GitHubAccess {
  /** The app's permissions page on GitHub. */
  permissions_url: string;
  /** "checks: write" and events the app isn't subscribed to. */
  missing: string[];
  installations: { account: string; html_url: string; checks: boolean }[];
}

export interface GitHubSettings {
  app: GitHubApp | null;
  webhook_url: string;
  callback_url: string;
  github_url: string;
}

/** A repository the GitHub App is installed on. */
export interface GitHubRepo {
  full_name: string;
  clone_url: string;
  private: boolean;
  default_branch: string;
}

export interface StepSummary {
  /** Index into the job's steps. */
  step: number;
  markdown: string;
}

export interface Annotation {
  level: "error" | "warning" | "notice";
  message: string;
  title?: string;
  file?: string;
  line?: number;
  end_line?: number;
  col?: number;
  end_col?: number;
  /** Where it is in the job's log: a step index and the line's number in it (from 1), or 0. */
  step: number;
  log_line?: number;
}

/** What a finished job reported besides its log. */
export interface JobReport {
  job_id: number;
  summaries: StepSummary[];
  annotations: Annotation[];
}
