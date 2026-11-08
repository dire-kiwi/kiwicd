import { useState, type FormEvent } from "react";
import { useMe } from "../App";
import { del, errorMessage, post } from "../api";
import { useFetch } from "../hooks";
import type { APIToken, Repo, Role } from "../types";
import { absoluteTime, relativeTime } from "../util";
import { CodeBlock, CopyField, Empty, ErrorBox, Flash, Modal, Spinner } from "./ui";

/** API tokens for the kiwicd CLI, its MCP server and scripts. */
export function APITokens() {
  const me = useMe();
  const list = useFetch<APIToken[]>("/api/tokens");
  // A token with a scope can't make or revoke others.
  const scoped = !!me?.access.token && (me.access.token.role !== "" || me.access.token.repos.length > 0);
  const [creating, setCreating] = useState(false);
  const [actionError, setActionError] = useState<string>();

  const revoke = async (t: APIToken) => {
    if (!window.confirm(`Revoke the token ${t.name}? Anything using it stops working.`)) return;
    setActionError(undefined);
    try {
      await del(`/api/tokens/${t.id}`);
      list.reload();
    } catch (e) {
      setActionError(errorMessage(e));
    }
  };

  return (
    <section className="box">
      <div className="box-header">
        <div>
          <h3>API tokens</h3>
          <p className="muted small">
            For the <code>kiwicd</code> command line tool, its MCP server for agents, and scripts. A token acts as whoever made it,
            with at most the role you give it, and only in the repositories you give it, if any.
            {me?.access.admin ? " You see everyone's tokens. " : " "}
            Changing the admin password revokes the admin's tokens.
          </p>
        </div>
        {!scoped && (
          <button className="btn btn-primary" onClick={() => setCreating(true)}>
            New token
          </button>
        )}
      </div>
      {actionError && <ErrorBox>{actionError}</ErrorBox>}
      {list.error && !list.data && <ErrorBox onRetry={list.reload}>{list.error}</ErrorBox>}
      {!list.data && list.loading && <Spinner />}
      {list.data && list.data.length === 0 && (
        <Empty title="No tokens yet">
          Make one for <code>kiwicd login --token</code>
          {me?.access.github ? "." : <>, or run <code>kiwicd login</code> with the admin password.</>}
        </Empty>
      )}
      {list.data && list.data.length > 0 && (
        <ul className="list">
          {list.data.map((t) => (
            <li key={t.id} className="list-row setting-row">
              <div className="setting-main">
                <span className="setting-name">{t.name}</span>
                <span className="muted small">
                  {t.user} · {scopeLabel(t)}
                </span>
              </div>
              <span className="muted small nowrap" title={t.last_used_at ? absoluteTime(t.last_used_at) : undefined}>
                {t.last_used_at ? `Used ${relativeTime(t.last_used_at)}` : "Never used"}
                {" · "}
                {t.expires_at ? <span title={absoluteTime(t.expires_at)}>Expires {absoluteTime(t.expires_at)}</span> : "Never expires"}
              </span>
              <div className="row-actions">
                {!scoped && (
                  <button className="btn btn-sm btn-danger-outline" onClick={() => revoke(t)}>
                    Revoke
                  </button>
                )}
              </div>
            </li>
          ))}
        </ul>
      )}
      {creating && (
        <NewTokenModal
          onClose={() => {
            setCreating(false);
            list.reload();
          }}
        />
      )}
    </section>
  );
}

/** "full", "read-only of t/a", ... */
export function scopeLabel(t: APIToken): string {
  const role = t.role === "" ? "full rights" : t.role === "viewer" ? "read-only" : t.role;
  return t.repos.length ? `${role}, only ${t.repos.join(", ")}` : role;
}

const roleRank: Record<Role, number> = { viewer: 1, maintainer: 2, admin: 3 };

