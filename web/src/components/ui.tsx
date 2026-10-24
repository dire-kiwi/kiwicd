import { useEffect, useRef, useState, type ReactNode } from "react";
import { classes } from "../util";

export function Spinner({ label = "Loading…" }: { label?: string }) {
  return (
    <div className="loading">
      <span className="spinner" aria-hidden="true" />
      <span>{label}</span>
    </div>
  );
}

export function ErrorBox({ children, onRetry }: { children: ReactNode; onRetry?: () => void }) {
  return (
    <div className="flash flash-error" role="alert">
      <div className="flash-body">{children}</div>
      {onRetry && (
        <button className="btn btn-sm" onClick={onRetry}>
          Retry
        </button>
      )}
    </div>
  );
}

export function Flash({ kind = "info", children }: { kind?: "info" | "warn" | "error" | "success"; children: ReactNode }) {
  return <div className={`flash flash-${kind}`}>{children}</div>;
}

export function Empty({ title, children }: { title: string; children?: ReactNode }) {
  return (
    <div className="empty">
      <div className="empty-title">{title}</div>
      {children && <div className="empty-body">{children}</div>}
    </div>
  );
}

async function copyText(text: string): Promise<boolean> {
  try {
    await navigator.clipboard.writeText(text);
    return true;
  } catch {
    // Fallback for non-secure contexts (plain http on a LAN address).
    const ta = document.createElement("textarea");
    ta.value = text;
    ta.style.position = "fixed";
    ta.style.opacity = "0";
    document.body.appendChild(ta);
    ta.select();
    let ok = false;
    try {
      ok = document.execCommand("copy");
    } catch {
      ok = false;
    }
    document.body.removeChild(ta);
    return ok;
  }
}

export function CopyButton({ text, label = "Copy", className }: { text: string; label?: string; className?: string }) {
  const [copied, setCopied] = useState(false);
  const timer = useRef<ReturnType<typeof setTimeout>>();
  useEffect(() => () => clearTimeout(timer.current), []);
  return (
    <button
      type="button"
      className={classes("btn btn-sm btn-copy", className)}
      onClick={async () => {
        if (await copyText(text)) {
          setCopied(true);
          clearTimeout(timer.current);
          timer.current = setTimeout(() => setCopied(false), 1500);
        }
      }}
      title="Copy to clipboard"
    >
      {copied ? "Copied" : label}
    </button>
  );
}

/** Single-line value with copy button (clone URL, webhook URL, …). */
export function CopyField({ value, secret }: { value: string; secret?: boolean }) {
  const [shown, setShown] = useState(!secret);
  return (
    <div className="copy-field">
      <input className="input mono" readOnly value={shown ? value : "•".repeat(Math.min(value.length, 32))} onFocus={(e) => e.target.select()} />
      {secret && (
        <button type="button" className="btn btn-sm" onClick={() => setShown((s) => !s)}>
          {shown ? "Hide" : "Show"}
        </button>
      )}
      <CopyButton text={value} />
    </div>
  );
}

/** Multi-line command block with copy button. */
export function CodeBlock({ code }: { code: string }) {
  return (
    <div className="code-block">
      <pre>
        <code>{code}</code>
      </pre>
      <CopyButton text={code} className="code-copy" />
    </div>
  );
}

export function Chip({ children, title }: { children: ReactNode; title?: string }) {
  return (
    <span className="chip" title={title}>
      {children}
    </span>
  );
}

export function BranchChip({ name }: { name: string }) {
  if (!name) return null;
  return (
    <span className="branch" title={name}>
      {name}
    </span>
  );
}

