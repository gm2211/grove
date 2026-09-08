import { useMutation, useQueryClient } from "@tanstack/react-query";
import type { FleetEntry } from "../api/types";
import { api } from "../api/client";
import { StatusPill, StateDot } from "./StatusPill";
import { Button } from "./Button";
import { ConfirmButton } from "./ConfirmButton";
import { SlotBar } from "./SlotBar";
import { formatMinutes } from "../lib/format";

function RecycleIcon() {
  return (
    <svg viewBox="0 0 16 16" width="13" height="13" fill="none" stroke="currentColor" strokeWidth="1.4">
      <path d="M2.5 5h11M6 5V3.5h4V5M3.5 5l.6 8.2a1 1 0 0 0 1 .8h5.8a1 1 0 0 0 1-.8L12.5 5" strokeLinecap="round" strokeLinejoin="round" />
      <path d="M6.5 7.5v4M9.5 7.5v4" strokeLinecap="round" />
    </svg>
  );
}

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

  return (
    <div
      className="mb-3 flex flex-col gap-3 rounded-lg border p-3 break-inside-avoid xl:mb-4"
      style={{ borderColor: "var(--border)", background: "var(--bg-elevated)" }}
    >
      {/* Line 1: host name + the one status pill on this page + the demoted pause/resume action. */}
      <div className="flex items-center justify-between gap-2">
        <div className="flex min-w-0 items-center gap-2">
          <span className="mono truncate text-base font-semibold">{host}</span>
          {worker && <StatusPill status={worker.status} />}
        </div>
        {worker &&
          (worker.cordoned ? (
            <Button
              variant="primary"
              size="sm"
              disabled={resume.isPending}
              onClick={() => resume.mutate()}
            >
              {resume.isPending ? "resuming…" : "Resume worker"}
            </Button>
          ) : (
            <Button size="sm" disabled={pause.isPending} onClick={() => pause.mutate()}>
              {pause.isPending ? "pausing…" : "Pause worker"}
            </Button>
          ))}
      </div>

      {/* Line 2: arch + segmented slot utilization bar. */}
      <div className="flex items-center gap-2 text-xs" style={{ color: "var(--fg-muted)" }}>
        {worker?.arch && <span className="shrink-0">{worker.arch}</span>}
        <SlotBar used={used} total={slots} />
        <span className="shrink-0 whitespace-nowrap">
          {used}/{slots} slots
        </span>
      </div>

      <div className="flex flex-col gap-1.5">
        <div className="text-[11px] font-semibold uppercase tracking-wide" style={{ color: "var(--fg-faint)" }}>
          VMs
        </div>
        {vms.length === 0 && (
          <div className="text-xs" style={{ color: "var(--fg-faint)" }}>
            no VMs
          </div>
        )}
        {vms.length > 0 && (
          <div className="overflow-x-auto">
            <table className="w-full border-collapse text-xs">
              <tbody>
                {vms.map((vm) => {
                  const ttl = (vm.raw?.ttlRemainingMinutes as number | undefined) ?? undefined;
                  return (
                    <tr key={vm.name} className="group border-t first:border-t-0" style={{ borderColor: "var(--border)" }}>
                      <td className="w-full py-1.5 pr-2 align-middle">
                        <div className="flex min-w-0 items-center gap-2">
                          <span
                            className="shrink-0 rounded px-1.5 py-0.5 font-medium"
                            style={{ background: "var(--bg-inset)", border: "1px solid var(--border)" }}
                          >
                            {vm.pool ?? "?"}
                          </span>
                          <span className="mono">{vm.name}</span>
                        </div>
                      </td>
                      <td className="py-1.5 px-2 whitespace-nowrap align-middle" style={{ color: "var(--fg-muted)" }}>
                        TTL {formatMinutes(ttl)}
                      </td>
                      <td className="py-1.5 px-2 whitespace-nowrap align-middle" style={{ color: "var(--fg-muted)" }}>
                        {vm.running} running
                      </td>
                      <td className="py-1.5 px-2 whitespace-nowrap align-middle">
                        <StateDot status={vm.status} />
                      </td>
                      <td className="py-1.5 pl-2 align-middle">
                        <ConfirmButton
                          label={<RecycleIcon />}
                          confirmLabel="sure?"
                          size="icon"
                          title="Recycle VM"
                          className="opacity-0 group-hover:opacity-100 group-focus-within:opacity-100 focus-visible:opacity-100"
                          onConfirm={() => recycle.mutate(vm.name)}
                          disabled={recycle.isPending}
                        />
                      </td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          </div>
        )}
      </div>

      {nodes.length > 0 && (
        <div className="flex flex-col gap-1.5">
          <div className="text-[11px] font-semibold uppercase tracking-wide" style={{ color: "var(--fg-faint)" }}>
            Nomad nodes
          </div>
          <table className="w-full border-collapse text-xs">
            <tbody>
              {nodes.map((n) => (
                <tr key={n.name} className="border-t first:border-t-0" style={{ borderColor: "var(--border)" }}>
                  <td className="w-full py-1.5 pr-2 align-middle">
                    <span className="mono">{n.name}</span>
                  </td>
                  <td className="py-1.5 pl-2 whitespace-nowrap align-middle">
                    <StateDot status={n.status} />
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
  );
}
