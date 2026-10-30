import { useState } from "react";
import { errorMessage, post } from "../api";
import { useFetch } from "../hooks";
import type { SecretsKey } from "../types";
import { CopyField, ErrorBox, Flash } from "./ui";

/** Whether secrets are sealed to a secrets service's public key, and resealing those the server can still read. */
export function SecretsKeySettings() {
  const key = useFetch<SecretsKey>("/api/secrets/key");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();
  const [done, setDone] = useState<number>();
  const k = key.data;
  if (!k) return key.error ? <ErrorBox onRetry={key.reload}>{key.error}</ErrorBox> : null;

  const reseal = async () => {
    if (!window.confirm(`Seal ${k.server_sealed} secrets to the public key ${k.key_id}? After this only the secrets service can read them.`)) return;
    setBusy(true);
    setError(undefined);
    try {
      const res = await post<{ resealed: number }>("/api/secrets/reseal");
      setDone(res.resealed);
      key.reload();
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
          <h3>Secrets encryption</h3>
          <p className="muted small">
            {k.public_key ? (
              <>
                Secrets are sealed to the public key of a secrets service (<span className="mono">kiwicd-secrets</span>), so this server
                can't read them. Agents get them from the service when a job starts (<span className="mono">kiwicd-agent run --secrets-url</span>).
              </>
            ) : (
              <>
                Secrets are encrypted with this server's own key, and it sends them to agents with their jobs. Start the server with{" "}
                <span className="mono">--secrets-public-key</span> to seal them to a secrets service's key instead, so the server can't read
                them.
              </>
            )}
          </p>
        </div>
        {k.public_key && k.server_sealed > 0 && (
          <button className="btn btn-primary" onClick={reseal} disabled={busy}>
            {busy ? "Sealing…" : `Seal ${k.server_sealed} to the public key`}
          </button>
        )}
      </div>
      {error && <Flash kind="error">{error}</Flash>}
      {done !== undefined && <Flash kind="success">Sealed {done} secrets to the public key.</Flash>}
      {k.public_key && (
        <div className="kv kv-pad">
          <span className="label">Public key</span>
          <CopyField value={k.public_key} />
          <span className="label">Key ID</span>
          <span className="mono">{k.key_id}</span>
          {k.server_sealed > 0 && (
            <>
              <span className="label">Readable by the server</span>
              <span>
                {k.server_sealed} secrets, stored before the public key was set. Seal them to it, or update them.
              </span>
            </>
          )}
          {k.other_key > 0 && (
            <>
              <span className="label">Older keys</span>
              <span>{k.other_key} secrets are sealed to another public key. Update them, or keep that key in the secrets service.</span>
            </>
          )}
        </div>
      )}
    </section>
  );
}
