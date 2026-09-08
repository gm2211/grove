import type { JobStatus } from "../api/types";

// Reused everywhere a status needs a color: the host-level StatusPill (the only pill on the
// Fleet page) and the plain colored-dot + lowercase-text rows for VMs/nodes (see StateDot below).
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

export function statusColor(status: string): string {
  return STATUS_COLORS[status] ?? "var(--fg-faint)";
}

export function StatusPill({ status }: { status: JobStatus | string }) {
  const color = statusColor(status);
  return (
    <span
      className="inline-flex items-center gap-1.5 rounded-full border px-2 py-0.5 text-[11px] font-medium uppercase tracking-wide"
      style={{ borderColor: color, color }}
    >
      <span className="h-1.5 w-1.5 rounded-full" style={{ background: color }} />
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

/** Row-level state: a colored dot + lowercase text. Pills are reserved for host status. */
export function StateDot({ status }: { status: JobStatus | string }) {
  const color = statusColor(status);
  return (
    <span className="inline-flex items-center gap-1.5 whitespace-nowrap lowercase" style={{ color }}>
      <span className="h-1.5 w-1.5 shrink-0 rounded-full" style={{ background: color }} />
      {status}
    </span>
  );
}
