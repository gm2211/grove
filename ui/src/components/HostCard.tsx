import { useMutation, useQueryClient } from "@tanstack/react-query";
import type { FleetEntry } from "../api/types";
import { api } from "../api/client";
import { StatusPill } from "./StatusPill";
import { Button } from "./Button";
import { ConfirmButton } from "./ConfirmButton";
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
      className="flex flex-col gap-3 rounded-lg border p-3"
      style={{ borderColor: "var(--border)", background: "var(--bg-elevated)" }}
    >
      <div className="flex items-start justify-between gap-2">
        <div>
          <div className="mono text-sm font-semibold">{host}</div>
          <div className="mt-0.5 flex items-center gap-2 text-xs" style={{ color: "var(--fg-muted)" }}>
            {worker?.arch && <span>{worker.arch}</span>}
            <span>
              {used}/{slots} slots
            </span>
          </div>
        </div>
        {worker && <StatusPill status={worker.status} />}
      </div>

      {worker && (
        <div className="flex gap-2">
          {worker.cordoned ? (
            <Button
              variant="primary"
              disabled={resume.isPending}
              onClick={() => resume.mutate()}
            >
              {resume.isPending ? "resuming…" : "Resume worker"}
            </Button>
          ) : (
            <Button disabled={pause.isPending} onClick={() => pause.mutate()}>
              {pause.isPending ? "pausing…" : "Pause worker"}
            </Button>
          )}
        </div>
      )}

      <div className="flex flex-col gap-1.5">
        <div className="text-[11px] font-semibold uppercase tracking-wide" style={{ color: "var(--fg-faint)" }}>
          VMs
        </div>
        {vms.length === 0 && (
          <div className="text-xs italic" style={{ color: "var(--fg-faint)" }}>
            none
          </div>
        )}
        {vms.map((vm) => {
          const ttl = (vm.raw?.ttlRemainingMinutes as number | undefined) ?? undefined;
          return (
            <div
              key={vm.name}
              className="flex items-center justify-between gap-2 rounded border px-2 py-1.5 text-xs"
              style={{ borderColor: "var(--border)", background: "var(--bg-inset)" }}
            >
              <div className="flex min-w-0 items-center gap-2">
                <span
                  className="rounded px-1.5 py-0.5 font-medium"
                  style={{ background: "var(--bg-elevated)", border: "1px solid var(--border)" }}
                >
                  {vm.labels?.pool ?? "?"}
                </span>
                <span className="mono truncate">{vm.name}</span>
              </div>
              <div className="flex items-center gap-2 whitespace-nowrap">
                <span style={{ color: "var(--fg-muted)" }}>TTL {formatMinutes(ttl)}</span>
                <span style={{ color: "var(--fg-muted)" }}>{vm.running} running</span>
                <StatusPill status={vm.status} />
                <ConfirmButton
                  label="Recycle"
                  confirmLabel="sure?"
                  onConfirm={() => recycle.mutate(vm.name)}
                  disabled={recycle.isPending}
                />
              </div>
            </div>
          );
        })}
      </div>

      {nodes.length > 0 && (
        <div className="flex flex-col gap-1.5">
          <div className="text-[11px] font-semibold uppercase tracking-wide" style={{ color: "var(--fg-faint)" }}>
            Nomad nodes
          </div>
          {nodes.map((n) => (
            <div key={n.name} className="flex items-center justify-between text-xs">
              <span className="mono truncate">{n.name}</span>
              <StatusPill status={n.status} />
            </div>
          ))}
        </div>
      )}
    </div>
  );
}
