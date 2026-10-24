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
}

export interface RunDetail {
  run: Run;
  jobs: Job[] | null;
  plan: PlanEntry[] | null;
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
