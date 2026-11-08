import { Fragment, useEffect, useMemo, useState, type FormEvent, type ReactNode } from "react";
import { Link, NavLink, Route, Routes, useNavigate, useParams } from "react-router-dom";
import { useMe } from "../App";
import { canMaintain, isAdmin } from "../access";
import { del, errorMessage, get, patch, post, put, repoPath } from "../api";
import { RunList } from "../components/RunList";
import { SettingsTable } from "../components/SettingsTable";
import { isSSHURL, KnownHostsField, SSHKeyField, sshHost, type SSHAuth } from "../components/SSHFields";
import { Chip, CodeBlock, CopyField, Empty, ErrorBox, Flash, Icon, Modal, Spinner } from "../components/ui";
import { useDocumentTitle, useFetch } from "../hooks";
import type { ApprovedContributor, ForkApproval, Repo, UploadedWorkflow, Workflow, WorkflowInput } from "../types";
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
  const me = useMe();

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
  const maintainer = canMaintain(me?.access, r.name);

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

      <RepoHeader repo={r} maintainer={maintainer} onChanged={repo.reload} />

      <nav className="tabs">
        <NavLink to={base} end>
          Runs
        </NavLink>
        <NavLink to={`${base}/workflows`}>Workflows</NavLink>
        {maintainer && (
          <>
            <NavLink to={`${base}/secrets`}>Secrets</NavLink>
            <NavLink to={`${base}/variables`}>Variables</NavLink>
            <NavLink to={`${base}/settings`}>Settings</NavLink>
          </>
        )}
      </nav>

      <Routes>
        <Route
          index
          element={
            <RunList
              repo={r.name}
              emptyHint={
                <>
                  Push a commit containing a workflow under <code>.github/workflows/</code>
                  {maintainer && (
                    <>
                      , or upload or start one from the <Link to={`${base}/workflows`}>Workflows</Link> tab
                    </>
                  )}
                  .
                </>
              }
            />
          }
        />
        <Route path="workflows" element={<WorkflowsTab repo={r} maintainer={maintainer} />} />
        {maintainer && (
          <>
            <Route path="secrets" element={<SettingsTable kind="secrets" repo={r.name} />} />
            <Route path="variables" element={<SettingsTable kind="variables" repo={r.name} />} />
            <Route path="settings" element={<RepoSettingsTab repo={r} admin={isAdmin(me)} onChanged={repo.reload} />} />
          </>
        )}
        <Route path="*" element={<Empty title="Not found" />} />
      </Routes>
    </div>
  );
}

