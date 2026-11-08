import { useEffect, useRef } from "react";
import { Link } from "react-router-dom";
import { useFetch } from "../hooks";
import type { Annotation, Job, JobReport } from "../types";
import { classes } from "../util";
import { Markdown } from "./Markdown";

/** logLink is the path to an annotation's line in its job's log. */
export function logLink(runId: number, jobId: number, a: Pick<Annotation, "step" | "log_line">): string {
  const base = `/runs/${runId}/jobs/${jobId}`;
  return a.log_line ? `${base}#step-${a.step}-L${a.log_line}` : base;
}

/**
 * A run's annotations and its jobs' step summaries, for its summary page:
 * jobs in the order they're shown, each job's summaries in the order its
 * steps wrote them.
 */
export function RunReports({ runId, jobs }: { runId: number; jobs: Job[] }) {
  const reports = useFetch<JobReport[]>(`/api/runs/${runId}/reports`);
  const { reload } = reports;
  // Reports arrive as jobs finish.
  const finished = jobs
    .filter((j) => j.status === "completed")
    .map((j) => j.id)
    .join(",");
  const first = useRef(true);
  useEffect(() => {
    if (first.current) first.current = false;
    else reload();
  }, [finished, reload]);

  const byJob = new Map((reports.data ?? []).map((r) => [r.job_id, r]));
  const annotated = jobs
    .map((job) => {
      const anns = (byJob.get(job.id)?.annotations ?? []).slice();
      // An error the server recorded, such as a runner that stopped responding.
      if (job.error) anns.push({ level: "error", message: job.error, step: 0 });
      return { job, anns };
    })
    .filter((x) => x.anns.length > 0);
  const summarized = jobs.filter((j) => (byJob.get(j.id)?.summaries.length ?? 0) > 0);

  return (
    <>
      {annotated.length > 0 && <Annotations runId={runId} items={annotated} />}
      {summarized.map((job) => (
        <section className="box" key={job.id}>
          <div className="box-header">
            <h3>
              <Link to={`/runs/${runId}/jobs/${job.id}`}>{job.name}</Link> summary
            </h3>
          </div>
          <div className="summary-body">
            {byJob.get(job.id)!.summaries.map((s, i) => (
              <Markdown key={i} source={s.markdown} />
            ))}
          </div>
        </section>
      ))}
    </>
  );
}

function Annotations({ runId, items }: { runId: number; items: { job: Job; anns: Annotation[] }[] }) {
  const count = { error: 0, warning: 0, notice: 0 };
  for (const { anns } of items) for (const a of anns) count[a.level]++;
  const parts = (["error", "warning", "notice"] as const)
    .filter((l) => count[l] > 0)
    .map((l) => `${count[l]} ${l}${count[l] === 1 ? "" : "s"}`);
  return (
    <section className="box">
      <div className="box-header">
        <h3>Annotations</h3>
        <span className="muted small">{parts.join(", ")}</span>
      </div>
      <ul className="annotations">
        {items.flatMap(({ job, anns }) =>
          anns.map((a, i) => {
            const step = job.steps?.[a.step]?.name;
            return (
              <li key={`${job.id}-${i}`} className={classes("annotation", `annotation-${a.level}`)}>
                <AnnotationIcon level={a.level} />
                <div className="annotation-body">
                  <div className="small">
                    <Link to={logLink(runId, job.id, a)}>
                      <strong>{job.name}</strong>
                      {a.log_line && step ? <span className="muted"> · {step}</span> : null}
                    </Link>
                    {a.file && (
                      <span className="mono muted annotation-file">
                        {a.file}
                        {a.line ? `#L${a.line}${a.end_line && a.end_line !== a.line ? `-L${a.end_line}` : ""}` : ""}
                      </span>
                    )}
                  </div>
                  {a.title && <div className="annotation-title">{a.title}</div>}
                  <div className="annotation-message">{a.message}</div>
                </div>
              </li>
            );
          }),
        )}
      </ul>
    </section>
  );
}

function AnnotationIcon({ level }: { level: Annotation["level"] }) {
  const label = { error: "Error", warning: "Warning", notice: "Notice" }[level];
  return (
    <svg className="annotation-icon" width={16} height={16} viewBox="0 0 16 16" role="img" aria-label={label}>
      <title>{label}</title>
      {level === "error" ? (
        <>
          <circle cx="8" cy="8" r="8" fill="currentColor" />
          <path d="M5.5 5.5l5 5M10.5 5.5l-5 5" stroke="var(--icon-fg)" strokeWidth="1.7" strokeLinecap="round" />
        </>
      ) : level === "warning" ? (
        <>
          <path d="M8 1.2 15.2 14.4H.8z" fill="currentColor" strokeLinejoin="round" stroke="currentColor" strokeWidth="1.2" />
          <path d="M8 6v4" stroke="var(--icon-fg)" strokeWidth="1.6" strokeLinecap="round" />
          <circle cx="8" cy="12.2" r="0.9" fill="var(--icon-fg)" />
        </>
      ) : (
        <>
          <circle cx="8" cy="8" r="8" fill="currentColor" />
          <path d="M8 7.2v4.4" stroke="var(--icon-fg)" strokeWidth="1.6" strokeLinecap="round" />
          <circle cx="8" cy="4.8" r="0.95" fill="var(--icon-fg)" />
        </>
      )}
    </svg>
  );
}
