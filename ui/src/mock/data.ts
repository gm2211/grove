// Realistic in-memory fake fleet + jobs for `npm run dev:mock` (VITE_MOCK=1). Standalone UI dev
// with no grove server running.
import type {
  EnableGitHubSourcingRequest,
  FleetEntry,
  FleetResponse,
  GitHubSourcingStatus,
  Job,
  JobKind,
  JobRequest,
  JobStatus,
  PendingJoin,
} from "../api/types";

// Deterministic, so `npm run screenshots` captures the same fleet every time.
let seed = 7;
function rand(): number {
  seed = (seed * 16807) % 2147483647;
  return (seed - 1) / 2147483646;
}

// Job ids are 16 hex characters, like internal/dispatch's randomID.
function hexId(): string {
  let s = "";
  for (let i = 0; i < 16; i++) s += Math.floor(rand() * 16).toString(16);
  return s;
}
function nextId(prefix: string): string {
  return `${prefix}-${hexId().slice(0, 8)}`;
}

const MIN = 60_000;
const NOW = Date.now();
const ago = (ms: number) => new Date(NOW - ms).toISOString();

interface HostDef {
  host: string;
  arch: "arm64" | "amd64";
  cores: number;
  memGB: number;
  vmSlots: number;
  online: boolean;
  paused?: boolean;
  snoozeMinutes?: number;
  // pool and age (minutes) of each VM; VMs live 12h, then grove recycles them
  vms: { pool: "linux" | "macos"; ageMin: number }[];
}

const hosts: HostDef[] = [
  {
    host: "mac-studio-1",
    arch: "arm64",
    cores: 24,
    memGB: 64,
    vmSlots: 4,
    online: true,
    vms: [
      { pool: "linux", ageMin: 268 },
      { pool: "linux", ageMin: 655 },
      { pool: "macos", ageMin: 131 },
    ],
  },
  {
    host: "mac-mini-1",
    arch: "arm64",
    cores: 10,
    memGB: 32,
    vmSlots: 2,
    online: true,
    vms: [
      { pool: "linux", ageMin: 402 },
      { pool: "macos", ageMin: 47 },
    ],
  },
  {
    host: "mac-mini-2",
    arch: "arm64",
    cores: 10,
    memGB: 32,
    vmSlots: 2,
    online: true,
    snoozeMinutes: 95,
    vms: [{ pool: "linux", ageMin: 512 }],
  },
  { host: "macbook-pro", arch: "arm64", cores: 14, memGB: 36, vmSlots: 2, online: false, vms: [] },
];

const VM_TTL_NS = 12 * 3600 * 1e9;

function makeWorkers(): FleetEntry[] {
  return hosts.map((h) => {
    const snoozed = h.snoozeMinutes !== undefined;
    return {
      name: h.host,
      kind: "worker",
      host: h.host,
      arch: h.arch,
      online: h.online,
      cordoned: snoozed || !!h.paused,
      status: !h.online ? "offline" : snoozed ? "snoozed" : h.paused ? "paused" : "online",
      ...(snoozed
        ? {
            snoozedUntil: new Date(NOW + (h.snoozeMinutes ?? 0) * MIN).toISOString(),
            snoozeMode: "drain",
            pauseReasons: ["snooze"],
          }
        : {}),
      capacity: { cpu: h.cores, memoryMiB: h.memGB * 1024, vmSlots: h.vmSlots },
      running: h.online ? h.vms.length : 0,
      labels: {},
      raw: {},
    };
  });
}

