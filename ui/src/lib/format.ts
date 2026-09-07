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
