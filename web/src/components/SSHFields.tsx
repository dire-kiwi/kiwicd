// Form fields for a mirror's SSH key and host keys.

export type SSHAuth = "none" | "generate" | "paste";

export function SSHKeyField({
  auth,
  onAuth,
  sshKey,
  onSshKey,
  allowNone = true,
}: {
  auth: SSHAuth;
  onAuth: (a: SSHAuth) => void;
  sshKey: string;
  onSshKey: (v: string) => void;
  allowNone?: boolean;
}) {
  return (
    <>
      <label className="field">
        <span className="label">SSH key</span>
        <select className="input" value={auth} onChange={(e) => onAuth(e.target.value as SSHAuth)}>
          {allowNone && <option value="none">None</option>}
          <option value="generate">Generate a deploy key</option>
          <option value="paste">Use an existing private key</option>
        </select>
        <span className="hint">
          {auth === "none" && "For HTTPS URLs, or to use the server's own SSH keys."}
          {auth === "generate" &&
            "kiwicd creates a key pair and shows you the public key to add to the remote (on GitHub: Settings → Deploy keys)."}
          {auth === "paste" && "The key must not have a passphrase. It is stored on the server and never shown again."}
        </span>
      </label>
      {auth === "paste" && (
        <label className="field">
          <span className="label">Private key</span>
          <textarea
            className="input mono"
            rows={6}
            value={sshKey}
            onChange={(e) => onSshKey(e.target.value)}
            placeholder={"-----BEGIN OPENSSH PRIVATE KEY-----\n…"}
            spellCheck={false}
            autoComplete="off"
            required
          />
        </label>
      )}
    </>
  );
}

export function KnownHostsField({ value, onChange, host }: { value: string; onChange: (v: string) => void; host?: string }) {
  return (
    <label className="field">
      <span className="label">
        known_hosts <span className="muted">(optional)</span>
      </span>
      <textarea
        className="input mono"
        rows={4}
        value={value}
        onChange={(e) => onChange(e.target.value)}
        placeholder={`Output of: ssh-keyscan ${host || "HOST"}`}
        spellCheck={false}
      />
      <span className="hint">Leave empty to trust the server's host key on first connection and pin it from then on.</span>
    </label>
  );
}

export function isSSHURL(url: string): boolean {
  const u = url.trim();
  return u.startsWith("ssh://") || u.startsWith("git+ssh://") || /^[^/:]+@[^/:]+:/.test(u);
}

/** The host to ssh-keyscan, including -p for a non-standard port. */
export function sshHost(url: string): string {
  const u = url.trim();
  const m = /^(?:git\+)?ssh:\/\/(?:[^@/]+@)?([^/:]+)(?::(\d+))?/.exec(u);
  if (m) return m[2] && m[2] !== "22" ? `-p ${m[2]} ${m[1]}` : m[1];
  const s = /^[^@]+@([^:]+):/.exec(u);
  return s ? s[1] : "";
}
