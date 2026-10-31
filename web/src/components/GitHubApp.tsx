import { useState, type FormEvent } from "react";
import { del, errorMessage, post, put } from "../api";
import { useFetch } from "../hooks";
import type { GitHubApp, GitHubSettings, GitHubUser, Role, RoleGrant } from "../types";
import { absoluteTime, relativeTime } from "../util";
import { CopyField, Empty, ErrorBox, Flash, Modal, Spinner } from "./ui";

/** What GitHub sent the browser back to /settings with after creating the app. */
function takeURLResult(): { created?: boolean; error?: string } {
  const params = new URLSearchParams(window.location.search);
  const created = params.get("github") === "created";
  const error = params.get("github_error") ?? undefined;
  if (!created && error === undefined) return {};
  window.history.replaceState(null, "", window.location.pathname);
  return { created, error };
}

/** Settings → GitHub: the GitHub App for webhooks, fetching and signing in. */
export function GitHubAppSettings() {
  const settings = useFetch<GitHubSettings>("/api/github/app");
  const [result] = useState(takeURLResult);

  return (
    <section className="box">
      <div className="box-header">
        <div>
          <h3>GitHub</h3>
          <p className="muted small">
            A GitHub App sends kiwicd the pushes and pull requests of the repositories it's installed on, so their mirrors need no
            webhook of their own, and lets mirrors of private ones fetch without a deploy key. It also lets people sign in with
            GitHub.
          </p>
        </div>
      </div>
      {result.created && (
        <Flash kind="success">
          The GitHub App is set up. Install it on the repositories (or organizations) kiwicd should hear from.
        </Flash>
      )}
      {result.error && <ErrorBox>{result.error}</ErrorBox>}
      {settings.error && !settings.data && <ErrorBox onRetry={settings.reload}>{settings.error}</ErrorBox>}
      {!settings.data && settings.loading && <Spinner />}
      {settings.data &&
        (settings.data.app ? (
          <AppDetails settings={settings.data} app={settings.data.app} onChange={settings.reload} />
        ) : (
          <CreateApp settings={settings.data} onChange={settings.reload} />
        ))}
      {settings.data && (
        <div className="kv-pad">
          <Roles hasApp={!!settings.data.app} />
        </div>
      )}
    </section>
  );
}

function CreateApp({ settings, onChange }: { settings: GitHubSettings; onChange: () => void }) {
  const [name, setName] = useState(() => `kiwicd ${window.location.hostname}`.slice(0, 34));
  const [owner, setOwner] = useState<"user" | "org">("user");
  const [org, setOrg] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();
  const local = /^(localhost|127\.|\[::1\])/.test(new URL(settings.webhook_url).host);

  const create = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError(undefined);
    try {
      const m = await post<{ action: string; manifest: string }>("/api/github/app/manifest", {
        name: name.trim(),
        org: owner === "org" ? org.trim() : "",
      });
      // GitHub takes the manifest as a form post, then sends the browser back.
      const form = document.createElement("form");
      form.method = "post";
      form.action = m.action;
      const input = document.createElement("input");
      input.type = "hidden";
      input.name = "manifest";
      input.value = m.manifest;
      form.appendChild(input);
      document.body.appendChild(form);
      form.submit();
    } catch (err) {
      setError(errorMessage(err));
      setBusy(false);
    }
  };

  return (
    <div className="kv-pad stack">
      <form className="form" onSubmit={create}>
        {error && <ErrorBox>{error}</ErrorBox>}
        {local && (
          <Flash kind="warn">
            <span>
              GitHub can't deliver webhooks to <code>{settings.webhook_url}</code>. Set the server's <code>--public-url</code> to an
              address GitHub can reach first.
            </span>
          </Flash>
        )}
        <label className="field">
          <span className="label">App name</span>
          <input className="input" value={name} onChange={(e) => setName(e.target.value)} maxLength={34} required />
          <span className="hint">Unique on GitHub; you can change it there before creating the app.</span>
        </label>
        <div className="segmented" role="radiogroup">
          <button type="button" role="radio" aria-checked={owner === "user"} className={owner === "user" ? "active" : ""} onClick={() => setOwner("user")}>
            <strong>Personal account</strong>
            <span>Owned by you</span>
          </button>
          <button type="button" role="radio" aria-checked={owner === "org"} className={owner === "org" ? "active" : ""} onClick={() => setOwner("org")}>
            <strong>Organization</strong>
            <span>Owned by an organization you manage</span>
          </button>
        </div>
        {owner === "org" && (
          <label className="field">
            <span className="label">Organization</span>
            <input className="input mono" value={org} onChange={(e) => setOrg(e.target.value)} placeholder="my-org" required />
          </label>
        )}
        <p className="muted small">
          The app is private, so it can only be installed on the account that owns it: create it in the organization whose
          repositories kiwicd mirrors. GitHub asks you to confirm, then sends you back here. The app gets read access to contents,
          pull requests and organization members, and the push and pull request events. Its owner (or the organization's members)
          becomes an admin.
        </p>
        <div className="form-actions">
          <button type="submit" className="btn btn-primary" disabled={busy || !name.trim() || (owner === "org" && !org.trim())}>
            {busy ? "Going to GitHub…" : "Create GitHub App"}
          </button>
        </div>
      </form>
      <details className="disclosure">
        <summary>Use an existing app</summary>
        <ExistingApp settings={settings} onChange={onChange} />
      </details>
    </div>
  );
}

