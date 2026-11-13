import { useMemo, useState, type ReactNode } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useSearchParams } from "react-router";
import { api } from "../api/client";
import type { Job } from "../api/types";
import { View, ViewHead } from "../components/Layout";
import { RefreshIcon } from "../components/Icons";
import { TONES, toneOf, type Tone } from "../components/StatusPill";
import { StuckVMs } from "../components/StuckVMs";
import { PendingJoins } from "../components/PendingJoins";
import { MapDetail } from "../components/MapDetail";
import { buildFleetModel, workerStatus, type FleetModel, type VMView, type WorkerView } from "../lib/fleetModel";
import { formatClock, formatShort, parseGoDurationMs, shortRepo } from "../lib/format";

/**
 * The live map, after Argos's, laid out the way a job travels: the per-pool queue, then the
 * worker Macs Nomad places onto, their VMs, and the jobs packed into each VM. Selecting a queued
 * job lights the Macs and VMs of its pool; selecting anything else lights its branch, dims the
 * rest, and opens it in the detail column. The selection lives in the URL (?w=&v=&j=) so a view can be linked.
 */
export function MapPage() {
  const qc = useQueryClient();
  const fleet = useQuery({ queryKey: ["fleet"], queryFn: api.getFleet, refetchInterval: 5000 });
  const jobs = useQuery({ queryKey: ["jobs"], queryFn: api.getJobs, refetchInterval: 3000 });
  const [params, setParams] = useSearchParams();
  const [view, setView] = useState<"graph" | "table">("graph");

  const model = useMemo(
    () => (fleet.data ? buildFleetModel(fleet.data, jobs.data ?? []) : undefined),
    [fleet.data, jobs.data],
  );

  if (fleet.isLoading) return <Loading />;
  if (fleet.error || !model) {
    return (
      <View>
        <ViewHead kicker="Map" title="Can't reach the control plane" lede={String(fleet.error ?? "no data")} />
      </View>
    );
  }

  const sel = resolveSelection(model, params);
  const pick = (next: { w?: string; v?: string; j?: string }) => {
    const p = new URLSearchParams();
    if (next.w) p.set("w", next.w);
    if (next.v) p.set("v", next.v);
    if (next.j) p.set("j", next.j);
    if (!next.w && !next.v && !next.j) p.set("sel", "none");
    setParams(p, { replace: true });
  };

  const headline = [
    `${model.queue.length} queued`,
    plural(model.workers.length, "worker"),
    plural(model.vmCount, "VM"),
    `${model.runningJobs} running`,
  ].join(" · ");
  const updated = Math.max(fleet.dataUpdatedAt, jobs.dataUpdatedAt);

  return (
    <View fill>
      <ViewHead
        kicker="Map · queue, workers, VMs, jobs"
        title={headline}
        lede="Walk left to right: jobs wait in their pool's queue until Nomad places them on a worker Mac whose Tart VMs serve that pool, each VM being one Nomad node. Select a queued job to see where it can land, or anything else to follow its branch."
        actions={
          <>
            <div className="seg">
              <button className="systab" data-on={view === "graph" ? "1" : "0"} onClick={() => setView("graph")}>
                Graph
              </button>
              <button className="systab" data-on={view === "table" ? "1" : "0"} onClick={() => setView("table")}>
                Table
              </button>
            </div>
            <button className="act" onClick={() => pick({})}>
              Clear selection
            </button>
            <span className="stat hidden lg:inline">
              updated <b>{updated ? new Date(updated).toLocaleTimeString() : "—"}</b>
            </span>
            <button
              className="iconbtn"
              title="Refresh"
              onClick={() => {
                qc.invalidateQueries({ queryKey: ["fleet"] });
                qc.invalidateQueries({ queryKey: ["jobs"] });
              }}
            >
              <RefreshIcon />
            </button>
          </>
        }
      />

      <PendingJoins />
      <StuckVMs problems={fleet.data?.problems} />

      {view === "graph" ? (
        <>
          <Crumbs sel={sel} />
          <Graph model={model} sel={sel} pick={pick} />
        </>
      ) : (
        <VMTable model={model} onPick={(w, v) => { pick({ w, v }); setView("graph"); }} />
      )}
    </View>
  );
}

