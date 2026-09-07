import { useMemo, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { Link } from "react-router";
import { api } from "../api/client";
import { StatusPill } from "../components/StatusPill";
import { formatJobDuration, formatRelativeAge } from "../lib/format";
import type { JobStatus } from "../api/types";

const ALL_STATUSES: JobStatus[] = ["pending", "running", "success", "failed", "canceled", "lost"];

export function JobsPage() {
  const { data, isLoading, error } = useQuery({
    queryKey: ["jobs"],
    queryFn: api.getJobs,
    refetchInterval: 3000,
  });
  const [statusFilter, setStatusFilter] = useState<string>("all");
  const [poolFilter, setPoolFilter] = useState<string>("all");

  const pools = useMemo(
    () => Array.from(new Set((data ?? []).map((j) => j.request.pool))).sort(),
    [data],
  );

  const filtered = useMemo(() => {
    return (data ?? [])
      .filter((j) => statusFilter === "all" || j.status === statusFilter)
      .filter((j) => poolFilter === "all" || j.request.pool === poolFilter)
      .sort((a, b) => new Date(b.submittedAt).getTime() - new Date(a.submittedAt).getTime());
  }, [data, statusFilter, poolFilter]);

  if (isLoading) return <div className="p-4 text-sm">Loading jobs…</div>;
  if (error) return <div className="p-4 text-sm text-red-500">Failed to load jobs: {String(error)}</div>;

  return (
    <div className="flex flex-col gap-3">
      <div className="flex items-center justify-between">
        <h1 className="text-lg font-semibold">Jobs</h1>
        <div className="flex gap-2 text-xs">
          <select
            className="rounded border px-2 py-1"
            style={{ borderColor: "var(--border)", background: "var(--bg-elevated)" }}
            value={statusFilter}
            onChange={(e) => setStatusFilter(e.target.value)}
          >
            <option value="all">all statuses</option>
            {ALL_STATUSES.map((s) => (
              <option key={s} value={s}>
                {s}
              </option>
            ))}
          </select>
          <select
            className="rounded border px-2 py-1"
            style={{ borderColor: "var(--border)", background: "var(--bg-elevated)" }}
            value={poolFilter}
            onChange={(e) => setPoolFilter(e.target.value)}
          >
            <option value="all">all pools</option>
            {pools.map((p) => (
              <option key={p} value={p}>
                {p}
              </option>
            ))}
          </select>
        </div>
      </div>

      <div className="overflow-x-auto rounded-lg border" style={{ borderColor: "var(--border)" }}>
        <table className="w-full min-w-[900px] border-collapse text-sm">
          <thead>
            <tr
              className="text-left text-[11px] uppercase tracking-wide"
              style={{ color: "var(--fg-faint)", background: "var(--bg-inset)" }}
            >
              <th className="px-3 py-2 font-medium">ID</th>
              <th className="px-3 py-2 font-medium">Kind</th>
              <th className="px-3 py-2 font-medium">Pool</th>
              <th className="px-3 py-2 font-medium">Repo@Ref</th>
              <th className="px-3 py-2 font-medium">Status</th>
              <th className="px-3 py-2 font-medium">Node</th>
              <th className="px-3 py-2 font-medium">Duration</th>
              <th className="px-3 py-2 font-medium">Requester</th>
            </tr>
          </thead>
          <tbody>
            {filtered.map((job) => (
              <tr
                key={job.id}
                className="cursor-pointer border-t hover:brightness-95"
                style={{ borderColor: "var(--border)" }}
              >
                <td className="p-0">
                  <Link to={`/jobs/${job.id}`} className="block px-3 py-2 mono">
                    {job.id}
                  </Link>
                </td>
                <td className="px-3 py-2">{job.request.kind}</td>
                <td className="px-3 py-2">{job.request.pool}</td>
                <td className="px-3 py-2 mono text-xs">
                  {job.request.repo ? `${job.request.repo.replace(/^github\.com\//, "")}@${job.request.ref}` : "—"}
                </td>
                <td className="px-3 py-2">
                  <StatusPill status={job.status} />
                </td>
                <td className="px-3 py-2 mono text-xs">{job.node ?? "—"}</td>
                <td className="px-3 py-2 text-xs">{formatJobDuration(job.startedAt, job.finishedAt)}</td>
                <td className="px-3 py-2 text-xs" style={{ color: "var(--fg-muted)" }}>
                  {job.request.requester ?? "—"}
                  <div className="text-[10px]" style={{ color: "var(--fg-faint)" }}>
                    {formatRelativeAge(job.submittedAt)}
                  </div>
                </td>
              </tr>
            ))}
            {filtered.length === 0 && (
              <tr>
                <td colSpan={8} className="px-3 py-6 text-center text-xs" style={{ color: "var(--fg-faint)" }}>
                  no jobs match this filter
                </td>
              </tr>
            )}
          </tbody>
        </table>
      </div>
    </div>
  );
}
