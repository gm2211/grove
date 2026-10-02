import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useParams } from "react-router";
import { api } from "../api/client";
import { StatusPill, statusColor } from "../components/StatusPill";
import { ConfirmButton } from "../components/ConfirmButton";
import { LogViewer } from "../components/LogViewer";
import { Card, PageMessage, Tag } from "../components/Page";
import { ChevronLeftIcon, DownloadIcon } from "../components/Icons";
import { formatBytes, formatJobDuration, formatRelativeAge } from "../lib/format";
import type { Job } from "../api/types";

const STAGES = ["pending", "running", "success"] as const;

function stageIndex(status: string): number {
  if (status === "pending") return 0;
  if (status === "running") return 1;
  return 2; // success, failed, canceled, lost all terminal
}

export function JobDetailPage() {
  const { id = "" } = useParams();
  const qc = useQueryClient();
  const [follow, setFollow] = useState(true);

  const {
    data: job,
    isLoading,
    error,
  } = useQuery({
    queryKey: ["job", id],
    queryFn: () => api.getJob(id),
    refetchInterval: 3000,
  });
  const { data: principal } = useQuery({ queryKey: ["whoami"], queryFn: api.whoAmI, staleTime: 60_000 });

  const cancel = useMutation({
    mutationFn: () => api.cancelJob(id),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["job", id] }),
  });

  if (isLoading) return <PageMessage>Loading job…</PageMessage>;
  if (error) return <PageMessage tone="error">Failed to load job: {String(error)}</PageMessage>;
  if (!job) return null;

  const terminal = ["success", "failed", "canceled", "lost"].includes(job.status);
  const isFailed = job.status === "failed" || job.status === "canceled" || job.status === "lost";
  const idx = stageIndex(job.status);
  const canCancel = principal?.scopes.includes("operator") || job.request.submittedBy === principal?.id;
  const repo = job.request.repo?.replace(/^(https:\/\/)?github\.com\//, "");

  return (
    <div className="flex flex-col gap-5">
      <div className="flex flex-col gap-3">
        <Link
          to="/jobs"
          className="flex w-fit items-center gap-1 text-xs font-medium no-underline hover:underline"
          style={{ color: "var(--fg-muted)" }}
        >
          <ChevronLeftIcon size={13} />
          Jobs
        </Link>
        <div className="flex flex-wrap items-center gap-x-3 gap-y-2">
          <h1 className="mono text-[22px] leading-tight font-semibold tracking-tight">{job.id}</h1>
          <StatusPill status={isFailed ? job.status : (STAGES[idx] ?? job.status)} />
          <div className="ml-auto">
            {!terminal && canCancel && (
              <ConfirmButton
                label="Cancel job"
                confirmLabel="Really cancel?"
                variant="default"
                onConfirm={() => cancel.mutate()}
                disabled={cancel.isPending}
              />
            )}
          </div>
        </div>
        <div className="flex flex-wrap items-center gap-2 text-[13px]" style={{ color: "var(--fg-muted)" }}>
          <Tag>{job.request.kind}</Tag>
          <span>
            on <span style={{ color: "var(--fg)" }}>{job.request.pool}</span>
          </span>
          {repo && (
            <>
              <span style={{ color: "var(--fg-faint)" }}>·</span>
              <span className="mono text-xs" style={{ color: "var(--fg)" }}>
                {repo}
                {job.request.ref ? `@${job.request.ref}` : ""}
              </span>
            </>
          )}
          <span style={{ color: "var(--fg-faint)" }}>·</span>
          <span>
            submitted {formatRelativeAge(job.submittedAt)} by {job.request.requester ?? "unknown"}
          </span>
        </div>
      </div>

      <Stepper job={job} />

      {job.status === "pending" && job.pendingReason && (
        <PageMessage>
          <span className="font-medium" style={{ color: "var(--fg)" }}>
            Waiting for placement:
          </span>{" "}
          {job.pendingReason}
        </PageMessage>
      )}

      <div className="grid grid-cols-1 gap-5 lg:grid-cols-3">
        <div className="flex min-w-0 flex-col gap-2 lg:col-span-2">
          <div className="flex items-center justify-between">
            <h2 className="text-sm font-semibold">Log</h2>
            <label className="flex cursor-pointer items-center gap-2 text-xs" style={{ color: "var(--fg-muted)" }}>
              <input
                type="checkbox"
                className="accent-[var(--accent)]"
                checked={follow}
                onChange={(e) => setFollow(e.target.checked)}
              />
              Follow live output
            </label>
          </div>
          <LogViewer jobId={job.id} follow={follow} jobStatus={job.status} />
        </div>

        <div className="flex min-w-0 flex-col gap-4">
          <Card title="Details" bodyClassName="px-4 py-2">
            <dl className="flex flex-col text-xs">
              <Field label="Node" value={job.node ?? "—"} mono />
              <Field label="Allocation" value={job.allocId ?? "—"} mono />
              <Field label="Duration" value={formatJobDuration(job.startedAt, job.finishedAt)} />
              <Field
                label="Exit code"
                value={job.exitCode === null || job.exitCode === undefined ? "—" : String(job.exitCode)}
                mono
                color={
                  job.exitCode === null || job.exitCode === undefined
                    ? undefined
                    : job.exitCode === 0
                      ? "var(--status-success)"
                      : "var(--status-failed)"
                }
              />
              {job.request.timeout && <Field label="Timeout" value={job.request.timeout} mono />}
              {repo && <Field label="Repository" value={job.request.repo!} mono />}
              {job.request.ref && <Field label="Ref" value={job.request.ref} mono />}
            </dl>
          </Card>

          <Card title="Script" bodyClassName="p-0">
            <pre
              className="mono m-0 overflow-auto px-4 py-3 text-xs leading-relaxed"
              style={{ background: "var(--bg-inset)" }}
            >
              {job.request.script}
            </pre>
          </Card>

          {job.request.env && Object.keys(job.request.env).length > 0 && (
            <Card title="Environment" bodyClassName="px-4 py-2">
              <dl className="flex flex-col text-xs">
                {Object.entries(job.request.env).map(([k, v]) => (
                  <Field key={k} label={k} value={v} mono labelMono />
                ))}
              </dl>
            </Card>
          )}

          {job.artifacts && job.artifacts.length > 0 && (
            <Card title="Artifacts" bodyClassName="p-1">
              {job.artifacts.map((a) => (
                <a
                  key={a.path}
                  href={a.url}
                  className="flex items-center gap-2 rounded-md px-3 py-2 text-xs no-underline hover:bg-[var(--bg-hover)]"
                >
                  <DownloadIcon size={13} />
                  <span className="mono truncate">{a.path}</span>
                  <span className="ml-auto shrink-0" style={{ color: "var(--fg-faint)" }}>
                    {formatBytes(a.size)}
                  </span>
                </a>
              ))}
            </Card>
          )}
        </div>
      </div>
    </div>
  );
}

