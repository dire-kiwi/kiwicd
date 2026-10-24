import { useCallback, useMemo, type ReactNode } from "react";
import { Link } from "react-router-dom";
import { useEventSource, useFetch, useNow } from "../hooks";
import type { Run } from "../types";
import { absoluteTime, elapsed, eventLabel, refName, relativeTime, visualState } from "../util";
import { StatusIcon } from "./StatusIcon";
import { BranchChip, Empty, ErrorBox, Icon, Spinner } from "./ui";

const LIMIT = 100;

/** Live-updating list of runs, optionally filtered to one repository. */
export function RunList({ repo, emptyHint }: { repo?: string; emptyHint?: ReactNode }) {
  const path = `/api/runs?limit=${LIMIT}${repo ? `&repo=${encodeURIComponent(repo)}` : ""}`;
  const runs = useFetch<Run[]>(path);
  const { setData, reload } = runs;

  const onRun = useCallback(
    (data: unknown) => {
      const r = data as Run;
      if (!r || typeof r.id !== "number") return;
      if (repo && r.repo !== repo) return;
      setData((prev) => {
        if (!prev) return prev;
        const i = prev.findIndex((x) => x.id === r.id);
        const next = prev.slice();
        if (i >= 0) next[i] = { ...next[i], ...r, workflow_yaml: undefined, event_payload: undefined };
        else next.push(r);
        next.sort((a, b) => b.id - a.id);
        return next.slice(0, LIMIT);
      });
    },
    [repo, setData],
  );
  useEventSource("/api/events", { run: onRun }, reload);

  const anyActive = useMemo(() => (runs.data ?? []).some((r) => r.status !== "completed"), [runs.data]);
  const now = useNow(true, anyActive ? 1000 : 30000);

  if (runs.error && !runs.data) return <ErrorBox onRetry={reload}>{runs.error}</ErrorBox>;
  if (!runs.data) return <Spinner />;
  if (runs.data.length === 0)
    return <Empty title="No workflow runs yet">{emptyHint ?? "Push to a repository with a workflow in .github/workflows to start a run."}</Empty>;

  return (
    <ul className="list run-list">
      {runs.data.map((r) => (
        <RunRow key={r.id} run={r} now={now} showRepo={!repo} />
      ))}
    </ul>
  );
}

function RunRow({ run, now, showRepo }: { run: Run; now: number; showRepo: boolean }) {
  const st = visualState(run.status, run.conclusion, run.cancel_requested);
  const dur = elapsed(run.started_at, run.finished_at, now);
  const branch = refName(run.ref);
  return (
    <li className="list-row run-row">
      <Link to={`/runs/${run.id}`} className="run-row-link" aria-label={`${run.title || run.workflow_name} #${run.number}`} />
      <div className="run-row-icon">
        <StatusIcon state={st} />
      </div>
      <div className="run-row-main">
        <div className="run-row-title">
          <Link to={`/runs/${run.id}`}>{run.title || run.workflow_name || run.workflow_file}</Link>
        </div>
        <div className="run-row-meta muted small">
          <span className="strong-muted">{run.workflow_name || run.workflow_file}</span>{" "}
          <span>#{run.number}</span>
          {run.attempt > 1 && <span> · attempt {run.attempt}</span>}
          {showRepo && (
            <>
              {" · "}
              <Link to={`/repos/${run.repo}`} className="muted-link">
                {run.repo}
              </Link>
            </>
          )}
          <span>
            {" · "}
            {eventLabel(run.event)}
          </span>
          {run.actor && <span> · {run.actor}</span>}
        </div>
      </div>
      <div className="run-row-branch">
        <BranchChip name={branch} />
      </div>
      <div className="run-row-times muted small">
        <span title={absoluteTime(run.created_at)}>
          <Icon name="calendar" size={14} /> {relativeTime(run.created_at, now)}
        </span>
        {dur && (
          <span>
            <Icon name="clock" size={14} /> {dur}
          </span>
        )}
      </div>
    </li>
  );
}
