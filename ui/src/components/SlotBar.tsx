/** Utilization color band, matching the dataviz meter convention: accent -> warning -> danger. */
function utilizationColor(used: number, total: number): string {
  if (total <= 0) return "var(--fg-faint)";
  const pct = used / total;
  if (pct >= 0.9) return "var(--status-failed)";
  if (pct >= 0.6) return "var(--status-lost)";
  return "var(--status-success)";
}

/**
 * VM slot utilization, rendered as filled segments (one per slot) colored by how full the host
 * is. Used on each host card (small counts) and, as a single continuous meter, in the fleet-wide
 * summary tile (see `variant="meter"`).
 */
export function SlotBar({
  used,
  total,
  variant = "segmented",
}: {
  used: number;
  total: number;
  variant?: "segmented" | "meter";
}) {
  const color = utilizationColor(used, total);
  const clampedUsed = Math.max(0, Math.min(used, total));

  if (variant === "meter" || total > 16) {
    const pct = total > 0 ? (clampedUsed / total) * 100 : 0;
    return (
      <div
        className="h-1.5 w-full overflow-hidden rounded-full"
        style={{ background: "var(--bg-inset)" }}
        role="img"
        aria-label={`${used} of ${total} slots used`}
      >
        <div
          className="h-full rounded-full transition-[width]"
          style={{ width: `${pct}%`, background: color }}
        />
      </div>
    );
  }

  const segments = Math.max(total, 1);
  return (
    <div
      className="flex flex-1 items-center gap-0.5"
      role="img"
      aria-label={`${used} of ${total} slots used`}
    >
      {Array.from({ length: segments }).map((_, i) => (
        <span
          key={i}
          className="h-1.5 flex-1 rounded-sm"
          style={{
            background: i < clampedUsed ? color : "var(--bg-inset)",
            border: "1px solid var(--border)",
          }}
        />
      ))}
    </div>
  );
}
