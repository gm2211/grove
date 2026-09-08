import { useQuery } from "@tanstack/react-query";
import { api } from "../api/client";
import { HostCard } from "../components/HostCard";
import { StatTile } from "../components/StatTile";
import { SlotBar } from "../components/SlotBar";

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

  const totalSlots = data.workers.reduce((sum, w) => sum + (w.capacity?.vmSlots ?? 0), 0);
  const usedSlots = data.workers.reduce((sum, w) => sum + w.running, 0);

  return (
    <div className="flex flex-col gap-4">
      <h1 className="text-lg font-semibold">Fleet</h1>

      {/* Summary strip: the fleet totals that used to sit in the header corner live here now,
          alongside the fleet-wide slot meter, so they're never shown in two places at once. */}
      <div className="grid grid-cols-2 gap-3 lg:grid-cols-4">
        <StatTile label="Workers" value={data.workers.length} />
        <StatTile label="VMs" value={data.vms.length} />
        <StatTile label="Nomad nodes" value={data.nodes.length} />
        <StatTile
          label="Slots used"
          value={`${usedSlots}/${totalSlots}`}
          bar={<SlotBar used={usedSlots} total={totalSlots} variant="meter" />}
        />
      </div>

      {/* Single column by default; a masonry-style two-column flow once there's room, so an odd
          number of cards never leaves a dead gap the way a strict 2-up grid would. */}
      <div className="columns-1 xl:columns-2 xl:gap-4">
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
