// Realistic in-memory fake fleet + jobs for `npm run dev:mock` (VITE_MOCK=1). Standalone UI dev
// with no grove server running.
import type {
  FleetEntry,
  FleetResponse,
  Job,
  JobKind,
  JobRequest,
  JobStatus,
} from "../api/types";

let idCounter = 1000;
function nextId(prefix: string): string {
  idCounter += 1;
  return `${prefix}-${idCounter.toString(36)}`;
}

interface HostDef {
  host: string;
  arch: "arm64" | "amd64";
  cordoned: boolean;
  online: boolean;
  vmSlots: number;
}

const hosts: HostDef[] = [
  { host: "mac-mini-1", arch: "arm64", cordoned: false, online: true, vmSlots: 4 },
  { host: "mac-studio-1", arch: "arm64", cordoned: false, online: true, vmSlots: 6 },
  { host: "mac-mini-2", arch: "arm64", cordoned: true, online: true, vmSlots: 4 },
  { host: "linux-box-1", arch: "amd64", cordoned: false, online: false, vmSlots: 8 },
];

const pools = ["linux", "macos"] as const;

function makeWorkers(): FleetEntry[] {
  return hosts.map((h) => ({
    name: h.host,
    kind: "worker",
    host: h.host,
    arch: h.arch,
    online: h.online,
    cordoned: h.cordoned,
    status: h.online ? (h.cordoned ? "cordoned" : "ready") : "offline",
    capacity: { cpu: h.arch === "arm64" ? 10 : 16, memoryMiB: 32768, vmSlots: h.vmSlots },
    running: h.online ? Math.min(h.vmSlots, 1 + Math.floor(Math.random() * h.vmSlots)) : 0,
    labels: { arch: h.arch },
    raw: {},
  }));
}

function makeVMs(): FleetEntry[] {
  const vms: FleetEntry[] = [];
  for (const h of hosts) {
    if (!h.online) continue;
    const count = h.cordoned ? 1 : 1 + Math.floor(Math.random() * 3);
    for (let i = 0; i < count; i++) {
      const pool = pools[(hosts.indexOf(h) + i) % pools.length];
      const ageMinutes = Math.floor(Math.random() * 600);
      const ttlMinutes = 720; // 12h
      vms.push({
        name: `${pool}-${h.host}-${i}`,
        kind: "vm",
        host: h.host,
        arch: h.arch,
        online: true,
        cordoned: false,
        status: Math.random() > 0.9 ? "pending" : "running",
        capacity: { cpu: 4, memoryMiB: 8192 },
        running: Math.floor(Math.random() * 3),
        labels: { pool, host: h.host },
        raw: {
          ageMinutes,
          ttlRemainingMinutes: Math.max(0, ttlMinutes - ageMinutes),
        },
      });
    }
  }
  return vms;
}

function makeNodes(vms: FleetEntry[]): FleetEntry[] {
  return vms.map((vm) => ({
    name: `nomad-${vm.name}`,
    kind: "node",
    host: vm.host,
    arch: vm.arch,
    online: vm.status === "running",
    cordoned: false,
    status: vm.status === "running" ? "ready" : "initializing",
    capacity: { cpu: vm.capacity?.cpu, memoryMiB: vm.capacity?.memoryMiB },
    running: vm.running,
    labels: { pool: vm.labels?.pool ?? "", host: vm.host ?? "" },
    raw: {},
  }));
}

export const state: { fleet: FleetResponse } = {
  fleet: (() => {
    const workers = makeWorkers();
    const vms = makeVMs();
    const nodes = makeNodes(vms);
    return { workers, vms, nodes };
  })(),
};

const repos = [
  "github.com/gm2211/grove",
  "github.com/gm2211/orchard",
  "github.com/gm2211/argos",
];
const kinds: JobKind[] = ["build", "agent", "shell"];
const requesters = ["argos", "mcp:claude-code", "cli"];

function randomStatus(ageMs: number): JobStatus {
  if (ageMs < 5_000) return "pending";
  if (ageMs < 30_000) return "running";
  const r = Math.random();
  if (r < 0.75) return "success";
  if (r < 0.9) return "failed";
  if (r < 0.97) return "canceled";
  return "lost";
}