function NewTokenModal({ onClose }: { onClose: () => void }) {
  const me = useMe();
  const github = !!me?.access.github;
  const myRole = me?.access.role ?? "viewer";
  const [name, setName] = useState("");
  const [days, setDays] = useState(90);
  const [role, setRole] = useState<Role | "">("");
  const [limit, setLimit] = useState(false);
  const [repos, setRepos] = useState<string[]>([]);
  const allRepos = useFetch<Repo[]>(limit ? "/api/repos" : null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();
  const [made, setMade] = useState<{ token: string; info: APIToken }>();

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError(undefined);
    try {
      setMade(
        await post<{ token: string; info: APIToken }>("/api/tokens", {
          name: name.trim(),
          expires_in_days: days,
          role,
          repos: limit ? repos : [],
        }),
      );
    } catch (err) {
      setError(errorMessage(err));
    }
    setBusy(false);
  };

  if (made) {
    return (
      <Modal title="New token" onClose={onClose} wide>
        <div className="stack">
          <Flash kind="info">Copy the token now: it won't be shown again.</Flash>
          <CopyField value={made.token} />
          <div>
            <h3 className="h4">Use it with the command line tool</h3>
            <CodeBlock code={`kiwicd login --url ${me?.public_url ?? window.location.origin} --token ${made.token}`} />
          </div>
          <div>
            <h3 className="h4">Or give agents the MCP server</h3>
            <CodeBlock code={`claude mcp add kiwicd -- kiwicd mcp`} />
          </div>
          <div className="form-actions">
            <button className="btn btn-primary" onClick={onClose}>
              Done
            </button>
          </div>
        </div>
      </Modal>
    );
  }

  return (
    <Modal title="New API token" onClose={onClose}>
      <form onSubmit={submit} className="form">
        {error && <ErrorBox>{error}</ErrorBox>}
        <label className="field">
          <span className="label">Name</span>
          <input
            className="input"
            value={name}
            onChange={(e) => setName(e.target.value)}
            placeholder="kiwicd CLI on my laptop"
            maxLength={100}
            autoFocus
            required
          />
        </label>
        <label className="field">
          <span className="label">Expires</span>
          <select className="input" value={days} onChange={(e) => setDays(Number(e.target.value))}>
            <option value={30}>In 30 days</option>
            <option value={90}>In 90 days</option>
            {!github && <option value={365}>In a year</option>}
            {!github && <option value={0}>Never</option>}
          </select>
          {github && <span className="hint">Tokens of people signed in with GitHub last at most 90 days.</span>}
        </label>
        <label className="field">
          <span className="label">Role</span>
          <select className="input" value={role} onChange={(e) => setRole(e.target.value as Role | "")}>
            <option value="">Yours ({myRole}), as it changes</option>
            {(["admin", "maintainer", "viewer"] as Role[])
              .filter((r) => roleRank[r] <= roleRank[myRole])
              .map((r) => (
                <option key={r} value={r}>
                  {r === "viewer" ? "Viewer: read-only" : r === "maintainer" ? "Maintainer, of what you maintain" : "Admin"}
                </option>
              ))}
          </select>
          <span className="hint">A token never has more than you do, even if your role changes later.</span>
        </label>
        <label className="check">
          <input type="checkbox" checked={limit} onChange={(e) => setLimit(e.target.checked)} /> Only some repositories
        </label>
        {limit && (
          <div className="field">
            {allRepos.error && <ErrorBox>{allRepos.error}</ErrorBox>}
            {!allRepos.data && !allRepos.error && <Spinner />}
            {allRepos.data?.map((r) => (
              <label key={r.id} className="check small mono">
                <input
                  type="checkbox"
                  checked={repos.includes(r.name)}
                  onChange={(e) => setRepos(e.target.checked ? [...repos, r.name] : repos.filter((x) => x !== r.name))}
                />{" "}
                {r.name}
              </label>
            ))}
            <span className="hint">
              The token sees only these, and is at most a maintainer of them: for starting runs from another
              system, or an agent's <code>kiwicd mcp</code>.
            </span>
          </div>
        )}
        <div className="form-actions">
          <button type="button" className="btn" onClick={onClose}>
            Cancel
          </button>
          <button
            type="submit"
            className="btn btn-primary"
            disabled={busy || !name.trim() || (limit && repos.length === 0) || (limit && role === "admin")}
          >
            {busy ? "Creating…" : "Create token"}
          </button>
        </div>
      </form>
    </Modal>
  );
}
