// Mirrors the Go JSON wire types in internal/dispatch, internal/orchard, internal/nomad.
// Keep in sync with ARCHITECTURE.md's "grove HTTP API (v1)" table.

export type JobKind = "build" | "agent" | "shell";

export type JobStatus =
  | "pending"
  | "running"
  | "success"
  | "failed"
  | "canceled"
  | "lost";

export interface JobRequest {
  kind: JobKind;
  pool: string;
  repo?: string;
  ref?: string;
  script: string;
  env?: Record<string, string>;
  secrets?: string[];
  // A duration string ("30m", "2h", "90s") or a number of SECONDS — never nanoseconds. See
  // internal/dispatch/duration.go's Duration type. Sent as a string; a completed Job.request
  // read back from the server round-trips as a string too (e.g. "2m0s").
  timeout?: string;
  meta?: Record<string, string>;
  requester?: string;
}

export interface Artifact {
  path: string;
  url: string;
  size: number;
}

export interface Job {
  id: string;
  request: JobRequest;
  status: JobStatus;
  allocId?: string;
  node?: string;
  exitCode?: number | null;
  submittedAt: string;
  startedAt?: string | null;
  finishedAt?: string | null;
  artifacts?: Artifact[];
  meta?: Record<string, string>;
  // Set only while status is "pending": why Nomad hasn't placed this job yet (constraint
  // filtered / resources exhausted / no nodes available), derived from its blocked evaluation.
  pendingReason?: string;
}

export type FleetEntryKind = "worker" | "vm" | "node";

export interface FleetCapacity {
  cpu?: number;
  memoryMiB?: number;
  vmSlots?: number;
}

export interface FleetEntry {
  name: string;
  kind: FleetEntryKind;
  host?: string;
  // Set for vm entries: which fleet.yaml pool the VM belongs to. Derived server-side from the VM
  // name, not from `labels` — Orchard VM labels are worker selectors the scheduler enforces, not
  // free-form metadata (see ARCHITECTURE.md's "Fleet spec" section).
  pool?: string;
  arch?: string;
  online: boolean;
  cordoned: boolean;
  status: string;
  capacity?: FleetCapacity;
  running: number;
  labels?: Record<string, string>;
  raw?: Record<string, unknown>;
}

export interface FleetResponse {
  workers: FleetEntry[];
  vms: FleetEntry[];
  nodes: FleetEntry[];
}

export interface HealthResponse {
  ok: boolean;
  orchard?: boolean;
  nomad?: boolean;
  version?: string;
  [key: string]: unknown;
}
