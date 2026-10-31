import { createContext, memo, useCallback, useContext, useEffect, useLayoutEffect, useMemo, useRef, useState } from "react";
import { useLocation, useNavigate } from "react-router-dom";
import { ansiToSegments, stripAnsi, type Segment } from "../ansi";
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
  // The whole log has arrived (the stream ended with the job done).
  const [complete, setComplete] = useState(false);
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
      setComplete(true);
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
  const atBottom = useRef(running && !parseLineHash(window.location.hash));
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

  // A link to lines (#step:3:42): open their step, then scroll to them once
  // they have arrived, which for a long log may be some pages in.
  const location = useLocation();
  const navigate = useNavigate();
  const target = useMemo(() => parseLineHash(location.hash), [location.hash]);
  const scrolledTo = useRef<string>();
  useEffect(() => {
    if (target) setOverrides((o) => (o[target.step] ? o : { ...o, [target.step]: true }));
  }, [target]);
  const targetLines = target ? (logs.byStep[target.step]?.length ?? 0) : 0;
  const targetMissing = !!target && complete && (target.from > targetLines || !visible.some((v) => v.index === target.step));
  useLayoutEffect(() => {
    if (!target || scrolledTo.current === location.hash) return;
    const el = document.getElementById(target.from ? lineID(target.step, target.from) : `step-${target.step}`);
    if (!el) return;
    scrolledTo.current = location.hash;
    atBottom.current = false;
    el.scrollIntoView({ block: target.from ? "center" : "start" });
  });
  const onSelect = useCallback(
    (step: number, n: number, extend: boolean) => {
      const cur = parseLineHash(window.location.hash);
      const hash = extend && cur && cur.step === step && cur.from ? lineHash(step, Math.min(cur.from, n), Math.max(cur.to, n)) : lineHash(step, n);
      // The line is already on screen: don't scroll to it.
      scrolledTo.current = hash;
      navigate({ hash }, { replace: true });
    },
    [navigate],
  );

  // Search: every matching line, in the order shown; the current one is
  // unfolded and scrolled to.
  const [search, setSearch] = useState("");
  const [query, setQuery] = useState("");
  useEffect(() => {
    const t = setTimeout(() => setQuery(search), 150);
    return () => clearTimeout(t);
  }, [search]);
  const matches = useMemo(() => findMatches(visible, logs.byStep, query), [visible, logs.byStep, query]);
  const [matchIndex, setMatchIndex] = useState(0);
  useEffect(() => setMatchIndex(0), [query]);
  const current = matches.length ? matches[Math.min(matchIndex, matches.length - 1)] : undefined;
  const currentKey = current && `${current.step}:${current.n}`;
  useEffect(() => {
    if (current) setOverrides((o) => (o[current.step] ? o : { ...o, [current.step]: true }));
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [currentKey]);
  const scrolledToMatch = useRef<string>();
  useLayoutEffect(() => {
    if (!current || scrolledToMatch.current === currentKey) return;
    const el = document.getElementById(lineID(current.step, current.n));
    if (!el) return;
    scrolledToMatch.current = currentKey;
    atBottom.current = false;
    el.scrollIntoView({ block: "center" });
  });
  const moveMatch = (by: number) => {
    if (!matches.length) return;
    scrolledToMatch.current = undefined;
    setMatchIndex((i) => (Math.min(i, matches.length - 1) + by + matches.length) % matches.length);
  };

  const lineView = useMemo(() => ({ showTs, onSelect, query }), [showTs, onSelect, query]);

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
          <div className="log-search">
            <Icon name="search" size={14} />
            <input
              type="search"
              placeholder="Search logs"
              aria-label="Search logs"
              value={search}
              onChange={(e) => setSearch(e.target.value)}
              onKeyDown={(e) => {
                if (e.key === "Enter") {
                  e.preventDefault();
                  setQuery(search);
                  if (query === search) moveMatch(e.shiftKey ? -1 : 1);
                } else if (e.key === "Escape") {
                  setSearch("");
                  setQuery("");
                }
              }}
            />
            {query && (
              <>
                <span className="log-search-count muted small">
                  {matches.length ? `${Math.min(matchIndex, matches.length - 1) + 1}/${matches.length}${matches.length >= maxMatches ? "+" : ""}` : "0/0"}
                </span>
                <button type="button" className="btn-icon log-search-btn" onClick={() => moveMatch(-1)} disabled={!matches.length} aria-label="Previous match">
                  <Icon name="chevron-up" size={12} />
                </button>
                <button type="button" className="btn-icon log-search-btn" onClick={() => moveMatch(1)} disabled={!matches.length} aria-label="Next match">
                  <Icon name="chevron-down" size={12} />
                </button>
              </>
            )}
          </div>
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
      {targetMissing && target && (
        <div className="flash flash-warn">
          {target.from ? `Line ${target.from} of step ${target.step + 1}` : `Step ${target.step + 1}`} isn't in this job's log.
        </div>
      )}

      <LineViewContext.Provider value={lineView}>
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
                  onToggle={() => toggle(index, open)}
                  marked={target?.step === index && target.from ? ([target.from, target.to] as const) : undefined}
                  reveal={current?.step === index ? current.n : target?.step === index && target.from ? target.from : undefined}
                current={current?.step === index ? current.n : undefined}
                />
              );
            })
          )}
        </div>
      </LineViewContext.Provider>

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

