import { useCallback, useMemo, type ReactNode } from "react";
import { Link, useSearchParams } from "react-router-dom";
import { useEventSource, useFetch, useNow } from "../hooks";
import type { Repo, Run, RunFilterValues } from "../types";
import { absoluteTime, elapsed, eventLabel, refName, relativeTime, visualState } from "../util";
import { StatusIcon } from "./StatusIcon";
import { BranchChip, Empty, ErrorBox, Icon, Spinner } from "./ui";

const LIMIT = 100;

/** The runs list's filters, kept in the page's URL so they can be linked to. */
const filterKeys = ["repo", "workflow", "branch", "event", "status", "actor"] as const;
type Filters = Partial<Record<(typeof filterKeys)[number], string>>;

const statuses: [string, string][] = [
  ["waiting", "Waiting for approval"],
  ["queued", "Queued"],
  ["in_progress", "In progress"],
  ["completed", "Completed"],
  ["success", "Success"],
  ["failure", "Failure"],
  ["cancelled", "Cancelled"],
  ["skipped", "Skipped"],
];

/** Whether a run passes the filters, as the server's ?workflow= and so on decide. */
function matches(r: Run, f: Filters): boolean {
  if (f.repo && r.repo !== f.repo) return false;
  if (f.workflow && r.workflow_file !== f.workflow && r.workflow_file !== `.github/workflows/${f.workflow}`) return false;
  if (f.branch && r.ref !== f.branch && r.ref !== `refs/heads/${f.branch}`) return false;
  if (f.event && r.event !== f.event) return false;
  if (f.actor && r.actor !== f.actor) return false;
  if (f.status) {
    if (["waiting", "queued", "in_progress", "completed", "pending"].includes(f.status)) return r.status === f.status;
    return r.status === "completed" && r.conclusion === f.status;
  }
  return true;
}

/** Live-updating list of runs, optionally of one repository, with filters from the URL. */
export function RunList({ repo, emptyHint }: { repo?: string; emptyHint?: ReactNode }) {
  const [params, setParams] = useSearchParams();
  const filters = useMemo(() => {
    const f: Filters = {};
    for (const k of filterKeys) {
      const v = params.get(k);
      if (v) f[k] = v;
    }
    if (repo) f.repo = repo;
    return f;
  }, [params, repo]);
  const query = new URLSearchParams({ limit: String(LIMIT), ...filters }).toString();
  const runs = useFetch<Run[]>(`/api/runs?${query}`);
  const { setData, reload } = runs;

  const onRun = useCallback(
    (data: unknown) => {
      const r = data as Run;
      if (!r || typeof r.id !== "number") return;
      setData((prev) => {
        if (!prev) return prev;
        const i = prev.findIndex((x) => x.id === r.id);
        const merged = i >= 0 ? { ...prev[i], ...r, workflow_yaml: undefined, event_payload: undefined } : r;
        if (!matches(merged, filters)) return i >= 0 ? prev.filter((x) => x.id !== r.id) : prev;
        const next = prev.slice();
        if (i >= 0) next[i] = merged;
        else next.push(r);
        next.sort((a, b) => b.id - a.id);
        return next.slice(0, LIMIT);
      });
    },
    [filters, setData],
  );
  useEventSource("/api/events", { run: onRun }, reload);

  const anyActive = useMemo(() => (runs.data ?? []).some((r) => r.status !== "completed"), [runs.data]);
  const now = useNow(true, anyActive ? 1000 : 30000);
  // Filters picked here, not the repository a repository's page shows.
  const filtered = filterKeys.some((k) => !!filters[k] && !(k === "repo" && repo));

  const setFilter = (key: keyof Filters, value: string) => {
    setParams(
      (p) => {
        const next = new URLSearchParams(p);
        if (value) next.set(key, value);
        else next.delete(key);
        return next;
      },
      { replace: true },
    );
  };
  const clear = () =>
    setParams(
      (p) => {
        const next = new URLSearchParams(p);
        for (const k of filterKeys) next.delete(k);
        return next;
      },
      { replace: true },
    );

  let body: ReactNode;
  if (runs.error && !runs.data) body = <ErrorBox onRetry={reload}>{runs.error}</ErrorBox>;
  else if (!runs.data) body = <Spinner />;
  else if (runs.data.length === 0)
    body = filtered ? (
      <Empty title="No runs match these filters">
        <button className="btn btn-sm" onClick={clear}>
          Clear filters
        </button>
      </Empty>
    ) : (
      <Empty title="No workflow runs yet">{emptyHint ?? "Push to a repository with a workflow in .github/workflows to start a run."}</Empty>
    );
  else
    body = (
      <ul className="list run-list">
        {runs.data.map((r) => (
          <RunRow key={r.id} run={r} now={now} showRepo={!repo} />
        ))}
      </ul>
    );

  return (
    <div className="stack">
      <RunFilterBar repo={repo} filters={filters} onChange={setFilter} onClear={filtered ? clear : undefined} />
      {body}
    </div>
  );
}

