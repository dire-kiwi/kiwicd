import { useEffect, useMemo, useState, type FormEvent, type ReactNode } from "react";
import { Link, NavLink, Route, Routes, useNavigate, useParams } from "react-router-dom";
import { del, errorMessage, get, patch, post, put, repoPath } from "../api";
import { RunList } from "../components/RunList";
import { SettingsTable } from "../components/SettingsTable";
import { isSSHURL, KnownHostsField, SSHKeyField, sshHost, type SSHAuth } from "../components/SSHFields";
import { Chip, CodeBlock, CopyField, Empty, ErrorBox, Flash, Icon, Modal, Spinner } from "../components/ui";
import { useDocumentTitle, useFetch } from "../hooks";
import type { Repo, UploadedWorkflow, Workflow, WorkflowInput } from "../types";
import { absoluteTime, eventLabel, relativeTime, shortSha } from "../util";

function branchesOf(repo: Repo): string[] {
  const out = Object.keys(repo.refs ?? {})
    .filter((r) => r.startsWith("refs/heads/"))
    .map((r) => r.slice(11));
  out.sort((a, b) => (a === repo.default_branch ? -1 : b === repo.default_branch ? 1 : a.localeCompare(b)));
  return out;
}

export function RepoPage() {
  const { owner = "", name = "" } = useParams();
  const fullName = `${owner}/${name}`;
  useDocumentTitle(fullName);
  const repo = useFetch<Repo>(repoPath(fullName));

  if (repo.error && !repo.data)
    return (
      <div className="page">
        <ErrorBox onRetry={repo.reload}>{repo.error}</ErrorBox>
      </div>
    );
  if (!repo.data)
    return (
      <div className="page">
        <Spinner />
      </div>
    );
  const r = repo.data;
  const base = `/repos/${r.name}`;

  return (
    <div className="page">
      <div className="repo-title">
        <Icon name="repo" size={20} />
        <h1>
          <Link to="/repos" className="muted-link">
            {owner}
          </Link>
          <span className="muted"> / </span>
          <Link to={base}>{name}</Link>
        </h1>
        <span className={`badge badge-${r.kind}`}>{r.kind === "mirror" ? "Mirror" : "Hosted"}</span>
      </div>

      <RepoHeader repo={r} onChanged={repo.reload} />

      <nav className="tabs">
        <NavLink to={base} end>
          Runs
        </NavLink>
        <NavLink to={`${base}/workflows`}>Workflows</NavLink>
        <NavLink to={`${base}/secrets`}>Secrets</NavLink>
        <NavLink to={`${base}/variables`}>Variables</NavLink>
        <NavLink to={`${base}/settings`}>Settings</NavLink>
      </nav>

      <Routes>
        <Route
          index
          element={
            <RunList
              repo={r.name}
              emptyHint={
                <>
                  Push a commit containing a workflow under <code>.github/workflows/</code>, or upload or start one from the{" "}
                  <Link to={`${base}/workflows`}>Workflows</Link> tab.
                </>
              }
            />
          }
        />
        <Route path="workflows" element={<WorkflowsTab repo={r} />} />
        <Route path="secrets" element={<SettingsTable kind="secrets" repo={r.name} />} />
        <Route path="variables" element={<SettingsTable kind="variables" repo={r.name} />} />
        <Route path="settings" element={<RepoSettingsTab repo={r} onChanged={repo.reload} />} />
        <Route path="*" element={<Empty title="Not found" />} />
      </Routes>
    </div>
  );
}

