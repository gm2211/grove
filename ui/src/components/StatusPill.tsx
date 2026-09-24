import type { JobStatus } from "../api/types";

// Reused everywhere a status needs a color: pills for job and host status, and the plain
// colored-dot + lowercase-text rows for VMs/nodes (see StateDot below).
export const STATUS_COLORS: Record<string, string> = {
  pending: "var(--status-pending)",
  running: "var(--status-running)",
  success: "var(--status-success)",
  failed: "var(--status-failed)",
  canceled: "var(--status-lost)",
  lost: "var(--status-lost)",
  ready: "var(--status-success)",
  offline: "var(--status-failed)",
  cordoned: "var(--status-lost)",
  initializing: "var(--status-pending)",
  down: "var(--status-failed)",
};

const LIVE_STATUSES = new Set(["running", "initializing"]);

export function statusColor(status: string): string {
  return STATUS_COLORS[status] ?? "var(--fg-faint)";
}

/** Soft, tinted pill: the status color at low opacity behind full-strength text. */
export function StatusPill({ status }: { status: JobStatus | string }) {
  const color = statusColor(status);
  return (
    <span
      className="inline-flex items-center gap-1.5 whitespace-nowrap rounded-full px-2 py-0.5 text-[11.5px] font-medium capitalize"
      style={{ color, background: `color-mix(in srgb, ${color} 12%, transparent)` }}
    >
      <PulseDot color={color} live={LIVE_STATUSES.has(status)} />
      {status}
    </span>
  );
}

export function Dot({ ok }: { ok: boolean }) {
  return (
    <span
      className="inline-block h-2 w-2 rounded-full"
      style={{ background: ok ? "var(--status-success)" : "var(--status-failed)" }}
    />
  );
}

/** Row-level state: a colored dot + lowercase text. Pills are reserved for host and job status. */
export function StateDot({ status }: { status: JobStatus | string }) {
  const color = statusColor(status);
  return (
    <span className="inline-flex items-center gap-1.5 whitespace-nowrap lowercase" style={{ color }}>
      <PulseDot color={color} live={LIVE_STATUSES.has(status)} />
      {status}
    </span>
  );
}

function PulseDot({ color, live }: { color: string; live: boolean }) {
  return (
    <span className="relative inline-flex h-1.5 w-1.5 shrink-0">
      {live && (
        <span
          className="absolute inline-flex h-full w-full animate-ping rounded-full opacity-60"
          style={{ background: color }}
        />
      )}
      <span className="relative inline-flex h-1.5 w-1.5 rounded-full" style={{ background: color }} />
    </span>
  );
}
