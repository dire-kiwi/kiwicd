import { useMemo, useState, type FormEvent } from "react";
import { Link, useNavigate } from "react-router-dom";
import { errorMessage, post } from "../api";
import { StatusIcon } from "../components/StatusIcon";
import { Empty, ErrorBox, Icon, Modal, Spinner } from "../components/ui";
import { useDocumentTitle, useFetch } from "../hooks";
import { isSSHURL, KnownHostsField, SSHKeyField, sshHost, type SSHAuth } from "../components/SSHFields";
import type { Repo, Run } from "../types";
import { absoluteTime, relativeTime, visualState } from "../util";

export function ReposPage() {
  useDocumentTitle("Repositories");
  const repos = useFetch<Repo[]>("/api/repos");
  const runs = useFetch<Run[]>("/api/runs?limit=500");
  const [creating, setCreating] = useState(false);

  const latest = useMemo(() => {
    const m = new Map<string, Run>();
    for (const r of runs.data ?? []) if (!m.has(r.repo)) m.set(r.repo, r);
    return m;
  }, [runs.data]);

  return (
    <div className="page">
      <div className="page-header">
        <div>
          <h1>Repositories</h1>
          <p className="muted">Hosted repositories live on this server; mirrors follow a repository elsewhere.</p>
        </div>
        <button className="btn btn-primary" onClick={() => setCreating(true)}>
          New repository
        </button>
      </div>
      {repos.error && !repos.data && <ErrorBox onRetry={repos.reload}>{repos.error}</ErrorBox>}
      {!repos.data && !repos.error && <Spinner />}
      {repos.data && repos.data.length === 0 && (
        <Empty title="No repositories yet">
          Create a hosted repository to push to, or mirror one from GitHub or any git server.
          <div style={{ marginTop: 12 }}>
            <button className="btn btn-primary" onClick={() => setCreating(true)}>
              New repository
            </button>
          </div>
        </Empty>
      )}
      {repos.data && repos.data.length > 0 && (
        <ul className="list">
          {repos.data
            .slice()
            .sort((a, b) => a.name.localeCompare(b.name))
            .map((r) => {
              const run = latest.get(r.name);
              const [owner, name] = r.name.split("/");
              return (
                <li key={r.id} className="list-row repo-row">
                  <Icon name="repo" />
                  <div className="repo-row-main">
                    <div className="repo-row-title">
                      <Link to={`/repos/${r.name}`}>
                        <span className="muted-strong">{owner} /</span> <strong>{name}</strong>
                      </Link>
                      <span className={`badge badge-${r.kind}`}>{r.kind === "mirror" ? "Mirror" : "Hosted"}</span>
                    </div>
                    <div className="muted small ellipsis">
                      {r.kind === "mirror" ? r.url : r.clone_url}
                      {r.kind === "mirror" && r.last_sync_error && <span className="text-danger"> · sync failed</span>}
                    </div>
                  </div>
                  <div className="repo-row-run small">
                    {run ? (
                      <Link to={`/runs/${run.id}`} className="muted-link inline-flex" title={run.title}>
                        <StatusIcon state={visualState(run.status, run.conclusion, run.cancel_requested)} size={14} />
                        <span>
                          {run.workflow_name} #{run.number}
                        </span>
                        <span className="muted" title={absoluteTime(run.created_at)}>
                          {relativeTime(run.created_at)}
                        </span>
                      </Link>
                    ) : (
                      <span className="muted">No runs</span>
                    )}
                  </div>
                </li>
              );
            })}
        </ul>
      )}
      {creating && <NewRepoModal onClose={() => setCreating(false)} />}
    </div>
  );
}

