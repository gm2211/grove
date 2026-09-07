import { useQuery } from "@tanstack/react-query";
import { api } from "../api/client";
import { HostCard } from "../components/HostCard";

export function FleetPage() {
  const { data, isLoading, error } = useQuery({
    queryKey: ["fleet"],
    queryFn: api.getFleet,
    refetchInterval: 5000,
  });

  if (isLoading) return <div className="p-4 text-sm">Loading fleet…</div>;
  if (error) return <div className="p-4 text-sm text-red-500">Failed to load fleet: {String(error)}</div>;
  if (!data) return null;

  const hosts = Array.from(
    new Set([
      ...data.workers.map((w) => w.host ?? w.name),
      ...data.vms.map((v) => v.host ?? ""),
      ...data.nodes.map((n) => n.host ?? ""),
    ]),
  ).filter(Boolean);
  hosts.sort();

  return (
    <div className="flex flex-col gap-4">
      <div className="flex items-baseline justify-between">
        <h1 className="text-lg font-semibold">Fleet</h1>
        <span className="text-xs" style={{ color: "var(--fg-muted)" }}>
          {data.workers.length} workers · {data.vms.length} VMs · {data.nodes.length} nomad nodes
        </span>
      </div>
      <div className="grid grid-cols-1 gap-3 md:grid-cols-2 xl:grid-cols-3">
        {hosts.map((host) => (
          <HostCard
            key={host}
            host={host}
            worker={data.workers.find((w) => (w.host ?? w.name) === host)}
            vms={data.vms.filter((v) => v.host === host)}
            nodes={data.nodes.filter((n) => n.host === host)}
          />
        ))}
      </div>
    </div>
  );
}