function RepoHeader({ repo, onChanged }: { repo: Repo; onChanged: () => void }) {
  const [syncing, setSyncing] = useState(false);
  const [syncError, setSyncError] = useState<string>();
  const [showHook, setShowHook] = useState(false);

  const sync = async () => {
    setSyncing(true);
    setSyncError(undefined);
    try {
      await post(`${repoPath(repo.name)}/sync`);
    } catch (e) {
      setSyncError(errorMessage(e));
    } finally {
      setSyncing(false);
      onChanged();
    }
  };

  if (repo.kind === "hosted") {
    return (
      <section className="box repo-header">
        <div className="repo-header-grid">
          <div className="field">
            <span className="label">Clone URL</span>
            <CopyField value={repo.clone_url} />
          </div>
        </div>
        <details className="disclosure" open={!repo.refs || Object.keys(repo.refs).length === 0}>
          <summary>Push an existing repository</summary>
          <CodeBlock code={`git remote add kiwicd ${repo.clone_url}\ngit push kiwicd ${repo.default_branch || "main"}`} />
          <p className="muted small">
            Git asks for credentials: use any username with your kiwicd password. Every push runs the matching{" "}
            <code>on: push</code> workflows.
          </p>
        </details>
      </section>
    );
  }

  return (
    <section className="box repo-header">
      <div className="repo-header-grid">
        <div className="field">
          <span className="label">Mirrors</span>
          <CopyField value={repo.url ?? ""} />
        </div>
        <div className="field">
          <span className="label">Last sync</span>
          <div className="sync-line">
            <span title={absoluteTime(repo.last_sync_at)}>{repo.last_sync_at ? relativeTime(repo.last_sync_at) : "never"}</span>
            {repo.last_sync_error ? <span className="text-danger">· failed</span> : repo.last_sync_at ? <span className="text-success">· ok</span> : null}
            <button className="btn btn-sm" onClick={sync} disabled={syncing}>
              {syncing ? "Syncing…" : "Sync now"}
            </button>
          </div>
        </div>
      </div>
      {repo.ssh_public_key && !repo.synced_at && (
        <Flash kind="info">
          <div className="stack-sm">
            <span>
              <strong>Add this deploy key to the remote repository</strong>, with read access, then sync. On GitHub:{" "}
              <em>Settings → Deploy keys → Add deploy key</em>.
            </span>
            <CopyField value={repo.ssh_public_key} />
          </div>
        </Flash>
      )}
      {repo.last_sync_error && (
        <ErrorBox>
          <strong>Last sync failed:</strong> <span className="mono pre-wrap">{repo.last_sync_error}</span>
        </ErrorBox>
      )}
      {syncError && <ErrorBox>{syncError}</ErrorBox>}
      <details className="disclosure" open={showHook} onToggle={(e) => setShowHook((e.target as HTMLDetailsElement).open)}>
        <summary>Trigger runs instantly with a webhook</summary>
        <p className="muted small">
          kiwicd polls mirrors periodically. To start runs as soon as you push, add a webhook to the upstream repository (on
          GitHub: <em>Settings → Webhooks → Add webhook</em>):
        </p>
        <div className="kv">
          <span className="label">Payload URL</span>
          <CopyField value={repo.webhook_url} />
          <span className="label">Content type</span>
          <span className="mono">application/json</span>
          <span className="label">Secret</span>
          {repo.webhook_secret ? <CopyField value={repo.webhook_secret} secret /> : <span className="muted">unavailable</span>}
          <span className="label">Events</span>
          <span>Just the push event</span>
        </div>
        <p className="muted small">
          Services that can't sign payloads can append <code>?secret=&lt;secret&gt;</code> to the payload URL instead.
        </p>
      </details>
    </section>
  );
}

const uploadedPrefix = "kiwicd:";

