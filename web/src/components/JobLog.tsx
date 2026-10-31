import { memo, useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState } from "react";
import { ansiToSegments } from "../ansi";
import { useNow } from "../hooks";
import { jobState, matrixLabel } from "../runmodel";
import type { Job, LogLine, Step } from "../types";
import { absoluteTime, classes, elapsed, relativeTime, visualState } from "../util";
import { StatusIcon } from "./StatusIcon";
import { Chip, ErrorBox, Icon } from "./ui";

interface LogState {
  byStep: Record<number, LogLine[]>;
  count: number;
}

function stepState(s: Step) {
  if (s.status === "completed") return visualState("completed", s.conclusion);
  if (s.status === "in_progress") return "in_progress" as const;
  return "pending" as const;
}

/**
 * Live log view for one job: collapsible steps streamed over SSE. Runners
 * with the withheld labels won't take it, as its run's ref isn't protected.
 */
export function JobLog({ job, onJob, withheld = [] }: { job: Job; onJob: (j: unknown) => void; withheld?: string[] }) {
  const [logs, setLogs] = useState<LogState>({ byStep: {}, count: 0 });
  const [streamError, setStreamError] = useState(false);
  const [overrides, setOverrides] = useState<Record<number, boolean>>({});
  const [showTs, setShowTs] = useState(false);
  const seen = useRef(new Set<number>());
  const lastSeq = useRef(0);
  const onJobRef = useRef(onJob);
  onJobRef.current = onJob;

  const append = useCallback((lines: LogLine[]) => {
    const fresh = lines.filter((l) => !seen.current.has(l.seq));
    if (!fresh.length) return;
    for (const l of fresh) {
      seen.current.add(l.seq);
      if (l.seq > lastSeq.current) lastSeq.current = l.seq;
    }
    setLogs((prev) => {
      const byStep = { ...prev.byStep };
      const touched = new Set<number>();
      for (const l of fresh) {
        if (!touched.has(l.step)) {
          byStep[l.step] = byStep[l.step] ? byStep[l.step].slice() : [];
          touched.add(l.step);
        }
        byStep[l.step].push(l);
      }
      for (const s of touched) {
        const arr = byStep[s];
        for (let i = 1; i < arr.length; i++) {
          if (arr[i].seq < arr[i - 1].seq) {
            arr.sort((a, b) => a.seq - b.seq);
            break;
          }
        }
      }
      return { byStep, count: prev.count + fresh.length };
    });
  }, []);

  // Stream logs + job state; reconnect with ?after= on failure; stop once the job is done.
  useEffect(() => {
    let es: EventSource | null = null;
    let retry: ReturnType<typeof setTimeout> | undefined;
    let stopTimer: ReturnType<typeof setTimeout> | undefined;
    let done = false;
    let failures = 0;
    const stop = () => {
      done = true;
      es?.close();
    };
    const connect = () => {
      es = new EventSource(`/api/jobs/${job.id}/events?after=${lastSeq.current}`);
      let first = true;
      es.onopen = () => {
        failures = 0;
        setStreamError(false);
      };
      es.addEventListener("logs", (ev) => {
        try {
          append(JSON.parse((ev as MessageEvent).data) as LogLine[]);
        } catch {
          /* ignore malformed */
        }
      });
      es.addEventListener("job", (ev) => {
        let j: Job;
        try {
          j = JSON.parse((ev as MessageEvent).data) as Job;
        } catch {
          return;
        }
        onJobRef.current(j);
        if (j.status === "completed") {
          // Already finished when we connected: the backlog is complete.
          // Finished while streaming: allow trailing log lines to arrive.
          if (first) stop();
          else if (!stopTimer) stopTimer = setTimeout(stop, 3000);
        }
        first = false;
      });
      es.onerror = () => {
        es?.close();
        if (done) return;
        failures++;
        if (failures > 2) setStreamError(true);
        retry = setTimeout(connect, Math.min(1000 * 2 ** failures, 15000));
      };
    };
    connect();
    return () => {
      done = true;
      clearTimeout(retry);
      clearTimeout(stopTimer);
      es?.close();
    };
  }, [job.id, append]);

  const running = job.status !== "completed";
  const now = useNow(running);
  const st = jobState(job);
  const steps = job.steps ?? [];

  // Visible steps: named ones, plus unnamed ones that produced output.
  const visible = useMemo(() => {
    const out: { index: number; step: Step | null; name: string }[] = [];
    steps.forEach((s, i) => {
      if (s.name || (logs.byStep[i]?.length ?? 0) > 0) out.push({ index: i, step: s, name: s.name || `Step ${i + 1}` });
    });
    const extra = Object.keys(logs.byStep)
      .map(Number)
      .filter((i) => i >= steps.length || i < 0)
      .sort((a, b) => a - b);
    for (const i of extra) out.push({ index: i, step: null, name: steps.length ? "Other output" : "Output" });
    return out;
  }, [steps, logs.byStep]);

  // Follow the log tail while the user is at the bottom of the page.
  const atBottom = useRef(running);
  useEffect(() => {
    const onScroll = () => {
      atBottom.current = window.innerHeight + window.scrollY >= document.documentElement.scrollHeight - 60;
    };
    window.addEventListener("scroll", onScroll, { passive: true });
    return () => window.removeEventListener("scroll", onScroll);
  }, []);
  useLayoutEffect(() => {
    if (running && atBottom.current && logs.count > 0) {
      window.scrollTo({ top: document.documentElement.scrollHeight });
    }
  }, [logs.count, running, job.steps]);

  const isOpen = (index: number, step: Step | null) => {
    if (index in overrides) return overrides[index];
    if (!step) return true;
    return step.status === "in_progress" || (step.status === "completed" && step.conclusion === "failure");
  };
  const toggle = (index: number, open: boolean) => setOverrides((o) => ({ ...o, [index]: !open }));
  const setAll = (open: boolean) => setOverrides(Object.fromEntries(visible.map((v) => [v.index, open])));

  const outputs = Object.entries(job.outputs ?? {});
  const matrix = matrixLabel(job);
  const verb: Record<string, string> = { success: "succeeded", failure: "failed", cancelled: "was cancelled", skipped: "was skipped" };
  const statusText =
    job.status === "completed"
      ? `${verb[st] ?? "completed"} ${relativeTime(job.finished_at, now)}${job.started_at ? ` in ${elapsed(job.started_at, job.finished_at)}` : ""}`
      : job.status === "in_progress"
        ? `started ${relativeTime(job.started_at, now)}`
        : "waiting for a runner";

  return (
    <div className="job-view">
      <div className="job-header">
        <div className="job-title">
          <StatusIcon state={st} size={20} />
          <div>
            <h2>{job.name}</h2>
            <div className="muted small" title={absoluteTime(job.finished_at || job.started_at || job.created_at)}>
              {statusText}
            </div>
          </div>
        </div>
        <div className="job-tools">
          <label className="check small">
            <input type="checkbox" checked={showTs} onChange={(e) => setShowTs(e.target.checked)} /> Timestamps
          </label>
          <button className="btn btn-sm" onClick={() => setAll(true)}>
            Expand all
          </button>
          <button className="btn btn-sm" onClick={() => setAll(false)}>
            Collapse all
          </button>
          <a className="btn btn-sm" href={`/api/jobs/${job.id}/logs?format=text`} target="_blank" rel="noreferrer">
            View raw logs
          </a>
        </div>
      </div>
      <div className="job-info small">
        <span>
          <span className="muted">Runner</span> {job.runner_name ? <strong>{job.runner_name}</strong> : <span className="muted">not assigned</span>}
        </span>
        {(job.runs_on ?? []).length > 0 && (
          <span className="job-info-labels">
            <span className="muted">runs-on</span>
            {(job.runs_on ?? []).map((l) => (
              <Chip key={l}>{l}</Chip>
            ))}
          </span>
        )}
        {matrix && (
          <span>
            <span className="muted">matrix</span> <span className="mono">{matrix}</span>
          </span>
        )}
        {job.started_at > 0 && (
          <span>
            <Icon name="clock" size={13} /> {elapsed(job.started_at, job.finished_at, now)}
          </span>
        )}
      </div>
      {job.error && (
        <ErrorBox>
          <span className="pre-wrap">{job.error}</span>
        </ErrorBox>
      )}
      {job.cancel_requested && job.status !== "completed" && <div className="flash flash-warn">Cancellation requested…</div>}
      {streamError && <div className="flash flash-warn">Lost connection to the log stream — retrying…</div>}

      <div className="log-box">
        {visible.length === 0 ? (
          <div className="log-empty muted">
            {job.status === "queued" ? (
              <>
                Waiting for a runner with labels{" "}
                {(job.runs_on ?? []).map((l) => (
                  <code key={l}>{l}</code>
                ))}{" "}
                to pick up this job…
                {withheld.length > 0 && (
                  <div className="small">
                    This run's ref isn't protected, so runners with the label{withheld.length > 1 ? "s" : ""}{" "}
                    {withheld.map((l) => (
                      <code key={l}>{l}</code>
                    ))}{" "}
                    won't take it.
                  </div>
                )}
              </>
            ) : job.status === "completed" ? (
              job.conclusion === "skipped" ? (
                "This job was skipped."
              ) : (
                "This job produced no steps."
              )
            ) : (
              "Starting…"
            )}
          </div>
        ) : (
          visible.map(({ index, step, name }) => {
            const open = isOpen(index, step);
            return (
              <StepSection
                key={index}
                index={index}
                name={name}
                state={step ? stepState(step) : st}
                duration={step ? elapsed(step.started_at, step.finished_at, now) : ""}
                lines={logs.byStep[index]}
                open={open}
                showTs={showTs}
                onToggle={() => toggle(index, open)}
              />
            );
          })
        )}
      </div>

      {outputs.length > 0 && (
        <section className="box">
          <div className="box-header">
            <h3>Outputs</h3>
          </div>
          <div className="kv kv-pad">
            {outputs.map(([k, v]) => (
              <OutputRow key={k} k={k} v={String(v)} />
            ))}
          </div>
        </section>
      )}
    </div>
  );
}