function Field({
  label,
  value,
  mono,
  labelMono,
  color,
}: {
  label: string;
  value: string;
  mono?: boolean;
  labelMono?: boolean;
  color?: string;
}) {
  return (
    <div
      className="flex items-center justify-between gap-4 border-b py-2 last:border-b-0"
      style={{ borderColor: "var(--border)" }}
    >
      <dt className={`shrink-0 ${labelMono ? "mono" : ""}`} style={{ color: "var(--fg-muted)" }}>
        {label}
      </dt>
      <dd className={`m-0 truncate text-right ${mono ? "mono" : ""}`} style={{ color }} title={value}>
        {value}
      </dd>
    </div>
  );
}

/** Horizontal submitted → started → finished progress, colored by the job's outcome. */
function Stepper({ job }: { job: Job }) {
  const finishedColor = job.finishedAt ? statusColor(job.status) : undefined;
  const steps = [
    { label: "Submitted", at: job.submittedAt, done: true, color: "var(--accent)" },
    { label: "Started", at: job.startedAt, done: !!job.startedAt, color: "var(--accent)" },
    {
      label: job.finishedAt ? `Finished · ${job.status}` : "Finished",
      at: job.finishedAt,
      done: !!job.finishedAt,
      color: finishedColor ?? "var(--accent)",
    },
  ];
  const active = steps.findIndex((s) => !s.done);

  return (
    <ol
      className="grid grid-cols-3 gap-0 overflow-hidden rounded-xl border"
      style={{ borderColor: "var(--border)", background: "var(--bg-elevated)", boxShadow: "var(--shadow-sm)" }}
    >
      {steps.map((s, i) => (
        <li
          key={s.label}
          className="relative flex flex-col gap-1 px-4 py-3"
          style={{ borderLeft: i > 0 ? "1px solid var(--border)" : undefined }}
        >
          <span
            className="absolute inset-x-0 top-0 h-0.5"
            style={{ background: s.done ? s.color : i === active ? "var(--border-strong)" : "transparent" }}
          />
          <span className="flex items-center gap-2 text-xs font-medium capitalize">
            <span
              className={`flex h-4 w-4 items-center justify-center rounded-full text-[9px] font-bold ${
                i === active && job.status === "running" ? "animate-pulse" : ""
              }`}
              style={{
                background: s.done ? s.color : "var(--bg-inset)",
                color: s.done ? "var(--bg-elevated)" : "var(--fg-faint)",
                border: s.done ? undefined : "1px solid var(--border-strong)",
              }}
            >
              {i + 1}
            </span>
            <span style={{ color: s.done ? "var(--fg)" : "var(--fg-faint)" }}>{s.label}</span>
          </span>
          <span className="tabular pl-6 text-xs" style={{ color: "var(--fg-faint)" }}>
            {s.at ? formatRelativeAge(s.at) : i === active ? "in progress…" : "—"}
          </span>
        </li>
      ))}
    </ol>
  );
}