function ExistingApp({ settings, onChange }: { settings: GitHubSettings; onChange: () => void }) {
  const [f, setF] = useState({ app_id: "", client_id: "", client_secret: "", private_key: "", webhook_secret: "" });
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();
  const set = (k: keyof typeof f) => (e: { target: { value: string } }) => setF({ ...f, [k]: e.target.value });

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError(undefined);
    try {
      await put("/api/github/app", { ...f, app_id: Number(f.app_id) });
      onChange();
    } catch (err) {
      setError(errorMessage(err));
      setBusy(false);
    }
  };

  return (
    <form className="form" onSubmit={submit}>
      <p className="muted small">
        Give the app these URLs on GitHub, read access to contents, pull requests and (to let organizations' members sign in)
        members, and subscribe it to push and pull request events.
      </p>
      <div className="kv">
        <span className="label">Webhook URL</span>
        <CopyField value={settings.webhook_url} />
        <span className="label">Callback URL</span>
        <CopyField value={settings.callback_url} />
      </div>
      {error && <ErrorBox>{error}</ErrorBox>}
      <label className="field">
        <span className="label">App ID</span>
        <input className="input mono" value={f.app_id} onChange={set("app_id")} inputMode="numeric" required />
      </label>
      <label className="field">
        <span className="label">Client ID</span>
        <input className="input mono" value={f.client_id} onChange={set("client_id")} required />
      </label>
      <label className="field">
        <span className="label">Client secret</span>
        <input className="input mono" type="password" value={f.client_secret} onChange={set("client_secret")} autoComplete="off" required />
      </label>
      <label className="field">
        <span className="label">Webhook secret</span>
        <input className="input mono" type="password" value={f.webhook_secret} onChange={set("webhook_secret")} autoComplete="off" required />
      </label>
      <label className="field">
        <span className="label">Private key</span>
        <textarea
          className="input mono"
          rows={5}
          value={f.private_key}
          onChange={set("private_key")}
          placeholder="-----BEGIN RSA PRIVATE KEY-----"
          spellCheck={false}
          required
        />
      </label>
      <p className="muted small">Then give people roles below to let them sign in.</p>
      <div className="form-actions">
        <button type="submit" className="btn btn-primary" disabled={busy}>
          {busy ? "Checking with GitHub…" : "Save"}
        </button>
      </div>
    </form>
  );
}

