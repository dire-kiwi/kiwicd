import { useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { errorMessage, get } from "../api";
import { Empty, ErrorBox, Spinner } from "../components/ui";
import { useDocumentTitle } from "../hooks";
import type { AuditEntry } from "../types";
import { absoluteTime, relativeTime } from "../util";

interface Page {
  entries: AuditEntry[];
  more: boolean;
}

/** The audit log: who changed secrets, variables, repositories' settings, runners, roles and tokens. Admins only. */
export function AuditPage() {
  useDocumentTitle("Audit log");
  const [entries, setEntries] = useState<AuditEntry[]>();
  const [more, setMore] = useState(false);
  const [repo, setRepo] = useState("");
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string>();

  const load = async (before?: number) => {
    setLoading(true);
    setError(undefined);
    const q = new URLSearchParams({ limit: "100" });
    if (repo.trim()) q.set("repo", repo.trim());
    if (before) q.set("before", String(before));
    try {
      const page = await get<Page>(`/api/audit?${q}`);
      setEntries((prev) => (before && prev ? [...prev, ...page.entries] : page.entries));
      setMore(page.more);
    } catch (e) {
      setError(errorMessage(e));
    } finally {
      setLoading(false);
    }
  };

  useEffect(() => {
    void load();
    // Only when the filter changes: load is a new function every render.
  }, [repo]);

  return (
    <div className="page">
      <div className="page-header">
        <div>
          <h1>Audit log</h1>
          <p className="muted">
            Who changed secrets, variables, repositories' settings, uploaded workflows, runners, roles, API tokens and the GitHub
            App. Secrets' values are never recorded.
          </p>
        </div>
      </div>
      <div className="stack">
        <label className="field">
          <span className="label">Repository</span>
          <input className="input mono" value={repo} onChange={(e) => setRepo(e.target.value)} placeholder="All, or owner/name" />
        </label>
        {error && <ErrorBox onRetry={() => load()}>{error}</ErrorBox>}
        {!entries && loading && <Spinner />}
        {entries && entries.length === 0 && <Empty title="Nothing recorded yet" />}
        {entries && entries.length > 0 && (
          <div className="box table-box">
            <table className="table">
              <thead>
                <tr>
                  <th>When</th>
                  <th>Who</th>
                  <th>What</th>
                  <th>Detail</th>
                </tr>
              </thead>
              <tbody>
                {entries.map((e) => (
                  <tr key={e.id}>
                    <td data-label="When" className="small nowrap" title={absoluteTime(e.at)}>
                      {relativeTime(e.at)}
                    </td>
                    <td data-label="Who" className="small">
                      <strong>{e.actor}</strong>
                      {e.token && <div className="muted">token {e.token}</div>}
                      {e.client && <div className="muted mono">{e.client}</div>}
                    </td>
                    <td data-label="What" className="small">
                      <code>{e.action}</code>
                      {e.repo && (
                        <>
                          {" "}
                          <Link to={`/repos/${e.repo}`}>{e.repo}</Link>
                        </>
                      )}
                      {e.target && <div className="mono">{e.target}</div>}
                    </td>
                    <td data-label="Detail" className="small pre-wrap">
                      {e.detail}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
        {more && entries && (
          <div className="inline-actions">
            <button className="btn" onClick={() => load(entries[entries.length - 1].id)} disabled={loading}>
              {loading ? "Loading…" : "Older"}
            </button>
          </div>
        )}
      </div>
    </div>
  );
}
