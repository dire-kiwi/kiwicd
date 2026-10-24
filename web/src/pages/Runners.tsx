import { useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { del, errorMessage, post } from "../api";
import { StatusIcon } from "../components/StatusIcon";
import { Chip, CodeBlock, Empty, ErrorBox, Flash, Modal, Spinner } from "../components/ui";
import { useDocumentTitle, useEventSource, useFetch, useNow } from "../hooks";
import type { RegistrationToken, Runner } from "../types";
import { absoluteTime, relativeTime } from "../util";

export function RunnersPage() {
  useDocumentTitle("Runners");
  const runners = useFetch<Runner[]>("/api/runners");
  const { reload } = runners;
  const [showNew, setShowNew] = useState(false);
  const [error, setError] = useState<string>();
  const now = useNow(true, 15000);

  // Runner state changes on registration (event) and on heartbeats (poll).
  useEventSource("/api/events", { runners: reload, run: reload }, reload);
  useEffect(() => {
    const t = setInterval(reload, 15000);
    return () => clearInterval(t);
  }, [reload]);

  const remove = async (r: Runner) => {
    if (!window.confirm(`Remove runner "${r.name}"? It will no longer receive jobs and must be registered again.`)) return;
    setError(undefined);
    try {
      await del(`/api/runners/${r.id}`);
      reload();
    } catch (e) {
      setError(errorMessage(e));
    }
  };

  return (
    <div className="page">
      <div className="page-header">
        <div>
          <h1>Runners</h1>
          <p className="muted">Machines running kiwicd-agent that pick up jobs whose <code>runs-on</code> labels they match.</p>
        </div>
        <button className="btn btn-primary" onClick={() => setShowNew(true)}>
          New runner
        </button>
      </div>
      {error && <ErrorBox>{error}</ErrorBox>}
      {runners.error && !runners.data && <ErrorBox onRetry={reload}>{runners.error}</ErrorBox>}
      {!runners.data && !runners.error && <Spinner />}
      {runners.data && runners.data.length === 0 && (
        <Empty title="No runners registered">
          Jobs stay queued until a runner with matching labels is online.
          <div style={{ marginTop: 12 }}>
            <button className="btn btn-primary" onClick={() => setShowNew(true)}>
              Add a runner
            </button>
          </div>
        </Empty>
      )}
      {runners.data && runners.data.length > 0 && (
        <div className="box table-box">
          <table className="table runners-table">
            <thead>
              <tr>
                <th>Name</th>
                <th>Status</th>
                <th>Labels</th>
                <th>Platform</th>
                <th>Version</th>
                <th>Last seen</th>
                <th aria-label="Actions" />
              </tr>
            </thead>
            <tbody>
              {runners.data
                .slice()
                .sort((a, b) => a.name.localeCompare(b.name))
                .map((r) => (
                  <tr key={r.id}>
                    <td data-label="Name">
                      <strong>{r.name}</strong>
                    </td>
                    <td data-label="Status">
                      <RunnerStatus runner={r} />
                    </td>
                    <td data-label="Labels">
                      <div className="chips">
                        {(r.labels ?? []).map((l) => (
                          <Chip key={l}>{l}</Chip>
                        ))}
                      </div>
                      {Object.entries(r.reserved ?? {}).map(([label, who]) => (
                        <div key={label} className="small muted">
                          Takes only jobs of {who.join(", ")} (<code>{label}</code> is reserved)
                        </div>
                      ))}
                      {(r.protected_labels ?? []).length > 0 && (
                        <div className="small muted">
                          Takes only jobs from protected refs (
                          {(r.protected_labels ?? []).map((l) => (
                            <code key={l}>{l}</code>
                          ))}
                          )
                        </div>
                      )}
                    </td>
                    <td data-label="Platform" className="mono small">
                      {[r.os, r.arch].filter(Boolean).join("/") || "—"}
                    </td>
                    <td data-label="Version" className="mono small">
                      {r.version || "—"}
                    </td>
                    <td data-label="Last seen" className="small nowrap" title={absoluteTime(r.last_seen_at)}>
                      {r.last_seen_at ? relativeTime(r.last_seen_at, now) : "never"}
                    </td>
                    <td className="cell-actions">
                      <button className="btn btn-sm btn-danger-outline" onClick={() => remove(r)}>
                        Remove
                      </button>
                    </td>
                  </tr>
                ))}
            </tbody>
          </table>
        </div>
      )}
      {showNew && <NewRunnerModal onClose={() => setShowNew(false)} />}
    </div>
  );
}

function RunnerStatus({ runner }: { runner: Runner }) {
  if (!runner.online)
    return (
      <span className="runner-status">
        <span className="dot dot-offline" /> Offline
      </span>
    );
  if (runner.busy)
    return (
      <span className="runner-status">
        <StatusIcon state="in_progress" size={14} />
        <span>
          Busy
          {runner.job_id ? (
            <>
              {" "}
              · <Link to={runner.run_id ? `/runs/${runner.run_id}/jobs/${runner.job_id}` : `/jobs/${runner.job_id}`}>{runner.job_name || `job ${runner.job_id}`}</Link>
            </>
          ) : null}
        </span>
      </span>
    );
  return (
    <span className="runner-status">
      <span className="dot dot-online" /> Idle
    </span>
  );
}

function NewRunnerModal({ onClose }: { onClose: () => void }) {
  const [tok, setTok] = useState<RegistrationToken>();
  const [error, setError] = useState<string>();

  useEffect(() => {
    let cancelled = false;
    post<RegistrationToken>("/api/runners/registration-token").then(
      (t) => !cancelled && setTok(t),
      (e) => !cancelled && setError(errorMessage(e)),
    );
    return () => {
      cancelled = true;
    };
  }, []);

  return (
    <Modal title="Add a runner" onClose={onClose} wide>
      {error && <ErrorBox>{error}</ErrorBox>}
      {!tok && !error && <Spinner label="Creating registration token…" />}
      {tok && (
        <div className="stack">
          <Flash kind="info">
            This registration token expires in 1 hour ({absoluteTime(tok.expires_at)}) and won't be shown again. It can register
            several runners until then.
          </Flash>
          <div>
            <h3 className="h4">Run the agent directly</h3>
            <p className="muted small">
              Register once, then start the agent. <code>--labels</code> adds extra labels (<code>name</code>, <code>name:host</code> or{" "}
              <code>name:docker://image</code>); <code>--name</code> defaults to the hostname.
            </p>
            <CodeBlock
              code={`kiwicd-agent register \\\n  --url ${tok.url} \\\n  --token ${tok.token} \\\n  --name my-runner --labels deploy\nkiwicd-agent run`}
            />
          </div>
          <div>
            <h3 className="h4">Or with Docker</h3>
            <CodeBlock
              code={`docker run \\\n  -e KIWICD_URL=${tok.url} \\\n  -e KIWICD_REGISTRATION_TOKEN=${tok.token} \\\n  -e KIWICD_LABELS=deploy \\\n  ghcr.io/dire-kiwi/kiwicd-agent`}
            />
          </div>
          <p className="muted small">The runner appears in the list as soon as it registers.</p>
          <div className="form-actions">
            <button className="btn btn-primary" onClick={onClose}>
              Done
            </button>
          </div>
        </div>
      )}
    </Modal>
  );
}
