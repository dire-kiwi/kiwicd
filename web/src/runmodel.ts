import type { Job, PlanEntry, Run } from "./types";
import { visualState, type VisualState } from "./util";

export interface PlanGroup {
  key: string;
  name: string;
  needs: string[];
  jobs: Job[];
  /** Expanded from a matrix (or several jobs share the key). */
  matrix: boolean;
  state: VisualState;
  depth: number;
}

export function jobState(j: Job): VisualState {
  return visualState(j.status, j.conclusion, j.cancel_requested);
}

export function aggregateState(states: VisualState[]): VisualState {
  if (states.length === 0) return "pending";
  const done = (s: VisualState) => s === "success" || s === "failure" || s === "cancelled" || s === "skipped";
  if (!states.every(done)) {
    if (states.some((s) => s === "in_progress" || s === "cancelling")) return "in_progress";
    if (states.some((s) => s === "queued")) return "queued";
    if (states.some(done)) return "in_progress";
    return "pending";
  }
  if (states.includes("failure")) return "failure";
  if (states.includes("cancelled")) return "cancelled";
  if (states.includes("success")) return "success";
  return "skipped";
}

/** Group jobs by plan entry, in plan order, with dependency depth. */
export function buildGroups(run: Run, plan: PlanEntry[], jobs: Job[]): PlanGroup[] {
  const byKey = new Map<string, Job[]>();
  for (const j of jobs.slice().sort((a, b) => a.id - b.id)) {
    const list = byKey.get(j.key) ?? [];
    list.push(j);
    byKey.set(j.key, list);
  }
  const entries: PlanEntry[] = plan.slice();
  // Jobs whose key isn't in the plan (shouldn't happen) still get shown.
  for (const key of byKey.keys()) {
    if (!entries.some((e) => e.key === key)) entries.push({ key, name: byKey.get(key)![0].name, needs: [] });
  }
  const needsOf = new Map(entries.map((e) => [e.key, (e.needs ?? []).filter((n) => entries.some((x) => x.key === n))]));
  const depth = new Map<string, number>();
  const visiting = new Set<string>();
  const depthOf = (key: string): number => {
    const d = depth.get(key);
    if (d !== undefined) return d;
    if (visiting.has(key)) return 0; // cycle guard
    visiting.add(key);
    const needs = needsOf.get(key) ?? [];
    const v = needs.length ? 1 + Math.max(...needs.map(depthOf)) : 0;
    visiting.delete(key);
    depth.set(key, v);
    return v;
  };
  // A re-run's kept matrix jobs come before the ones it runs again: show
  // them in the matrix's order, by name.
  for (const list of byKey.values()) list.sort((a, b) => a.name.localeCompare(b.name, undefined, { numeric: true }));
  return entries.map((e) => {
    const js = byKey.get(e.key) ?? [];
    let state: VisualState;
    if (js.length) state = aggregateState(js.map(jobState));
    else if (run.status === "completed") state = run.conclusion === "cancelled" ? "cancelled" : "skipped";
    else state = "pending";
    return {
      key: e.key,
      name: e.name || e.key,
      needs: needsOf.get(e.key) ?? [],
      jobs: js,
      matrix: js.length > 1 || (js.length === 1 && !!js[0].matrix && Object.keys(js[0].matrix).length > 0),
      state,
      depth: depthOf(e.key),
    };
  });
}

export function matrixLabel(j: Job): string {
  if (!j.matrix) return "";
  return Object.entries(j.matrix)
    .map(([k, v]) => `${k}: ${typeof v === "object" ? JSON.stringify(v) : String(v)}`)
    .join(", ");
}
