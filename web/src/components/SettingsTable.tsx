import { useState, type FormEvent } from "react";
import { del, errorMessage, put } from "../api";
import { useFetch } from "../hooks";
import type { Setting } from "../types";
import { absoluteTime, relativeTime } from "../util";
import { Empty, ErrorBox, Modal, Spinner } from "./ui";

type Kind = "secrets" | "variables";

/** CRUD table for secrets or variables, global (no repo) or repo-scoped. */
export function SettingsTable({ kind, repo }: { kind: Kind; repo?: string }) {
  const q = repo ? `?repo=${encodeURIComponent(repo)}` : "";
  const list = useFetch<Setting[]>(`/api/${kind}${q}`);
  const [editing, setEditing] = useState<{ name: string; value: string; isNew: boolean } | null>(null);
  const [actionError, setActionError] = useState<string>();
  const secret = kind === "secrets";
  const noun = secret ? "secret" : "variable";

  const remove = async (name: string) => {
    if (!window.confirm(`Delete ${noun} ${name}?`)) return;
    setActionError(undefined);
    try {
      await del(`/api/${kind}?${repo ? `repo=${encodeURIComponent(repo)}&` : ""}name=${encodeURIComponent(name)}`);
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
            {repo ? " Repository values override global ones." : " Global values apply to every repository."}
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
  initial,
  onClose,
  onSaved,
}: {
  kind: Kind;
  repo?: string;
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
      await put(`/api/${kind}${repo ? `?repo=${encodeURIComponent(repo)}` : ""}`, { name, value });
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