function OutputRow({ k, v }: { k: string; v: string }) {
  return (
    <>
      <span className="label mono">{k}</span>
      <span className="mono pre-wrap">{v}</span>
    </>
  );
}

const StepSection = memo(function StepSection({
  index,
  name,
  state,
  duration,
  lines,
  open,
  showTs,
  onToggle,
}: {
  index: number;
  name: string;
  state: ReturnType<typeof stepState>;
  duration: string;
  lines: LogLine[] | undefined;
  open: boolean;
  showTs: boolean;
  onToggle: () => void;
}) {
  const blocks = useMemo(() => logBlocks(lines ?? []), [lines]);
  const [groupOverrides, setGroupOverrides] = useState<Record<number, boolean>>({});
  // Groups start folded, except one still being written to.
  const groupOpen = (g: Extract<LogBlock, { kind: "group" }>) =>
    groupOverrides[g.header.seq] ?? (state === "in_progress" && !g.ended && g === blocks[blocks.length - 1]);
  return (
    <div className={classes("step", open && "step-open")} id={`step-${index}`}>
      <button className="step-header" onClick={onToggle} aria-expanded={open}>
        <span className="step-chevron">
          <Icon name="chevron" size={12} />
        </span>
        <StatusIcon state={state} size={16} />
        <span className="step-name">{name}</span>
        <span className="step-dur">{duration}</span>
      </button>
      {open && (
        <div className="step-log">
          {lines && lines.length > 0 ? (
            <div className="log-lines">
              {blocks.map((b) =>
                b.kind === "line" ? (
                  <LogLineRow key={b.line.seq} n={b.n} line={b.line} showTs={showTs} anchor={`step-${index}-L${b.n}`} />
                ) : (
                  <LogGroup
                    key={b.header.seq}
                    index={index}
                    group={b}
                    open={groupOpen(b)}
                    showTs={showTs}
                    onToggle={() => setGroupOverrides((o) => ({ ...o, [b.header.seq]: !groupOpen(b) }))}
                  />
                ),
              )}
            </div>
          ) : (
            <div className="log-none">{state === "in_progress" ? "Waiting for output…" : "No output"}</div>
          )}
        </div>
      )}
    </div>
  );
});

