import { useState, type ReactNode } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useParams } from "react-router";
import { api } from "../api/client";
import type { Job } from "../api/types";
import { View, ViewHead } from "../components/Layout";
import { StatusPill, TONES, toneOf, type Tone } from "../components/StatusPill";
import { ConfirmButton } from "../components/ConfirmButton";
import { LogViewer } from "../components/LogViewer";
import { isTerminal } from "../lib/fleetModel";
import { formatBytes, formatJobDuration, formatRelativeAge, shortRepo } from "../lib/format";

export function JobDetailPage() {
  const { id = "" } = useParams();
  const qc = useQueryClient();
  const [follow, setFollow] = useState(true);

  const { data: job, isLoading, error } = useQuery({
    queryKey: ["job", id],
    queryFn: () => api.getJob(id),
    refetchInterval: 3000,
  });
  const { data: principal } = useQuery({ queryKey: ["whoami"], queryFn: api.whoAmI, staleTime: 60_000 });

  const cancel = useMutation({
    mutationFn: () => api.cancelJob(id),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["job", id] }),
  });

  if (isLoading) {
    return (
      <View>
        <ViewHead kicker="Job" title="Reading job…" />
      </View>
    );
  }
  if (error || !job) {
    return (
      <View>
        <ViewHead kicker="Job" title={`No job ${id}`} lede={String(error ?? "not found")} />
        <Link to="/jobs" className="mono text-[12px]">
          ← all jobs
        </Link>
      </View>
    );
  }

  const terminal = isTerminal(job.status);
  const canCancel = principal?.scopes.includes("operator") || job.request.submittedBy === principal?.id;
  const subject = job.request.repo
    ? `${shortRepo(job.request.repo)}@${job.request.ref ?? "HEAD"}`
    : job.request.script.split("\n")[0];

  return (
    <View fill>
      <div className="mono flex items-center gap-[9px] text-[11.5px] text-[#68767b]">
        <Link to="/jobs">jobs</Link>
        <span className="text-[#2a343a]">›</span>
        {job.node && (
          <>
            <Link to={`/map?v=${encodeURIComponent(job.node)}&j=${encodeURIComponent(job.id)}`}>{job.node}</Link>
            <span className="text-[#2a343a]">›</span>
          </>
        )}
        <span>{job.id}</span>
      </div>
      <ViewHead
        kicker={`Job · ${job.request.kind} · ${job.request.pool} pool`}
        title={
          <span className="flex flex-wrap items-center gap-3">
            {subject}
            <StatusPill status={job.status} />
          </span>
        }
        lede={lede(job)}
        actions={
          !terminal && canCancel ? (
            <ConfirmButton
              label={cancel.isPending ? "canceling…" : "Cancel job"}
              confirmLabel="sure? cancel"
              onConfirm={() => cancel.mutate()}
              disabled={cancel.isPending}
            />
          ) : undefined
        }
      />

      <div className="grid min-h-0 flex-1 grid-cols-1 gap-6 xl:grid-cols-[360px_minmax(0,1fr)]">
        <aside className="flex min-h-0 flex-col gap-5 xl:overflow-auto xl:pr-2">
          <Lifecycle job={job} />

          {job.quarantined && (
            <div className="rounded-lg border px-3 py-2 text-[12.5px]" style={{ borderColor: "var(--red-line)", background: "var(--red-wash)", color: "var(--red-text)" }}>
              Quarantined: {job.failureReason ?? "lost on two different machines"}. Don't resubmit it unchanged.
            </div>
          )}

          <Section label="Request">
            <div className="facts">
              <Fact k="job id" v={job.id} />
              <Fact k="repo" v={job.request.repo ?? "—"} />
              <Fact k="ref" v={job.request.ref ?? "—"} />
              <Fact k="requester" v={job.request.requester ?? "—"} />
              <Fact k="node" v={job.node ?? "—"} />
              <Fact k="alloc" v={job.allocId ?? "—"} />
              <Fact k="timeout" v={job.request.timeout ?? "—"} />
              <Fact k="exit code" v={job.exitCode == null ? "—" : String(job.exitCode)} />
            </div>
          </Section>

          <Section label="Script">
            <pre className="m-0 overflow-auto rounded-lg border border-[#171d21] bg-[#06080a] p-3 text-[11.5px] leading-[1.6] text-[#c3d1d5]">
              {job.request.script}
            </pre>
          </Section>

          {job.request.env && Object.keys(job.request.env).length > 0 && (
            <Section label="Env">
              <div className="facts">
                {Object.entries(job.request.env).map(([k, v]) => (
                  <Fact key={k} k={k} v={v} />
                ))}
              </div>
            </Section>
          )}

          {job.artifacts && job.artifacts.length > 0 && (
            <Section label="Artifacts">
              <div className="facts">
                {job.artifacts.map((a) => (
                  <div key={a.path} className="fact" style={{ gridTemplateColumns: "minmax(0,1fr) auto" }}>
                    <a href={a.url} className="v truncate" style={{ color: "var(--accent)" }}>
                      {a.path}
                    </a>
                    <span className="k">{formatBytes(a.size)}</span>
                  </div>
                ))}
              </div>
            </Section>
          )}
        </aside>

        <section className="flex min-h-0 flex-col gap-2">
          <div className="flex items-center justify-between">
            <span className="kicker" style={{ fontSize: 10 }}>
              Live log
            </span>
            <label className="mono flex items-center gap-2 text-[11px] text-[#8c9a9e]">
              <input type="checkbox" checked={follow} onChange={(e) => setFollow(e.target.checked)} className="accent-[#7ee7c7]" />
              follow
            </label>
          </div>
          <LogViewer jobId={job.id} follow={follow} jobStatus={job.status} />
        </section>
      </div>
    </View>
  );
}