/** What every log line needs from the view, besides its own state. */
interface LineView {
  showTs: boolean;
  /** Search text to highlight. */
  query: string;
  /** A click on a line number: links to the line, or with shift to a range from the linked line. */
  onSelect: (step: number, n: number, extend: boolean) => void;
}

const LineViewContext = createContext<LineView>({ showTs: false, query: "", onSelect: () => {} });

const StepSection = memo(function StepSection({
  index,
  name,
  state,
  duration,
  lines,
  open,
  onToggle,
  marked,
  reveal,
  current,
}: {
  index: number;
  name: string;
  state: ReturnType<typeof stepState>;
  duration: string;
  lines: LogLine[] | undefined;
  open: boolean;
  onToggle: () => void;
  /** Lines linked to (first and last line number). */
  marked?: readonly [number, number];
  /** A line to show: the group it's in is unfolded. */
  reveal?: number;
  /** The current search match's line. */
  current?: number;
}) {
  const blocks = useMemo(() => logBlocks(lines ?? []), [lines]);
  const [groupOverrides, setGroupOverrides] = useState<Record<number, boolean>>({});
  // Groups start folded, except one still being written to.
  const groupOpen = (g: LogGroupBlock) =>
    groupOverrides[g.header.seq] ?? (state === "in_progress" && !g.ended && g === blocks[blocks.length - 1]);
  // Unfold the group of a line being revealed, even if it was folded by hand.
  const revealed = reveal !== undefined ? blocks.find((b): b is LogGroupBlock => b.kind === "group" && b.n < reveal && reveal <= b.last) : undefined;
  useEffect(() => {
    if (revealed) setGroupOverrides((o) => (o[revealed.header.seq] ? o : { ...o, [revealed.header.seq]: true }));
  }, [revealed]);
  const isMarked = (n: number) => !!marked && marked[0] <= n && n <= marked[1];
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
                  <LogLineRow key={b.line.seq} step={index} n={b.n} line={b.line} marked={isMarked(b.n)} current={current === b.n} />
                ) : (
                  <div key={b.header.seq} className={classes("log-group-block", groupOpen(b) && "log-group-open")}>
                    <LogLineRow
                      step={index}
                      n={b.n}
                      line={b.header}
                      marked={isMarked(b.n)}
                      current={current === b.n}
                      open={groupOpen(b)}
                      onToggle={() => setGroupOverrides((o) => ({ ...o, [b.header.seq]: !groupOpen(b) }))}
                    />
                    {groupOpen(b) && b.lines.map((l) => (
                        <LogLineRow key={l.line.seq} step={index} n={l.n} line={l.line} marked={isMarked(l.n)} current={current === l.n} />
                      ))}
                  </div>
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

interface LogGroupBlock {
  kind: "group";
  header: LogLine;
  n: number;
  lines: NumberedLine[];
  /** Number of the group's last line (its ::endgroup:: if it has one). */
  last: number;
  ended: boolean;
}

type LogBlock = ({ kind: "line" } & NumberedLine) | LogGroupBlock;

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
  let group: LogGroupBlock | null = null;
  lines.forEach((line, i) => {
    const n = i + 1;
    if (isGroupStart(line.text)) {
      group = { kind: "group", header: line, n, lines: [], last: n, ended: false };
      out.push(group);
    } else if (group && isGroupEnd(line.text)) {
      group.ended = true;
      group.last = n;
      group = null;
    } else if (group) {
      group.lines.push({ line, n });
      group.last = n;
    } else {
      out.push({ kind: "line", line, n });
    }
  });
  return out;
}

/**
 * A link to lines of a job's log: #step:S:L or #step:S:L-M, with S the
 * step's number (from 1) and L the line's within the step, as on GitHub.
 */
export interface LineTarget {
  step: number; // index into the job's steps
  from: number;
  to: number;
}

export function parseLineHash(hash: string): LineTarget | undefined {
  const m = /^#step:(-?\d+)(?::(\d+)(?:-(\d+))?)?$/.exec(hash);
  if (!m) return undefined;
  const from = m[2] ? Number(m[2]) : 0;
  const to = m[3] ? Number(m[3]) : from;
  return { step: Number(m[1]) - 1, from: Math.min(from, to), to: Math.max(from, to) };
}

export function lineHash(step: number, from: number, to = from): string {
  return `#step:${step + 1}` + (from ? `:${from}${to !== from ? `-${to}` : ""}` : "");
}

export function lineID(step: number, n: number): string {
  return `step-${step}-L${n}`;
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

// Stop counting search matches after this many.
const maxMatches = 10000;

interface Match {
  step: number;
  n: number;
}

/** Lines whose shown text contains query (ignoring case), in the order shown. */
function findMatches(steps: { index: number }[], byStep: Record<number, LogLine[]>, query: string): Match[] {
  const q = query.toLowerCase();
  const out: Match[] = [];
  if (!q) return out;
  for (const { index } of steps) {
    const lines = byStep[index] ?? [];
    for (let i = 0; i < lines.length; i++) {
      const { cls, text } = classify(lines[i].text);
      if (cls === "log-endgroup") continue;
      if (stripAnsi(text).toLowerCase().includes(q)) {
        out.push({ step: index, n: i + 1 });
        if (out.length >= maxMatches) return out;
      }
    }
  }
  return out;
}

/** Splits segments where they match query (ignoring case), marking the matches. */
function highlight(segs: Segment[], query: string): (Segment & { match?: boolean })[] {
  if (!query) return segs;
  const q = query.toLowerCase();
  const plain = segs.map((s) => s.text).join("").toLowerCase();
  const ranges: [number, number][] = [];
  for (let i = plain.indexOf(q); i >= 0; i = plain.indexOf(q, i + q.length)) ranges.push([i, i + q.length]);
  if (!ranges.length) return segs;
  const out: (Segment & { match?: boolean })[] = [];
  let pos = 0;
  let r = 0;
  for (const seg of segs) {
    const end = pos + seg.text.length;
    let at = pos;
    while (at < end) {
      while (r < ranges.length && ranges[r][1] <= at) r++;
      const inMatch = r < ranges.length && ranges[r][0] <= at;
      const stop = r >= ranges.length ? end : Math.min(end, inMatch ? ranges[r][1] : ranges[r][0]);
      out.push({ ...seg, text: seg.text.slice(at - pos, stop - pos), match: inMatch });
      at = stop;
    }
    pos = end;
  }
  return out;
}

function fmtTs(ts: number): string {
  const d = new Date(ts);
  return d.toLocaleTimeString(undefined, { hour12: false }) + "." + String(d.getMilliseconds()).padStart(3, "0");
}

const LogLineRow = memo(function LogLineRow({
  step,
  n,
  line,
  marked,
  current,
  onToggle,
  open,
}: {
  step: number;
  n: number;
  line: LogLine;
  marked: boolean;
  current: boolean;
  /** Set for a group's header: folds or unfolds the group. */
  onToggle?: () => void;
  open?: boolean;
}) {
  const { showTs, onSelect, query } = useContext(LineViewContext);
  const { cls, text } = classify(line.text);
  if (cls === "log-endgroup") return null;
  const segs = highlight(ansiToSegments(text), query);
  return (
    <div className={classes("log-line", cls, onToggle && "log-group-header", marked && "log-marked", current && "log-current")} id={lineID(step, n)} onClick={onToggle}>
      <a
        className="ln"
        href={lineHash(step, n)}
        data-n={n}
        aria-label={`Line ${n}`}
        onClick={(e) => {
          e.preventDefault();
          e.stopPropagation();
          onSelect(step, n, e.shiftKey);
        }}
      />
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
          s.match ? (
            <mark key={i} className={s.cls} style={s.style}>
              {s.text}
            </mark>
          ) : s.cls || s.style ? (
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