interface NumberedLine {
  line: LogLine;
  /** Line number within the step, counting every line. */
  n: number;
}

type LogBlock =
  | ({ kind: "line" } & NumberedLine)
  | { kind: "group"; header: LogLine; n: number; lines: NumberedLine[]; ended: boolean };

function isGroupStart(text: string) {
  return text.startsWith("##[group]") || text.startsWith("::group::");
}

function isGroupEnd(text: string) {
  return text.startsWith("##[endgroup]") || text.startsWith("::endgroup::");
}

/**
 * Splits a step's lines into ::group:: sections and the lines between them.
 * Groups don't nest: a group runs to its ::endgroup:: or the next ::group::,
 * as on GitHub.
 */
function logBlocks(lines: LogLine[]): LogBlock[] {
  const out: LogBlock[] = [];
  let group: Extract<LogBlock, { kind: "group" }> | null = null;
  lines.forEach((line, i) => {
    const n = i + 1;
    if (isGroupStart(line.text)) {
      group = { kind: "group", header: line, n, lines: [], ended: false };
      out.push(group);
    } else if (group && isGroupEnd(line.text)) {
      group.ended = true;
      group = null;
    } else if (group) {
      group.lines.push({ line, n });
    } else {
      out.push({ kind: "line", line, n });
    }
  });
  return out;
}