function makeJob(i: number): Job {
  const kind = kinds[i % kinds.length];
  const pool = pools[i % pools.length];
  const submittedAt = new Date(Date.now() - i * 137_000 - Math.random() * 60_000);
  const ageMs = Date.now() - submittedAt.getTime();
  const status = randomStatus(ageMs);
  const started =
    status === "pending"
      ? null
      : new Date(submittedAt.getTime() + 2_000 + Math.random() * 3_000);
  const finished =
    status === "success" || status === "failed" || status === "canceled" || status === "lost"
      ? new Date((started ?? submittedAt).getTime() + 20_000 + Math.random() * 240_000)
      : null;

  const req: JobRequest = {
    kind,
    pool,
    repo: kind === "shell" ? undefined : repos[i % repos.length],
    ref: kind === "shell" ? undefined : i % 3 === 0 ? "main" : `pr-${100 + i}`,
    script:
      kind === "build"
        ? "make test && make build"
        : kind === "agent"
          ? "claude --print 'implement bd-1234'"
          : "df -h && uptime",
    env: { CI: "true" },
    timeout: 3_600_000_000_000,
    requester: requesters[i % requesters.length],
    meta: { bead: `bd-${1000 + i}` },
  };

  return {
    id: `job-${1000 + i}`,
    request: req,
    status,
    node: status === "pending" ? undefined : `nomad-${pool}-${hosts[i % hosts.length].host}-0`,
    allocId: status === "pending" ? undefined : nextId("alloc"),
    exitCode: status === "success" ? 0 : status === "failed" ? 1 : null,
    submittedAt: submittedAt.toISOString(),
    startedAt: started?.toISOString() ?? null,
    finishedAt: finished?.toISOString() ?? null,
    artifacts:
      kind === "build" && status === "success"
        ? [{ path: "artifacts/bin/grove", url: "#", size: 15_234_112 }]
        : [],
    meta: req.meta,
  };
}

export const jobs: Job[] = Array.from({ length: 27 }, (_, i) => makeJob(i));

export function findJob(id: string): Job | undefined {
  return jobs.find((j) => j.id === id);
}

export function submitJob(req: JobRequest): Job {
  const job: Job = {
    id: nextId("job"),
    request: req,
    status: "pending",
    submittedAt: new Date().toISOString(),
    meta: req.meta,
  };
  jobs.unshift(job);
  // Simulate lifecycle progression so the detail/list pages have something to poll.
  setTimeout(() => {
    job.status = "running";
    job.startedAt = new Date().toISOString();
    job.node = `nomad-${req.pool}-${hosts[0].host}-0`;
    job.allocId = nextId("alloc");
  }, 2500);
  setTimeout(() => {
    const ok = Math.random() > 0.2;
    job.status = ok ? "success" : "failed";
    job.exitCode = ok ? 0 : 1;
    job.finishedAt = new Date().toISOString();
  }, 9000);
  return job;
}

export function cancelJob(id: string): void {
  const job = findJob(id);
  if (job && (job.status === "pending" || job.status === "running")) {
    job.status = "canceled";
    job.finishedAt = new Date().toISOString();
  }
}

export function pauseWorker(name: string): void {
  const w = state.fleet.workers.find((w) => w.name === name);
  if (w) {
    w.cordoned = true;
    w.status = "cordoned";
  }
}

export function resumeWorker(name: string): void {
  const w = state.fleet.workers.find((w) => w.name === name);
  if (w) {
    w.cordoned = false;
    w.status = "ready";
  }
}

export function recycleVM(name: string): void {
  const idx = state.fleet.vms.findIndex((v) => v.name === name);
  if (idx === -1) return;
  const old = state.fleet.vms[idx];
  state.fleet.vms[idx] = {
    ...old,
    status: "pending",
    raw: { ageMinutes: 0, ttlRemainingMinutes: 720 },
  };
  setTimeout(() => {
    const cur = state.fleet.vms.find((v) => v.name === name);
    if (cur) cur.status = "running";
  }, 3000);
}

const LOG_LINES = [
  "Cloning repository...",
  "Checking out ref...",
  "Resolving dependencies...",
  "Running build script...",
  "==> make test",
  "go vet ./...",
  "go test ./... -count=1",
  "ok  \tgithub.com/gm2211/grove/internal/dispatch\t0.412s",
  "ok  \tgithub.com/gm2211/grove/internal/orchard\t0.201s",
  "==> make build",
  "building bin/grove ...",
  "uploading artifacts/bin/grove (14.5 MiB)",
  "job finished",
];

export function mockLogStream(
  jobId: string,
  onLine: (line: string) => void,
): () => void {
  let i = 0;
  const job = findJob(jobId);
  const interval = setInterval(() => {
    if (i >= LOG_LINES.length || (job && ["success", "failed", "canceled", "lost"].includes(job.status) && i > 3)) {
      clearInterval(interval);
      return;
    }
    onLine(LOG_LINES[i % LOG_LINES.length]);
    i += 1;
  }, 700);
  return () => clearInterval(interval);
}
