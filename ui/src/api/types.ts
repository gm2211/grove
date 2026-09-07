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
  timeout?: number; // nanoseconds, as encoded by Go's time.Duration
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