function LogGroup({
  index,
  group,
  open,
  showTs,
  onToggle,
}: {
  index: number;
  group: Extract<LogBlock, { kind: "group" }>;
  open: boolean;
  showTs: boolean;
  onToggle: () => void;
}) {
  return (
    <div className={classes("log-group-block", open && "log-group-open")}>
      <LogLineRow n={group.n} line={group.header} showTs={showTs} anchor={`step-${index}-L${group.n}`} onToggle={onToggle} open={open} />
      {open && group.lines.map((l) => <LogLineRow key={l.line.seq} n={l.n} line={l.line} showTs={showTs} anchor={`step-${index}-L${l.n}`} />)}
    </div>
  );
}

function classify(text: string): { cls: string; text: string } {
  if (text.startsWith("##[act]")) return { cls: "log-act", text: text.slice(7) };
  if (text.startsWith("##[error]")) return { cls: "log-error", text: text.slice(9) };
  if (text.startsWith("##[warning]")) return { cls: "log-warning", text: text.slice(11) };
  if (text.startsWith("##[notice]")) return { cls: "log-notice", text: text.slice(10) };
  if (text.startsWith("##[debug]")) return { cls: "log-debug", text: text.slice(9) };
  if (text.startsWith("##[command]")) return { cls: "log-command", text: text.slice(11) };
  if (text.startsWith("##[group]")) return { cls: "log-group", text: text.slice(9) };
  if (text.startsWith("::group::")) return { cls: "log-group", text: text.slice(9) };
  if (text.startsWith("##[endgroup]") || text.startsWith("::endgroup::")) return { cls: "log-endgroup", text: "" };
  const m = /^::(error|warning|notice|debug)(?: [^:]*)?::(.*)$/.exec(text);
  if (m) return { cls: `log-${m[1]}`, text: m[2] };
  return { cls: "", text };
}

function fmtTs(ts: number): string {
  const d = new Date(ts);
  return d.toLocaleTimeString(undefined, { hour12: false }) + "." + String(d.getMilliseconds()).padStart(3, "0");
}

const LogLineRow = memo(function LogLineRow({
  n,
  line,
  showTs,
  anchor,
  onToggle,
  open,
}: {
  n: number;
  line: LogLine;
  showTs: boolean;
  anchor: string;
  /** Set for a group's header: folds or unfolds the group. */
  onToggle?: () => void;
  open?: boolean;
}) {
  const { cls, text } = classify(line.text);
  if (cls === "log-endgroup") return null;
  const segs = ansiToSegments(text);
  return (
    <div className={classes("log-line", cls, onToggle && "log-group-header")} id={anchor} onClick={onToggle}>
      <a className="ln" href={`#${anchor}`} data-n={n} aria-label={`Line ${n}`} onClick={(e) => e.stopPropagation()} />
      {showTs && <span className="ts">{fmtTs(line.ts)}</span>}
      {onToggle && (
        <button
          type="button"
          className={classes("group-chevron", open && "group-chevron-open")}
          aria-expanded={open}
          aria-label={open ? "Collapse group" : "Expand group"}
        >
          <Icon name="chevron" size={10} />
        </button>
      )}
      <span className="lt">
        {segs.map((s, i) =>
          s.cls || s.style ? (
            <span key={i} className={s.cls} style={s.style}>
              {s.text}
            </span>
          ) : (
            s.text
          ),
        )}
      </span>
    </div>
  );
});
