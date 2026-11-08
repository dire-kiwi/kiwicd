import { useState } from "react";
import { Link } from "react-router-dom";
import { groupJobs, jobState, type PlanGroup } from "../runmodel";
import type { Job, Run, Step } from "../types";
import { absoluteTime, classes, formatDuration, visualState } from "../util";
import { StatusIcon } from "./StatusIcon";
import { Icon } from "./ui";

/** How long a job waited for a runner: from being queued to starting, or until now. "" if it never queued. */
export function queuedFor(job: Job, now: number): string {
  if (!job.runs_on?.length) return ""; // decided by the server, never queued
  if (job.started_at) return formatDuration(job.started_at - job.created_at);
  return job.status === "queued" ? formatDuration(now - job.created_at) : "";
}

function ranFor(start: number, end: number, now: number): string {
  return start ? formatDuration((end || now) - start) : "";
}

/**
 * Each job's queue time and duration, and its steps', on a timeline from the
 * run's start. Jobs a re-run kept ran in an earlier attempt, off this
 * attempt's timeline.
 */
export function RunTiming({ run, groups, now }: { run: Run; groups: PlanGroup[]; now: number }) {
  const [open, setOpen] = useState<Record<number, boolean>>({});
  const jobs = groups.flatMap(groupJobs);
  const here = jobs.filter((j) => !j.copied_from);
  if (jobs.length === 0) return null;
  const start = Math.min(run.created_at, ...here.map((j) => j.created_at));
  const end = Math.max(start + 1, run.finished_at || now, ...here.map((j) => j.finished_at || (j.status === "completed" ? 0 : now)));
  const pos = (t: number) => `${(((t || now) - start) / (end - start)) * 100}%`;
  const span = (from: number, to: number) => ({ left: pos(from), width: `calc(${pos(to)} - ${pos(from)})` });

  return (
    <section className="box">
      <div className="box-header">
        <div>
          <h3>Timing</h3>
          <p className="muted small">
            Queued is how long a job waited for a runner. The timeline runs from the run's creation, {absoluteTime(start)}, over{" "}
            {formatDuration(end - start)}.
          </p>
        </div>
      </div>
      <div className="timing" role="table">
        <div className="timing-row timing-head muted small" role="row">
          <span />
          <span role="columnheader">Job</span>
          <span role="columnheader">Queued</span>
          <span role="columnheader">Duration</span>
          <span role="columnheader">Timeline</span>
        </div>
        {jobs.map((j) => {
          const steps = (j.steps ?? []).filter((s) => s.name);
          const isOpen = !!open[j.id] && steps.length > 0;
          return (
            <div key={j.id} className="timing-job">
              <div className="timing-row" role="row">
                <button
                  className={classes("btn-icon timing-toggle", isOpen && "timing-toggle-open")}
                  onClick={() => setOpen((o) => ({ ...o, [j.id]: !o[j.id] }))}
                  disabled={steps.length === 0}
                  aria-expanded={isOpen}
                  aria-label={isOpen ? `Hide ${j.name}'s steps` : `Show ${j.name}'s steps`}
                >
                  <Icon name="chevron" size={12} />
                </button>
                <span className="timing-name" role="cell">
                  <StatusIcon state={jobState(j)} size={14} />
                  <Link to={`/runs/${run.id}/jobs/${j.id}`} className="ellipsis">
                    {j.name}
                  </Link>
                </span>
                <span className="timing-num" role="cell">
                  {queuedFor(j, now) || "—"}
                </span>
                <span className="timing-num" role="cell">
                  {ranFor(j.started_at, j.finished_at, now) || "—"}
                </span>
                <span className={classes("timing-track", !!j.copied_from && "timing-track-elsewhere")} role="cell">
                  {j.copied_from ? (
                    <span className="muted small">ran in attempt {j.copied_attempt}</span>
                  ) : (
                    <>
                      {(j.started_at || j.status === "queued") && (
                        <span className="timing-bar timing-queued" style={span(j.created_at, j.started_at)} title={`Queued ${queuedFor(j, now)}`} />
                      )}
                      {j.started_at > 0 && (
                        <span
                          className={classes("timing-bar", `timing-${jobState(j)}`)}
                          style={span(j.started_at, j.finished_at)}
                          title={`Ran ${ranFor(j.started_at, j.finished_at, now)}`}
                        />
                      )}
                    </>
                  )}
                </span>
              </div>
              {isOpen &&
                steps.map((s, i) => (
                  <div key={i} className="timing-row timing-step small" role="row">
                    <span />
                    <span className="timing-name" role="cell">
                      <StatusIcon state={stepState(s)} size={12} />
                      <span className="ellipsis">{s.name}</span>
                    </span>
                    <span />
                    <span className="timing-num" role="cell">
                      {ranFor(s.started_at, s.finished_at, now) || "—"}
                    </span>
                    <span className="timing-track" role="cell">
                      {!j.copied_from && s.started_at > 0 && (
                        <span className={classes("timing-bar", `timing-${stepState(s)}`)} style={span(s.started_at, s.finished_at)} />
                      )}
                    </span>
                  </div>
                ))}
            </div>
          );
        })}
      </div>
    </section>
  );
}

function stepState(s: Step) {
  if (s.status === "completed") return visualState("completed", s.conclusion);
  return s.status === "in_progress" ? ("in_progress" as const) : ("pending" as const);
}
