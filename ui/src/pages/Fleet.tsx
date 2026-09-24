import { useQuery } from "@tanstack/react-query";
import { api } from "../api/client";
import { HostCard } from "../components/HostCard";
import { StatTile } from "../components/StatTile";
import { SlotBar } from "../components/SlotBar";
import { PendingJoins } from "../components/PendingJoins";
import { EmptyState, PageHeader, PageMessage } from "../components/Page";
import { BoxIcon, GaugeIcon, NodeIcon, ServerIcon } from "../components/Icons";

export function FleetPage() {
  const { data, isLoading, error } = useQuery({
    queryKey: ["fleet"],
    queryFn: api.getFleet,
    refetchInterval: 5000,
  });

  const header = <PageHeader title="Fleet" description="Worker Macs, the VMs they host, and their Nomad nodes." />;

  if (isLoading)
    return (
      <>
        {header}
        <PageMessage>Loading fleet…</PageMessage>
      </>
    );
  if (error)
    return (
      <>
        {header}
        <PageMessage tone="error">Failed to load fleet: {String(error)}</PageMessage>
      </>
    );
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
  const onlineWorkers = data.workers.filter((w) => w.online && !w.cordoned).length;
  const readyNodes = data.nodes.filter((n) => n.status === "ready").length;
  const runningVMs = data.vms.filter((v) => v.status === "running").length;
  const pct = totalSlots > 0 ? Math.round((usedSlots / totalSlots) * 100) : 0;

  return (
    <div>
      {header}
      <div className="flex flex-col gap-5">
        {/* Above the totals, because a Mac waiting on a human is the only thing on this page that
          stops by itself if nobody looks. It renders nothing when nobody is waiting. */}
        <PendingJoins />

        <div className="grid grid-cols-2 gap-3 lg:grid-cols-4">
          <StatTile
            icon={<ServerIcon size={14} />}
            label="Workers"
            value={data.workers.length}
            sublabel={data.workers.length > 0 ? `${onlineWorkers} available` : undefined}
          />
          <StatTile
            icon={<BoxIcon size={14} />}
            label="VMs"
            value={data.vms.length}
            sublabel={data.vms.length > 0 ? `${runningVMs} running` : undefined}
          />
          <StatTile
            icon={<NodeIcon size={14} />}
            label="Nomad nodes"
            value={data.nodes.length}
            sublabel={data.nodes.length > 0 ? `${readyNodes} ready` : undefined}
          />
          <StatTile
            icon={<GaugeIcon size={14} />}
            label="Slots used"
            value={`${usedSlots}/${totalSlots}`}
            sublabel={totalSlots > 0 ? `${pct}%` : undefined}
            bar={<SlotBar used={usedSlots} total={totalSlots} variant="meter" />}
          />
        </div>

        {hosts.length === 0 ? (
          <EmptyState icon={<ServerIcon size={18} />} title="No hosts yet">
            Enroll a Mac by running <code className="mono text-xs">grove install --role worker</code> on it, then
            approve it here when it asks to join.
          </EmptyState>
        ) : (
          <div>
            <div className="mb-3 flex items-baseline justify-between">
              <h2 className="text-sm font-semibold">
                Hosts{" "}
                <span className="font-normal" style={{ color: "var(--fg-faint)" }}>
                  {hosts.length}
                </span>
              </h2>
            </div>
            {/* Masonry-style two-column flow once there's room, so an odd number of cards never
              leaves a dead gap the way a strict 2-up grid would. */}
            <div className="columns-1 gap-4 xl:columns-2">
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
        )}
      </div>
    </div>
  );
}
