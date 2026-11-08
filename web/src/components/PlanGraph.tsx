import { useLayoutEffect, useRef, useState } from "react";
import { Link } from "react-router-dom";
import { jobState, matrixLabel, type CalledItem, type PlanGroup } from "../runmodel";
import { elapsed } from "../util";
import { StatusIcon } from "./StatusIcon";

/** Jobs laid out in columns by dependency depth, with connecting curves. */
export function PlanGraph({ runId, groups, now }: { runId: number; groups: PlanGroup[]; now: number }) {
  const wrap = useRef<HTMLDivElement>(null);
  const cards = useRef(new Map<string, HTMLDivElement>());
  const [edges, setEdges] = useState<{ d: string; key: string; active: boolean }[]>([]);
  const [size, setSize] = useState({ w: 0, h: 0 });

  const maxDepth = Math.max(0, ...groups.map((g) => g.depth));
  const columns: PlanGroup[][] = Array.from({ length: maxDepth + 1 }, () => []);
  for (const g of groups) columns[g.depth].push(g);

  const layoutKey = groups.map((g) => `${g.key}:${g.jobs.length}:${g.items.length}:${g.needs.join(",")}`).join("|");

  useLayoutEffect(() => {
    const el = wrap.current;
    if (!el) return;
    const measure = () => {
      const base = el.getBoundingClientRect();
      const out: { d: string; key: string; active: boolean }[] = [];
      for (const g of groups) {
        const to = cards.current.get(g.key);
        if (!to) continue;
        const tr = to.getBoundingClientRect();
        const tx = tr.left - base.left + el.scrollLeft;
        const ty = tr.top - base.top + 22;
        for (const n of g.needs) {
          const from = cards.current.get(n);
          if (!from) continue;
          const fr = from.getBoundingClientRect();
          const fx = fr.right - base.left + el.scrollLeft;
          const fy = fr.top - base.top + 22;
          const mx = (fx + tx) / 2;
          out.push({ key: `${n}->${g.key}`, d: `M${fx},${fy} C${mx},${fy} ${mx},${ty} ${tx},${ty}`, active: g.state === "in_progress" });
        }
      }
      setEdges(out);
      setSize({ w: el.scrollWidth, h: el.scrollHeight });
    };
    measure();
    const ro = new ResizeObserver(measure);
    ro.observe(el);
    cards.current.forEach((c) => ro.observe(c));
    return () => ro.disconnect();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [layoutKey, groups]);

  return (
    <div className="graph" ref={wrap}>
      <svg className="graph-edges" width={size.w} height={size.h} aria-hidden="true">
        {edges.map((e) => (
          <g key={e.key}>
            <path d={e.d} className={e.active ? "edge edge-active" : "edge"} />
          </g>
        ))}
      </svg>
      {columns.map((col, i) => (
        <div className="graph-col" key={i}>
          {col.map((g) => (
            <div
              key={g.key}
              className="graph-card"
              ref={(el) => {
                if (el) cards.current.set(g.key, el);
                else cards.current.delete(g.key);
              }}
            >
              {g.uses ? (
                <>
                  <div className="graph-group-title" title={g.uses}>
                    <StatusIcon state={g.state} size={14} />
                    <span className="ellipsis">{g.name}</span>
                    <span className="muted small">workflow</span>
                  </div>
                  {g.items.map((item) => (
                    <CalledJob key={item.key} runId={runId} item={item} now={now} />
                  ))}
                </>
              ) : g.jobs.length === 0 ? (
                <div className="graph-job graph-job-pending">
                  <StatusIcon state={g.state} size={14} />
                  <span className="ellipsis">{g.name}</span>
                </div>
              ) : g.matrix ? (
                <>
                  <div className="graph-group-title">
                    <StatusIcon state={g.state} size={14} />
                    <span className="ellipsis">{g.name}</span>
                    <span className="muted small">{g.jobs.length} jobs</span>
                  </div>
                  {g.jobs.map((j) => (
                    <Link key={j.id} to={`/runs/${runId}/jobs/${j.id}`} className="graph-job graph-job-sub" title={matrixLabel(j)}>
                      <StatusIcon state={jobState(j)} size={14} />
                      <span className="ellipsis">{j.name}</span>
                      <span className="muted small">{elapsed(j.started_at, j.finished_at, now)}</span>
                    </Link>
                  ))}
                </>
              ) : (
                <Link to={`/runs/${runId}/jobs/${g.jobs[0].id}`} className="graph-job">
                  <StatusIcon state={jobState(g.jobs[0])} size={14} />
                  <span className="ellipsis">{g.jobs[0].name}</span>
                  <span className="muted small">{elapsed(g.jobs[0].started_at, g.jobs[0].finished_at, now)}</span>
                </Link>
              )}
            </div>
          ))}
        </div>
      ))}
    </div>
  );
}

/** A job of a called workflow in its calling job's card. */
function CalledJob({ runId, item, now }: { runId: number; item: CalledItem; now: number }) {
  const indent = { paddingLeft: 10 + item.depth * 14 };
  const j = item.job;
  if (!j) {
    return (
      <div className={item.caller ? "graph-job graph-job-sub graph-job-caller graph-job-pending" : "graph-job graph-job-sub graph-job-pending"} style={indent}>
        <StatusIcon state="pending" size={14} />
        <span className="ellipsis">{item.label}</span>
      </div>
    );
  }
  if (item.caller) {
    return (
      <div className="graph-job graph-job-sub graph-job-caller" style={indent} title={j.uses}>
        <StatusIcon state={jobState(j)} size={14} />
        <span className="ellipsis">{item.label}</span>
        <span className="muted small">{elapsed(j.started_at, j.finished_at, now)}</span>
      </div>
    );
  }
  return (
    <Link to={`/runs/${runId}/jobs/${j.id}`} className="graph-job graph-job-sub" style={indent} title={matrixLabel(j) || j.name}>
      <StatusIcon state={jobState(j)} size={14} />
      <span className="ellipsis">{item.label}</span>
      <span className="muted small">{elapsed(j.started_at, j.finished_at, now)}</span>
    </Link>
  );
}