function WorkflowsTab({ repo }: { repo: Repo }) {
  const branches = useMemo(() => branchesOf(repo), [repo]);
  const [ref, setRef] = useState(repo.default_branch);
  const wfs = useFetch<Workflow[]>(`${repoPath(repo.name)}/workflows?ref=${encodeURIComponent(ref)}`);
  const [dispatching, setDispatching] = useState<Workflow | null>(null);
  const [editing, setEditing] = useState<{ file: string; isNew: boolean } | null>(null);
  const [actionError, setActionError] = useState<string>();
  const sha = repo.refs?.[`refs/heads/${ref}`];
  const committed = wfs.data?.filter((w) => !w.uploaded);
  const uploaded = wfs.data?.filter((w) => w.uploaded);

  const remove = async (file: string) => {
    if (!window.confirm(`Delete the uploaded workflow ${file}? Its past runs are kept.`)) return;
    setActionError(undefined);
    try {
      await del(`${repoPath(repo.name)}/uploaded-workflows/${encodeURIComponent(file)}`);
      wfs.reload();
    } catch (e) {
      setActionError(errorMessage(e));
    }
  };

  return (
    <div className="stack">
      <section className="box">
        <div className="box-header">
          <div>
            <h3>Workflows</h3>
            <p className="muted small">
              Files in <code>.github/workflows/</code> on{" "}
              {branches.length > 1 ? (
                <select className="input input-inline" value={ref} onChange={(e) => setRef(e.target.value)}>
                  {branches.map((b) => (
                    <option key={b}>{b}</option>
                  ))}
                </select>
              ) : (
                <code>{ref}</code>
              )}
              {sha && <span className="mono"> @ {shortSha(sha)}</span>}
            </p>
          </div>
        </div>
        {wfs.error && !wfs.data && <ErrorBox onRetry={wfs.reload}>{wfs.error}</ErrorBox>}
        {!wfs.data && !wfs.error && <Spinner />}
        {committed && committed.length === 0 && (
          <Empty title="No workflows on this branch">
            Add a YAML file under <code>.github/workflows/</code> and push it, or upload one below.
          </Empty>
        )}
        {committed && committed.length > 0 && (
          <ul className="list">
            {committed.map((w) => (
              <WorkflowRow key={w.file} workflow={w} file={w.file} onRun={() => setDispatching(w)} />
            ))}
          </ul>
        )}
      </section>

      <section className="box">
        <div className="box-header">
          <div>
            <h3>Uploaded workflows</h3>
            <p className="muted small">
              Stored in kiwicd instead of the repository, for workflows you don't want to commit. They run on pushes to every
              branch, as if they were in <code>.github/workflows/</code>, with the pushed commit checked out.
            </p>
          </div>
          <button className="btn btn-primary" onClick={() => setEditing({ file: "", isNew: true })}>
            Upload workflow
          </button>
        </div>
        {actionError && <ErrorBox>{actionError}</ErrorBox>}
        {uploaded && uploaded.length === 0 && (
          <Empty title="No uploaded workflows">
            Upload a workflow file, or with curl:{" "}
            <code>
              curl -u admin -T deploy.yml {repo.clone_url.replace(/\/git\/.*$/, "")}
              {repoPath(repo.name)}/uploaded-workflows/deploy.yml
            </code>
          </Empty>
        )}
        {uploaded && uploaded.length > 0 && (
          <ul className="list">
            {uploaded.map((w) => {
              const file = w.file.slice(uploadedPrefix.length);
              return (
                <WorkflowRow key={w.file} workflow={w} file={file} onRun={() => setDispatching(w)}>
                  <div className="row-actions">
                    <button className="btn btn-sm" onClick={() => setEditing({ file, isNew: false })}>
                      Edit
                    </button>
                    <button className="btn btn-sm btn-danger-outline" onClick={() => remove(file)}>
                      Delete
                    </button>
                  </div>
                </WorkflowRow>
              );
            })}
          </ul>
        )}
      </section>

      {dispatching && <DispatchModal repo={repo} branches={branches} initialRef={ref} workflow={dispatching} onClose={() => setDispatching(null)} />}
      {editing && (
        <UploadModal
          repo={repo}
          initial={editing}
          existing={(uploaded ?? []).map((w) => w.file.slice(uploadedPrefix.length))}
          onClose={() => setEditing(null)}
          onSaved={() => {
            setEditing(null);
            wfs.reload();
          }}
        />
      )}
    </div>
  );
}

function WorkflowRow({ workflow: w, file, onRun, children }: { workflow: Workflow; file: string; onRun: () => void; children?: ReactNode }) {
  return (
    <li className="list-row workflow-row">
      <div className="workflow-main">
        <div className="strong">{w.name || file}</div>
        <div className="muted small mono ellipsis">{file}</div>
        {w.error && <div className="text-danger small pre-wrap">{w.error}</div>}
        {(w.events ?? []).length > 0 && (
          <div className="chips">
            {(w.events ?? []).map((e) => (
              <Chip key={e}>{eventLabel(e)}</Chip>
            ))}
          </div>
        )}
      </div>
      {w.dispatch && !w.error && (
        <button className="btn" onClick={onRun}>
          <Icon name="play" size={12} /> Run workflow
        </button>
      )}
      {children}
    </li>
  );
}

const uploadNameRE = /^[A-Za-z0-9_][A-Za-z0-9_.-]*\.ya?ml$/;

