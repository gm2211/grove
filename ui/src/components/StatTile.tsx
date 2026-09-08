import type { ReactNode } from "react";

/** A single stat in the Fleet summary strip: label, big number, optional sublabel/bar. */
export function StatTile({
  label,
  value,
  sublabel,
  bar,
}: {
  label: string;
  value: string | number;
  sublabel?: string;
  bar?: ReactNode;
}) {
  return (
    <div
      className="flex min-w-0 flex-1 flex-col gap-1.5 rounded-lg border p-3"
      style={{ borderColor: "var(--border)", background: "var(--bg-elevated)" }}
    >
      <div className="text-xs" style={{ color: "var(--fg-muted)" }}>
        {label}
      </div>
      <div className="text-2xl leading-none font-semibold">{value}</div>
      {sublabel && (
        <div className="text-[11px]" style={{ color: "var(--fg-faint)" }}>
          {sublabel}
        </div>
      )}
      {bar}
    </div>
  );
}