function AppDetails({ settings, app, onChange }: { settings: GitHubSettings; app: GitHubApp; onChange: () => void }) {
  const [error, setError] = useState<string>();

  const remove = async () => {
    if (
      !window.confirm(
        `Remove the GitHub App ${app.name || app.slug} from kiwicd? Its webhooks stop working and everyone signed in with GitHub is signed out. ` +
          "The app stays on GitHub, where you can delete it.",
      )
    )
      return;
    setError(undefined);
    try {
      await del("/api/github/app");
      onChange();
    } catch (err) {
      setError(errorMessage(err));
    }
  };

  return (
    <div className="kv-pad stack">
      {error && <ErrorBox>{error}</ErrorBox>}
      <div className="kv">
        <span className="label">App</span>
        <span>
          {app.html_url ? (
            <a href={app.html_url} target="_blank" rel="noreferrer">
              {app.name || app.slug}
            </a>
          ) : (
            app.name || app.slug
          )}{" "}
          <span className="muted">
            (ID {app.app_id}
            {app.owner && <>, owned by {app.owner}</>})
          </span>
        </span>
        <span className="label">Webhook URL</span>
        <CopyField value={settings.webhook_url} />
      </div>
      <p className="muted small">
        Install the app on the repositories kiwicd mirrors: their pushes and pull requests then reach their mirrors at once, and
        mirrors with an HTTPS URL on GitHub fetch with the app's token. Remove any webhook you added to such a repository for
        kiwicd, or its pull requests start runs twice.
      </p>
      <div className="inline-actions">
        {app.install_url && (
          <a className="btn btn-primary" href={app.install_url} target="_blank" rel="noreferrer">
            Install on repositories
          </a>
        )}
        <button type="button" className="btn btn-danger-outline" onClick={remove}>
          Remove app
        </button>
      </div>
    </div>
  );
}

const kindLabel: Record<RoleGrant["kind"], string> = { user: "User", org: "Organization", team: "Team" };

function grantScope(g: { role: Role | ""; repos?: string[]; maintain_all?: boolean; maintain?: string[] }): string {
  if (g.role === "") return "no role";
  if (g.role !== "maintainer") return g.role;
  const repos = g.repos ?? (g.maintain_all ? [] : (g.maintain ?? []));
  return repos.length ? `maintainer of ${repos.join(", ")}` : "maintainer of every repository";
}

/** Roles of GitHub logins, organizations' members and teams', and who has signed in. */
function Roles({ hasApp }: { hasApp: boolean }) {
  const grants = useFetch<RoleGrant[]>("/api/roles");
  const users = useFetch<GitHubUser[]>("/api/users");
  const [editing, setEditing] = useState<RoleGrant | null>(null);
  const [error, setError] = useState<string>();
  const reload = () => {
    grants.reload();
    users.reload();
  };

  const remove = async (g: RoleGrant) => {
    const who = `${kindLabel[g.kind].toLowerCase()} ${g.name}`;
    if (!window.confirm(`Take ${g.role} away from ${who}? Anyone left without a role is signed out.`)) return;
    setError(undefined);
    try {
      await del(`/api/roles?${new URLSearchParams({ kind: g.kind, name: g.name })}`);
      reload();
    } catch (e) {
      setError(errorMessage(e));
    }
  };

  return (
    <div className="stack">
      <div className="box-header">
        <div>
          <h4>Who may sign in</h4>
          <p className="muted small">
            People sign in with GitHub if their login, an organization they're a member of or one of its teams has a role, and get
            the most their roles give. <strong>Admins</strong> can do everything; <strong>maintainers</strong> of a repository
            manage its secrets, variables, settings and uploaded workflows and start, re-run and cancel its runs;{" "}
            <strong>viewers</strong> see repositories, runs and logs. Organizations' and teams' members are checked when they
            sign in, which needs the app installed on the organization; sessions last a week. The admin password is always an
            admin.
            {!hasApp && " Nobody can sign in with GitHub until the app above is set up."}
          </p>
        </div>
        <button
          className="btn btn-primary"
          onClick={() => setEditing({ kind: "user", name: "", role: "viewer", repos: [], updated_at: 0 })}
        >
          Add role
        </button>
      </div>
      {error && <ErrorBox>{error}</ErrorBox>}
      {grants.error && !grants.data && <ErrorBox onRetry={grants.reload}>{grants.error}</ErrorBox>}
      {!grants.data && grants.loading && <Spinner />}
      {grants.data && grants.data.length === 0 && <Empty title="Nobody may sign in with GitHub yet" />}
      {grants.data && grants.data.length > 0 && (
        <ul className="list">
          {grants.data.map((g) => (
            <li key={`${g.kind}:${g.name}`} className="list-row setting-row">
              <div className="setting-main">
                <span className="setting-name">{g.name}</span>
                <span className="muted small">
                  {kindLabel[g.kind]} · {grantScope(g)}
                </span>
              </div>
              <div className="row-actions">
                <button className="btn btn-sm" onClick={() => setEditing(g)}>
                  Edit
                </button>
                <button className="btn btn-sm btn-danger-outline" onClick={() => remove(g)}>
                  Remove
                </button>
              </div>
            </li>
          ))}
        </ul>
      )}
      {users.data && users.data.length > 0 && (
        <details className="disclosure">
          <summary>People who have signed in ({users.data.length})</summary>
          <ul className="list">
            {users.data.map((u) => (
              <li key={u.login} className="list-row setting-row">
                <div className="setting-main">
                  <span className="setting-name">{u.login}</span>
                  <span className="muted small">
                    {grantScope(u)}
                    {u.memberships.length > 0 && <> · member of {u.memberships.map((m) => m.replace(/^\w+:/, "")).join(", ")}</>}
                  </span>
                </div>
                <span className="muted small nowrap" title={u.signed_in_at ? absoluteTime(u.signed_in_at) : undefined}>
                  {u.signed_in_at ? `Signed in ${relativeTime(u.signed_in_at)}` : "Signed in before roles"}
                </span>
              </li>
            ))}
          </ul>
        </details>
      )}
      {editing && (
        <RoleModal
          grant={editing}
          onClose={() => setEditing(null)}
          onSaved={() => {
            setEditing(null);
            reload();
          }}
        />
      )}
    </div>
  );
}

