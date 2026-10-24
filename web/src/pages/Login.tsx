import { useState, type FormEvent } from "react";
import { errorMessage, post } from "../api";
import { Logo } from "../components/Logo";
import { ErrorBox } from "../components/ui";

export function LoginPage({ onLogin }: { onLogin: () => void }) {
  const [password, setPassword] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();

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
        <form onSubmit={submit} className="form box login-box">
          {error && <ErrorBox>{error}</ErrorBox>}
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
              autoFocus
              autoComplete="current-password"
              required
            />
          </label>
          <button type="submit" className="btn btn-primary btn-block" disabled={busy}>
            {busy ? "Signing in…" : "Sign in"}
          </button>
        </form>
      </div>
    </div>
  );
}