export interface Selection {
  worker?: WorkerView;
  vm?: VMView;
  job?: Job;
}

/**
 * Reads ?w=&v=&j= and fills in the rest the way Argos does: with nothing picked (and the
 * operator hasn't cleared on purpose) the map opens on the busiest worker, its busiest VM and
 * that VM's running job, so it never shows three empty columns. A picked job implies its VM and
 * worker; a picked VM its worker.
 */
function resolveSelection(model: FleetModel, params: URLSearchParams): Selection {
  if (params.get("sel") === "none") return {};
  const w = params.get("w");
  const v = params.get("v");
  const j = params.get("j");

  const job = j ? model.jobsById.get(j) : undefined;
  const vmName = v ?? job?.node ?? job?.attempts?.at(-1)?.vm;
  let worker = w ? model.workers.find((x) => x.name === w) : undefined;
  let vm: VMView | undefined;
  for (const wk of worker ? [worker] : model.workers) {
    const hit = vmName ? wk.vms.find((x) => x.vm.name === vmName) : undefined;
    if (hit) {
      vm = hit;
      worker = wk;
      break;
    }
  }
  if (job || vm) return { worker, vm, job };
  if (worker) return { worker, vm: busiestVM(worker) };

  // Default: the worker with the most running jobs (then the most VMs).
  const busiest = [...model.workers].sort(
    (a, b) => b.running - a.running || b.vms.length - a.vms.length || a.name.localeCompare(b.name),
  )[0];
  if (!busiest) return {};
  const vmPick = busiestVM(busiest);
  return { worker: busiest, vm: vmPick, job: vmPick?.running[0] };
}

function busiestVM(w: WorkerView): VMView | undefined {
  return [...w.vms].sort((a, b) => b.running.length - a.running.length || b.jobs.length - a.jobs.length)[0];
}

function Crumbs({ sel }: { sel: Selection }) {
  const parts = [sel.worker?.name, sel.vm?.vm.name, sel.job?.id].filter(Boolean) as string[];
  return (
    <div className="mono flex flex-wrap items-center gap-[9px] text-[11.5px] text-[#8a979c]">
      {parts.length === 0 && <span>nothing selected · pick a worker to walk the tree</span>}
      {parts.map((p) => (
        <span key={p} className="flex items-center gap-[9px]">
          <span className="text-[#7ee7c7]">{p}</span>
          <span className="text-[#474f57]">›</span>
        </span>
      ))}
      {parts.length > 0 && <span>selected</span>}
    </div>
  );
}

