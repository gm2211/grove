import { NavLink, Outlet } from "react-router";
import { useQuery } from "@tanstack/react-query";
import { api } from "../api/client";
import { Dot } from "./StatusPill";

const NAV = [
  { to: "/fleet", label: "Fleet" },
  { to: "/jobs", label: "Jobs" },
  { to: "/dispatch", label: "Dispatch" },
  { to: "/settings", label: "Settings" },
];

export function Layout() {
  const health = useQuery({
    queryKey: ["health"],
    queryFn: api.getHealth,
    refetchInterval: 15_000,
    retry: false,
  });

  return (
    <div className="flex h-full min-h-screen flex-col">
      <header
        className="flex items-center gap-6 border-b px-4 py-2"
        style={{ borderColor: "var(--border)", background: "var(--bg-elevated)" }}
      >
        <div className="flex items-center gap-2 font-semibold tracking-tight">
          <span className="mono text-base">grove</span>
        </div>
        <nav className="flex gap-1">
          {NAV.map((item) => (
            <NavLink
              key={item.to}
              to={item.to}
              className={({ isActive }) =>
                `rounded px-3 py-1.5 text-sm font-medium transition-colors ${
                  isActive ? "" : "opacity-60 hover:opacity-100"
                }`
              }
              style={({ isActive }) => ({
                background: isActive ? "var(--bg-inset)" : "transparent",
              })}
            >
              {item.label}
            </NavLink>
          ))}
        </nav>
        <div className="ml-auto flex items-center gap-2 text-xs" style={{ color: "var(--fg-muted)" }}>
          <Dot ok={!!health.data?.ok} />
          {health.isLoading ? "checking…" : health.data?.ok ? "control plane reachable" : "unreachable"}
          {import.meta.env.VITE_MOCK === "1" && (
            <span
              className="rounded border px-1 py-0.5 text-[10px] font-medium"
              style={{ borderColor: "var(--border)", color: "var(--fg-faint)" }}
            >
              MOCK
            </span>
          )}
        </div>
      </header>
      <main className="flex-1 overflow-auto p-4">
        <Outlet />
      </main>
    </div>
  );
}
