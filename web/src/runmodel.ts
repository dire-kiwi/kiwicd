import type { Job, PlanEntry, Run } from "./types";
import { visualState, type VisualState } from "./util";

export interface PlanGroup {
  key: string;
  name: string;
  needs: string[];
  /** The rows of the run's workflow's job: one per matrix combination. */
  jobs: Job[];
  /** Expanded from a matrix (or several jobs share the key). */
  matrix: boolean;
  state: VisualState;
  depth: number;
  /** For a job that calls a reusable workflow: what it calls. */
  uses?: string;
  /** The called workflows' jobs under each calling row, or their plan before there are rows. */
  items: CalledItem[];
}

/** A job in a called workflow, under the job calling it. */
export interface CalledItem {
  key: string;
  /** Its name without the calling jobs' names. */
  label: string;
  /** Nesting below the group: 0 for a calling row of the group itself. */
  depth: number;
  /** A calling job: shown as a heading of the jobs below it. */
  caller: boolean;
  /** Its row; none for a planned job that hasn't started. */
  job?: Job;
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

/** The rows under a calling row, depth first, with calling rows as headings. */
function calledItems(caller: Job, children: Map<number, Job[]>, depth: number): CalledItem[] {
  const out: CalledItem[] = [];
  for (const j of children.get(caller.id) ?? []) {
    const prefix = caller.name + " / ";
    const label = j.name.startsWith(prefix) ? j.name.slice(prefix.length) : j.name;
    out.push({ key: `job-${j.id}`, label, depth, caller: !!j.uses, job: j });
    if (j.uses) out.push(...calledItems(j, children, depth + 1));
  }
  return out;
}

/** A called workflow's planned jobs, before the calling job has rows. */
function plannedItems(entries: PlanEntry[], depth: number, prefix: string): CalledItem[] {
  return entries.flatMap((e) => [
    { key: `plan-${prefix}${e.key}`, label: e.name || e.key, depth, caller: !!e.uses },
    ...plannedItems(e.jobs ?? [], depth + 1, `${prefix}${e.key}/`),
  ]);
}

/** Every row of a group: its own and those of the workflows it calls. */
export function groupJobs(g: PlanGroup): Job[] {
  if (!g.uses) return g.jobs;
  return g.items.flatMap((i) => (i.job ? [i.job] : []));
}

/** Group jobs by plan entry, in plan order, with dependency depth. */
export function buildGroups(run: Run, plan: PlanEntry[], all: Job[]): PlanGroup[] {
  const sorted = all.slice().sort((a, b) => a.id - b.id);
  // Jobs of called workflows go under their calling rows.
  const children = new Map<number, Job[]>();
  for (const j of sorted) {
    if (!j.parent_id) continue;
    const list = children.get(j.parent_id) ?? [];
    list.push(j);
    children.set(j.parent_id, list);
  }
  const jobs = sorted.filter((j) => !j.parent_id);
  const byKey = new Map<string, Job[]>();
  for (const j of jobs) {
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
    const matrix = js.length > 1 || (js.length === 1 && !!js[0].matrix && Object.keys(js[0].matrix).length > 0);
    let items: CalledItem[] = [];
    if (e.uses) {
      items = js.length
        ? js.flatMap((j) => [
            // A calling row heads its jobs when there are several.
            ...(matrix ? [{ key: `job-${j.id}`, label: j.name, depth: 0, caller: true, job: j }] : []),
            ...calledItems(j, children, matrix ? 1 : 0),
          ])
        : plannedItems(e.jobs ?? [], 0, "");
    }
    return {
      key: e.key,
      name: e.name || e.key,
      needs: needsOf.get(e.key) ?? [],
      jobs: js,
      matrix,
      state,
      depth: depthOf(e.key),
      uses: e.uses,
      items,
    };
  });
}

export function matrixLabel(j: Job): string {
  if (!j.matrix) return "";
  return Object.entries(j.matrix)
    .map(([k, v]) => `${k}: ${typeof v === "object" ? JSON.stringify(v) : String(v)}`)
    .join(", ");
}