function NewRepoModal({ onClose }: { onClose: () => void }) {
  const navigate = useNavigate();
  const [mode, setMode] = useState<"hosted" | "mirror">("hosted");
  const [name, setName] = useState("");
  const [url, setUrl] = useState("");
  const [branch, setBranch] = useState("");
  const [auth, setAuth] = useState<SSHAuth>("none");
  const [authTouched, setAuthTouched] = useState(false);
  const [sshKey, setSshKey] = useState("");
  const [knownHosts, setKnownHosts] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();
  const validName = /^[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+$/.test(name.trim().replace(/\.git$/, ""));

  // Suggest owner/name from a mirror URL.
  const onUrl = (v: string) => {
    setUrl(v);
    if (!name || name === guessName(url)) setName(guessName(v));
    if (!authTouched) setAuth(isSSHURL(v) ? "generate" : "none");
  };

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError(undefined);
    try {
      const body: Record<string, string | boolean> = { name: name.trim(), kind: mode };
      if (branch.trim()) body.default_branch = branch.trim();
      if (mode === "mirror") {
        body.url = url.trim();
        if (auth === "generate") body.generate_ssh_key = true;
        if (auth === "paste") body.ssh_key = sshKey;
        if (knownHosts.trim()) body.ssh_known_hosts = knownHosts;
      }
      const repo = await post<Repo>("/api/repos", body);
      navigate(`/repos/${repo.name}`);
    } catch (err) {
      setError(errorMessage(err));
      setBusy(false);
    }
  };

  return (
    <Modal title="New repository" onClose={onClose}>
      <form className="form" onSubmit={submit}>
        {error && <ErrorBox>{error}</ErrorBox>}
        <div className="segmented" role="radiogroup">
          <button type="button" role="radio" aria-checked={mode === "hosted"} className={mode === "hosted" ? "active" : ""} onClick={() => setMode("hosted")}>
            <strong>Hosted</strong>
            <span>Push directly to kiwicd</span>
          </button>
          <button type="button" role="radio" aria-checked={mode === "mirror"} className={mode === "mirror" ? "active" : ""} onClick={() => setMode("mirror")}>
            <strong>Mirror</strong>
            <span>Follow a remote repository</span>
          </button>
        </div>
        {mode === "mirror" && (
          <label className="field">
            <span className="label">Remote URL</span>
            <input
              className="input mono"
              value={url}
              onChange={(e) => onUrl(e.target.value)}
              placeholder="https://github.com/owner/repo.git"
              required
              autoFocus
            />
            <span className="hint">
              HTTPS (with credentials in the URL for a private repository) or SSH, like <code>git@github.com:owner/repo.git</code>.
            </span>
          </label>
        )}
        {mode === "mirror" && (
          <SSHKeyField
            auth={auth}
            onAuth={(a) => {
              setAuth(a);
              setAuthTouched(true);
            }}
            sshKey={sshKey}
            onSshKey={setSshKey}
          />
        )}
        {mode === "mirror" && isSSHURL(url) && (
          <details className="disclosure">
            <summary>Host keys</summary>
            <KnownHostsField value={knownHosts} onChange={setKnownHosts} host={sshHost(url)} />
          </details>
        )}
        <label className="field">
          <span className="label">Name</span>
          <input
            className="input mono"
            value={name}
            onChange={(e) => setName(e.target.value)}
            placeholder="owner/repo"
            required
            autoFocus={mode === "hosted"}
          />
          {name && !validName && <span className="field-error">Use the form owner/repo.</span>}
        </label>
        <label className="field">
          <span className="label">
            Default branch <span className="muted">(optional)</span>
          </span>
          <input className="input mono" value={branch} onChange={(e) => setBranch(e.target.value)} placeholder="main" />
        </label>
        <div className="form-actions">
          <button type="button" className="btn" onClick={onClose}>
            Cancel
          </button>
          <button
            type="submit"
            className="btn btn-primary"
            disabled={busy || !validName || (mode === "mirror" && (!url.trim() || (auth === "paste" && !sshKey.trim())))}
          >
            {busy ? (mode === "mirror" ? "Cloning…" : "Creating…") : "Create repository"}
          </button>
        </div>
      </form>
    </Modal>
  );
}

function guessName(url: string): string {
  const m = /[/:]([A-Za-z0-9_.-]+)\/([A-Za-z0-9_.-]+?)(?:\.git)?\/?$/.exec(url.trim());
  return m ? `${m[1]}/${m[2]}` : "";
}
