import { useEffect, useState } from "react";
import { NavLink, Outlet } from "react-router";
import { useQuery } from "@tanstack/react-query";
import { api } from "../api/client";
import { Logo } from "./Logo";
import { GearIcon, ListIcon, MapIcon, SendIcon } from "./Icons";

const NAV = [
  { to: "/map", label: "Map", icon: <MapIcon /> },
  { to: "/jobs", label: "Jobs", icon: <ListIcon /> },
  { to: "/dispatch", label: "Dispatch", icon: <SendIcon /> },
  { to: "/settings", label: "Settings", icon: <GearIcon /> },
];

function Clock() {
  const [now, setNow] = useState(() => new Date());
  useEffect(() => {
    const t = setInterval(() => setNow(new Date()), 1000);
    return () => clearInterval(t);
  }, []);
  return <span className="mono hidden text-[12.5px] text-[#c3d1d5] md:inline">{now.toLocaleTimeString()}</span>;
}

export function Layout() {
  const health = useQuery({
    queryKey: ["health"],
    queryFn: api.getHealth,
    refetchInterval: 15_000,
    retry: false,
  });
  const ok = !!health.data?.ok;
  const label = health.isLoading ? "checking" : ok ? "control plane" : "unreachable";

  return (
    <div className="flex h-full min-h-screen flex-col">
      <header className="topbar">
        <Logo />
        <span className="hidden h-5 w-px bg-[#171d21] sm:block" />
        <nav className="flex min-w-0 gap-5 overflow-x-auto sm:gap-6">
          {NAV.map((item) => (
            <NavLink key={item.to} to={item.to} className={({ isActive }) => `navlink ${isActive ? "active" : ""}`}>
              <span className="hidden sm:inline-flex">{item.icon}</span>
              {item.label}
            </NavLink>
          ))}
        </nav>
        <div className="ml-auto flex items-center gap-4">
          {import.meta.env.VITE_MOCK === "1" && (
            <span className="chip hidden sm:inline" style={{ borderColor: "var(--border-3)", color: "var(--fg-faint)" }}>
              mock
            </span>
          )}
          <span
            className="mono flex items-center gap-2 text-[11px] uppercase tracking-[0.12em]"
            style={{ color: ok ? "var(--fg-muted)" : "var(--red-text)" }}
            title={health.data?.version ? `grove ${health.data.version}` : undefined}
          >
            <span
              className={`dot ${ok ? "live" : ""}`}
              style={{ background: health.isLoading ? "var(--grey)" : ok ? "var(--accent)" : "var(--red)" }}
            />
            <span className="hidden sm:inline">{label}</span>
          </span>
          <Clock />
        </div>
      </header>
      <main className="flex min-h-0 flex-1 flex-col">
        <Outlet />
      </main>
    </div>
  );
}

/** The padded column every view sits in, matching Argos's 34/26px view padding. */
export function View({ children, fill = false }: { children: React.ReactNode; fill?: boolean }) {
  return (
    <div
      className={`flex flex-col gap-4 px-4 pt-7 pb-6 sm:px-[26px] sm:pt-[34px] ${fill ? "xl:h-[calc(100vh-50px)] xl:min-h-0 xl:flex-1" : ""}`}
    >
      {children}
    </div>
  );
}

/** Kicker + serif headline + lede, the head of every view. */
export function ViewHead({
  kicker,
  title,
  lede,
  actions,
}: {
  kicker: string;
  title: React.ReactNode;
  lede?: React.ReactNode;
  actions?: React.ReactNode;
}) {
  return (
    <div className="flex flex-wrap items-end gap-4">
      <div className="flex max-w-[74ch] flex-col gap-[7px]">
        <span className="kicker">{kicker}</span>
        <h1 className="headline m-0">{title}</h1>
        {lede && <p className="lede m-0">{lede}</p>}
      </div>
      {actions && <div className="ml-auto flex flex-wrap items-center gap-2">{actions}</div>}
    </div>
  );
}
