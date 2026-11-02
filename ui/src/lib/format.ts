export function formatRelativeAge(iso: string | null | undefined): string {
  if (!iso) return "—";
  const ms = Date.now() - new Date(iso).getTime();
  return formatDurationMs(ms) + " ago";
}

export function formatDurationMs(ms: number): string {
  if (ms < 0) ms = 0;
  const s = Math.floor(ms / 1000);
  if (s < 60) return `${s}s`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m ${s % 60}s`;
  const h = Math.floor(m / 60);
  if (h < 24) return `${h}h ${m % 60}m`;
  const d = Math.floor(h / 24);
  return `${d}d ${h % 24}h`;
}

export function formatJobDuration(startedAt?: string | null, finishedAt?: string | null): string {
  if (!startedAt) return "—";
  const end = finishedAt ? new Date(finishedAt).getTime() : Date.now();
  return formatDurationMs(end - new Date(startedAt).getTime());
}

export function formatMinutes(min: number | undefined): string {
  if (min === undefined || min === null) return "—";
  if (min < 60) return `${Math.round(min)}m`;
  return `${(min / 60).toFixed(1)}h`;
}

export function formatBytes(n: number): string {
  if (n < 1024) return `${n} B`;
  const units = ["KiB", "MiB", "GiB", "TiB"];
  let v = n / 1024;
  let i = 0;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i += 1;
  }
  return `${v.toFixed(1)} ${units[i]}`;
}

export function formatNanosAsDuration(ns: number | undefined): string {
  if (!ns) return "—";
  return formatDurationMs(ns / 1e6);
}

export function parseDurationToNanos(input: string): number | undefined {
  const trimmed = input.trim();
  if (!trimmed) return undefined;
  const match = trimmed.match(/^(\d+(?:\.\d+)?)(ms|s|m|h)$/);
  if (!match) return undefined;
  const value = parseFloat(match[1]);
  const unit = match[2];
  const factor: Record<string, number> = {
    ms: 1e6,
    s: 1e9,
    m: 60e9,
    h: 3600e9,
  };
  return Math.round(value * factor[unit]);
}

/**
 * Parses a Go duration string ("1h0m0s", "30m", "90s", "1.5h") into milliseconds. A bare number
 * is seconds, matching internal/dispatch/duration.go. Returns undefined for anything else.
 */
export function parseGoDurationMs(input: string | undefined): number | undefined {
  if (!input) return undefined;
  const s = input.trim();
  if (/^\d+(\.\d+)?$/.test(s)) return parseFloat(s) * 1000;
  const unit: Record<string, number> = { ns: 1e-6, us: 1e-3, "µs": 1e-3, ms: 1, s: 1000, m: 60_000, h: 3_600_000 };
  const re = /(\d+(?:\.\d+)?)(ns|us|µs|ms|s|m|h)/g;
  let total = 0;
  let consumed = 0;
  for (let m = re.exec(s); m; m = re.exec(s)) {
    total += parseFloat(m[1]) * unit[m[2]];
    consumed += m[0].length;
  }
  return consumed === s.length && consumed > 0 ? total : undefined;
}

/** "14:30" in the viewer's locale. */
export function formatClock(iso: string | Date | undefined | null): string {
  if (!iso) return "—";
  return new Date(iso).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
}

/** Short, glanceable duration: 42s, 7m, 3.4h, 2d. */
export function formatShort(ms: number): string {
  if (ms < 0) ms = 0;
  const s = ms / 1000;
  if (s < 60) return `${Math.floor(s)}s`;
  const m = s / 60;
  if (m < 60) return `${Math.floor(m)}m`;
  const h = m / 60;
  if (h < 24) return `${h.toFixed(1)}h`;
  return `${Math.floor(h / 24)}d`;
}

/** "github.com/gm2211/grove" → "gm2211/grove". */
export function shortRepo(repo: string | undefined): string {
  if (!repo) return "";
  return repo.replace(/^https?:\/\//, "").replace(/^github\.com\//, "").replace(/\.git$/, "");
}
