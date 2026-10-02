import { useMemo, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { Link, useNavigate } from "react-router";
import { api } from "../api/client";
import { StatusPill, statusColor } from "../components/StatusPill";
import { Button } from "../components/Button";
import { EmptyState, PageHeader, PageMessage, Tag } from "../components/Page";
import { DispatchIcon, JobsIcon, SearchIcon } from "../components/Icons";
import { formatJobDuration, formatRelativeAge } from "../lib/format";
import type { Job, JobStatus } from "../api/types";

const ALL_STATUSES: JobStatus[] = ["pending", "running", "success", "failed", "canceled", "lost"];

/** What the job builds: repo@ref for build/agent jobs, the script's first line for shell jobs. */
function jobSource(job: Job): { text: string; mono: boolean } | null {
  if (job.request.repo) {
    const repo = job.request.repo.replace(/^(https:\/\/)?github\.com\//, "");
    return { text: job.request.ref ? `${repo}@${job.request.ref}` : repo, mono: true };
  }
  const first = job.request.script?.split("\n").find((l) => l.trim());
  return first ? { text: first.trim(), mono: true } : null;
}

export function JobsPage() {
  const navigate = useNavigate();
  const { data, isLoading, error } = useQuery({
    queryKey: ["jobs"],
    queryFn: api.getJobs,
    refetchInterval: 3000,
  });
  const [statusFilter, setStatusFilter] = useState<string>("all");
  const [poolFilter, setPoolFilter] = useState<string>("all");
  const [search, setSearch] = useState("");

  const pools = useMemo(() => Array.from(new Set((data ?? []).map((j) => j.request.pool))).sort(), [data]);

  const counts = useMemo(() => {
    const c: Record<string, number> = { all: 0 };
    for (const j of data ?? []) {
      if (poolFilter !== "all" && j.request.pool !== poolFilter) continue;
      c.all += 1;
      c[j.status] = (c[j.status] ?? 0) + 1;
    }
    return c;
  }, [data, poolFilter]);

  const filtered = useMemo(() => {
    const q = search.trim().toLowerCase();
    return (data ?? [])
      .filter((j) => statusFilter === "all" || j.status === statusFilter)
      .filter((j) => poolFilter === "all" || j.request.pool === poolFilter)
      .filter(
        (j) =>
          !q ||
          j.id.toLowerCase().includes(q) ||
          (j.request.repo ?? "").toLowerCase().includes(q) ||
          (j.request.requester ?? "").toLowerCase().includes(q) ||
          (j.node ?? "").toLowerCase().includes(q),
      )
      .sort((a, b) => new Date(b.submittedAt).getTime() - new Date(a.submittedAt).getTime());
  }, [data, statusFilter, poolFilter, search]);

  const header = (
    <PageHeader
      title="Jobs"
      description="Everything dispatched to the fleet, newest first."
      actions={
        <Button variant="primary" onClick={() => navigate("/dispatch")}>
          <DispatchIcon size={14} />
          Dispatch job
        </Button>
      }
    />
  );

  if (isLoading)
    return (
      <>
        {header}
        <PageMessage>Loading jobs…</PageMessage>
      </>
    );
  if (error)
    return (
      <>
        {header}
        <PageMessage tone="error">Failed to load jobs: {String(error)}</PageMessage>
      </>
    );

  if ((data ?? []).length === 0)
    return (
      <>
        {header}
        <EmptyState
          icon={<JobsIcon size={18} />}
          title="No jobs yet"
          action={
            <Button variant="primary" onClick={() => navigate("/dispatch")}>
              Dispatch your first job
            </Button>
          }
        >
          Jobs submitted from the CLI, the MCP server or this UI show up here.
        </EmptyState>
      </>
    );

  return (
    <div className="flex flex-col">
      {header}

      {/* Toolbar: status tabs with live counts on the left, pool + search on the right. */}
      <div className="mb-3 flex flex-wrap items-center gap-3">
        <div
          className="flex max-w-full gap-0.5 overflow-x-auto rounded-lg border p-0.5"
          style={{ borderColor: "var(--border)", background: "var(--bg-elevated)" }}
          role="tablist"
          aria-label="Filter by status"
        >
          {(["all", ...ALL_STATUSES] as string[]).map((s) => {
            const active = statusFilter === s;
            const n = counts[s] ?? 0;
            return (
              <button
                key={s}
                role="tab"
                aria-selected={active}
                onClick={() => setStatusFilter(s)}
                className="flex shrink-0 items-center gap-1.5 rounded-md px-2.5 py-1 text-xs font-medium capitalize transition-colors"
                style={{
                  background: active ? "var(--bg-inset)" : "transparent",
                  color: active ? "var(--fg)" : "var(--fg-muted)",
                  boxShadow: active ? "inset 0 0 0 1px var(--border)" : undefined,
                }}
              >
                {s !== "all" && <span className="h-1.5 w-1.5 rounded-full" style={{ background: statusColor(s) }} />}
                {s}
                <span className="tabular" style={{ color: "var(--fg-faint)" }}>
                  {n}
                </span>
              </button>
            );
          })}
        </div>

        <div className="ml-auto flex flex-wrap items-center gap-2">
          <label className="relative block">
            <span className="sr-only">Search jobs</span>
            <span
              className="pointer-events-none absolute top-1/2 left-2.5 -translate-y-1/2"
              style={{ color: "var(--fg-faint)" }}
            >
              <SearchIcon size={14} />
            </span>
            <input
              className="control w-56 !pl-8"
              placeholder="Search id, repo, node…"
              value={search}
              onChange={(e) => setSearch(e.target.value)}
            />
          </label>
          <select
            className="control !w-auto"
            aria-label="Filter by pool"
            value={poolFilter}
            onChange={(e) => setPoolFilter(e.target.value)}
          >
            <option value="all">All pools</option>
            {pools.map((p) => (
              <option key={p} value={p}>
                {p}
              </option>
            ))}
          </select>
        </div>
      </div>

      <div
        className="overflow-x-auto rounded-xl border"
        style={{ borderColor: "var(--border)", background: "var(--bg-elevated)", boxShadow: "var(--shadow-sm)" }}
      >
        <table className="w-full min-w-[880px] border-collapse text-[13px]">
          <thead>
            <tr className="text-left" style={{ background: "var(--bg-inset)" }}>
              <th className="eyebrow px-4 py-2.5">Job</th>
              <th className="eyebrow px-3 py-2.5">Status</th>
              <th className="eyebrow px-3 py-2.5">Source</th>
              <th className="eyebrow px-3 py-2.5">Pool</th>
              <th className="eyebrow px-3 py-2.5">Node</th>
              <th className="eyebrow px-3 py-2.5 text-right">Duration</th>
              <th className="eyebrow px-4 py-2.5 text-right">Submitted</th>
            </tr>
          </thead>
          <tbody>
            {filtered.map((job) => {
              const source = jobSource(job);
              return (
                <tr
                  key={job.id}
                  className="cursor-pointer border-t transition-colors hover:bg-[var(--bg-hover)]"
                  style={{ borderColor: "var(--border)" }}
                  onClick={() => navigate(`/jobs/${job.id}`)}
                >
                  <td className="px-4 py-2.5">
                    <div className="flex items-center gap-2">
                      <Link
                        to={`/jobs/${job.id}`}
                        className="mono font-medium no-underline hover:underline"
                        onClick={(e) => e.stopPropagation()}
                      >
                        {job.id}
                      </Link>
                      <Tag>{job.request.kind}</Tag>
                    </div>
                  </td>
                  <td className="px-3 py-2.5">
                    <StatusPill status={job.status} />
                  </td>
                  <td className="max-w-[320px] px-3 py-2.5">
                    {source ? (
                      <span
                        className={`block truncate text-xs ${source.mono ? "mono" : ""}`}
                        style={{ color: job.request.repo ? "var(--fg)" : "var(--fg-muted)" }}
                        title={source.text}
                      >
                        {source.text}
                      </span>
                    ) : (
                      <span style={{ color: "var(--fg-faint)" }}>—</span>
                    )}
                  </td>
                  <td className="px-3 py-2.5">{job.request.pool}</td>
                  <td className="mono px-3 py-2.5 text-xs" style={{ color: "var(--fg-muted)" }}>
                    {job.node ?? "—"}
                  </td>
                  <td className="tabular px-3 py-2.5 text-right text-xs">
                    {formatJobDuration(job.startedAt, job.finishedAt)}
                  </td>
                  <td className="px-4 py-2.5 text-right text-xs whitespace-nowrap">
                    <div>{formatRelativeAge(job.submittedAt)}</div>
                    <div className="text-[11px]" style={{ color: "var(--fg-faint)" }}>
                      by {job.request.requester ?? "unknown"}
                    </div>
                  </td>
                </tr>
              );
            })}
            {filtered.length === 0 && (
              <tr>
                <td colSpan={7} className="px-4 py-10 text-center text-[13px]" style={{ color: "var(--fg-faint)" }}>
                  No jobs match these filters.
                </td>
              </tr>
            )}
          </tbody>
        </table>
      </div>
      <div className="mt-2 text-xs" style={{ color: "var(--fg-faint)" }}>
        Showing {filtered.length} of {(data ?? []).length} jobs · refreshes every 3s
      </div>
    </div>
  );
}
