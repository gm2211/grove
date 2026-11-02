// Turns the flat /fleet + /jobs answers into the tree the map walks: worker Mac → Tart VM (one
// Nomad node each, same name) → the jobs Nomad placed on that node. Pending jobs have no node
// yet; they form the queue, per pool.
import type { FleetEntry, FleetResponse, Job } from "../api/types";

export interface VMView {
  vm: FleetEntry;
  node?: FleetEntry;
  jobs: Job[]; // running first, then newest
  running: Job[];
  createdAt?: number;
  ttlMs?: number;
  ttlLeftMs?: number;
}

export interface WorkerView {
  name: string;
  worker?: FleetEntry; // absent when VMs name a host Orchard doesn't list (e.g. it was removed)
  vms: VMView[];
  slots: number;
  used: number;
  running: number; // running jobs across its VMs
}

export interface FleetModel {
  workers: WorkerView[];
  queue: Job[]; // pending, oldest first
  runningJobs: number;
  vmCount: number;
  jobsById: Map<string, Job>;
}

const TERMINAL = new Set(["success", "failed", "canceled", "lost"]);
export const isTerminal = (status: string) => TERMINAL.has(status);

function vmTimes(vm: FleetEntry): Pick<VMView, "createdAt" | "ttlMs" | "ttlLeftMs"> {
  const raw = (vm.raw ?? {}) as Record<string, unknown>;
  const created = typeof raw.createdAt === "string" ? Date.parse(raw.createdAt) : NaN;
  // orchard.VM.TTL is a Go time.Duration: nanoseconds on the wire.
  const ttlMs = typeof raw.ttl === "number" && raw.ttl > 0 ? raw.ttl / 1e6 : undefined;
  // Go's zero time ("0001-01-01T00:00:00Z") parses to a negative epoch: treat it as unknown.
  if (!Number.isNaN(created) && created > 0) {
    return {
      createdAt: created,
      ttlMs,
      ttlLeftMs: ttlMs !== undefined ? created + ttlMs - Date.now() : undefined,
    };
  }
  return {};
}

function jobOrder(a: Job, b: Job): number {
  const ra = a.status === "running" ? 0 : 1;
  const rb = b.status === "running" ? 0 : 1;
  if (ra !== rb) return ra - rb;
  return Date.parse(b.submittedAt) - Date.parse(a.submittedAt);
}

export function buildFleetModel(fleet: FleetResponse, jobs: Job[]): FleetModel {
  const nodesByName = new Map(fleet.nodes.map((n) => [n.name, n]));
  const jobsByNode = new Map<string, Job[]>();
  for (const j of jobs) {
    const where = new Set<string>();
    if (j.node) where.add(j.node);
    for (const a of j.attempts ?? []) where.add(a.vm);
    for (const name of where) {
      const list = jobsByNode.get(name) ?? [];
      list.push(j);
      jobsByNode.set(name, list);
    }
  }

  const names = new Set<string>();
  for (const w of fleet.workers) names.add(w.host ?? w.name);
  for (const v of fleet.vms) if (v.host) names.add(v.host);

  const workers: WorkerView[] = [...names].sort().map((name) => {
    const worker = fleet.workers.find((w) => (w.host ?? w.name) === name);
    const vms: VMView[] = fleet.vms
      .filter((v) => v.host === name)
      .sort((a, b) => a.name.localeCompare(b.name))
      .map((vm) => {
        const vmJobs = (jobsByNode.get(vm.name) ?? []).slice().sort(jobOrder);
        return {
          vm,
          node: nodesByName.get(vm.name),
          jobs: vmJobs,
          running: vmJobs.filter((j) => j.status === "running"),
          ...vmTimes(vm),
        };
      });
    return {
      name,
      worker,
      vms,
      slots: worker?.capacity?.vmSlots ?? 0,
      used: worker?.running ?? vms.length,
      running: vms.reduce((n, v) => n + v.running.length, 0),
    };
  });

  return {
    workers,
    queue: jobs
      .filter((j) => j.status === "pending")
      .sort((a, b) => Date.parse(a.submittedAt) - Date.parse(b.submittedAt)),
    runningJobs: jobs.filter((j) => j.status === "running").length,
    vmCount: fleet.vms.length,
    jobsById: new Map(jobs.map((j) => [j.id, j])),
  };
}

/** The worker status the UI shows: snoozed > offline > paused > online. */
export function workerStatus(w: WorkerView): string {
  const e = w.worker;
  if (!e) return "unknown";
  if (e.snoozedUntil && Date.parse(e.snoozedUntil) > Date.now() && e.online) return "snoozed";
  if (!e.online) return "offline";
  if (e.cordoned) return "paused";
  return "online";
}
