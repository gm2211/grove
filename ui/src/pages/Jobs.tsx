import { useMemo, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { useNavigate } from "react-router";
import { api } from "../api/client";
import { View, ViewHead } from "../components/Layout";
import { StatusPill, TONES, toneOf } from "../components/StatusPill";
import { formatJobDuration, formatRelativeAge, shortRepo } from "../lib/format";
import type { JobStatus } from "../api/types";

const ALL_STATUSES: JobStatus[] = ["running", "pending", "success", "failed", "canceled", "lost"];
const COLS = "minmax(0,1.1fr) 0.55fr 0.6fr minmax(0,1.5fr) 0.8fr minmax(0,1.2fr) 0.65fr minmax(0,1fr)";

export function JobsPage() {
  const navigate = useNavigate();
  const { data, isLoading, error } = useQuery({
    queryKey: ["jobs"],
    queryFn: api.getJobs,
    refetchInterval: 3000,
  });
  const [statusFilter, setStatusFilter] = useState<string>("all");
  const [poolFilter, setPoolFilter] = useState<string>("all");

  const all = useMemo(() => data ?? [], [data]);
  const pools = useMemo(() => Array.from(new Set(all.map((j) => j.request.pool))).sort(), [all]);
  const counts = useMemo(() => {
    const c: Record<string, number> = {};
    for (const j of all) c[j.status] = (c[j.status] ?? 0) + 1;
    return c;
  }, [all]);

  const filtered = useMemo(
    () =>
      all
        .filter((j) => statusFilter === "all" || j.status === statusFilter)
        .filter((j) => poolFilter === "all" || j.request.pool === poolFilter)
        .sort((a, b) => new Date(b.submittedAt).getTime() - new Date(a.submittedAt).getTime()),
    [all, statusFilter, poolFilter],
  );

  const title = isLoading
    ? "Reading jobs…"
    : `${all.length} jobs · ${counts.running ?? 0} running · ${counts.pending ?? 0} queued · ${counts.failed ?? 0} failed`;

  return (
    <View fill>
      <ViewHead
        kicker="Jobs · every dispatch, newest first"
        title={title}
        lede="Builds, agent sessions and shell jobs, each in its own VM. Open one for its lifecycle, attempts and live log."
      />

      <div className="flex flex-wrap items-center gap-2">
        <button className="selbtn" data-on={statusFilter === "all" ? "1" : "0"} onClick={() => setStatusFilter("all")}>
          all <span className="n">{all.length}</span>
        </button>
        {ALL_STATUSES.map((s) => (
          <button key={s} className="selbtn" data-on={statusFilter === s ? "1" : "0"} onClick={() => setStatusFilter(s)}>
            <span className="dot" style={{ background: TONES[toneOf(s)].dot }} />
            {s} <span className="n">{counts[s] ?? 0}</span>
          </button>
        ))}
        <select
          className="input ml-auto w-auto py-[6px]"
          value={poolFilter}
          onChange={(e) => setPoolFilter(e.target.value)}
          aria-label="pool"
        >
          <option value="all">all pools</option>
          {pools.map((p) => (
            <option key={p} value={p}>
              {p}
            </option>
          ))}
        </select>
      </div>

      {error && <div className="mono text-[12px] text-[#eda3a3]">Failed to load jobs: {String(error)}</div>}

      <div className="panel min-h-0 flex-1 overflow-auto">
        <div className="min-w-[980px]">
          <div className="trow thead sticky top-0 z-[1] bg-[#24282d]" style={{ gridTemplateColumns: COLS }}>
            <span>Job</span>
            <span>Kind</span>
            <span>Pool</span>
            <span>Repo @ ref</span>
            <span>State</span>
            <span>Node</span>
            <span>Took</span>
            <span>Requester</span>
          </div>
          {filtered.map((job) => (
            <div
              key={job.id}
              role="link"
              tabIndex={0}
              className="trow cursor-pointer"
              style={{ gridTemplateColumns: COLS }}
              onClick={() => navigate(`/jobs/${encodeURIComponent(job.id)}`)}
              onKeyDown={(e) => e.key === "Enter" && navigate(`/jobs/${encodeURIComponent(job.id)}`)}
            >
              <span className="mono flex min-w-0 items-center gap-2 text-[12.5px] text-[#eaf4f4]">
                <span
                  className={`dot ${job.status === "running" ? "live" : ""}`}
                  style={{ background: TONES[toneOf(job.status)].dot }}
                />
                <span className="truncate">{job.id}</span>
              </span>
              <span className="mono text-[12px] text-[#9fb0b5]">{job.request.kind}</span>
              <span className="mono text-[12px] text-[#9fb0b5]">{job.request.pool}</span>
              <span className="mono truncate text-[12px] text-[#c3d1d5]">
                {job.request.repo ? (
                  `${shortRepo(job.request.repo)}@${job.request.ref ?? "HEAD"}`
                ) : (
                  <span className="text-[#8a979c]">{job.request.script.split("\n")[0]}</span>
                )}
              </span>
              <span>
                <StatusPill status={job.status} />
              </span>
              <span className="mono truncate text-[11.5px] text-[#a3b0b4]">{job.node ?? "—"}</span>
              <span className="mono text-[12px] text-[#9fb0b5]">{formatJobDuration(job.startedAt, job.finishedAt)}</span>
              <span className="flex min-w-0 flex-col gap-0.5">
                <span className="mono truncate text-[11.5px] text-[#9fb0b5]">{job.request.requester ?? "—"}</span>
                <span className="mono text-[10.5px] text-[#77838a]">{formatRelativeAge(job.submittedAt)}</span>
              </span>
            </div>
          ))}
          {!isLoading && filtered.length === 0 && (
            <div className="mapcol-empty px-[18px] py-8">no jobs match this filter</div>
          )}
        </div>
      </div>
    </View>
  );
}
