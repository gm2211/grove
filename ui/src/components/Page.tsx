import type { ReactNode } from "react";

/** Title row every page opens with: title + one-line description on the left, actions right. */
export function PageHeader({
  title,
  description,
  actions,
  eyebrow,
}: {
  title: ReactNode;
  description?: ReactNode;
  actions?: ReactNode;
  eyebrow?: ReactNode;
}) {
  return (
    <div className="mb-6 flex flex-wrap items-end justify-between gap-x-6 gap-y-3">
      <div className="flex min-w-0 flex-col gap-1">
        {eyebrow}
        <h1 className="text-[22px] leading-tight font-semibold tracking-tight">{title}</h1>
        {description && (
          <p className="text-[13px]" style={{ color: "var(--fg-muted)" }}>
            {description}
          </p>
        )}
      </div>
      {actions && <div className="flex flex-wrap items-center gap-2">{actions}</div>}
    </div>
  );
}

/** Elevated surface with an optional header row (title, description, actions). */
export function Card({
  title,
  description,
  actions,
  children,
  className = "",
  bodyClassName = "p-4",
}: {
  title?: ReactNode;
  description?: ReactNode;
  actions?: ReactNode;
  children: ReactNode;
  className?: string;
  bodyClassName?: string;
}) {
  return (
    <section
      className={`overflow-hidden rounded-xl border ${className}`}
      style={{ borderColor: "var(--border)", background: "var(--bg-elevated)", boxShadow: "var(--shadow-sm)" }}
    >
      {(title || actions) && (
        <header
          className="flex flex-wrap items-center justify-between gap-3 border-b px-4 py-3"
          style={{ borderColor: "var(--border)" }}
        >
          <div className="flex min-w-0 flex-col gap-0.5">
            {title && <h2 className="text-sm font-semibold">{title}</h2>}
            {description && (
              <p className="text-xs" style={{ color: "var(--fg-muted)" }}>
                {description}
              </p>
            )}
          </div>
          {actions && <div className="flex items-center gap-2">{actions}</div>}
        </header>
      )}
      <div className={bodyClassName}>{children}</div>
    </section>
  );
}

/** Centered placeholder for an empty list: icon, headline, a line of guidance, optional action. */
export function EmptyState({
  icon,
  title,
  children,
  action,
}: {
  icon?: ReactNode;
  title: ReactNode;
  children?: ReactNode;
  action?: ReactNode;
}) {
  return (
    <div
      className="flex flex-col items-center gap-2 rounded-xl border border-dashed px-6 py-12 text-center"
      style={{ borderColor: "var(--border-strong)" }}
    >
      {icon && (
        <span
          className="mb-1 flex h-10 w-10 items-center justify-center rounded-full"
          style={{ background: "var(--bg-inset)", color: "var(--fg-muted)" }}
        >
          {icon}
        </span>
      )}
      <div className="text-sm font-semibold">{title}</div>
      {children && (
        <div className="max-w-md text-[13px]" style={{ color: "var(--fg-muted)" }}>
          {children}
        </div>
      )}
      {action && <div className="mt-2">{action}</div>}
    </div>
  );
}

/** Inline loading / error line used while a page's first query is in flight. */
export function PageMessage({ tone = "muted", children }: { tone?: "muted" | "error"; children: ReactNode }) {
  return (
    <div
      className="rounded-xl border px-4 py-3 text-[13px]"
      style={{
        borderColor: tone === "error" ? "color-mix(in srgb, var(--status-failed) 40%, transparent)" : "var(--border)",
        background:
          tone === "error" ? "color-mix(in srgb, var(--status-failed) 8%, transparent)" : "var(--bg-elevated)",
        color: tone === "error" ? "var(--status-failed)" : "var(--fg-muted)",
      }}
    >
      {children}
    </div>
  );
}

/** Small neutral tag (pool names, arch, kind). */
export function Tag({ children, mono }: { children: ReactNode; mono?: boolean }) {
  return (
    <span
      className={`inline-flex shrink-0 items-center rounded-md px-1.5 py-px text-[11.5px] font-medium ${mono ? "mono" : ""}`}
      style={{ background: "var(--bg-inset)", color: "var(--fg-muted)", border: "1px solid var(--border)" }}
    >
      {children}
    </span>
  );
}