function RoleModal({ grant, onClose, onSaved }: { grant: RoleGrant; onClose: () => void; onSaved: () => void }) {
  const isNew = !grant.name;
  const [kind, setKind] = useState(grant.kind);
  const [name, setName] = useState(grant.name);
  const [role, setRole] = useState<Role>(grant.role);
  const [repos, setRepos] = useState(grant.repos.join(", "));
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError(undefined);
    try {
      await put("/api/roles", { kind, name: name.trim(), role, repos: role === "maintainer" ? splitNames(repos) : [] });
      onSaved();
    } catch (err) {
      setError(errorMessage(err));
      setBusy(false);
    }
  };

  return (
    <Modal title={isNew ? "Add role" : `Role of ${grant.name}`} onClose={onClose}>
      <form className="form" onSubmit={submit}>
        {error && <ErrorBox>{error}</ErrorBox>}
        <label className="field">
          <span className="label">Who</span>
          <select className="input" value={kind} onChange={(e) => setKind(e.target.value as RoleGrant["kind"])} disabled={!isNew}>
            <option value="user">A GitHub user</option>
            <option value="org">An organization's members</option>
            <option value="team">A team's members</option>
          </select>
        </label>
        <label className="field">
          <span className="label">{kind === "user" ? "Login" : kind === "org" ? "Organization" : "Team"}</span>
          <input
            className="input mono"
            value={name}
            onChange={(e) => setName(e.target.value)}
            placeholder={kind === "user" ? "octocat" : kind === "org" ? "my-org" : "my-org/team-slug"}
            disabled={!isNew}
            autoFocus={isNew}
            required
          />
        </label>
        <label className="field">
          <span className="label">Role</span>
          <select className="input" value={role} onChange={(e) => setRole(e.target.value as Role)}>
            <option value="viewer">Viewer: repositories, runs and logs</option>
            <option value="maintainer">Maintainer of repositories</option>
            <option value="admin">Admin: everything</option>
          </select>
        </label>
        {role === "maintainer" && (
          <label className="field">
            <span className="label">Repositories</span>
            <input
              className="input mono"
              value={repos}
              onChange={(e) => setRepos(e.target.value)}
              placeholder="All, or owner/name, owner/*"
            />
            <span className="hint">Empty for every repository. Maintainers see the others too, as viewers.</span>
          </label>
        )}
        <div className="form-actions">
          <button type="button" className="btn" onClick={onClose}>
            Cancel
          </button>
          <button type="submit" className="btn btn-primary" disabled={busy || !name.trim()}>
            {busy ? "Saving…" : "Save"}
          </button>
        </div>
      </form>
    </Modal>
  );
}

function splitNames(s: string): string[] {
  return s
    .split(/[\s,]+/)
    .map((v) => v.replace(/^@/, ""))
    .filter(Boolean);
}
