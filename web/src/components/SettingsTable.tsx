import { useState, type FormEvent } from "react";
import { del, errorMessage, put } from "../api";
import { useFetch } from "../hooks";
import { seal } from "../seal";
import type { SecretsKey, Setting } from "../types";
import { absoluteTime, relativeTime } from "../util";
import { Chip, Empty, ErrorBox, Modal, Spinner } from "./ui";

type Kind = "secrets" | "variables";

/** "?repo=…&environment=…" for a scope, followed by extra parameters. */
function scopeQuery(repo?: string, environment?: string, extra: Record<string, string> = {}): string {
  const q = new URLSearchParams();
  if (repo) q.set("repo", repo);
  if (environment) q.set("environment", environment);
  for (const [k, v] of Object.entries(extra)) q.set(k, v);
  const s = q.toString();
  return s ? `?${s}` : "";
}

/** CRUD table for secrets or variables: global (no repo), a repository's, or one of its environments'. */
export function SettingsTable({ kind, repo, environment }: { kind: Kind; repo?: string; environment?: string }) {
  const list = useFetch<Setting[]>(`/api/${kind}${scopeQuery(repo, environment)}`);
  const secret = kind === "secrets";
  const key = useFetch<SecretsKey>(secret ? "/api/secrets/key" : null);
  const [editing, setEditing] = useState<{ name: string; value: string; isNew: boolean } | null>(null);
  const [actionError, setActionError] = useState<string>();
  const noun = secret ? "secret" : "variable";
  const keyID = key.data?.key_id;

  const remove = async (name: string) => {
    if (!window.confirm(`Delete ${noun} ${name}?`)) return;
    setActionError(undefined);
    try {
      await del(`/api/${kind}${scopeQuery(repo, environment, { name })}`);
      list.reload();
    } catch (e) {
      setActionError(errorMessage(e));
    }
  };

  return (
    <section className="box">
      <div className="box-header">
        <div>
          <h3>{secret ? "Secrets" : "Variables"}</h3>
          <p className="muted small">
            {secret
              ? <>Encrypted values available to workflows as <code>{"${{ secrets.NAME }}"}</code>. Values are write-only and masked in logs.</>
              : <>Plain-text configuration available to workflows as <code>{"${{ vars.NAME }}"}</code>.</>}
            {environment
              ? " Jobs that deploy to this environment get them, over the repository's and the global ones."
              : repo
                ? " Repository values override global ones; an environment's override both."
                : " Global values apply to every repository."}
            {secret &&
              (repo
                ? " Only runs from protected refs get them: the default branch and the refs chosen in Settings."
                : " Only runs from each repository's protected refs get them: its default branch and the refs chosen in its settings.")}
            {secret && keyID && " They are sealed in your browser to the secrets service's public key, so the server can't read them."}
          </p>
        </div>
        <button className="btn btn-primary" onClick={() => setEditing({ name: "", value: "", isNew: true })}>
          New {noun}
        </button>
      </div>
      {actionError && <ErrorBox>{actionError}</ErrorBox>}
      {list.error && !list.data && <ErrorBox onRetry={list.reload}>{list.error}</ErrorBox>}
      {!list.data && list.loading && <Spinner />}
      {list.data && list.data.length === 0 && (
        <Empty title={`No ${noun}s yet`}>
          {secret ? "Add a secret to use credentials in your workflows." : "Add a variable to share configuration across workflows."}
        </Empty>
      )}
      {list.data && list.data.length > 0 && (
        <ul className="list">
          {list.data.map((s) => (
            <li key={s.name} className="list-row setting-row">
              <div className="setting-main">
                <code className="setting-name">{s.name}</code>
                {!secret && <span className="setting-value mono" title={s.value}>{s.value}</span>}
                {secret && keyID && !s.key_id && (
                  <Chip title="Encrypted with the server's own key, so the server can read it. Reseal secrets in Settings, or update it.">
                    server key
                  </Chip>
                )}
                {secret && keyID && s.key_id && s.key_id !== keyID && (
                  <Chip title={`Sealed to an older public key (${s.key_id}). Update it to seal it to the current one (${keyID}).`}>old key</Chip>
                )}
              </div>
              <span className="muted small nowrap" title={absoluteTime(s.updated_at)}>
                Updated {relativeTime(s.updated_at)}
              </span>
              <div className="row-actions">
                <button className="btn btn-sm" onClick={() => setEditing({ name: s.name, value: secret ? "" : (s.value ?? ""), isNew: false })}>
                  {secret ? "Update" : "Edit"}
                </button>
                <button className="btn btn-sm btn-danger-outline" onClick={() => remove(s.name)}>
                  Delete
                </button>
              </div>
            </li>
          ))}
        </ul>
      )}
      {editing && (
        <SettingEditor
          kind={kind}
          repo={repo}
          environment={environment}
          publicKey={key.data?.public_key}
          initial={editing}
          onClose={() => setEditing(null)}
          onSaved={() => {
            setEditing(null);
            list.reload();
          }}
        />
      )}
    </section>
  );
}

function SettingEditor({
  kind,
  repo,
  environment,
  publicKey,
  initial,
  onClose,
  onSaved,
}: {
  kind: Kind;
  repo?: string;
  environment?: string;
  /** The secrets service's public key to seal secrets to, if the server has one. */
  publicKey?: string;
  initial: { name: string; value: string; isNew: boolean };
  onClose: () => void;
  onSaved: () => void;
}) {
  const [name, setName] = useState(initial.name);
  const [value, setValue] = useState(initial.value);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();
  const secret = kind === "secrets";
  const noun = secret ? "secret" : "variable";
  const validName = /^[A-Za-z_][A-Za-z0-9_]*$/.test(name);

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    if (!validName) return;
    setBusy(true);
    setError(undefined);
    try {
      // With a public key, the server only ever sees the sealed value.
      const body =
        secret && publicKey
          ? { name, encrypted_value: await seal(publicKey, { repository: repo, environment, name }, value) }
          : { name, value };
      await put(`/api/${kind}${scopeQuery(repo, environment)}`, body);
      onSaved();
    } catch (err) {
      setError(errorMessage(err));
      setBusy(false);
    }
  };

  return (
    <Modal title={initial.isNew ? `New ${noun}` : `Update ${noun}`} onClose={onClose}>
      <form onSubmit={submit} className="form">
        {error && <ErrorBox>{error}</ErrorBox>}
        <label className="field">
          <span className="label">Name</span>
          <input
            className="input mono"
            value={name}
            onChange={(e) => setName(e.target.value.toUpperCase())}
            disabled={!initial.isNew}
            placeholder="MY_NAME"
            autoFocus={initial.isNew}
            required
          />
          {name && !validName && <span className="field-error">Use letters, digits and underscores; must not start with a digit.</span>}
        </label>
        <label className="field">
          <span className="label">{secret ? "Secret" : "Value"}</span>
          <textarea
            className="input mono"
            rows={secret ? 5 : 3}
            value={value}
            onChange={(e) => setValue(e.target.value)}
            placeholder={secret && !initial.isNew ? "Enter a new value" : ""}
            autoFocus={!initial.isNew}
            spellCheck={false}
          />
        </label>
        <div className="form-actions">
          <button type="button" className="btn" onClick={onClose}>
            Cancel
          </button>
          <button type="submit" className="btn btn-primary" disabled={busy || !validName}>
            {busy ? "Saving…" : initial.isNew ? `Add ${noun}` : `Update ${noun}`}
          </button>
        </div>
      </form>
    </Modal>
  );
}
