export type Status = "queued" | "in_progress" | "completed" | "pending";
export type Conclusion = "" | "success" | "failure" | "cancelled" | "skipped";

export interface Me {
  user: string;
  version: string;
  public_url: string;
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
  /** Branches whose runs get secrets: the default branch and those matching protected_refs. */
  protected_branches?: string[];
}

export interface WorkflowInput {
  name: string;
  description: string;
  required: boolean;
  default: string;
  type: string;
  options?: string[];
}

export interface Workflow {
  file: string;
  name: string;
  events: string[] | null;
  dispatch: boolean;
  inputs: WorkflowInput[] | null;
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
  /** The workflow's concurrency group, of which one run of the repository runs at a time. */
  concurrency_group?: string;
  created_at: number;
  started_at: number;
  finished_at: number;
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
  log_seq: number;
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
}

export interface GitHubApp {
  app_id: number;
  slug: string;
  name: string;
  html_url: string;
  /** The account or organization that owns the app. */
  owner: string;
  client_id: string;
  /** GitHub logins that may sign in. */
  allowed_users: string[];
  /** Organizations whose members may sign in (the app must be installed on them). */
  allowed_orgs: string[];
  created_at: number;
  install_url?: string;
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