function makeVMs(): FleetEntry[] {
  const vms: FleetEntry[] = [];
  for (const h of hosts) {
    const perPool: Record<string, number> = {};
    for (const v of h.vms) {
      const n = perPool[v.pool] ?? 0;
      perPool[v.pool] = n + 1;
      vms.push({
        // <pool>-<worker>-<n>, the name fleet.ParseVMName reads back
        name: `${v.pool}-${h.host}-${n}`,
        kind: "vm",
        host: h.host,
        pool: v.pool,
        online: true,
        cordoned: false,
        status: "running",
        capacity: v.pool === "macos" ? { cpu: 4, memoryMiB: 8192 } : { cpu: 6, memoryMiB: 12288 },
        running: 1,
        labels: {},
        raw: {
          image: v.pool === "macos" ? "ghcr.io/gm2211/grove-macos-worker:latest" : "ghcr.io/gm2211/grove-linux-worker:latest",
          worker: h.host,
          createdAt: ago(v.ageMin * MIN),
          ttl: VM_TTL_NS,
          restartCount: 0,
        },
      });
    }
  }
  return vms;
}

// A Nomad node registers under its VM's own name (see internal/fleet/rollcall.go).
function makeNodes(vms: FleetEntry[]): FleetEntry[] {
  return vms.map((vm) => ({
    name: vm.name,
    kind: "node",
    host: vm.host,
    online: true,
    cordoned: false,
    status: "ready",
    capacity: { memoryMiB: vm.capacity?.memoryMiB },
    running: 0,
    labels: { pool: vm.pool ?? "", host: vm.host ?? "" },
    raw: {},
  }));
}

export const state: { fleet: FleetResponse } = {
  fleet: (() => {
    const workers = makeWorkers();
    const vms = makeVMs();
    const nodes = makeNodes(vms);
    // One stuck VM, so the Map's "stuck" panel shows in mock mode and screenshots.
    const mac = vms.find((v) => v.pool === "macos");
    const problems: FleetResponse["problems"] = mac
      ? [{ kind: "needs-unlock", vm: mac.name, pool: "macos", worker: mac.host, restarts: 10,
          detail: `the worker user on ${mac.host} has no login session (macOS guests need its unlocked login keychain)` }]
      : [];
    return { workers, vms, nodes, problems };
  })(),
};

const requesters = ["argos", "mcp:claude-code", "cli:gio-macbook", "ci:github"];

interface JobDef {
  kind: JobKind;
  pool: "linux" | "macos";
  vm?: string;
  status: JobStatus;
  repo?: string;
  ref?: string;
  script: string;
  agoMin: number;
  tookMin?: number;
  reason?: string;
}

const B = "make test && make build";
const SWIFT = "xcodebuild -scheme App -destination 'platform=macOS' test";
const AGENT = "claude -p \"$GROVE_TASK\" --permission-mode acceptEdits";

