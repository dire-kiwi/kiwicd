import type { VisualState } from "../util";
import { stateLabel } from "../util";

export function StatusIcon({ state, size = 16, title }: { state: VisualState; size?: number; title?: string }) {
  const label = title ?? stateLabel(state);
  const common = {
    width: size,
    height: size,
    viewBox: "0 0 16 16",
    className: `status-icon status-${state}`,
    role: "img",
    "aria-label": label,
  } as const;
  switch (state) {
    case "success":
      return (
        <svg {...common}>
          <title>{label}</title>
          <circle cx="8" cy="8" r="8" fill="currentColor" />
          <path d="M4.6 8.3l2.2 2.2 4.6-4.8" fill="none" stroke="var(--icon-fg)" strokeWidth="1.7" strokeLinecap="round" strokeLinejoin="round" />
        </svg>
      );
    case "failure":
      return (
        <svg {...common}>
          <title>{label}</title>
          <circle cx="8" cy="8" r="8" fill="currentColor" />
          <path d="M5.5 5.5l5 5M10.5 5.5l-5 5" stroke="var(--icon-fg)" strokeWidth="1.7" strokeLinecap="round" />
        </svg>
      );
    case "cancelled":
    case "skipped":
      return (
        <svg {...common}>
          <title>{label}</title>
          <circle cx="8" cy="8" r="6.75" fill="none" stroke="currentColor" strokeWidth="1.5" />
          <path d="M3.4 12.6L12.6 3.4" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" />
        </svg>
      );
    case "in_progress":
    case "cancelling":
      return (
        <svg {...common}>
          <title>{label}</title>
          <circle cx="8" cy="8" r="6.5" fill="none" stroke="currentColor" strokeOpacity="0.3" strokeWidth="2" />
          <path className="spin" d="M8 1.5a6.5 6.5 0 0 1 6.5 6.5" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" />
          <circle cx="8" cy="8" r="2.5" fill="currentColor" />
        </svg>
      );
    case "queued":
      return (
        <svg {...common}>
          <title>{label}</title>
          <circle cx="8" cy="8" r="6.5" fill="none" stroke="currentColor" strokeWidth="2" />
        </svg>
      );
    case "approval":
      return (
        <svg {...common}>
          <title>{label}</title>
          <circle cx="8" cy="8" r="6.5" fill="none" stroke="currentColor" strokeWidth="1.5" />
          <path d="M6.3 5.4v5.2M9.7 5.4v5.2" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" />
        </svg>
      );
    case "pending":
    default:
      return (
        <svg {...common}>
          <title>{label}</title>
          <circle cx="8" cy="8" r="6.5" fill="none" stroke="currentColor" strokeWidth="1.5" strokeDasharray="2.6 2" />
        </svg>
      );
  }
}
