import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useNavigate, useParams } from "react-router";
import { api } from "../api/client";
import { StatusPill } from "../components/StatusPill";
import { ConfirmButton } from "../components/ConfirmButton";
import { LogViewer } from "../components/LogViewer";
import { formatBytes, formatJobDuration, formatRelativeAge } from "../lib/format";

const STAGES = ["pending", "running", "success"] as const;

function stageIndex(status: string): number {
  if (status === "pending") return 0;
  if (status === "running") return 1;
  return 2; // success, failed, canceled, lost all terminal
}

export function JobDetailPage() {
  const { id = "" } = useParams();
  const navigate = useNavigate();
  const qc = useQueryClient();
  const [follow, setFollow] = useState(true);

  const { data: job, isLoading, error } = useQuery({
    queryKey: ["job", id],
    queryFn: () => api.getJob(id),
    refetchInterval: 3000,
  });

  const cancel = useMutation({
    mutationFn: () => api.cancelJob(id),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["job", id] }),
  });

  if (isLoading) return <div className="p-4 text-sm">Loading job…</div>;
  if (error) return <div className="p-4 text-sm text-red-500">Failed to load job: {String(error)}</div>;
  if (!job) return null;

  const terminal = ["success", "failed", "canceled", "lost"].includes(job.status);
  const isFailed = job.status === "failed" || job.status === "canceled" || job.status === "lost";
  const idx = stageIndex(job.status);

  return (
    <div className="flex flex-col gap-4">
      <div className="flex items-center gap-3">
        <button
          onClick={() => navigate(-1)}
          className="text-xs"
          style={{ color: "var(--fg-muted)" }}
        >
          ← back
        </button>
        <h1 className="mono text-lg font-semibold">{job.id}</h1>
        <StatusPill status={isFailed ? job.status : STAGES[idx] ?? job.status} />
        {!terminal && (
          <ConfirmButton label="Cancel job" onConfirm={() => cancel.mutate()} disabled={cancel.isPending} />
        )}
      </div>

      <div className="grid grid-cols-1 gap-4 lg:grid-cols-3">
        <div
          className="flex flex-col gap-2 rounded-lg border p-3 text-sm lg:col-span-1"
          style={{ borderColor: "var(--border)", background: "var(--bg-elevated)" }}
        >
          <div className="text-[11px] font-semibold uppercase tracking-wide" style={{ color: "var(--fg-faint)" }}>
            Request
          </div>
          <Field label="kind" value={job.request.kind} />
          <Field label="pool" value={job.request.pool} />
          {job.request.repo && <Field label="repo" value={job.request.repo} mono />}
          {job.request.ref && <Field label="ref" value={job.request.ref} mono />}
          <Field label="requester" value={job.request.requester ?? "—"} />
          <Field label="node" value={job.node ?? "—"} mono />
          <Field label="alloc" value={job.allocId ?? "—"} mono />
          {job.status === "pending" && job.pendingReason && (
            <Field label="pending reason" value={job.pendingReason} />
          )}
          <Field label="submitted" value={formatRelativeAge(job.submittedAt)} />
          <Field label="duration" value={formatJobDuration(job.startedAt, job.finishedAt)} />
          <Field label="exit code" value={job.exitCode === null || job.exitCode === undefined ? "—" : String(job.exitCode)} />

          <div className="mt-2 text-[11px] font-semibold uppercase tracking-wide" style={{ color: "var(--fg-faint)" }}>
            Script
          </div>
          <pre
            className="mono overflow-auto rounded p-2 text-xs"
            style={{ background: "var(--bg-inset)" }}
          >
            {job.request.script}
          </pre>

          {job.request.env && Object.keys(job.request.env).length > 0 && (
            <>
              <div className="mt-2 text-[11px] font-semibold uppercase tracking-wide" style={{ color: "var(--fg-faint)" }}>
                Env
              </div>
              <div className="mono text-xs">
                {Object.entries(job.request.env).map(([k, v]) => (
                  <div key={k}>
                    {k}={v}
                  </div>
                ))}
              </div>
            </>
          )}

          <div className="mt-2 text-[11px] font-semibold uppercase tracking-wide" style={{ color: "var(--fg-faint)" }}>
            Status timeline
          </div>
          <Timeline job={job} />

          {job.artifacts && job.artifacts.length > 0 && (
            <>
              <div className="mt-2 text-[11px] font-semibold uppercase tracking-wide" style={{ color: "var(--fg-faint)" }}>
                Artifacts
              </div>
              {job.artifacts.map((a) => (
                <a key={a.path} href={a.url} className="mono flex justify-between text-xs underline">
                  <span>{a.path}</span>
                  <span style={{ color: "var(--fg-muted)" }}>{formatBytes(a.size)}</span>
                </a>
              ))}
            </>
          )}
        </div>

        <div className="lg:col-span-2">
          <div className="mb-2 flex items-center justify-between">
            <div className="text-[11px] font-semibold uppercase tracking-wide" style={{ color: "var(--fg-faint)" }}>
              Live log
            </div>
            <label className="flex items-center gap-1.5 text-xs">
              <input type="checkbox" checked={follow} onChange={(e) => setFollow(e.target.checked)} />
              follow
            </label>
          </div>
          <LogViewer jobId={job.id} follow={follow} jobStatus={job.status} />
        </div>
      </div>

      <Link to="/jobs" className="text-xs underline" style={{ color: "var(--fg-muted)" }}>
        ← all jobs
      </Link>
    </div>
  );
}

function Field({ label, value, mono }: { label: string; value: string; mono?: boolean }) {
  return (
    <div className="flex justify-between gap-2 text-xs">
      <span style={{ color: "var(--fg-faint)" }}>{label}</span>
      <span className={mono ? "mono truncate" : "truncate"}>{value}</span>
    </div>
  );
}

function Timeline({ job }: { job: { submittedAt: string; startedAt?: string | null; finishedAt?: string | null; status: string } }) {
  const steps = [
    { label: "submitted", at: job.submittedAt, done: true },
    { label: "started", at: job.startedAt, done: !!job.startedAt },
    { label: "finished", at: job.finishedAt, done: !!job.finishedAt },
  ];
  return (
    <div className="flex flex-col gap-1 text-xs">
      {steps.map((s) => (
        <div key={s.label} className="flex items-center gap-2">
          <span
            className="h-2 w-2 rounded-full"
            style={{ background: s.done ? "var(--status-success)" : "var(--border)" }}
          />
          <span style={{ color: "var(--fg-muted)" }}>{s.label}</span>
          <span className="ml-auto">{s.at ? formatRelativeAge(s.at) : "—"}</span>
        </div>
      ))}
    </div>
  );
}
