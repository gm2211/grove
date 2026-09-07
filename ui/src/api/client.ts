import { getServerUrl, getToken } from "../lib/settings";
import type { FleetResponse, HealthResponse, Job, JobRequest } from "./types";
import * as mock from "../mock/data";

const isMock = import.meta.env.VITE_MOCK === "1";

class ApiError extends Error {
  constructor(
    public status: number,
    message: string,
  ) {
    super(message);
    this.name = "ApiError";
  }
}

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const base = getServerUrl();
  const token = getToken();
  const res = await fetch(`${base}${path}`, {
    ...init,
    headers: {
      "Content-Type": "application/json",
      ...(token ? { Authorization: `Bearer ${token}` } : {}),
      ...init?.headers,
    },
  });
  if (!res.ok) {
    const body = await res.text().catch(() => "");
    throw new ApiError(res.status, body || res.statusText);
  }
  if (res.status === 204) return undefined as T;
  return (await res.json()) as T;
}

function delay<T>(value: T, ms = 250): Promise<T> {
  return new Promise((resolve) => setTimeout(() => resolve(value), ms));
}

export const api = {
  async getFleet(): Promise<FleetResponse> {
    if (isMock) return delay(structuredClone(mock.state.fleet));
    return request<FleetResponse>("/api/v1/fleet");
  },

  async getJobs(): Promise<Job[]> {
    if (isMock) return delay(structuredClone(mock.jobs));
    return request<Job[]>("/api/v1/jobs");
  },

  async getJob(id: string): Promise<Job> {
    if (isMock) {
      const job = mock.findJob(id);
      if (!job) throw new ApiError(404, "job not found");
      return delay(structuredClone(job));
    }
    return request<Job>(`/api/v1/jobs/${encodeURIComponent(id)}`);
  },

  async submitJob(req: JobRequest): Promise<Job> {
    if (isMock) return delay(structuredClone(mock.submitJob(req)), 400);
    return request<Job>("/api/v1/jobs", {
      method: "POST",
      body: JSON.stringify(req),
    });
  },

  async cancelJob(id: string): Promise<void> {
    if (isMock) {
      mock.cancelJob(id);
      return delay(undefined);
    }
    await request<void>(`/api/v1/jobs/${encodeURIComponent(id)}`, { method: "DELETE" });
  },

  async recycleVM(name: string): Promise<void> {
    if (isMock) {
      mock.recycleVM(name);
      return delay(undefined);
    }
    await request<void>(`/api/v1/vms/${encodeURIComponent(name)}/recycle`, { method: "POST" });
  },

  async pauseWorker(name: string): Promise<void> {
    if (isMock) {
      mock.pauseWorker(name);
      return delay(undefined);
    }
    await request<void>(`/api/v1/workers/${encodeURIComponent(name)}/pause`, { method: "POST" });
  },

  async resumeWorker(name: string): Promise<void> {
    if (isMock) {
      mock.resumeWorker(name);
      return delay(undefined);
    }
    await request<void>(`/api/v1/workers/${encodeURIComponent(name)}/resume`, { method: "POST" });
  },

  async getHealth(): Promise<HealthResponse> {
    if (isMock) return delay({ ok: true, orchard: true, nomad: true, version: "mock" });
    return request<HealthResponse>("/api/v1/healthz");
  },
};

export { ApiError };

export interface LogLine {
  offset: number;
  ts: string;
  stream: "stdout" | "stderr";
  line: string;
}

interface StreamJobLogsHandlers {
  onLine: (entry: LogLine) => void;
  onOpen?: () => void;
  onError?: () => void;
  onDone?: () => void;
  isTerminal: () => boolean;
}

const RECONNECT_DELAY_MS = 1500;

/**
 * Streams a job's logs over fetch + NDJSON (EventSource can't set an Authorization header, so it
 * isn't usable here — the server intentionally accepts only the header, never a token in the
 * URL). Returns an unsubscribe function. `onLine` fires per log line, `onDone` fires once the
 * stream is done for good (job terminal and server closed, or unsubscribed).
 *
 * Reconnects with `sinceOffset` set to the last processed offset whenever the server closes the
 * stream (or fails to connect, e.g. the job has no Nomad allocation yet) and the job is not yet
 * terminal, per `isTerminal`.
 */
export function streamJobLogs(jobId: string, handlers: StreamJobLogsHandlers): () => void {
  if (isMock) {
    handlers.onOpen?.();
    let i = 0;
    const stop = mock.mockLogStream(jobId, (line) => {
      handlers.onLine({ offset: i, ts: new Date().toISOString(), stream: "stdout", line });
      i += 1;
    });
    return stop;
  }

  let stopped = false;
  let lastOffset = 0;
  let reconnectTimer: ReturnType<typeof setTimeout> | undefined;
  let controller: AbortController | undefined;

  const base = getServerUrl();
  const token = getToken();

  async function connect() {
    if (stopped) return;
    controller = new AbortController();
    const url = new URL(`${base}/api/v1/jobs/${encodeURIComponent(jobId)}/logs`);
    url.searchParams.set("follow", "1");
    if (lastOffset > 0) url.searchParams.set("sinceOffset", String(lastOffset));

    try {
      const res = await fetch(url.toString(), {
        headers: {
          Accept: "application/x-ndjson",
          ...(token ? { Authorization: `Bearer ${token}` } : {}),
        },
        signal: controller.signal,
      });

      if (!res.ok || !res.body) {
        handlers.onError?.();
        scheduleReconnectOrStop();
        return;
      }

      handlers.onOpen?.();

      const reader = res.body.getReader();
      const decoder = new TextDecoder();
      let buffer = "";

      for (;;) {
        const { value, done } = await reader.read();
        if (stopped) return;
        if (done) break;
        buffer += decoder.decode(value, { stream: true });
        const parts = buffer.split("\n");
        buffer = parts.pop() ?? "";
        for (const part of parts) {
          if (!part.trim()) continue;
          try {
            const entry = JSON.parse(part) as LogLine;
            lastOffset = Math.max(lastOffset, entry.offset + entry.line.length + 1);
            handlers.onLine(entry);
          } catch {
            // skip lines that fail to parse rather than crashing the stream
          }
        }
      }

      if (stopped) return;
      if (handlers.isTerminal()) {
        handlers.onDone?.();
      } else {
        scheduleReconnectOrStop();
      }
    } catch (err) {
      if (stopped || (err instanceof DOMException && err.name === "AbortError")) return;
      handlers.onError?.();
      scheduleReconnectOrStop();
    }
  }

  function scheduleReconnectOrStop() {
    if (stopped) return;
    if (handlers.isTerminal()) {
      handlers.onDone?.();
      return;
    }
    reconnectTimer = setTimeout(connect, RECONNECT_DELAY_MS);
  }

  connect();

  return () => {
    stopped = true;
    if (reconnectTimer) clearTimeout(reconnectTimer);
    controller?.abort();
  };
}
