import type { JobStatus } from "../api/types";

/**
 * One status → one tone, everywhere. Teal is healthy or working, blue is waiting, amber is
 * paused or gave up, red is broken, grey is nothing happening.
 */
export type Tone = "teal" | "blue" | "amber" | "red" | "grey";

const TONE_OF: Record<string, Tone> = {
  // jobs
  pending: "blue",
  running: "teal",
  success: "teal",
  failed: "red",
  canceled: "amber",
  lost: "amber",
  // workers
  online: "teal",
  ready: "teal",
  paused: "amber",
  cordoned: "amber",
  snoozed: "amber",
  offline: "red",
  // VMs / nodes
  initializing: "blue",
  starting: "blue",
  down: "red",
  // job attempts
  exited: "grey",
  setup: "amber",
};

export const TONES: Record<Tone, { dot: string; text: string; line: string; wash: string }> = {
  teal: { dot: "var(--accent)", text: "var(--accent)", line: "var(--accent-line)", wash: "var(--accent-wash-2)" },
  blue: { dot: "var(--blue)", text: "var(--blue)", line: "var(--blue-line)", wash: "var(--blue-wash)" },
  amber: { dot: "var(--amber)", text: "var(--amber)", line: "var(--amber-line)", wash: "var(--amber-wash)" },
  red: { dot: "var(--red)", text: "var(--red-text)", line: "var(--red-line)", wash: "var(--red-wash)" },
  grey: { dot: "var(--grey)", text: "var(--fg-muted)", line: "var(--border-3)", wash: "transparent" },
};

export function toneOf(status: string | undefined): Tone {
  return TONE_OF[status ?? ""] ?? "grey";
}

export function statusColor(status: string): string {
  return TONES[toneOf(status)].dot;
}

/** The Argos `.chip`: mono, uppercase, coloured hairline. */
export function StatusPill({ status, label }: { status: JobStatus | string; label?: string }) {
  const t = TONES[toneOf(status)];
  return (
    <span className="chip" style={{ borderColor: t.line, color: t.text, background: t.wash }}>
      {label ?? status}
    </span>
  );
}

export function Dot({ ok }: { ok: boolean }) {
  return <span className={`dot ${ok ? "live" : ""}`} style={{ background: ok ? "var(--accent)" : "var(--red)" }} />;
}

/** Row-level state: a coloured dot + lowercase mono text. */
export function StateDot({ status }: { status: JobStatus | string }) {
  const t = TONES[toneOf(status)];
  return (
    <span className="mono inline-flex items-center gap-2 whitespace-nowrap text-[11.5px] lowercase" style={{ color: t.text }}>
      <span className={`dot ${status === "running" ? "live" : ""}`} style={{ background: t.dot }} />
      {status}
    </span>
  );
}
