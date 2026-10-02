import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import type { FleetEntry } from "../api/types";
import { api } from "../api/client";
import { StatusPill, StateDot } from "./StatusPill";
import { Button } from "./Button";
import { ConfirmButton } from "./ConfirmButton";
import { SlotBar } from "./SlotBar";
import { Tag } from "./Page";
import { NodeIcon, ServerIcon, TrashIcon } from "./Icons";
import { formatMinutes } from "../lib/format";

export function HostCard({
  host,
  worker,
  vms,
  nodes,
}: {
  host: string;
  worker?: FleetEntry;
  vms: FleetEntry[];
  nodes: FleetEntry[];
}) {
  const qc = useQueryClient();
  const { data: principal } = useQuery({ queryKey: ["whoami"], queryFn: api.whoAmI, staleTime: 60_000 });
  const operator = principal?.scopes.includes("operator") ?? false;
  const invalidate = () => qc.invalidateQueries({ queryKey: ["fleet"] });

  const pause = useMutation({
    mutationFn: () => api.pauseWorker(host),
    onSuccess: invalidate,
  });
  const resume = useMutation({
    mutationFn: () => api.resumeWorker(host),
    onSuccess: invalidate,
  });
  const recycle = useMutation({
    mutationFn: (name: string) => api.recycleVM(name),
    onSuccess: invalidate,
  });

  const slots = worker?.capacity?.vmSlots ?? 0;
  const used = worker?.running ?? vms.length;
  const offline = worker?.status === "offline";

  return (
    <div
      className="mb-4 flex break-inside-avoid flex-col overflow-hidden rounded-xl border"
      style={{ borderColor: "var(--border)", background: "var(--bg-elevated)", boxShadow: "var(--shadow-sm)" }}
    >
      {/* Header: host identity + the one status pill on this card + the demoted pause/resume action. */}
      <div className="flex items-start justify-between gap-3 px-4 pt-4">
        <div className="flex min-w-0 items-center gap-3">
          <span
            className="flex h-9 w-9 shrink-0 items-center justify-center rounded-lg"
            style={{
              background: offline ? "var(--bg-inset)" : "var(--accent-soft)",
              color: offline ? "var(--fg-faint)" : "var(--accent)",
            }}
          >
            <ServerIcon size={18} />
          </span>
          <div className="flex min-w-0 flex-col gap-1">
            <div className="flex min-w-0 items-center gap-2">
              <span className="mono truncate text-[14px] font-semibold">{host}</span>
              {worker && <StatusPill status={worker.status} />}
            </div>
            <div className="flex items-center gap-1.5 text-xs" style={{ color: "var(--fg-faint)" }}>
              {worker?.arch && <span className="mono">{worker.arch}</span>}
              {worker?.arch && <span>·</span>}
              <span>
                {vms.length} {vms.length === 1 ? "VM" : "VMs"}
              </span>
              <span>·</span>
              <span>
                {nodes.length} {nodes.length === 1 ? "node" : "nodes"}
              </span>
            </div>
          </div>
        </div>
        {operator &&
          worker &&
          (worker.cordoned ? (
            <Button variant="primary" size="sm" disabled={resume.isPending} onClick={() => resume.mutate()}>
              {resume.isPending ? "Resuming…" : "Resume"}
            </Button>
          ) : (
            <Button size="sm" disabled={pause.isPending} onClick={() => pause.mutate()}>
              {pause.isPending ? "Pausing…" : "Pause"}
            </Button>
          ))}
      </div>

      {/* Segmented slot utilization. */}
      <div className="flex items-center gap-3 px-4 pt-3 pb-4 text-xs" style={{ color: "var(--fg-muted)" }}>
        <SlotBar used={used} total={slots} />
        <span className="tabular shrink-0 whitespace-nowrap">
          <span className="font-semibold" style={{ color: "var(--fg)" }}>
            {used}
          </span>
          /{slots} slots
        </span>
      </div>

      <div className="border-t" style={{ borderColor: "var(--border)" }}>
        {vms.length === 0 ? (
          <div className="px-4 py-3 text-xs" style={{ color: "var(--fg-faint)" }}>
            No VMs running on this host.
          </div>
        ) : (
          <table className="w-full border-collapse text-xs">
            <thead>
              <tr style={{ background: "var(--bg-inset)" }}>
                <th className="eyebrow px-4 py-1.5 text-left font-semibold">VM</th>
                <th className="eyebrow hidden px-2 py-1.5 text-left font-semibold sm:table-cell">TTL</th>
                <th className="eyebrow hidden px-2 py-1.5 text-left font-semibold sm:table-cell">Jobs</th>
                <th className="eyebrow px-2 py-1.5 text-left font-semibold">State</th>
                {operator && <th className="w-10" />}
              </tr>
            </thead>
            <tbody>
              {vms.map((vm) => {
                const ttl = (vm.raw?.ttlRemainingMinutes as number | undefined) ?? undefined;
                return (
                  <tr
                    key={vm.name}
                    className="group border-t transition-colors hover:bg-[var(--bg-hover)]"
                    style={{ borderColor: "var(--border)" }}
                  >
                    <td className="w-full py-2 pr-2 pl-4 align-middle">
                      <div className="flex min-w-0 items-center gap-2">
                        <Tag>{vm.pool ?? "?"}</Tag>
                        <span className="mono truncate">{vm.name}</span>
                      </div>
                    </td>
                    <td
                      className="tabular hidden px-2 py-2 whitespace-nowrap align-middle sm:table-cell"
                      style={{ color: "var(--fg-muted)" }}
                    >
                      {formatMinutes(ttl)}
                    </td>
                    <td
                      className="tabular hidden px-2 py-2 whitespace-nowrap align-middle sm:table-cell"
                      style={{ color: "var(--fg-muted)" }}
                    >
                      {vm.running}
                    </td>
                    <td className="px-2 py-2 whitespace-nowrap align-middle">
                      <StateDot status={vm.status} />
                    </td>
                    {operator && (
                      <td className="py-1 pr-3 pl-1 text-right align-middle">
                        <ConfirmButton
                          label={<TrashIcon size={14} />}
                          confirmLabel="Recycle?"
                          size="icon"
                          variant="ghost"
                          title="Recycle VM"
                          className="opacity-0 group-hover:opacity-100 group-focus-within:opacity-100 focus-visible:opacity-100"
                          onConfirm={() => recycle.mutate(vm.name)}
                          disabled={recycle.isPending}
                        />
                      </td>
                    )}
                  </tr>
                );
              })}
            </tbody>
          </table>
        )}
      </div>

      {nodes.length > 0 && (
        <div
          className="flex flex-wrap items-center gap-1.5 border-t px-4 py-3"
          style={{ borderColor: "var(--border)", background: "var(--bg)" }}
        >
          <span className="mr-1 flex items-center gap-1.5 text-xs" style={{ color: "var(--fg-faint)" }}>
            <NodeIcon size={13} />
            Nomad
          </span>
          {nodes.map((n) => (
            <span
              key={n.name}
              className="inline-flex max-w-full items-center gap-2 rounded-md border px-2 py-0.5 text-[11.5px]"
              style={{ borderColor: "var(--border)", background: "var(--bg-elevated)" }}
              title={`${n.name}: ${n.status}`}
            >
              <span className="mono truncate">{n.name}</span>
              <StateDot status={n.status} />
            </span>
          ))}
        </div>
      )}
    </div>
  );
}