function lede(job: Job): string {
  switch (job.status) {
    case "pending":
      return job.pendingReason ?? `Waiting for a free slot in the ${job.request.pool} pool.`;
    case "running":
      return `Running on ${job.node ?? "a VM"} for ${formatJobDuration(job.startedAt, null)}, submitted ${formatRelativeAge(job.submittedAt)} by ${job.request.requester ?? "someone"}.`;
    default:
      return (
        job.failureReason ??
        `${job.status[0].toUpperCase()}${job.status.slice(1)} after ${formatJobDuration(job.startedAt, job.finishedAt)}${job.exitCode != null ? ` with exit ${job.exitCode}` : ""}.`
      );
  }
}

function Section({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div className="flex flex-col gap-2">
      <span className="kicker" style={{ fontSize: 10 }}>
        {label}
      </span>
      {children}
    </div>
  );
}

function Fact({ k, v }: { k: string; v: string }) {
  return (
    <div className="fact">
      <span className="k">{k}</span>
      <span className="v">{v}</span>
    </div>
  );
}

/** The Argos stage rail: submitted → placed → each attempt → running → finished. */
function Lifecycle({ job }: { job: Job }) {
  const terminal = isTerminal(job.status);
  const rows: { name: string; tone: Tone | "off"; what?: string; note?: string; live?: boolean }[] = [
    { name: "submitted", tone: "teal", what: `by ${job.request.requester ?? "—"}`, note: formatRelativeAge(job.submittedAt) },
    {
      name: "placed",
      tone: job.node || job.startedAt ? "teal" : job.status === "pending" ? "blue" : "off",
      live: job.status === "pending",
      what: job.node ? `on ${job.node}` : job.pendingReason ?? "Nomad is looking for room",
    },
    ...(job.attempts ?? []).map((a, i) => ({
      name: `attempt ${i + 1} · ${a.outcome}`,
      tone: (a.outcome === "running" ? "teal" : toneOf(a.outcome === "exited" ? (a.exitCode === 0 ? "success" : "failed") : a.outcome)) as Tone,
      live: a.outcome === "running",
      what: a.reason ?? `${a.vm}${a.worker ? ` on ${a.worker}` : ""}`,
      note: a.finishedAt ? formatRelativeAge(a.finishedAt) : formatRelativeAge(a.startedAt),
    })),
    {
      name: "running",
      tone: job.startedAt ? "teal" : "off",
      live: job.status === "running",
      note: job.startedAt ? formatJobDuration(job.startedAt, job.finishedAt) : undefined,
    },
    {
      name: terminal ? job.status : "finished",
      tone: terminal ? toneOf(job.status) : "off",
      what: terminal && job.exitCode != null ? `exit ${job.exitCode}` : undefined,
      note: job.finishedAt ? formatRelativeAge(job.finishedAt) : undefined,
    },
  ];
  return (
    <Section label="Lifecycle">
      <div className="stages">
        {rows.map((s, i) => {
          const off = s.tone === "off";
          return (
            <div key={i} className="stage">
              <span className="spine">
                <span className={s.live ? "dot live" : ""} style={{ background: off ? "#1e262b" : TONES[s.tone as Tone].dot }} />
              </span>
              <span className="flex min-w-0 flex-col gap-0.5">
                <span className="name" style={{ color: off ? "#4f5c61" : s.live ? "var(--accent)" : "#c3d1d5" }}>
                  {s.name}
                </span>
                {s.what && <span className="what">{s.what}</span>}
              </span>
              <span className="note">{s.note ?? ""}</span>
            </div>
          );
        })}
      </div>
    </Section>
  );
}