function Graph({
  model,
  sel,
  pick,
}: {
  model: FleetModel;
  sel: Selection;
  pick: (s: { w?: string; v?: string; j?: string }) => void;
}) {
  // A queued job hasn't been placed yet: light up the workers and VMs its pool could land on.
  const queuedPool = sel.job?.status === "pending" ? sel.job.request.pool : undefined;
  const canTake = (w: WorkerView, pool: string) => w.vms.some((v) => v.vm.pool === pool);
  const vms = queuedPool
    ? model.workers.flatMap((w) => w.vms.filter((v) => v.vm.pool === queuedPool))
    : (sel.worker?.vms ?? []);
  const vmJobs = sel.vm ? sel.vm.jobs.slice(0, 8) : [];
  const workerPools = new Set((sel.worker?.vms ?? []).map((v) => v.vm.pool).filter(Boolean));

  return (
    <div className="mapgrid">
      <Column label="Queue" count={model.queue.length} empty="nothing waiting · every job found a slot">
        {model.queue.map((j) => (
          <QueuedNode
            key={j.id}
            job={j}
            on={sel.job?.id === j.id}
            dim={queuedPool ? sel.job?.id !== j.id : !!sel.worker && !workerPools.has(j.request.pool)}
            onPick={() => pick({ j: j.id })}
          />
        ))}
      </Column>

      <Column label="Workers" count={model.workers.length} empty="no workers have joined yet · run grove setup on a Mac">
        {model.workers.map((w) => (
          <WorkerNode
            key={w.name}
            w={w}
            on={queuedPool ? false : sel.worker?.name === w.name}
            dim={queuedPool ? !canTake(w, queuedPool) : !!sel.worker && sel.worker.name !== w.name}
            onPick={() => pick({ w: w.name })}
          />
        ))}
      </Column>

      <Column
        label={queuedPool ? `VMs · ${queuedPool} pool` : "VMs"}
        count={sel.worker || queuedPool ? vms.length : undefined}
        empty={
          queuedPool
            ? `no ${queuedPool} VMs yet · grove fleet apply creates them`
            : sel.worker
              ? "no VMs on this worker · grove fleet apply creates them"
              : "select a worker"
        }
      >
        {vms.map((v) => (
          <VMNode
            key={v.vm.name}
            v={v}
            on={sel.vm?.vm.name === v.vm.name}
            dim={!!sel.vm && sel.vm.vm.name !== v.vm.name}
            onPick={() => pick({ w: v.vm.host, v: v.vm.name })}
          />
        ))}
      </Column>

      <Column
        label="Jobs"
        count={sel.vm ? sel.vm.jobs.length : undefined}
        empty={queuedPool ? "not placed yet · Nomad picks a VM with room" : sel.vm ? "no jobs have run here yet" : "select a VM"}
      >
        {vmJobs.map((j) => (
          <JobNode
            key={j.id}
            job={j}
            on={sel.job?.id === j.id}
            dim={!!sel.job && sel.job.id !== j.id}
            onPick={() => pick({ w: sel.worker?.name, v: sel.vm?.vm.name, j: j.id })}
          />
        ))}
        {sel.vm && sel.vm.jobs.length > vmJobs.length && (
          <span className="mapcol-empty">+{sel.vm.jobs.length - vmJobs.length} older on the Jobs page</span>
        )}
      </Column>

      <MapDetail model={model} sel={sel} />
    </div>
  );
}

function Column({
  label,
  count,
  empty,
  children,
}: {
  label: string;
  count?: number;
  empty: string;
  children: ReactNode;
}) {
  const hasChildren = Array.isArray(children) ? children.flat().some(Boolean) : !!children;
  return (
    <div className="mapcol">
      <div className="mapcol-head">
        <span className="kicker" style={{ fontSize: 10 }}>
          {label}
        </span>
        {count !== undefined && <span className="mono text-[10.5px] text-[#3d4a51]">{count}</span>}
      </div>
      {hasChildren ? children : <span className="mapcol-empty">{empty}</span>}
    </div>
  );
}

function Node({
  tone,
  live,
  name,
  badge,
  sub,
  pct,
  meterTone,
  metric,
  on,
  dim,
  link = true,
  onPick,
}: {
  tone: Tone;
  live?: boolean;
  name: string;
  badge?: string;
  sub: string;
  pct?: number;
  meterTone?: Tone;
  metric?: string;
  on: boolean;
  dim: boolean;
  link?: boolean;
  onPick: () => void;
}) {
  const t = TONES[tone];
  return (
    <button
      type="button"
      className="mapnode"
      data-on={on ? "1" : "0"}
      data-dim={dim ? "1" : "0"}
      data-link={link ? "1" : "0"}
      onClick={onPick}
      title={name}
    >
      <span className="flex min-w-0 items-center gap-2">
        <span className={`dot ${live ? "live" : ""}`} style={{ background: t.dot }} />
        <span className="nm">{name}</span>
        {badge && <span className="badge">{badge}</span>}
      </span>
      <span className="sub">{sub}</span>
      {(pct !== undefined || metric) && (
        <span className="flex items-center gap-[7px]">
          <span className="meter">
            <span style={{ width: `${Math.round(Math.max(0, Math.min(1, pct ?? 0)) * 100)}%`, background: TONES[meterTone ?? tone].dot }} />
          </span>
          {metric && <span className="meter-label">{metric}</span>}
        </span>
      )}
    </button>
  );
}