const JOBS: JobDef[] = [
  // running now
  { kind: "build", pool: "linux", vm: "linux-mac-studio-1-0", status: "running", repo: "github.com/gm2211/grove", ref: "pr-151", script: B, agoMin: 3 },
  { kind: "agent", pool: "linux", vm: "linux-mac-studio-1-0", status: "running", repo: "github.com/gm2211/argos", ref: "main", script: AGENT, agoMin: 11 },
  { kind: "build", pool: "linux", vm: "linux-mac-studio-1-1", status: "running", repo: "github.com/gm2211/orchard", ref: "main", script: B, agoMin: 6 },
  { kind: "agent", pool: "macos", vm: "macos-mac-studio-1-0", status: "running", repo: "github.com/gm2211/skim", ref: "argos/sk-41", script: AGENT, agoMin: 18 },
  { kind: "build", pool: "macos", vm: "macos-mac-mini-1-0", status: "running", repo: "github.com/gm2211/turbol", ref: "release/2.4", script: SWIFT, agoMin: 4 },
  { kind: "build", pool: "linux", vm: "linux-mac-mini-2-0", status: "running", repo: "github.com/gm2211/motive", ref: "pr-88", script: B, agoMin: 9 },
  // waiting for room
  { kind: "build", pool: "macos", status: "pending", repo: "github.com/gm2211/turbol", ref: "pr-212", script: SWIFT, agoMin: 2, reason: "resources exhausted (vm-slots 2) — 0/2 nodes available" },
  { kind: "agent", pool: "macos", status: "pending", repo: "github.com/gm2211/skim", ref: "argos/sk-44", script: AGENT, agoMin: 1, reason: "resources exhausted (vm-slots 2) — 0/2 nodes available" },
  { kind: "build", pool: "linux", status: "pending", repo: "github.com/gm2211/jev-polls", ref: "main", script: B, agoMin: 0.4 },
  // history
  { kind: "build", pool: "linux", vm: "linux-mac-studio-1-0", status: "success", repo: "github.com/gm2211/grove", ref: "main", script: B, agoMin: 22, tookMin: 6 },
  { kind: "build", pool: "linux", vm: "linux-mac-studio-1-1", status: "failed", repo: "github.com/gm2211/grove", ref: "pr-149", script: B, agoMin: 31, tookMin: 4 },
  { kind: "agent", pool: "macos", vm: "macos-mac-studio-1-0", status: "success", repo: "github.com/gm2211/skim", ref: "argos/sk-39", script: AGENT, agoMin: 46, tookMin: 21 },
  { kind: "shell", pool: "macos", vm: "macos-mac-mini-1-0", status: "success", script: "sw_vers && xcrun --show-sdk-version", agoMin: 50, tookMin: 0.3 },
  { kind: "build", pool: "linux", vm: "linux-mac-mini-1-0", status: "success", repo: "github.com/gm2211/orchard", ref: "pr-31", script: B, agoMin: 58, tookMin: 7 },
  { kind: "build", pool: "linux", vm: "linux-mac-mini-2-0", status: "canceled", repo: "github.com/gm2211/motive", ref: "pr-87", script: B, agoMin: 64, tookMin: 2 },
  { kind: "agent", pool: "linux", vm: "linux-mac-studio-1-0", status: "success", repo: "github.com/gm2211/argos", ref: "main", script: AGENT, agoMin: 75, tookMin: 33 },
  { kind: "build", pool: "macos", vm: "macos-mac-mini-1-0", status: "lost", repo: "github.com/gm2211/turbol", ref: "pr-209", script: SWIFT, agoMin: 96, tookMin: 12 },
  { kind: "build", pool: "linux", vm: "linux-mac-studio-1-1", status: "success", repo: "github.com/gm2211/grove", ref: "pr-148", script: B, agoMin: 110, tookMin: 6 },
  { kind: "build", pool: "linux", vm: "linux-mac-mini-1-0", status: "success", repo: "github.com/gm2211/jev-polls", ref: "main", script: B, agoMin: 128, tookMin: 3 },
  { kind: "agent", pool: "macos", vm: "macos-mac-studio-1-0", status: "failed", repo: "github.com/gm2211/skim", ref: "argos/sk-37", script: AGENT, agoMin: 140, tookMin: 15 },
  { kind: "shell", pool: "linux", vm: "linux-mac-studio-1-0", status: "success", script: "df -h && uptime", agoMin: 151, tookMin: 0.2 },
  { kind: "build", pool: "linux", vm: "linux-mac-studio-1-1", status: "success", repo: "github.com/gm2211/orchard", ref: "main", script: B, agoMin: 177, tookMin: 8 },
  { kind: "build", pool: "macos", vm: "macos-mac-mini-1-0", status: "success", repo: "github.com/gm2211/turbol", ref: "main", script: SWIFT, agoMin: 204, tookMin: 14 },
  { kind: "build", pool: "linux", vm: "linux-mac-mini-2-0", status: "success", repo: "github.com/gm2211/motive", ref: "main", script: B, agoMin: 233, tookMin: 5 },
];

