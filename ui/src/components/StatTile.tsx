import type { ReactNode } from "react";

/** A single stat in a page's summary strip: icon + label, big number, optional hint/bar. */
export function StatTile({
  label,
  value,
  sublabel,
  icon,
  bar,
}: {
  label: string;
  value: string | number;
  sublabel?: ReactNode;
  icon?: ReactNode;
  bar?: ReactNode;
}) {
  return (
    <div
      className="flex min-w-0 flex-col gap-2 rounded-xl border p-4"
      style={{ borderColor: "var(--border)", background: "var(--bg-elevated)", boxShadow: "var(--shadow-sm)" }}
    >
      <div className="flex items-center gap-2 text-xs font-medium" style={{ color: "var(--fg-muted)" }}>
        {icon && (
          <span
            className="flex h-6 w-6 items-center justify-center rounded-md"
            style={{ background: "var(--bg-inset)", color: "var(--fg-muted)" }}
          >
            {icon}
          </span>
        )}
        {label}
      </div>
      <div className="flex items-baseline gap-2">
        <div className="tabular text-[26px] leading-none font-semibold tracking-tight">{value}</div>
        {sublabel && (
          <div className="truncate text-xs" style={{ color: "var(--fg-faint)" }}>
            {sublabel}
          </div>
        )}
      </div>
      {bar}
    </div>
  );
}