export function Modal({ title, onClose, children, wide }: { title: string; onClose: () => void; children: ReactNode; wide?: boolean }) {
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") onClose();
    };
    window.addEventListener("keydown", onKey);
    const prev = document.body.style.overflow;
    document.body.style.overflow = "hidden";
    return () => {
      window.removeEventListener("keydown", onKey);
      document.body.style.overflow = prev;
    };
  }, [onClose]);
  return (
    <div className="modal-backdrop" onMouseDown={(e) => e.target === e.currentTarget && onClose()}>
      <div className={classes("modal", wide && "modal-wide")} role="dialog" aria-modal="true" aria-label={title}>
        <div className="modal-header">
          <h2>{title}</h2>
          <button className="btn-icon" onClick={onClose} aria-label="Close">
            <svg width="16" height="16" viewBox="0 0 16 16">
              <path d="M4 4l8 8M12 4l-8 8" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" />
            </svg>
          </button>
        </div>
        <div className="modal-body">{children}</div>
      </div>
    </div>
  );
}

export function Icon({ name, size = 16 }: { name: "clock" | "calendar" | "branch" | "repo" | "commit" | "user" | "chevron" | "play" | "bolt"; size?: number }) {
  const p = { width: size, height: size, viewBox: "0 0 16 16", className: "icon", "aria-hidden": true } as const;
  switch (name) {
    case "clock":
      return (
        <svg {...p}>
          <circle cx="8" cy="8" r="6.25" fill="none" stroke="currentColor" strokeWidth="1.5" />
          <path d="M8 4.5V8l2.5 1.5" fill="none" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" />
        </svg>
      );
    case "calendar":
      return (
        <svg {...p}>
          <rect x="2" y="3" width="12" height="11" rx="1.5" fill="none" stroke="currentColor" strokeWidth="1.5" />
          <path d="M2 6.5h12M5 1.5v3M11 1.5v3" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" />
        </svg>
      );
    case "branch":
      return (
        <svg {...p}>
          <circle cx="4.5" cy="3.5" r="1.75" fill="none" stroke="currentColor" strokeWidth="1.4" />
          <circle cx="4.5" cy="12.5" r="1.75" fill="none" stroke="currentColor" strokeWidth="1.4" />
          <circle cx="11.5" cy="4.5" r="1.75" fill="none" stroke="currentColor" strokeWidth="1.4" />
          <path d="M4.5 5.25v5.5M11.5 6.25c0 3-7 2-7 4.5" fill="none" stroke="currentColor" strokeWidth="1.4" />
        </svg>
      );
    case "repo":
      return (
        <svg {...p}>
          <path d="M3 13V3a1.5 1.5 0 0 1 1.5-1.5H13v10H4.5A1.5 1.5 0 0 0 3 13a1.5 1.5 0 0 0 1.5 1.5H13" fill="none" stroke="currentColor" strokeWidth="1.4" strokeLinejoin="round" />
        </svg>
      );
    case "commit":
      return (
        <svg {...p}>
          <circle cx="8" cy="8" r="2.75" fill="none" stroke="currentColor" strokeWidth="1.5" />
          <path d="M1 8h4.25M10.75 8H15" stroke="currentColor" strokeWidth="1.5" />
        </svg>
      );
    case "user":
      return (
        <svg {...p}>
          <circle cx="8" cy="5.5" r="3" fill="none" stroke="currentColor" strokeWidth="1.5" />
          <path d="M2.5 14.5c.6-3 2.8-4.5 5.5-4.5s4.9 1.5 5.5 4.5" fill="none" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" />
        </svg>
      );
    case "chevron":
      return (
        <svg {...p}>
          <path d="M6 3.5L10.5 8 6 12.5" fill="none" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" strokeLinejoin="round" />
        </svg>
      );
    case "play":
      return (
        <svg {...p}>
          <path d="M5 3.2v9.6L12.5 8z" fill="currentColor" />
        </svg>
      );
    case "bolt":
      return (
        <svg {...p}>
          <path d="M9 1.5L3.5 9h4L7 14.5 12.5 7h-4z" fill="none" stroke="currentColor" strokeWidth="1.4" strokeLinejoin="round" />
        </svg>
      );
  }
}