function makeJob(d: JobDef, i: number): Job {
  const submitted = NOW - d.agoMin * MIN - 20_000;
  const started = d.status === "pending" ? undefined : submitted + 4_000 + rand() * 6_000;
  const finished = started !== undefined && d.tookMin !== undefined ? started + d.tookMin * MIN : undefined;
  const req: JobRequest = {
    kind: d.kind,
    pool: d.pool,
    repo: d.repo ? `https://${d.repo}` : undefined,
    ref: d.ref,
    script: d.script,
    env: { CI: "true" },
    timeout: d.kind === "agent" ? "2h0m0s" : "30m0s",
    requester: requesters[i % requesters.length],
  };
  return {
    id: hexId(),
    request: req,
    status: d.status,
    node: d.vm,
    allocId: d.vm ? `${hexId().slice(0, 8)}-${hexId().slice(0, 4)}` : undefined,
    exitCode: d.status === "success" ? 0 : d.status === "failed" ? 2 : null,
    submittedAt: new Date(submitted).toISOString(),
    startedAt: started ? new Date(started).toISOString() : null,
    finishedAt: finished ? new Date(finished).toISOString() : null,
    pendingReason: d.reason,
    failureReason: d.status === "lost" ? "the VM stopped answering mid-run; requeue it" : undefined,
    attempts: d.vm
      ? [
          {
            vm: d.vm,
            worker: d.vm.replace(/^[a-z]+-/, "").replace(/-\d+$/, ""),
            startedAt: new Date(started ?? submitted).toISOString(),
            finishedAt: finished ? new Date(finished).toISOString() : null,
            outcome: d.status === "running" ? "running" : d.status === "lost" ? "lost" : d.status === "canceled" ? "canceled" : "exited",
            exitCode: d.status === "success" ? 0 : d.status === "failed" ? 2 : null,
          },
        ]
      : undefined,
    artifacts:
      d.kind === "build" && d.status === "success"
        ? [{ path: "artifacts/bin/app", url: "#", size: 15_234_112 }]
        : [],
  };
}

export const jobs: Job[] = JOBS.map(makeJob);

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
    job.node = `${req.pool}-${hosts[0].host}-0`;
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
    w.status = "paused";
    w.pauseReasons = [...new Set([...(w.pauseReasons ?? []), "operator"])];
  }
}

export function snoozeWorker(name: string, forDuration: string): void {
  const w = state.fleet.workers.find((w) => w.name === name);
  if (!w) return;
  const m = /^(\d+)(m|h)$/.exec(forDuration);
  const minutes = m ? Number(m[1]) * (m[2] === "h" ? 60 : 1) : 120;
  w.cordoned = true;
  w.status = "snoozed";
  w.snoozedUntil = new Date(Date.now() + minutes * 60_000).toISOString();
  w.snoozeMode = "drain";
  w.pauseReasons = ["snooze"];
}

export function wakeWorker(name: string): void {
  const w = state.fleet.workers.find((w) => w.name === name);
  if (!w) return;
  delete w.snoozedUntil;
  delete w.snoozeMode;
  w.pauseReasons = (w.pauseReasons ?? []).filter((r) => r !== "snooze");
  if (w.pauseReasons.length === 0) {
    w.cordoned = false;
    w.status = "online";
  } else {
    w.status = "paused";
  }
}

export function resumeWorker(name: string): void {
  const w = state.fleet.workers.find((w) => w.name === name);
  if (w) {
    w.cordoned = false;
    w.status = "online";
    w.pauseReasons = [];
  }
}

export function recycleVM(name: string): void {
  const idx = state.fleet.vms.findIndex((v) => v.name === name);
  if (idx === -1) return;
  const old = state.fleet.vms[idx];
  state.fleet.vms[idx] = {
    ...old,
    status: "pending",
    raw: { ...(old.raw as Record<string, unknown>), createdAt: new Date().toISOString() },
  };
  setTimeout(() => {
    const cur = state.fleet.vms.find((v) => v.name === name);
    if (cur) cur.status = "running";
  }, 3000);
}

