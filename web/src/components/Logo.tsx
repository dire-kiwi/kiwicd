export function Logo({ size = 22 }: { size?: number }) {
  return (
    <svg width={size} height={size} viewBox="0 0 32 32" aria-hidden="true" className="logo">
      <circle cx="16" cy="16" r="15" fill="#5a7d2a" />
      <circle cx="16" cy="16" r="9.5" fill="#a6cf4f" />
      <g fill="#2b2b2b">
        <circle cx="16" cy="9.3" r="0.9" />
        <circle cx="21.8" cy="12.6" r="0.9" />
        <circle cx="21.8" cy="19.4" r="0.9" />
        <circle cx="16" cy="22.7" r="0.9" />
        <circle cx="10.2" cy="19.4" r="0.9" />
        <circle cx="10.2" cy="12.6" r="0.9" />
      </g>
      <circle cx="16" cy="16" r="3" fill="#f4f1e0" />
    </svg>
  );
}
