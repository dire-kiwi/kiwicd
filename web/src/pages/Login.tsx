import { useEffect, useState, type FormEvent } from "react";
import { errorMessage, get, post } from "../api";
import { Logo } from "../components/Logo";
import { ErrorBox } from "../components/ui";

/** The error a failed sign-in with GitHub comes back with, as /login?error=. */
function takeURLError(): string | undefined {
  const params = new URLSearchParams(window.location.search);
  const error = params.get("error");
  if (error === null) return undefined;
  params.delete("error");
  const q = params.toString();
  window.history.replaceState(null, "", window.location.pathname + (q ? `?${q}` : "") + window.location.hash);
  return error;
}

export function LoginPage({ onLogin }: { onLogin: () => void }) {
  const [password, setPassword] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | undefined>(takeURLError);
  const [github, setGithub] = useState(false);

  useEffect(() => {
    get<{ github: boolean }>("/api/auth/providers").then(
      (p) => setGithub(p.github),
      () => {},
    );
  }, []);

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError(undefined);
    try {
      await post("/api/login", { password });
      onLogin();
    } catch (err) {
      setError(errorMessage(err));
      setBusy(false);
    }
  };

  return (
    <div className="login-page">
      <div className="login-card">
        <div className="login-brand">
          <Logo size={44} />
          <h1>Sign in to kiwicd</h1>
        </div>
        {error && (
          <div className="login-github">
            <ErrorBox>{error}</ErrorBox>
          </div>
        )}
        {github && (
          <div className="box login-box login-github">
            <a className="btn btn-primary btn-block" href="/api/auth/github">
              Sign in with GitHub
            </a>
          </div>
        )}
        <form onSubmit={submit} className="form box login-box">
          {github && <span className="muted small">Or as the admin:</span>}
          <label className="field">
            <span className="label">Username</span>
            <input className="input" value="admin" readOnly autoComplete="username" tabIndex={-1} />
          </label>
          <label className="field">
            <span className="label">Password</span>
            <input
              className="input"
              type="password"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              autoFocus={!github}
              autoComplete="current-password"
              required
            />
          </label>
          <button type="submit" className={`btn btn-block ${github ? "" : "btn-primary"}`} disabled={busy}>
            {busy ? "Signing in…" : "Sign in"}
          </button>
        </form>
      </div>
    </div>
  );
}
