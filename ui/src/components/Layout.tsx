import type { ComponentType } from "react";
import { NavLink, Outlet } from "react-router";
import { useQuery } from "@tanstack/react-query";
import { api } from "../api/client";
import { getServerUrl } from "../lib/settings";
import { DispatchIcon, FleetIcon, GroveMark, JobsIcon, SettingsIcon } from "./Icons";

const NAV: { to: string; label: string; icon: ComponentType<{ size?: number }> }[] = [
  { to: "/fleet", label: "Fleet", icon: FleetIcon },
  { to: "/jobs", label: "Jobs", icon: JobsIcon },
  { to: "/dispatch", label: "Dispatch", icon: DispatchIcon },
  { to: "/settings", label: "Settings", icon: SettingsIcon },
];

/**
 * App shell: a fixed sidebar (brand, nav, control-plane health) on desktop, collapsing to a
 * compact top bar with the same nav on narrow screens.
 */
export function Layout() {
  const health = useQuery({
    queryKey: ["health"],
    queryFn: api.getHealth,
    refetchInterval: 15_000,
    retry: false,
  });
  const ok = !!health.data?.ok;
  const mock = import.meta.env.VITE_MOCK === "1";
  const healthLabel = health.isLoading ? "Checking…" : ok ? "Connected" : "Unreachable";
  const host = (() => {
    try {
      return new URL(getServerUrl()).host;
    } catch {
      return window.location.host;
    }
  })();

  return (
    <div className="flex min-h-screen flex-col md:flex-row">
      <aside
        className="sticky top-0 z-10 flex shrink-0 flex-col border-b md:h-screen md:w-60 md:border-r md:border-b-0"
        style={{ borderColor: "var(--border)", background: "var(--bg-elevated)" }}
      >
        <div className="flex items-center gap-3 px-4 py-3 md:px-5 md:py-5">
          <Brand />
          <div className="ml-auto flex items-center gap-2 md:hidden">
            <HealthDot ok={ok} loading={health.isLoading} />
            {mock && <MockBadge />}
          </div>
        </div>

        <nav className="flex gap-1 overflow-x-auto px-2 pb-2 md:flex-col md:px-3 md:pb-0">
          {NAV.map(({ to, label, icon: Icon }) => (
            <NavLink
              key={to}
              to={to}
              className="group flex shrink-0 items-center gap-2.5 rounded-lg px-3 py-2 text-[13px] font-medium transition-colors"
              style={({ isActive }) => ({
                background: isActive ? "var(--accent-soft)" : "transparent",
                color: isActive ? "var(--accent)" : "var(--fg-muted)",
              })}
            >
              <Icon size={16} />
              {label}
            </NavLink>
          ))}
        </nav>

        <div className="mt-auto hidden p-3 md:block">
          <div
            className="flex flex-col gap-1 rounded-lg border px-3 py-2.5"
            style={{ borderColor: "var(--border)", background: "var(--bg)" }}
          >
            <div className="flex items-center gap-2 text-xs font-medium">
              <HealthDot ok={ok} loading={health.isLoading} />
              <span>{healthLabel}</span>
              {mock && <MockBadge />}
            </div>
            <div className="mono truncate text-[11px]" style={{ color: "var(--fg-faint)" }} title={host}>
              {host}
            </div>
          </div>
        </div>
      </aside>

      <main className="min-w-0 flex-1">
        <div className="mx-auto w-full max-w-[1400px] px-4 py-5 md:px-8 md:py-7">
          <Outlet />
        </div>
      </main>
    </div>
  );
}

function Brand() {
  return (
    <div className="flex items-center gap-2.5">
      <span
        className="flex h-8 w-8 items-center justify-center rounded-lg"
        style={{ background: "var(--accent)", color: "var(--accent-fg)" }}
      >
        <GroveMark size={18} />
      </span>
      <div className="flex flex-col leading-tight">
        <span className="text-[15px] font-semibold tracking-tight">grove</span>
        <span className="hidden text-[11px] md:block" style={{ color: "var(--fg-faint)" }}>
          control plane
        </span>
      </div>
    </div>
  );
}

function HealthDot({ ok, loading }: { ok: boolean; loading: boolean }) {
  const color = loading ? "var(--status-pending)" : ok ? "var(--status-success)" : "var(--status-failed)";
  return (
    <span className="relative inline-flex h-2 w-2" title={ok ? "control plane reachable" : "unreachable"}>
      {ok && (
        <span
          className="absolute inline-flex h-full w-full animate-ping rounded-full opacity-40"
          style={{ background: color }}
        />
      )}
      <span className="relative inline-flex h-2 w-2 rounded-full" style={{ background: color }} />
    </span>
  );
}

function MockBadge() {
  return (
    <span
      className="rounded px-1.5 py-px text-[10px] font-semibold tracking-wide"
      style={{ background: "var(--bg-inset)", color: "var(--fg-faint)" }}
    >
      MOCK
    </span>
  );
}