function WorkerNode({ w, on, dim, onPick }: { w: WorkerView; on: boolean; dim: boolean; onPick: () => void }) {
  const status = workerStatus(w);
  const e = w.worker;
  const parts: string[] = [];
  if (status === "snoozed" && e?.snoozedUntil) parts.push(`snoozed until ${formatClock(e.snoozedUntil)}`);
  else if (status === "offline") parts.push("not checking in");
  else if (status === "paused") parts.push(`paused · ${(e?.pauseReasons ?? ["operator"]).join(", ")}`);
  else {
    if (e?.capacity?.cpu) parts.push(`${e.capacity.cpu} cores`);
    if (e?.capacity?.memoryMiB) parts.push(`${Math.round(e.capacity.memoryMiB / 1024)} GB`);
  }
  parts.push(plural(w.vms.length, "VM"));
  const full = w.slots > 0 && w.used >= w.slots;
  return (
    <Node
      tone={toneOf(status)}
      live={status === "online" && w.running > 0}
      name={w.name}
      badge={e?.arch ?? status}
      sub={parts.join(" · ")}
      pct={w.slots > 0 ? w.used / w.slots : 0}
      meterTone={status !== "online" ? toneOf(status) : full ? "amber" : "teal"}
      metric={w.slots > 0 ? `${w.used}/${w.slots} slots` : "no slots"}
      on={on}
      dim={dim}
      onPick={onPick}
    />
  );
}

function VMNode({ v, on, dim, onPick }: { v: VMView; on: boolean; dim: boolean; onPick: () => void }) {
  const nodeStatus = v.node?.status ?? "no node";
  const vmTone = toneOf(v.vm.status);
  const tone: Tone = vmTone === "teal" && v.node?.status !== "ready" ? "blue" : vmTone;
  const sub = [
    v.running.length > 0 ? `${v.running.length} running` : "idle",
    `node ${nodeStatus}`,
    v.vm.capacity?.cpu ? `${v.vm.capacity.cpu} vCPU` : "",
  ]
    .filter(Boolean)
    .join(" · ");
  const elapsed = v.ttlMs && v.ttlLeftMs !== undefined ? 1 - v.ttlLeftMs / v.ttlMs : undefined;
  return (
    <Node
      tone={tone}
      live={v.running.length > 0}
      name={v.vm.name}
      badge={v.vm.pool}
      sub={sub}
      pct={elapsed ?? 0}
      meterTone={v.ttlLeftMs !== undefined && v.ttlLeftMs < 30 * 60_000 ? "amber" : "teal"}
      metric={v.ttlLeftMs !== undefined ? `ttl ${formatShort(v.ttlLeftMs)}` : "no ttl"}
      on={on}
      dim={dim}
      onPick={onPick}
    />
  );
}

function jobSub(j: Job): string {
  if (j.request.repo) return `${shortRepo(j.request.repo)}${j.request.ref ? `@${j.request.ref}` : ""}`;
  return j.request.script.split("\n")[0];
}

