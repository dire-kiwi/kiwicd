import { createContext, useCallback, useContext, useEffect, useState } from "react";
import { Link, NavLink, Navigate, Route, Routes } from "react-router-dom";
import { ApiError, errorMessage, get, onUnauthorized, post } from "./api";
import { ErrorBox, Spinner } from "./components/ui";
import { Logo } from "./components/Logo";
import type { Me } from "./types";
import { LoginPage } from "./pages/Login";
import { RunsPage } from "./pages/Runs";
import { ReposPage } from "./pages/Repos";
import { RepoPage } from "./pages/Repo";
import { RunPage } from "./pages/Run";
import { RunnersPage } from "./pages/Runners";
import { SettingsPage } from "./pages/Settings";
import { JobRedirect } from "./pages/JobRedirect";

const MeContext = createContext<Me | null>(null);
export const useMe = () => useContext(MeContext);

export function App() {
  const [me, setMe] = useState<Me | null>(null);
  const [state, setState] = useState<"loading" | "anon" | "ok" | "error">("loading");
  const [error, setError] = useState<string>();

  const load = useCallback(() => {
    setState("loading");
    get<Me>("/api/me").then(
      (m) => {
        setMe(m);
        setState("ok");
      },
      (e) => {
        if (e instanceof ApiError && e.status === 401) setState("anon");
        else {
          setError(errorMessage(e));
          setState("error");
        }
      },
    );
  }, []);

  useEffect(load, [load]);
  useEffect(
    () =>
      onUnauthorized(() => {
        setMe(null);
        setState("anon");
      }),
    [],
  );

  if (state === "loading")
    return (
      <div className="center-page">
        <Spinner />
      </div>
    );
  if (state === "error")
    return (
      <div className="center-page">
        <ErrorBox onRetry={load}>Could not reach the kiwicd server: {error}</ErrorBox>
      </div>
    );
  if (state === "anon" || !me) return <LoginPage onLogin={load} />;

  const logout = async () => {
    try {
      await post("/api/logout");
    } finally {
      setMe(null);
      setState("anon");
    }
  };

  return (
    <MeContext.Provider value={me}>
      <div className="app">
        <header className="topbar">
          <div className="topbar-inner">
            <Link to="/" className="brand">
              <Logo />
              <span>kiwicd</span>
            </Link>
            <nav className="nav">
              <NavLink to="/" end>
                Runs
              </NavLink>
              <NavLink to="/repos">
                <span className="nav-long">Repositories</span>
                <span className="nav-short">Repos</span>
              </NavLink>
              <NavLink to="/runners">Runners</NavLink>
              <NavLink to="/settings">Settings</NavLink>
            </nav>
            <div className="topbar-right">
              <span className="topbar-user" title={`kiwicd ${me.version}`}>
                {me.user}
              </span>
              <button className="btn btn-sm btn-ghost-inverse" onClick={logout}>
                Sign out
              </button>
            </div>
          </div>
        </header>
        <main className="main">
          <Routes>
            <Route path="/" element={<RunsPage />} />
            <Route path="/repos" element={<ReposPage />} />
            <Route path="/repos/:owner/:name/*" element={<RepoPage />} />
            <Route path="/runs/:id" element={<RunPage />} />
            <Route path="/runs/:id/jobs/:jobId" element={<RunPage />} />
            <Route path="/jobs/:jobId" element={<JobRedirect />} />
            <Route path="/runners" element={<RunnersPage />} />
            <Route path="/settings" element={<SettingsPage />} />
            <Route path="/login" element={<Navigate to="/" replace />} />
            <Route path="*" element={<NotFound />} />
          </Routes>
        </main>
        <footer className="footer muted small">
          kiwicd {me.version} · <a href={me.public_url}>{me.public_url.replace(/^https?:\/\//, "")}</a>
        </footer>
      </div>
    </MeContext.Provider>
  );
}

function NotFound() {
  return (
    <div className="page">
      <div className="empty">
        <div className="empty-title">Page not found</div>
        <div className="empty-body">
          <Link to="/">Back to runs</Link>
        </div>
      </div>
    </div>
  );
}
