import { useCallback, useMemo, useState } from "react";
import { Link, NavLink, useParams } from "react-router-dom";
import { errorMessage, post } from "../api";
import { JobLog } from "../components/JobLog";
import { PlanGraph } from "../components/PlanGraph";
import { RerunModal, type RerunWhat } from "../components/RerunModal";
import { RunTiming } from "../components/RunTiming";
import { StatusIcon } from "../components/StatusIcon";
import { BranchChip, CodeBlock, ErrorBox, Flash, Icon, Spinner } from "../components/ui";
import { useDocumentTitle, useEventSource, useFetch, useNow } from "../hooks";
import { buildGroups, jobState, type PlanGroup } from "../runmodel";
import type { Job, Run, RunDetail } from "../types";
import { absoluteTime, classes, elapsed, eventLabel, refName, relativeTime, shortSha, stateLabel, visualState } from "../util";

export function RunPage() {
  const { id = "", jobId } = useParams();
  const detail = useFetch<RunDetail>(`/api/runs/${id}`);
  const { setData, reload } = detail;
  const [actionError, setActionError] = useState<string>();
  const [busy, setBusy] = useState(false);
  const [rerun, setRerun] = useState<RerunWhat>();

  const onRun = useCallback(
    (data: unknown) => {
      const r = data as Run;
      if (!r || String(r.id) !== id) return;
      setData((d) => d && { ...d, run: { ...d.run, ...r, workflow_yaml: r.workflow_yaml || d.run.workflow_yaml, event_payload: r.event_payload ?? d.run.event_payload } });
    },
    [id, setData],
  );
  const upsertJob = useCallback(
    (data: unknown) => {
      const j = data as Job;
      if (!j || String(j.run_id) !== id) return;
      setData((d) => {
        if (!d) return d;
        const jobs = (d.jobs ?? []).slice();
        const i = jobs.findIndex((x) => x.id === j.id);
        if (i >= 0) jobs[i] = j;
        else jobs.push(j);
        return { ...d, jobs };
      });
    },
    [id, setData],
  );
  useEventSource(`/api/events?run=${encodeURIComponent(id)}`, { run: onRun, job: upsertJob }, reload);

  const run = detail.data?.run;
  const jobs = useMemo(() => detail.data?.jobs ?? [], [detail.data]);
  const groups = useMemo(() => (run ? buildGroups(run, detail.data?.plan ?? [], jobs) : []), [run, detail.data, jobs]);
  const now = useNow(!!run && run.status !== "completed");
  useDocumentTitle(run ? `${run.title || run.workflow_name} · ${run.workflow_name} #${run.number}` : undefined);

  if (detail.error && !detail.data)
    return (
      <div className="page">
        <ErrorBox onRetry={reload}>{detail.error}</ErrorBox>
      </div>
    );
  if (!run)
    return (
      <div className="page">
        <Spinner />
      </div>
    );

  const st = visualState(run.status, run.conclusion, run.cancel_requested);
  const selectedJob = jobId ? jobs.find((j) => String(j.id) === jobId) : undefined;
  const pushed = run.event === "repository_push" ? pushedTo(run.event_payload) : undefined;
  const pull = run.event === "repository_pull_request" ? pullRequestOf(run.event_payload) : undefined;
  const waiting = !!run.concurrency_group && run.status === "queued" && !run.cancel_requested && jobs.length === 0;

  const cancel = async () => {
    setBusy(true);
    setActionError(undefined);
    try {
      await post(`/api/runs/${run.id}/cancel`);
      reload();
    } catch (e) {
      setActionError(errorMessage(e));
    } finally {
      setBusy(false);
    }
  };
  const anyFailed = jobs.some((j) => j.conclusion === "failure" || j.conclusion === "cancelled");

  return (
    <div className="page page-wide">
      <div className="breadcrumbs small">
        <Link to={`/repos/${run.repo}`}>{run.repo}</Link>
        <span className="muted"> / </span>
        <Link to={`/repos/${run.repo}`}>{run.workflow_name || run.workflow_file}</Link>
      </div>
      <div className="run-header">
        <div className="run-title">
          <StatusIcon state={st} size={24} />
          <h1>
            {run.title || run.workflow_name}
            <span className="muted run-number"> #{run.number}</span>
          </h1>
        </div>
        <div className="run-actions">
          {jobs.length > 0 && (
            <a className="btn" href={`/api/runs/${run.id}/logs`} download title="Download every job's log as a zip file">
              <Icon name="download" size={14} /> Download logs
            </a>
          )}
          {run.status !== "completed" ? (
            <button className="btn btn-danger-outline" onClick={cancel} disabled={busy || run.cancel_requested}>
              {run.cancel_requested ? "Cancelling…" : "Cancel run"}
            </button>
          ) : (
            <>
              {anyFailed && (
                <button className="btn" onClick={() => setRerun({ kind: "failed" })}>
                  Re-run failed jobs
                </button>
              )}
              <button className="btn" onClick={() => setRerun({ kind: "all" })}>
                Re-run all jobs
              </button>
            </>
          )}
        </div>
      </div>
      <div className="run-meta">
        <div className="meta-item">
          <span className="meta-label">Status</span>
          <span className={classes("meta-value", `text-${st}`)}>{stateLabel(st)}</span>
        </div>
        <div className="meta-item">
          <span className="meta-label">Triggered via {eventLabel(run.event)}</span>
          <span className="meta-value" title={absoluteTime(run.created_at)}>
            {run.actor ? <strong>{run.actor}</strong> : null} {relativeTime(run.created_at, now)}
          </span>
        </div>
        <div className="meta-item">
          <span className="meta-label">Commit</span>
          <span className="meta-value meta-inline">
            <BranchChip name={refName(run.ref)} />
            <span className="mono" title={run.sha}>
              <Icon name="commit" size={14} /> {shortSha(run.sha)}
            </span>
          </span>
        </div>
        {pushed && (
          <div className="meta-item">
            <span className="meta-label">Pushed to</span>
            <span className="meta-value meta-inline">
              <Link to={`/repos/${pushed.repo}`}>{pushed.repo}</Link>
              <BranchChip name={refName(pushed.ref)} />
              <span className="mono" title={pushed.sha}>
                <Icon name="commit" size={14} /> {shortSha(pushed.sha)}
              </span>
            </span>
          </div>
        )}
        {pull && (
          <div className="meta-item">
            <span className="meta-label">Pull request</span>
            <span className="meta-value meta-inline">
              <Link to={`/repos/${pull.repo}`}>{pull.repo}</Link>
              <span>
                #{pull.number} {pull.action}
              </span>
              <BranchChip name={pull.head} />
              <span className="mono" title={pull.sha}>
                <Icon name="commit" size={14} /> {shortSha(pull.sha)}
              </span>
              {pull.fork && <span className="muted small">from {pull.headRepo || "a deleted fork"}</span>}
            </span>
          </div>
        )}
        <div className="meta-item">
          <span className="meta-label">Total duration</span>
          <span className="meta-value">{elapsed(run.started_at, run.finished_at, now) || "—"}</span>
        </div>
        <div className="meta-item">
          <span className="meta-label">Workflow</span>
          <span className="meta-value mono small" title={run.workflow_file}>
            {run.workflow_file.replace(/^\.github\/workflows\//, "")}
            {run.attempt > 1 && <span className="muted"> · attempt {run.attempt}</span>}
            {run.debug && <span className="muted"> · debug logging</span>}
          </span>
        </div>
      </div>
      {run.error && (
        <ErrorBox>
          <span className="pre-wrap">{run.error}</span>
        </ErrorBox>
      )}
      {!run.protected && !run.error && (
        <Flash kind="warn">
          <span>
            <span className="mono">{refName(run.ref)}</span> isn't a protected ref of {run.repo}, so this run's jobs get no secrets except{" "}
            <span className="mono">KIWICD_TOKEN</span>, which only fetches {run.repo}. Protected refs are set in the repository's{" "}
            <Link to={`/repos/${run.repo}/settings`}>Settings</Link>.
          </span>
        </Flash>
      )}
      {waiting && (
        <Flash>
          <span>
            Waiting for an earlier run of the concurrency group <span className="mono">{run.concurrency_group}</span> to finish.
          </span>
        </Flash>
      )}
      {actionError && <ErrorBox>{actionError}</ErrorBox>}

      <div className="run-layout">
        <aside className="run-sidebar">
          <NavLink to={`/runs/${run.id}`} end className="side-link side-summary">
            <Icon name="bolt" size={14} /> Summary
          </NavLink>
          <div className="side-heading">Jobs</div>
          <JobNav runId={run.id} groups={groups} now={now} />
        </aside>
        <section className="run-content">
          {jobId ? (
            selectedJob ? (
              <JobLog
                key={selectedJob.id}
                job={selectedJob}
                onJob={upsertJob}
                withheld={detail.data?.protected_labels ?? []}
                onRerun={run.status === "completed" ? () => setRerun({ kind: "job", job: selectedJob }) : undefined}
              />
            ) : (
              <ErrorBox>
                Job {jobId} is not part of this run. <Link to={`/runs/${run.id}`}>Back to summary</Link>
              </ErrorBox>
            )
          ) : (
            <RunSummary run={run} groups={groups} now={now} />
          )}
        </section>
      </div>
      {rerun && <RerunModal run={run} what={rerun} onClose={() => setRerun(undefined)} />}
    </div>
  );
}

function JobNav({ runId, groups, now }: { runId: number; groups: PlanGroup[]; now: number }) {
  return (
    <ul className="side-jobs">
      {groups.map((g) =>
        g.jobs.length === 0 ? (
          <li key={g.key}>
            <div className="side-link side-disabled" title={g.state === "pending" ? "Waiting for dependencies" : undefined}>
              <StatusIcon state={g.state} size={14} />
              <span className="ellipsis">{g.name}</span>
            </div>
          </li>
        ) : g.matrix ? (
          <li key={g.key} className="side-group">
            <div className="side-group-title">
              <StatusIcon state={g.state} size={14} />
              <span className="ellipsis">{g.name}</span>
              <span className="muted small">{g.jobs.length}</span>
            </div>
            <ul>
              {g.jobs.map((j) => (
                <JobNavItem key={j.id} runId={runId} job={j} now={now} />
              ))}
            </ul>
          </li>
        ) : (
          <JobNavItem key={g.key} runId={runId} job={g.jobs[0]} now={now} />
        ),
      )}
    </ul>
  );
}

function JobNavItem({ runId, job, now }: { runId: number; job: Job; now: number }) {
  return (
    <li>
      <NavLink to={`/runs/${runId}/jobs/${job.id}`} className="side-link">
        <StatusIcon state={jobState(job)} size={14} />
        <span className="ellipsis">{job.name}</span>
        <span className="muted small side-dur">{elapsed(job.started_at, job.finished_at, now)}</span>
      </NavLink>
    </li>
  );
}

function RunSummary({ run, groups, now }: { run: Run; groups: PlanGroup[]; now: number }) {
  const outputs = groups.flatMap((g) => g.jobs).filter((j) => j.outputs && Object.keys(j.outputs).length > 0);
  return (
    <div className="stack">
      <section className="box">
        <div className="box-header">
          <div>
            <h3>{run.workflow_name || run.workflow_file}</h3>
            <p className="muted small">
              on: {eventLabel(run.event)}
            </p>
          </div>
        </div>
        {groups.length === 0 ? (
          <div className="empty">
            <div className="empty-body">No jobs in this run.</div>
          </div>
        ) : (
          <PlanGraph runId={run.id} groups={groups} now={now} />
        )}
      </section>
      <RunTiming run={run} groups={groups} now={now} />
      {outputs.length > 0 && (
        <section className="box">
          <div className="box-header">
            <h3>Job outputs</h3>
          </div>
          <table className="table">
            <thead>
              <tr>
                <th>Job</th>
                <th>Output</th>
                <th>Value</th>
              </tr>
            </thead>
            <tbody>
              {outputs.flatMap((j) =>
                Object.entries(j.outputs ?? {}).map(([k, v]) => (
                  <tr key={`${j.id}-${k}`}>
                    <td>
                      <Link to={`/runs/${run.id}/jobs/${j.id}`}>{j.name}</Link>
                    </td>
                    <td className="mono">{k}</td>
                    <td className="mono pre-wrap">{String(v)}</td>
                  </tr>
                )),
              )}
            </tbody>
          </table>
        </section>
      )}
      {run.workflow_yaml && (
        <details className="box disclosure-box">
          <summary>
            Workflow file <span className="mono muted small">{run.workflow_file}</span>
          </summary>
          <CodeBlock code={run.workflow_yaml} />
        </details>
      )}
    </div>
  );
}

// pushedTo reads the other repository's push from a repository_push run's event.
function pushedTo(payload: unknown): { repo: string; ref: string; sha: string } | undefined {
  const p = payload as { ref?: string; after?: string; repository?: { full_name?: string } } | undefined;
  if (!p?.repository?.full_name || !p.ref || !p.after) return undefined;
  return { repo: p.repository.full_name, ref: p.ref, sha: p.after };
}

interface PullRequestPayload {
  action?: string;
  number?: number;
  repository?: { full_name?: string };
  pull_request?: {
    head?: { ref?: string; sha?: string; repo?: { full_name?: string } | null };
    base?: { repo?: { full_name?: string } | null };
  };
}

// pullRequestOf reads the other repository's pull request from a repository_pull_request run's event.
function pullRequestOf(
  payload: unknown,
): { repo: string; number: number; action: string; head: string; sha: string; headRepo?: string; fork: boolean } | undefined {
  const p = payload as PullRequestPayload | undefined;
  const head = p?.pull_request?.head;
  if (!p?.repository?.full_name || !p.number || !head?.sha) return undefined;
  const headRepo = head.repo?.full_name;
  const baseRepo = p.pull_request?.base?.repo?.full_name;
  return {
    repo: p.repository.full_name,
    number: p.number,
    action: p.action ?? "",
    head: head.ref ?? "",
    sha: head.sha,
    headRepo,
    fork: !headRepo || !baseRepo || headRepo.toLowerCase() !== baseRepo.toLowerCase(),
  };
}