function RunFilterBar({
  repo,
  filters,
  onChange,
  onClear,
}: {
  repo?: string;
  filters: Filters;
  onChange: (key: keyof Filters, value: string) => void;
  onClear?: () => void;
}) {
  const values = useFetch<RunFilterValues>(`/api/runs/filters${filters.repo ? `?repo=${encodeURIComponent(filters.repo)}` : ""}`);
  const repos = useFetch<Repo[]>(repo ? null : "/api/repos");
  const v = values.data;
  // Branches go in the URL by name, other refs whole.
  const branch = (ref: string) => (ref.startsWith("refs/heads/") ? ref.slice(11) : ref);
  return (
    <div className="run-filters">
      {!repo && (
        <FilterSelect label="Repository" value={filters.repo} onChange={(x) => onChange("repo", x)} options={(repos.data ?? []).map((r) => [r.name, r.name])} />
      )}
      <FilterSelect
        label="Workflow"
        value={filters.workflow}
        onChange={(x) => onChange("workflow", x)}
        options={(v?.workflows ?? []).map((w) => [w.file, w.name && w.name !== w.file ? `${w.name} (${w.file.replace(/^\.github\/workflows\//, "")})` : w.file])}
      />
      <FilterSelect label="Branch" value={filters.branch} onChange={(x) => onChange("branch", x)} options={(v?.refs ?? []).map((r) => [branch(r), refName(r)])} />
      <FilterSelect label="Event" value={filters.event} onChange={(x) => onChange("event", x)} options={(v?.events ?? []).map((e) => [e, eventLabel(e)])} />
      <FilterSelect label="Status" value={filters.status} onChange={(x) => onChange("status", x)} options={statuses} />
      <FilterSelect label="Actor" value={filters.actor} onChange={(x) => onChange("actor", x)} options={(v?.actors ?? []).map((a) => [a, a])} />
      {onClear && (
        <button className="btn btn-sm" onClick={onClear}>
          Clear filters
        </button>
      )}
    </div>
  );
}

function FilterSelect({
  label,
  value = "",
  options,
  onChange,
}: {
  label: string;
  value?: string;
  options: [string, string][];
  onChange: (value: string) => void;
}) {
  // A value from a link that isn't among the recent runs' still shows.
  const all = value && !options.some(([v]) => v === value) ? [[value, value] as [string, string], ...options] : options;
  return (
    <label className={value ? "run-filter run-filter-on" : "run-filter"}>
      <span className="run-filter-label">{label}</span>
      <select className="input" value={value} onChange={(e) => onChange(e.target.value)}>
        <option value="">All</option>
        {all.map(([v, text]) => (
          <option key={v} value={v}>
            {text}
          </option>
        ))}
      </select>
    </label>
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
