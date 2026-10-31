import { useState, type FormEvent } from "react";
import { del, errorMessage, patch, post, put } from "../api";
import { useFetch } from "../hooks";
import type { GitHubAccess, GitHubApp, GitHubSettings } from "../types";
import { CopyField, ErrorBox, Flash, Spinner } from "./ui";

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
            webhook of their own, lets mirrors of private ones fetch without a deploy key, and shows their runs on GitHub as check
            runs. It also lets people sign in with GitHub.
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
          pull requests and organization members, write access to checks, and the push, pull request, check run and check suite
          events. Its owner may sign in.
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
  const [f, setF] = useState({ app_id: "", client_id: "", client_secret: "", private_key: "", webhook_secret: "", allowed_users: "" });
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();
  const set = (k: keyof typeof f) => (e: { target: { value: string } }) => setF({ ...f, [k]: e.target.value });

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError(undefined);
    try {
      await put("/api/github/app", { ...f, app_id: Number(f.app_id), allowed_users: splitNames(f.allowed_users) });
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
        members, read and write access to checks, and subscribe it to push, pull request, check run and check suite events.
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
      <label className="field">
        <span className="label">GitHub users who may sign in</span>
        <input className="input mono" value={f.allowed_users} onChange={set("allowed_users")} placeholder="octocat, hubot" />
      </label>
      <div className="form-actions">
        <button type="submit" className="btn btn-primary" disabled={busy}>
          {busy ? "Checking with GitHub…" : "Save"}
        </button>
      </div>
    </form>
  );
}

function AppDetails({ settings, app, onChange }: { settings: GitHubSettings; app: GitHubApp; onChange: () => void }) {
  const [users, setUsers] = useState(app.allowed_users.join(", "));
  const [orgs, setOrgs] = useState(app.allowed_orgs.join(", "));
  const [busy, setBusy] = useState(false);
  const [saved, setSaved] = useState(false);
  const [error, setError] = useState<string>();
  const removing = app.allowed_users.some((u) => !hasFold(splitNames(users), u)) || app.allowed_orgs.some((o) => !hasFold(splitNames(orgs), o));

  const save = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setSaved(false);
    setError(undefined);
    try {
      const res = await patch<GitHubSettings>("/api/github/app", { allowed_users: splitNames(users), allowed_orgs: splitNames(orgs) });
      if (res.app) {
        setUsers(res.app.allowed_users.join(", "));
        setOrgs(res.app.allowed_orgs.join(", "));
      }
      setSaved(true);
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

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
        Install the app on the repositories kiwicd mirrors: their pushes and pull requests then reach their mirrors at once,
        mirrors with an HTTPS URL on GitHub fetch with the app's token, and their runs show on their commits and pull requests as
        check runs. Remove any webhook you added to such a repository for kiwicd, or its pull requests start runs twice.
      </p>
      {app.access && <AccessWarning access={app.access} />}
      {app.access_error && (
        <Flash kind="warn">
          <span>Couldn't ask GitHub what the app may do: {app.access_error}</span>
        </Flash>
      )}
      <div className="inline-actions">
        {app.install_url && (
          <a className="btn btn-primary" href={app.install_url} target="_blank" rel="noreferrer">
            Install on repositories
          </a>
        )}
      </div>
      <form className="form" onSubmit={save}>
        <h4>Sign in with GitHub</h4>
        <p className="muted small">
          Everyone who signs in can do everything the admin can. Organizations' members are checked when they sign in, which needs
          the app installed on the organization; sessions last a week.
        </p>
        <label className="field">
          <span className="label">Users</span>
          <input className="input mono" value={users} onChange={(e) => setUsers(e.target.value)} placeholder="octocat, hubot" />
        </label>
        <label className="field">
          <span className="label">Members of organizations</span>
          <input className="input mono" value={orgs} onChange={(e) => setOrgs(e.target.value)} placeholder="my-org" />
          {removing && <span className="hint">Removing someone signs out everyone signed in with GitHub.</span>}
        </label>
        <div className="form-actions">
          {saved && <span className="muted small">Saved.</span>}
          <button type="button" className="btn btn-danger-outline" onClick={remove}>
            Remove app
          </button>
          <button type="submit" className="btn btn-primary" disabled={busy}>
            {busy ? "Saving…" : "Save"}
          </button>
        </div>
      </form>
    </div>
  );
}

/** What the app or its installations lack for reporting check runs. */
function AccessWarning({ access }: { access: GitHubAccess }) {
  const unapproved = access.installations.filter((i) => !i.checks);
  if (access.missing.length === 0 && unapproved.length === 0) return null;
  return (
    <Flash kind="warn">
      <div className="stack-sm">
        {access.missing.length > 0 && (
          <span>
            The app needs {access.missing.join(", ")} to report runs to GitHub as check runs and re-run them from there. Add them
            under the app's{" "}
            <a href={access.permissions_url} target="_blank" rel="noreferrer">
              permissions and events
            </a>{" "}
            on GitHub (Checks: Read and write; Check run and Check suite events), then accept the new permissions on each
            installation.
          </span>
        )}
        {unapproved.length > 0 && (
          <span>
            {access.missing.length > 0 ? "These installations" : "The app asks for the checks permission, but these installations"}{" "}
            haven't accepted it yet, so their runs aren't reported:{" "}
            {unapproved.map((i, n) => (
              <span key={i.html_url}>
                {n > 0 && ", "}
                <a href={i.html_url} target="_blank" rel="noreferrer">
                  {i.account}
                </a>
              </span>
            ))}
            .
          </span>
        )}
      </div>
    </Flash>
  );
}

function splitNames(s: string): string[] {
  return s
    .split(/[\s,]+/)
    .map((v) => v.replace(/^@/, ""))
    .filter(Boolean);
}

function hasFold(list: string[], v: string): boolean {
  return list.some((x) => x.toLowerCase() === v.toLowerCase());
}