function RepoHeader({ repo, maintainer, onChanged }: { repo: Repo; maintainer: boolean; onChanged: () => void }) {
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
        {maintainer && (
          <details className="disclosure" open={!repo.refs || Object.keys(repo.refs).length === 0}>
            <summary>Push an existing repository</summary>
            <CodeBlock code={`git remote add kiwicd ${repo.clone_url}\ngit push kiwicd ${repo.default_branch || "main"}`} />
            <p className="muted small">
              Git asks for credentials: use any username with the admin password or an API token (Settings → API tokens). Every
              push runs the matching <code>on: push</code> workflows.
            </p>
          </details>
        )}
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
            {maintainer && (
              <button className="btn btn-sm" onClick={sync} disabled={syncing}>
                {syncing ? "Syncing…" : "Sync now"}
              </button>
            )}
          </div>
        </div>
      </div>
      {maintainer && repo.ssh_public_key && !repo.synced_at && (
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
      {maintainer && (
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
            Services that can't sign payloads can append <code>?secret=&lt;secret&gt;</code> to the payload URL instead. A GitHub
            repository that kiwicd's GitHub App (<em>Settings → GitHub</em>) is installed on needs no webhook: the app sends its events.
          </p>
        </details>
      )}
    </section>
  );
}

const uploadedPrefix = "kiwicd:";

function WorkflowsTab({ repo, maintainer }: { repo: Repo; maintainer: boolean }) {
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
              <WorkflowRow key={w.file} workflow={w} file={w.file} defaultBranch={repo.default_branch} onRun={maintainer ? () => setDispatching(w) : undefined} />
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
              branch, as if they were in <code>.github/workflows/</code>, with the pushed commit checked out. Their schedules run on the
              default branch's latest commit.
            </p>
          </div>
          {maintainer && (
            <button className="btn btn-primary" onClick={() => setEditing({ file: "", isNew: true })}>
              Upload workflow
            </button>
          )}
        </div>
        {actionError && <ErrorBox>{actionError}</ErrorBox>}
        {uploaded && uploaded.length === 0 && (
          <Empty title="No uploaded workflows">
            {maintainer && (
              <>
                Upload a workflow file, or with curl:{" "}
                <code>
                  curl -u admin -T deploy.yml {repo.clone_url.replace(/\/git\/.*$/, "")}
                  {repoPath(repo.name)}/uploaded-workflows/deploy.yml
                </code>
              </>
            )}
          </Empty>
        )}
        {uploaded && uploaded.length > 0 && (
          <ul className="list">
            {uploaded.map((w) => {
              const file = w.file.slice(uploadedPrefix.length);
              return (
                <WorkflowRow key={w.file} workflow={w} file={file} defaultBranch={repo.default_branch} onRun={maintainer ? () => setDispatching(w) : undefined}>
                  {maintainer && (
                    <div className="row-actions">
                      <button className="btn btn-sm" onClick={() => setEditing({ file, isNew: false })}>
                        Edit
                      </button>
                      <button className="btn btn-sm btn-danger-outline" onClick={() => remove(file)}>
                        Delete
                      </button>
                    </div>
                  )}
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

function WorkflowRow({
  workflow: w,
  file,
  defaultBranch,
  onRun,
  children,
}: {
  workflow: Workflow;
  file: string;
  defaultBranch: string;
  onRun?: () => void;
  children?: ReactNode;
}) {
  return (
    <li className="list-row workflow-row">
      <div className="workflow-main">
        <div className="strong">{w.name || file}</div>
        <div className="muted small mono ellipsis">{file}</div>
        {w.error && <div className="text-danger small pre-wrap">{w.error}</div>}
        {(w.events ?? []).length > 0 && (
          <div className="chips">
            {(w.events ?? []).map((e) => {
              const details = w.event_details?.[e];
              return (
                <Chip key={e} title={details}>
                  {eventLabel(e)}
                  {details && <span className="muted"> · {details}</span>}
                </Chip>
              );
            })}
          </div>
        )}
        {(w.schedules ?? []).length > 0 && (
          <ul className="schedules small">
            {(w.schedules ?? []).map((s) => (
              <li key={`${s.cron} ${s.timezone ?? ""}`}>
                <Icon name="clock" size={12} /> <code>{s.cron}</code> <span className="muted">{s.timezone || "UTC"}</span>
                {" · "}
                {s.error ? (
                  <span className="text-danger">{s.error}</span>
                ) : s.next_run_at ? (
                  <span title={absoluteTime(s.next_run_at)}>next run {relativeTime(s.next_run_at)}</span>
                ) : (
                  <span className="muted">
                    runs only from <code>{defaultBranch}</code>
                  </span>
                )}
              </li>
            ))}
          </ul>
        )}
      </div>
      {onRun && w.dispatch && !w.error && (
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
        {!unavailable && !(repo.protected_branches ?? [repo.default_branch]).includes(ref) && (
          <Flash kind="warn">
            <span>
              {ref} isn't a protected ref, so the run gets no secrets except <span className="mono">KIWICD_TOKEN</span>.
            </span>
          </Flash>
        )}
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

function MirrorSettings({ repo, admin, onChanged }: { repo: Repo; admin: boolean; onChanged: () => void }) {
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
          <input className="input mono" value={url} onChange={(e) => setUrl(e.target.value)} readOnly={!admin} required />
          {admin && (
            <button type="submit" className="btn btn-sm" disabled={busy || !url.trim() || url === (repo.url ?? "")}>
              Save
            </button>
          )}
        </form>
        {!admin && (
          <>
            <span />
            <span className="hint">Only an admin can change the URL.</span>
          </>
        )}
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

/** Saves a list setting of a repository, keeping its error and busy state. */
function useListSetting(repo: Repo, field: "readers" | "protected_refs", onChanged: () => void) {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();
  const save = async (next: string[]): Promise<boolean> => {
    setBusy(true);
    setError(undefined);
    try {
      await patch(repoPath(repo.name), { [field]: next });
      onChanged();
      return true;
    } catch (e) {
      setError(errorMessage(e));
      return false;
    } finally {
      setBusy(false);
    }
  };
  return { busy, error, save };
}

/** A list of names with Remove buttons and a field to add one. */
function ListEditor({
  id,
  items,
  fixed,
  empty,
  placeholder,
  candidates = [],
  same = (a, b) => a === b,
  confirmRemove,
  busy,
  save,
}: {
  id: string;
  items: string[];
  /** Shown first, without a Remove button. */
  fixed?: ReactNode;
  empty: ReactNode;
  placeholder: string;
  candidates?: string[];
  same?: (a: string, b: string) => boolean;
  confirmRemove: (item: string) => string;
  busy: boolean;
  save: (next: string[]) => Promise<boolean>;
}) {
  const [name, setName] = useState("");
  const has = (n: string) => items.some((r) => same(r, n));
  return (
    <div className="stack-sm">
      {items.length > 0 || fixed ? (
        <ul className="plain-list name-list">
          {fixed && <li>{fixed}</li>}
          {items.map((r) => (
            <li key={r}>
              <span className="mono">{r}</span>
              <button
                className="btn btn-sm"
                disabled={busy}
                onClick={() => {
                  if (confirm(confirmRemove(r))) void save(items.filter((x) => x !== r));
                }}
              >
                Remove
              </button>
            </li>
          ))}
        </ul>
      ) : (
        <span className="muted">{empty}</span>
      )}
      <form
        className="copy-field"
        onSubmit={async (e) => {
          e.preventDefault();
          if (await save([...items, name.trim()])) setName("");
        }}
      >
        <input
          className="input mono"
          placeholder={placeholder}
          list={candidates.length ? `${id}-candidates` : undefined}
          value={name}
          onChange={(e) => setName(e.target.value)}
        />
        {candidates.length > 0 && (
          <datalist id={`${id}-candidates`}>
            {candidates.filter((n) => !has(n)).map((n) => (
              <option key={n} value={n} />
            ))}
          </datalist>
        )}
        <button type="submit" className="btn btn-sm" disabled={busy || !name.trim() || has(name.trim())}>
          Add
        </button>
      </form>
    </div>
  );
}

/** The repositories whose jobs may fetch this one with secrets.KIWICD_TOKEN. */
function ReadersSettings({ repo, onChanged }: { repo: Repo; onChanged: () => void }) {
  const repos = useFetch<Repo[]>("/api/repos");
  const { busy, error, save } = useListSetting(repo, "readers", onChanged);
  const candidates = (repos.data ?? []).map((r) => r.name).filter((n) => n !== repo.name);

  return (
    <section className="box">
      <div className="box-header">
        <div>
          <h3>Job access</h3>
          <p className="muted small">
            Repositories whose <span className="mono">repository_push</span>,{" "}
            <span className="mono">repository_pull_request</span> and manually run jobs can fetch this one with{" "}
            <span className="mono">secrets.KIWICD_TOKEN</span>, when their workflow listens to it and their run is from a protected
            ref. Jobs can always fetch their own repository.
          </p>
        </div>
      </div>
      {error && <Flash kind="error">{error}</Flash>}
      <div className="kv kv-pad">
        <span className="label">Readers</span>
        <ListEditor
          id="reader"
          items={repo.readers ?? []}
          empty="None: only this repository's own jobs can fetch it."
          placeholder="owner/name"
          candidates={candidates}
          same={(a, b) => a.toLowerCase() === b.toLowerCase()}
          confirmRemove={(r) => `Stop ${r}'s jobs fetching ${repo.name}?`}
          busy={busy}
          save={save}
        />
      </div>
    </section>
  );
}

/** The refs whose runs get secrets. */
function ProtectedRefsSettings({ repo, onChanged }: { repo: Repo; onChanged: () => void }) {
  const { busy, error, save } = useListSetting(repo, "protected_refs", onChanged);
  const protectedBranches = repo.protected_branches ?? [];

  return (
    <section className="box">
      <div className="box-header">
        <div>
          <h3>Protected refs</h3>
          <p className="muted small">
            Only runs from these refs get this repository's secrets and the global ones. A run's workflow comes from its ref, so anyone
            who can push a branch could otherwise read every secret with a workflow on it. Runs from other refs only get{" "}
            <span className="mono">secrets.KIWICD_TOKEN</span>, which fetches just this repository. Patterns are branch names, as in a
            push filter (<span className="mono">release/*</span>), or full refs (<span className="mono">refs/tags/v*</span>).
          </p>
        </div>
      </div>
      {error && <Flash kind="error">{error}</Flash>}
      <div className="kv kv-pad">
        <span className="label">Refs</span>
        <ListEditor
          id="protected-ref"
          items={repo.protected_refs ?? []}
          fixed={
            <>
              <span className="mono">{repo.default_branch}</span>
              <span className="muted small">default branch</span>
            </>
          }
          empty=""
          placeholder="branch, release/* or refs/tags/v*"
          candidates={branchesOf(repo)}
          confirmRemove={(r) => `Stop giving secrets to runs from ${r}?`}
          busy={busy}
          save={save}
        />
        <span className="label">Protected branches</span>
        <span className="chips">
          {protectedBranches.length ? protectedBranches.map((b) => <Chip key={b}>{b}</Chip>) : <span className="muted">none yet</span>}
        </span>
      </div>
    </section>
  );
}

/** The badge of a workflow file: ci.yml for .github/workflows/ci.yml. */
function badgeName(file: string): string {
  return file.replace(/^\.github\/workflows\//, "");
}

function BadgeSettings({ repo, onChanged }: { repo: Repo; onChanged: () => void }) {
  const me = useMe();
  const workflows = useFetch<Workflow[]>(`${repoPath(repo.name)}/workflows`);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();
  const base = me?.public_url ?? window.location.origin;

  const setPublic = async (on: boolean) => {
    setBusy(true);
    setError(undefined);
    try {
      await patch(repoPath(repo.name), { public_badges: on });
      onChanged();
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <section className="box">
      <div className="box-header">
        <div>
          <h3>Status badges</h3>
          <p className="muted small">
            A badge shows how the latest finished run of a workflow on the default branch ended; add{" "}
            <span className="mono">?branch=NAME</span> for another branch or <span className="mono">&amp;event=push</span> for one event.
            Badges need signing in unless they're public, so to show one in a README elsewhere make them public: then anyone can see this
            repository's workflow names and whether their runs pass.
          </p>
        </div>
      </div>
      {error && <Flash kind="error">{error}</Flash>}
      <div className="kv kv-pad">
        <span className="label">Public</span>
        <label className="check">
          <input type="checkbox" checked={repo.public_badges ?? false} disabled={busy} onChange={(e) => setPublic(e.target.checked)} />
          Anyone can see this repository's status badges
        </label>
        {(workflows.data ?? []).map((w) => {
          const url = `${base}${repoPath(repo.name)}/badge/${encodeURIComponent(badgeName(w.file))}.svg`;
          const runs = `${base}/repos/${repo.name}?workflow=${encodeURIComponent(w.file)}`;
          return (
            <Fragment key={w.file}>
              <span className="label">
                <img src={url} alt={`${w.name || w.file} status`} className="badge-preview" />
              </span>
              <CopyField value={`[![${w.name || badgeName(w.file)}](${url})](${runs})`} />
            </Fragment>
          );
        })}
      </div>
    </section>
  );
}

/** How strict each fork policy is: approving fewer pull requests needs an admin. */
const forkPolicies: { value: "off" | ForkApproval; label: string; strictness: number }[] = [
  { value: "off", label: "Don't run them", strictness: 3 },
  { value: "outside", label: "Run them once approved, unless the author is a member or collaborator", strictness: 2 },
  { value: "first_time", label: "Run them once approved, for first-time contributors", strictness: 1 },
  { value: "none", label: "Run them without approval", strictness: 0 },
];

/** Whether pull requests from forks run the repository's own pull_request workflows, and which wait for approval. */
function PullRequestSettings({ repo, admin, onChanged }: { repo: Repo; admin: boolean; onChanged: () => void }) {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();
  const approval = repo.fork_approval ?? "first_time";
  const policy = repo.fork_pull_requests ? approval : "off";
  const strictness = (v: string) => forkPolicies.find((p) => p.value === v)?.strictness ?? 1;
  const contributors = useFetch<ApprovedContributor[]>(`${repoPath(repo.name)}/approved-contributors`);
  const set = async (v: string) => {
    if (v === "none" && !confirm(`Run ${repo.name}'s pull_request workflows on every pull request from a fork without approval? Anyone can open one on a public repository.`))
      return;
    setBusy(true);
    setError(undefined);
    try {
      await patch(repoPath(repo.name), v === "off" ? { fork_pull_requests: false } : { fork_pull_requests: true, fork_approval: v });
      onChanged();
    } catch (e) {
      setError(errorMessage(e));
    } finally {
      setBusy(false);
    }
  };
  const forget = async (c: ApprovedContributor) => {
    if (!confirm(`Forget ${c.login}? Their pull requests from forks will wait for approval again.`)) return;
    setError(undefined);
    try {
      await del(`${repoPath(repo.name)}/approved-contributors/${c.user_id}`);
      contributors.reload();
    } catch (e) {
      setError(errorMessage(e));
    }
  };

  return (
    <section className="box">
      <div className="box-header">
        <div>
          <h3>Pull requests</h3>
          <p className="muted small">
            GitHub's <span className="mono">pull_request</span> webhook runs this repository's{" "}
            <span className="mono">on: pull_request</span> workflows on a merge of the pull request into its base branch. Their workflow
            comes from the pull request, so their jobs get no secrets except <span className="mono">KIWICD_TOKEN</span> and can't use
            protected or reserved runners. Pull requests from forks only run them if allowed here; their jobs run the fork's code on
            your runners, so runs of authors you don't know can wait until a maintainer approves them on the run's page.
          </p>
        </div>
      </div>
      {error && <Flash kind="error">{error}</Flash>}
      <div className="kv kv-pad">
        <span className="label">Forks</span>
        <select className="input" value={policy} disabled={busy} onChange={(e) => void set(e.target.value)}>
          {forkPolicies.map((p) => (
            <option key={p.value} value={p.value} disabled={!admin && p.strictness < strictness(policy)}>
              {p.label}
            </option>
          ))}
        </select>
      </div>
      {!admin && <p className="muted small">Only an admin can approve fewer pull requests from forks.</p>}
      {contributors.data && contributors.data.length > 0 && (
        <>
          <p className="muted small">
            Approved contributors: someone approved a run of theirs, so with first-time contributors' approval their pull requests run
            straight away.
          </p>
          <ul className="list">
            {contributors.data.map((c) => (
              <li key={c.user_id} className="list-row setting-row">
                <div className="setting-main">
                  <span className="setting-name">{c.login}</span>
                  <span className="muted small" title={absoluteTime(c.approved_at)}>
                    approved by {c.approved_by} {relativeTime(c.approved_at)}
                  </span>
                </div>
                <div className="row-actions">
                  <button className="btn btn-sm btn-danger-outline" onClick={() => void forget(c)}>
                    Forget
                  </button>
                </div>
              </li>
            ))}
          </ul>
        </>
      )}
    </section>
  );
}

/** Whether the repository's own .github/workflows/ files run here. */
function RepoWorkflowsSettings({ repo, onChanged }: { repo: Repo; onChanged: () => void }) {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();
  const set = async (on: boolean) => {
    setBusy(true);
    setError(undefined);
    try {
      await patch(repoPath(repo.name), { repo_workflows: on });
      onChanged();
    } catch (e) {
      setError(errorMessage(e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <section className="box">
      <div className="box-header">
        <div>
          <h3>Workflows</h3>
          <p className="muted small">
            Turn this off for a mirror whose <span className="mono">.github/workflows/</span> run on GitHub Actions: their jobs ask
            for runners like <span className="mono">ubuntu-latest</span> that kiwicd doesn't have, and would wait forever. Uploaded
            workflows still run, and other repositories can still listen to this one and call its reusable workflows.
          </p>
        </div>
      </div>
      {error && <Flash kind="error">{error}</Flash>}
      <div className="kv-pad">
        <label className="check">
          <input type="checkbox" checked={repo.repo_workflows ?? true} disabled={busy} onChange={(e) => void set(e.target.checked)} />
          Run this repository's workflows
        </label>
      </div>
    </section>
  );
}

/** Whether runs of workflows that aren't the GitHub repository's own are reported to it. */
function ChecksSettings({ repo, onChanged }: { repo: Repo; onChanged: () => void }) {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();
  const set = async (on: boolean) => {
    setBusy(true);
    setError(undefined);
    try {
      await patch(repoPath(repo.name), { report_checks: on });
      onChanged();
    } catch (e) {
      setError(errorMessage(e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <section className="box">
      <div className="box-header">
        <div>
          <h3>GitHub checks</h3>
          <p className="muted small">
            With the GitHub App installed, runs of mirrors of GitHub repositories show on their commits and pull requests as check
            runs, one per job, linking back here. Runs of this repository's <span className="mono">repository_push</span> and{" "}
            <span className="mono">repository_pull_request</span> workflows go on the commit of the repository they listen to, and
            uploaded workflows' runs on this mirror's commit, only if this is on: it shows their workflow and job names, and links to
            this server, to everyone who can see that repository on GitHub.
          </p>
        </div>
      </div>
      {error && <Flash kind="error">{error}</Flash>}
      <div className="kv-pad">
        <label className="check">
          <input type="checkbox" checked={!!repo.report_checks} disabled={busy} onChange={(e) => void set(e.target.checked)} />
          Report listener and uploaded workflows' runs to GitHub
        </label>
      </div>
    </section>
  );
}

function RepoSettingsTab({ repo, admin, onChanged }: { repo: Repo; admin: boolean; onChanged: () => void }) {
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
      {repo.kind === "mirror" && <MirrorSettings repo={repo} admin={admin} onChanged={onChanged} />}
      {repo.kind === "mirror" && <PullRequestSettings repo={repo} admin={admin} onChanged={onChanged} />}
      <RepoWorkflowsSettings repo={repo} onChanged={onChanged} />
      <ProtectedRefsSettings repo={repo} onChanged={onChanged} />
      <ReadersSettings repo={repo} onChanged={onChanged} />
      <BadgeSettings repo={repo} onChanged={onChanged} />
      <ChecksSettings repo={repo} onChanged={onChanged} />
      {admin && (
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
      )}
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