function UploadModal({
  repo,
  initial,
  existing,
  onClose,
  onSaved,
}: {
  repo: Repo;
  initial: { file: string; isNew: boolean };
  existing: string[];
  onClose: () => void;
  onSaved: () => void;
}) {
  const [file, setFile] = useState(initial.file);
  const [content, setContent] = useState("");
  const [loaded, setLoaded] = useState(initial.isNew);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();
  const path = `${repoPath(repo.name)}/uploaded-workflows/${encodeURIComponent(file)}`;
  const validName = uploadNameRE.test(file);
  const replaces = initial.isNew && existing.includes(file);

  useEffect(() => {
    if (initial.isNew) return;
    get<UploadedWorkflow>(path).then(
      (u) => {
        setContent(u.content);
        setLoaded(true);
      },
      (e) => setError(errorMessage(e)),
    );
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const pick = async (f: File | undefined) => {
    if (!f) return;
    setContent(await f.text());
    if (initial.isNew && !file) setFile(f.name);
  };

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    if (!validName) return;
    setBusy(true);
    setError(undefined);
    try {
      await put(path, { content });
      onSaved();
    } catch (err) {
      setError(errorMessage(err));
      setBusy(false);
    }
  };

  return (
    <Modal title={initial.isNew ? "Upload workflow" : `Edit ${initial.file}`} onClose={onClose} wide>
      <form className="form" onSubmit={submit}>
        {error && <ErrorBox>{error}</ErrorBox>}
        <label className="field">
          <span className="label">File name</span>
          <input
            className="input mono"
            value={file}
            onChange={(e) => setFile(e.target.value.trim())}
            disabled={!initial.isNew}
            placeholder="deploy.yml"
            autoFocus={initial.isNew}
            required
          />
          {file && !validName && <span className="field-error">End the name in .yml or .yaml; use letters, digits, '.', '-' and '_'.</span>}
          {replaces && <span className="hint">An uploaded workflow with this name exists: saving replaces it.</span>}
        </label>
        <label className="field">
          <span className="label">Workflow</span>
          <textarea
            className="input mono"
            rows={18}
            value={content}
            onChange={(e) => setContent(e.target.value)}
            placeholder={loaded ? "name: Deploy\non:\n  push:\n    branches: [main]\njobs:\n  …" : "Loading…"}
            disabled={!loaded}
            spellCheck={false}
          />
        </label>
        <label className="field">
          <span className="label">Or read it from a file</span>
          <input type="file" accept=".yml,.yaml" onChange={(e) => void pick(e.target.files?.[0])} disabled={!loaded} />
        </label>
        <div className="form-actions">
          <button type="button" className="btn" onClick={onClose}>
            Cancel
          </button>
          <button type="submit" className="btn btn-primary" disabled={busy || !loaded || !validName || !content.trim()}>
            {busy ? "Saving…" : initial.isNew ? "Upload" : "Save"}
          </button>
        </div>
      </form>
    </Modal>
  );
}

function defaultValue(inp: WorkflowInput): string {
  if (inp.default !== undefined && inp.default !== null && inp.default !== "") return String(inp.default);
  if (inp.type === "boolean") return "false";
  if (inp.type === "choice" && inp.options && inp.options.length) return inp.options[0];
  return "";
}

function DispatchModal({
  repo,
  branches,
  initialRef,
  workflow,
  onClose,
}: {
  repo: Repo;
  branches: string[];
  initialRef: string;
  workflow: Workflow;
  onClose: () => void;
}) {
  const navigate = useNavigate();
  const [ref, setRef] = useState(initialRef);
  // Inputs can differ between branches: refetch the workflow for the chosen one.
  const wfs = useFetch<Workflow[]>(ref === initialRef ? null : `${repoPath(repo.name)}/workflows?ref=${encodeURIComponent(ref)}`);
  const wf = ref === initialRef ? workflow : wfs.data?.find((w) => w.file === workflow.file);
  const inputs = wf?.inputs ?? [];
  const [values, setValues] = useState<Record<string, string>>({});
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();

  useEffect(() => {
    setValues((prev) => {
      const next: Record<string, string> = {};
      for (const i of inputs) next[i.name] = prev[i.name] ?? defaultValue(i);
      return next;
    });
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [wf]);

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError(undefined);
    try {
      const res = await post<{ run_id: number }>(`${repoPath(repo.name)}/dispatch`, { workflow: workflow.file, ref, inputs: values });
      navigate(`/runs/${res.run_id}`);
    } catch (err) {
      setError(errorMessage(err));
      setBusy(false);
    }
  };

  const unavailable = ref !== initialRef && wfs.data && (!wf || !wf.dispatch);

  return (
    <Modal title={`Run ${workflow.name || workflow.file}`} onClose={onClose}>
      <form className="form" onSubmit={submit}>
        {error && <ErrorBox>{error}</ErrorBox>}
        <label className="field">
          <span className="label">Use workflow from</span>
          <select className="input" value={ref} onChange={(e) => setRef(e.target.value)}>
            {(branches.length ? branches : [initialRef]).map((b) => (
              <option key={b} value={b}>
                {b}
              </option>
            ))}
          </select>
        </label>
        {ref !== initialRef && wfs.loading && <Spinner label="Loading workflow…" />}
        {wfs.error && <ErrorBox>{wfs.error}</ErrorBox>}
        {unavailable && <Flash kind="warn">This workflow can't be dispatched on {ref}.</Flash>}
        {inputs.map((inp) => (
          <InputField key={inp.name} input={inp} value={values[inp.name] ?? ""} onChange={(v) => setValues((s) => ({ ...s, [inp.name]: v }))} />
        ))}
        <div className="form-actions">
          <button type="button" className="btn" onClick={onClose}>
            Cancel
          </button>
          <button type="submit" className="btn btn-primary" disabled={busy || !wf || !!unavailable}>
            {busy ? "Starting…" : "Run workflow"}
          </button>
        </div>
      </form>
    </Modal>
  );
}

function InputField({ input, value, onChange }: { input: WorkflowInput; value: string; onChange: (v: string) => void }) {
  const label = (
    <span className="label">
      {input.description || input.name}
      {input.required && <span className="text-danger"> *</span>}
      {input.description && <span className="muted mono small"> {input.name}</span>}
    </span>
  );
  if (input.type === "boolean") {
    return (
      <label className="field field-check">
        <input type="checkbox" checked={value === "true"} onChange={(e) => onChange(e.target.checked ? "true" : "false")} />
        {label}
      </label>
    );
  }
  if (input.type === "choice" && input.options && input.options.length) {
    return (
      <label className="field">
        {label}
        <select className="input" value={value} onChange={(e) => onChange(e.target.value)} required={input.required}>
          {input.options.map((o) => (
            <option key={o}>{o}</option>
          ))}
        </select>
      </label>
    );
  }
  return (
    <label className="field">
      {label}
      <input
        className="input"
        type={input.type === "number" ? "number" : "text"}
        value={value}
        onChange={(e) => onChange(e.target.value)}
        required={input.required}
      />
    </label>
  );
}

/** Saves mirror settings, then syncs so problems show up straight away. */
function useMirrorUpdate(repo: Repo, onChanged: () => void) {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();
  const [notice, setNotice] = useState<string>();
  const save = async (body: Record<string, unknown>, done?: string): Promise<boolean> => {
    setBusy(true);
    setError(undefined);
    setNotice(undefined);
    try {
      await patch(repoPath(repo.name), body);
    } catch (e) {
      setError(errorMessage(e));
      setBusy(false);
      return false;
    }
    try {
      await post(`${repoPath(repo.name)}/sync`);
      setNotice(done ? `${done} Sync succeeded.` : "Saved. Sync succeeded.");
    } catch (e) {
      setError(`Saved, but the sync failed: ${errorMessage(e)}`);
    }
    setBusy(false);
    onChanged();
    return true;
  };
  return { busy, error, notice, save };
}

function MirrorSettings({ repo, onChanged }: { repo: Repo; onChanged: () => void }) {
  const { busy, error, notice, save } = useMirrorUpdate(repo, onChanged);
  const [url, setUrl] = useState(repo.url ?? "");
  const [keyModal, setKeyModal] = useState(false);
  const [hostsModal, setHostsModal] = useState(false);
  const hostKeys = repo.ssh_host_keys ?? [];
  const ssh = isSSHURL(repo.url ?? "");

  return (
    <section className="box">
      <div className="box-header">
        <div>
          <h3>Remote</h3>
          <p className="muted small">Where kiwicd fetches this mirror from, and how it authenticates.</p>
        </div>
      </div>
      {error && <Flash kind="error">{error}</Flash>}
      {notice && <Flash kind="success">{notice}</Flash>}
      <div className="kv kv-pad">
        <span className="label">URL</span>
        <form
          className="copy-field"
          onSubmit={(e) => {
            e.preventDefault();
            void save({ url: url.trim() }, "URL changed.");
          }}
        >
          <input className="input mono" value={url} onChange={(e) => setUrl(e.target.value)} required />
          <button type="submit" className="btn btn-sm" disabled={busy || !url.trim() || url === (repo.url ?? "")}>
            Save
          </button>
        </form>
        {(repo.url ?? "").includes("***@") && (
          <>
            <span />
            <span className="hint">The credentials in this URL are hidden. Type them again if you change it.</span>
          </>
        )}

        <span className="label">SSH key</span>
        <div className="stack-sm">
          {repo.ssh_public_key ? (
            <>
              <CopyField value={repo.ssh_public_key} />
              <span className="hint">The public half of the key kiwicd fetches with. The private key is never shown.</span>
            </>
          ) : (
            <span className="muted">None{ssh ? ": the server's own SSH keys are used." : "."}</span>
          )}
          <div className="inline-actions">
            <button className="btn btn-sm" onClick={() => setKeyModal(true)} disabled={busy}>
              {repo.ssh_public_key ? "Replace key…" : "Add key…"}
            </button>
            {repo.ssh_public_key && (
              <button
                className="btn btn-sm btn-danger"
                disabled={busy}
                onClick={() => {
                  if (confirm("Remove the SSH key? Fetches will use the server's own SSH keys.")) void save({ ssh_key: "" }, "Key removed.");
                }}
              >
                Remove
              </button>
            )}
          </div>
        </div>

        <span className="label">Host keys</span>
        <div className="stack-sm">
          {hostKeys.length > 0 ? (
            <ul className="plain-list mono small">
              {hostKeys.map((k) => (
                <li key={k}>{k}</li>
              ))}
            </ul>
          ) : (
            <span className="muted">{ssh ? "None yet: the host key is trusted on the first connection." : "Only used for SSH URLs."}</span>
          )}
          {hostKeys.length > 0 && (
            <span className="hint">
              {repo.ssh_known_hosts ? "From the known_hosts you configured." : "Trusted on the first connection and pinned since."}
            </span>
          )}
          <div className="inline-actions">
            <button className="btn btn-sm" onClick={() => setHostsModal(true)} disabled={busy}>
              Edit known_hosts…
            </button>
            {(repo.ssh_known_hosts || hostKeys.length > 0) && (
              <button
                className="btn btn-sm"
                disabled={busy}
                onClick={() => {
                  if (confirm("Forget the pinned host keys? The next connection trusts whatever key the host presents."))
                    void save({ ssh_known_hosts: "" }, "Host keys reset.");
                }}
              >
                Reset
              </button>
            )}
          </div>
        </div>
      </div>
      {keyModal && (
        <SSHKeyModal
          hasKey={!!repo.ssh_public_key}
          onClose={() => setKeyModal(false)}
          onSave={async (body) => {
            if (await save(body, body.generate_ssh_key ? "New key generated: add it to the remote." : "Key saved.")) setKeyModal(false);
          }}
          busy={busy}
          error={error}
        />
      )}
      {hostsModal && (
        <KnownHostsModal
          initial={repo.ssh_known_hosts ?? ""}
          host={sshHost(repo.url ?? "")}
          onClose={() => setHostsModal(false)}
          onSave={async (v) => {
            if (await save({ ssh_known_hosts: v }, "Host keys saved.")) setHostsModal(false);
          }}
          busy={busy}
          error={error}
        />
      )}
    </section>
  );
}

function SSHKeyModal({
  hasKey,
  onClose,
  onSave,
  busy,
  error,
}: {
  hasKey: boolean;
  onClose: () => void;
  onSave: (body: { ssh_key?: string; generate_ssh_key?: boolean }) => void;
  busy: boolean;
  error?: string;
}) {
  const [auth, setAuth] = useState<SSHAuth>("generate");
  const [key, setKey] = useState("");
  return (
    <Modal title={hasKey ? "Replace SSH key" : "Add SSH key"} onClose={onClose}>
      <form
        className="form"
        onSubmit={(e) => {
          e.preventDefault();
          onSave(auth === "generate" ? { generate_ssh_key: true } : { ssh_key: key });
        }}
      >
        {error && <ErrorBox>{error}</ErrorBox>}
        {hasKey && <Flash kind="warn">The current key is discarded. Remove it from the remote once the new one works.</Flash>}
        <SSHKeyField auth={auth} onAuth={setAuth} sshKey={key} onSshKey={setKey} allowNone={false} />
        <div className="form-actions">
          <button type="button" className="btn" onClick={onClose}>
            Cancel
          </button>
          <button type="submit" className="btn btn-primary" disabled={busy || (auth === "paste" && !key.trim())}>
            {busy ? "Saving…" : auth === "generate" ? "Generate key" : "Save key"}
          </button>
        </div>
      </form>
    </Modal>
  );
}

function KnownHostsModal({
  initial,
  host,
  onClose,
  onSave,
  busy,
  error,
}: {
  initial: string;
  host: string;
  onClose: () => void;
  onSave: (v: string) => void;
  busy: boolean;
  error?: string;
}) {
  const [value, setValue] = useState(initial);
  return (
    <Modal title="known_hosts" onClose={onClose} wide>
      <form
        className="form"
        onSubmit={(e) => {
          e.preventDefault();
          onSave(value);
        }}
      >
        {error && <ErrorBox>{error}</ErrorBox>}
        <KnownHostsField value={value} onChange={setValue} host={host} />
        <div className="form-actions">
          <button type="button" className="btn" onClick={onClose}>
            Cancel
          </button>
          <button type="submit" className="btn btn-primary" disabled={busy}>
            {busy ? "Saving…" : "Save"}
          </button>
        </div>
      </form>
    </Modal>
  );
}

function RepoSettingsTab({ repo, onChanged }: { repo: Repo; onChanged: () => void }) {
  const navigate = useNavigate();
  const [confirm, setConfirm] = useState(false);
  const [typed, setTyped] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();
  const branches = branchesOf(repo);

  const remove = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError(undefined);
    try {
      await del(repoPath(repo.name));
      navigate("/repos");
    } catch (err) {
      setError(errorMessage(err));
      setBusy(false);
    }
  };

  return (
    <div className="stack">
      <section className="box">
        <div className="box-header">
          <h3>General</h3>
        </div>
        <div className="kv kv-pad">
          <span className="label">Name</span>
          <span className="mono">{repo.name}</span>
          <span className="label">Type</span>
          <span>{repo.kind === "mirror" ? "Mirror" : "Hosted"}</span>
          <span className="label">Default branch</span>
          <span className="mono">{repo.default_branch}</span>
          <span className="label">Branches</span>
          <span className="chips">
            {branches.length ? branches.map((b) => <Chip key={b}>{b}</Chip>) : <span className="muted">none yet</span>}
          </span>
          <span className="label">Created</span>
          <span title={absoluteTime(repo.created_at)}>{relativeTime(repo.created_at)}</span>
        </div>
      </section>
      {repo.kind === "mirror" && <MirrorSettings repo={repo} onChanged={onChanged} />}
      <section className="box box-danger">
        <div className="box-header">
          <div>
            <h3>Danger zone</h3>
            <p className="muted small">
              Deleting a repository removes its git data, runs, logs, secrets and variables. This can't be undone.
            </p>
          </div>
          <button className="btn btn-danger" onClick={() => setConfirm(true)}>
            Delete repository
          </button>
        </div>
      </section>
      {confirm && (
        <Modal title="Delete repository" onClose={() => setConfirm(false)}>
          <form className="form" onSubmit={remove}>
            {error && <ErrorBox>{error}</ErrorBox>}
            <Flash kind="warn">
              This permanently deletes <strong>{repo.name}</strong>
              {repo.kind === "hosted" ? ", including its git history" : ""}.
            </Flash>
            <label className="field">
              <span className="label">
                Type <strong className="mono">{repo.name}</strong> to confirm
              </span>
              <input className="input mono" value={typed} onChange={(e) => setTyped(e.target.value)} autoFocus />
            </label>
            <div className="form-actions">
              <button type="button" className="btn" onClick={() => setConfirm(false)}>
                Cancel
              </button>
              <button type="submit" className="btn btn-danger" disabled={busy || typed !== repo.name}>
                {busy ? "Deleting…" : "Delete this repository"}
              </button>
            </div>
          </form>
        </Modal>
      )}
    </div>
  );
}
