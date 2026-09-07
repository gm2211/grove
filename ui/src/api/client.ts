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

/**
 * Streams a job's logs. Returns an unsubscribe function. `onLine` fires per log line, `onDone`
 * fires when the stream closes (job finished or connection ended).
 */
export function streamJobLogs(
  jobId: string,
  handlers: { onLine: (line: string) => void; onOpen?: () => void; onError?: () => void; onDone?: () => void },
): () => void {
  if (isMock) {
    handlers.onOpen?.();
    const stop = mock.mockLogStream(jobId, handlers.onLine);
    return stop;
  }

  const base = getServerUrl();
  const token = getToken();
  const url = new URL(`${base}/api/v1/jobs/${encodeURIComponent(jobId)}/logs`);
  url.searchParams.set("follow", "1");
  // EventSource can't set headers, so pass the token as a query param when present. The server
  // must accept ?token= as a fallback to the Authorization header for this endpoint.
  if (token) url.searchParams.set("token", token);

  const source = new EventSource(url.toString());
  source.onopen = () => handlers.onOpen?.();
  source.onmessage = (evt) => handlers.onLine(evt.data);
  source.onerror = () => {
    handlers.onError?.();
  };
  return () => {
    source.close();
    handlers.onDone?.();
  };
}
