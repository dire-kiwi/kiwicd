import type { Conclusion, Permissions, Status } from "./types";

export function formatDuration(ms: number): string {
  if (!isFinite(ms) || ms < 0) ms = 0;
  let s = Math.floor(ms / 1000);
  const h = Math.floor(s / 3600);
  s -= h * 3600;
  const m = Math.floor(s / 60);
  s -= m * 60;
  if (h > 0) return `${h}h ${m}m ${s}s`;
  if (m > 0) return `${m}m ${s}s`;
  return `${s}s`;
}

/** Duration between start and end (or now when still running); "" when not started. */
export function elapsed(start: number, end: number, now = Date.now()): string {
  if (!start) return "";
  return formatDuration((end || now) - start);
}

const rtf = typeof Intl !== "undefined" ? new Intl.RelativeTimeFormat(undefined, { numeric: "auto" }) : null;

export function relativeTime(ts: number, now = Date.now()): string {
  if (!ts) return "";
  const diff = (ts - now) / 1000; // negative = past
  const abs = Math.abs(diff);
  if (abs < 45) return diff <= 0 ? "just now" : "in a few seconds";
  const units: [Intl.RelativeTimeFormatUnit, number][] = [
    ["minute", 60],
    ["hour", 3600],
    ["day", 86400],
    ["week", 604800],
    ["month", 2629800],
    ["year", 31557600],
  ];
  let unit: Intl.RelativeTimeFormatUnit = "minute";
  let div = 60;
  for (let i = 0; i < units.length; i++) {
    const next = units[i + 1];
    unit = units[i][0];
    div = units[i][1];
    if (!next || abs < next[1] * 0.95) break;
  }
  const v = Math.round(diff / div);
  if (rtf) return rtf.format(v, unit);
  return `${Math.abs(v)} ${unit}${Math.abs(v) === 1 ? "" : "s"} ago`;
}

export function absoluteTime(ts: number): string {
  if (!ts) return "";
  return new Date(ts).toLocaleString();
}

export function refName(ref: string): string {
  if (!ref) return "";
  if (ref.startsWith("refs/heads/")) return ref.slice(11);
  if (ref.startsWith("refs/tags/")) return ref.slice(10);
  const pr = /^refs\/pull\/(\d+)\//.exec(ref);
  if (pr) return `#${pr[1]}`;
  return ref;
}

export function shortSha(sha: string): string {
  return sha ? sha.slice(0, 7) : "";
}

/** Normalised visual state used by the status icon. */
export type VisualState =
  | "success"
  | "failure"
  | "cancelled"
  | "skipped"
  | "in_progress"
  | "queued"
  | "pending"
  | "cancelling";

export function visualState(status: Status | string, conclusion: Conclusion | string, cancelRequested = false): VisualState {
  if (status === "completed") {
    switch (conclusion) {
      case "success":
        return "success";
      case "failure":
        return "failure";
      case "cancelled":
        return "cancelled";
      case "skipped":
        return "skipped";
      default:
        return "success";
    }
  }
  if (cancelRequested) return "cancelling";
  if (status === "in_progress") return "in_progress";
  if (status === "queued") return "queued";
  return "pending";
}

export function stateLabel(s: VisualState): string {
  switch (s) {
    case "success":
      return "Success";
    case "failure":
      return "Failure";
    case "cancelled":
      return "Cancelled";
    case "skipped":
      return "Skipped";
    case "in_progress":
      return "In progress";
    case "queued":
      return "Queued";
    case "pending":
      return "Waiting";
    case "cancelling":
      return "Cancelling";
  }
}

export function eventLabel(ev: string): string {
  return ev.replace(/_/g, " ");
}

export function classes(...xs: (string | false | null | undefined)[]): string {
  return xs.filter(Boolean).join(" ");
}

/** Describes a permissions: setting, e.g. "read-all" or "contents: read, packages: write". */
export function permissionsLabel(p: Permissions): string {
  if (p.all) return `${p.all}-all`;
  const scopes = Object.entries(p.scopes ?? {}).sort(([a], [b]) => a.localeCompare(b));
  if (scopes.length === 0) return "none";
  return scopes.map(([k, v]) => `${k}: ${v}`).join(", ");
}