function JobNode({ job, on, dim, onPick }: { job: Job; on: boolean; dim: boolean; onPick: () => void }) {
  const started = job.startedAt ? Date.parse(job.startedAt) : undefined;
  const end = job.finishedAt ? Date.parse(job.finishedAt) : Date.now();
  const elapsed = started ? end - started : 0;
  const limit = parseGoDurationMs(job.request.timeout);
  const running = job.status === "running";
  const metric = running
    ? `${formatShort(elapsed)}${limit ? ` of ${formatShort(limit)}` : ""}`
    : job.status === "success" || job.status === "failed"
      ? `exit ${job.exitCode ?? "?"} · ${formatShort(elapsed)}`
      : job.status;
  return (
    <Node
      tone={toneOf(job.status)}
      live={running}
      name={job.id}
      badge={job.request.kind}
      sub={jobSub(job)}
      pct={running ? (limit ? elapsed / limit : 0.5) : 1}
      metric={metric}
      on={on}
      dim={dim}
      link={false}
      onPick={onPick}
    />
  );
}

function QueuedNode({ job, on, dim, onPick }: { job: Job; on: boolean; dim: boolean; onPick: () => void }) {
  return (
    <Node
      tone="blue"
      name={job.id}
      badge={job.request.pool}
      sub={job.pendingReason ?? jobSub(job)}
      metric={`waiting ${formatShort(Date.now() - Date.parse(job.submittedAt))}`}
      pct={0}
      on={on}
      dim={dim}
      onPick={onPick}
    />
  );
}

/** The same fleet as one flat table: every VM with its worker, pool, node, job and TTL. */
function VMTable({ model, onPick }: { model: FleetModel; onPick: (worker: string, vm: string) => void }) {
  const cols = "minmax(0,1.6fr) minmax(0,1fr) 0.6fr 0.8fr minmax(0,1.3fr) 0.6fr";
  const rows = model.workers.flatMap((w) => w.vms.map((v) => ({ w, v })));
  return (
    <div className="flex min-h-0 flex-1 flex-col gap-3">
      <span className="mono text-[11.5px] text-[#8a979c]">
        The same machinery as a table: every VM, the Mac it runs on, its Nomad node and what it is running.
      </span>
      <div className="panel min-h-0 flex-1 overflow-auto">
        <div className="min-w-[860px]">
          <div className="trow thead" style={{ gridTemplateColumns: cols }}>
            <span>VM</span>
            <span>Worker</span>
            <span>Pool</span>
            <span>Node</span>
            <span>Running</span>
            <span>TTL</span>
          </div>
          {rows.map(({ w, v }) => (
            <div
              key={v.vm.name}
              className="trow cursor-pointer"
              style={{ gridTemplateColumns: cols }}
              onClick={() => onPick(w.name, v.vm.name)}
            >
              <span className="mono flex min-w-0 items-center gap-2 text-[12.5px] text-[#eaf4f4]">
                <span className={`dot ${v.running.length ? "live" : ""}`} style={{ background: TONES[toneOf(v.vm.status)].dot }} />
                <span className="truncate">{v.vm.name}</span>
              </span>
              <span className="mono truncate text-[12px] text-[#9fb0b5]">{w.name}</span>
              <span className="mono text-[12px] text-[#9fb0b5]">{v.vm.pool ?? "—"}</span>
              <span className="mono text-[12px]" style={{ color: TONES[toneOf(v.node?.status)].text }}>
                {v.node?.status ?? "no node"}
              </span>
              <span className="mono truncate text-[12px] text-[#c3d1d5]">
                {v.running.length ? v.running.map((j) => j.id).join(", ") : <span className="text-[#46545a]">idle</span>}
              </span>
              <span className="mono text-[12px] text-[#a3b0b4]">
                {v.ttlLeftMs !== undefined ? formatShort(v.ttlLeftMs) : "—"}
              </span>
            </div>
          ))}
          {rows.length === 0 && <div className="mapcol-empty px-[18px] py-6">no VMs yet · grove fleet apply creates them</div>}
        </div>
      </div>
    </div>
  );
}

function Loading() {
  return (
    <View>
      <ViewHead kicker="Map" title="Reading the fleet…" />
    </View>
  );
}

function plural(n: number, word: string): string {
  return `${n} ${word}${n === 1 ? "" : "s"}`;
}
