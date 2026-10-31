import type { Access, Me } from "./types";

/** Whether repo is one of patterns: "owner/name", or "owner/*" for all of owner's. */
export function matchRepoPatterns(patterns: string[], repo: string): boolean {
  const r = repo.toLowerCase();
  const owner = r.split("/")[0];
  return patterns.some((p) => {
    const l = p.toLowerCase();
    return l.endsWith("/*") ? l.slice(0, -2) === owner : l === r;
  });
}

export function canView(a: Access | undefined, repo: string): boolean {
  return !!a && (a.repos === null || a.repos.some((r) => r.toLowerCase() === repo.toLowerCase()));
}

/** Whether a may change repo's settings and secrets, and start, re-run and cancel its runs. */
export function canMaintain(a: Access | undefined, repo: string): boolean {
  if (!a || !canView(a, repo) || (a.role !== "admin" && a.role !== "maintainer")) return false;
  return a.maintain_all || matchRepoPatterns(a.maintain, repo);
}

export function isAdmin(me: Me | null | undefined): boolean {
  return !!me?.access.admin;
}

/** "an admin", "a maintainer of t/a, t/*", ... */
export function describeAccess(a: Access): string {
  let s = "a viewer";
  if (a.admin) s = "an admin";
  else if (a.role === "maintainer" && a.maintain_all) s = "a maintainer of every repository";
  else if (a.role === "maintainer") s = `a maintainer of ${a.maintain.join(", ")}`;
  if (a.repos) s += `, seeing only ${a.repos.join(", ")}`;
  return s;
}