const LOG_LINES = [
  "grove: cloning https://github.com/gm2211/grove @ pr-151",
  "Cloning into '/work/src'...",
  "HEAD is now at 4be2c1e Map: queue column follows the selected pool",
  "grove: source ready in 2.1s, handing over to the job",
  "==> make test",
  "go vet ./...",
  "go test ./... -count=1",
  "ok  \tgithub.com/gm2211/grove/internal/broker\t1.904s",
  "ok  \tgithub.com/gm2211/grove/internal/dispatch\t3.412s",
  "ok  \tgithub.com/gm2211/grove/internal/fleet\t2.207s",
  "ok  \tgithub.com/gm2211/grove/internal/install\t4.981s",
  "ok  \tgithub.com/gm2211/grove/internal/server\t2.733s",
  "ok  \tgithub.com/gm2211/grove/internal/snooze\t0.318s",
  "==> make build",
  "go build -ldflags '-X main.version=dev' -o bin/grove ./cmd/grove",
  "uploading artifacts/bin/grove (14.5 MiB)",
  "job finished",
];

export function mockLogStream(
  jobId: string,
  onLine: (line: string) => void,
): () => void {
  const job = findJob(jobId);
  const done = () => !!job && ["success", "failed", "canceled", "lost"].includes(job.status);
  // A real stream replays from offset 0, so a job that has been running a while arrives with a
  // backlog; then it trickles.
  let i = 0;
  const backlog = job?.startedAt ? Math.min(10, Math.floor((Date.now() - Date.parse(job.startedAt)) / 30_000)) : 0;
  for (; i < backlog; i++) onLine(LOG_LINES[i]);
  const interval = setInterval(() => {
    if (i >= LOG_LINES.length - (done() ? 0 : 1)) {
      clearInterval(interval);
      return;
    }
    onLine(LOG_LINES[i]);
    i += 1;
  }, 900);
  return () => clearInterval(interval);
}

// Private-repo sourcing starts off, exactly as a real control plane does.
export const githubSourcing: GitHubSourcingStatus = { enabled: false };

export function enableGitHubSourcing(req: EnableGitHubSourcingRequest): GitHubSourcingStatus {
  const now = new Date();
  githubSourcing.enabled = true;
  githubSourcing.hosts = req.hosts?.length ? req.hosts : ["github.com"];
  githubSourcing.repos = req.repos ?? [];
  githubSourcing.tokenFingerprint = "mockf1n6erpr";
  githubSourcing.enabledAt = now.toISOString();
  githubSourcing.enabledBy = "mock-operator";
  githubSourcing.expiresAt = req.ttl ? new Date(now.getTime() + 4 * 3600_000).toISOString() : undefined;
  return structuredClone(githubSourcing);
}

export function disableGitHubSourcing(): GitHubSourcingStatus {
  githubSourcing.enabled = false;
  githubSourcing.hosts = undefined;
  githubSourcing.repos = undefined;
  githubSourcing.tokenFingerprint = undefined;
  githubSourcing.enabledAt = undefined;
  githubSourcing.enabledBy = undefined;
  githubSourcing.expiresAt = undefined;
  githubSourcing.lastUsedAt = undefined;
  return structuredClone(githubSourcing);
}

// Two Macs waiting to be let in, so `npm run dev:mock` shows the approval panel with something
// in it. Approving one drops it from the list, the same way the real control plane does.
export const pendingJoins: PendingJoin[] = [
  {
    code: "K7QP2M4A",
    name: "mac-mini-3",
    tailnetIp: "100.64.0.31",
    expiresAt: new Date(Date.now() + 9 * 60_000).toISOString(),
  },
  {
    code: "B3XR9T1C",
    name: "macbook-1",
    tailnetIp: "100.64.0.42",
    expiresAt: new Date(Date.now() + 10 * 60_000).toISOString(),
  },
];

export function approveJoin(code: string): void {
  const at = pendingJoins.findIndex((join) => join.code === code);
  if (at >= 0) pendingJoins.splice(at, 1);
}
