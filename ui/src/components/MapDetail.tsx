import type { ReactNode } from "react";
import { Link } from "react-router";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api } from "../api/client";
import type { Job } from "../api/types";
import { TONES, toneOf, type Tone } from "./StatusPill";
import { ConfirmButton } from "./ConfirmButton";
import { LogLines, useJobLog } from "./LogViewer";
import { isTerminal, workerStatus, type FleetModel, type VMView, type WorkerView } from "../lib/fleetModel";
import { formatClock, formatJobDuration, formatRelativeAge, formatShort, shortRepo } from "../lib/format";
import type { Selection } from "../pages/Map";

/** The right-hand column: whatever is selected, what it is doing, and what you can do to it. */
export function MapDetail({ model, sel }: { model: FleetModel; sel: Selection }) {
  if (sel.job) return <JobDetail job={sel.job} vm={sel.vm} />;
  if (sel.vm && sel.worker) return <VMDetail v={sel.vm} worker={sel.worker} />;
  if (sel.worker) return <WorkerDetail w={sel.worker} />;
  return <Nothing model={model} />;
}

function usePrincipal() {
  return useQuery({ queryKey: ["whoami"], queryFn: api.whoAmI, staleTime: 60_000 }).data;
}

function Head({ kind, state, tone, name, what }: { kind: string; state: string; tone: Tone; name: string; what: ReactNode }) {
  const t = TONES[tone];
  return (
    <>
      <div className="flex items-baseline gap-[9px]">
        <span className="kicker" style={{ fontSize: 10 }}>
          {kind}
        </span>
        <span className="chip" style={{ borderColor: t.line, color: t.text, background: t.wash }}>
          {state}
        </span>
      </div>
      <span className="mono text-[14px] break-all text-[#eaf4f4]">{name}</span>
      <span className="text-[13px] leading-[1.6] text-[#9fb0b5]">{what}</span>
    </>
  );
}

function Facts({ rows }: { rows: [string, ReactNode][] }) {
  return (
    <div className="facts">
      {rows.map(([k, v]) => (
        <div key={k} className="fact">
          <span className="k">{k}</span>
          <span className="v">{v}</span>
        </div>
      ))}
    </div>
  );
}

interface StageRow {
  name: string;
  tone: Tone | "off";
  what?: string;
  note?: string;
  live?: boolean;
}

function Stages({ label, rows }: { label: string; rows: StageRow[] }) {
  return (
    <div className="flex flex-col gap-2">
      <span className="kicker" style={{ fontSize: 10 }}>
        {label}
      </span>
      <div className="stages">
        {rows.map((s, i) => {
          const off = s.tone === "off";
          const dot = off ? "#1e262b" : TONES[s.tone as Tone].dot;
          return (
            <div key={i} className="stage">
              <span className="spine">
                <span className={s.live ? "dot live" : ""} style={{ background: dot }} />
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
    </div>
  );
}

function WorkerDetail({ w }: { w: WorkerView }) {
  const qc = useQueryClient();
  const principal = usePrincipal();
  const operator = principal?.scopes.includes("operator") ?? false;
  // The holder of worker:snooze:<name> may snooze and wake this worker, like an operator.
  const canSnooze = operator || (principal?.scopes.includes(`worker:snooze:${w.name}`) ?? false);
  const invalidate = () => qc.invalidateQueries({ queryKey: ["fleet"] });
  const pause = useMutation({ mutationFn: () => api.pauseWorker(w.name), onSuccess: invalidate });
  const resume = useMutation({ mutationFn: () => api.resumeWorker(w.name), onSuccess: invalidate });
  const snooze = useMutation({ mutationFn: () => api.snoozeWorker(w.name, "2h"), onSuccess: invalidate });
  const wake = useMutation({ mutationFn: () => api.wakeWorker(w.name), onSuccess: invalidate });

  const status = workerStatus(w);
  const e = w.worker;
  const reasons = e?.pauseReasons ?? [];
  const what = {
    online: `Taking work. ${w.used} of ${w.slots} VM slots in use, ${w.running} job${w.running === 1 ? "" : "s"} running across its VMs.`,
    paused: `Paused: Orchard places no new VMs here${reasons.length ? ` (${reasons.join(", ")})` : ""}. Running VMs keep their jobs.`,
    snoozed: `Snoozed until ${formatClock(e?.snoozedUntil)}${e?.snoozeMode === "now" ? ", jobs moved off" : ", running jobs finish first"}. Queued jobs wait for another Mac or for the snooze to end.`,
    offline: "Not checking in with Orchard. Its VMs can't take jobs until it is back.",
    unknown: "Orchard doesn't list this Mac, but VMs still name it as their host.",
  }[status];

  const busy = pause.isPending || resume.isPending || snooze.isPending || wake.isPending;
  const err = pause.error ?? resume.error ?? snooze.error ?? wake.error;

  return (
    <div className="detail">
      <Head kind="Worker" state={status} tone={toneOf(status)} name={w.name} what={what} />
      <Stages
        label="VMs on this Mac"
        rows={
          w.vms.length
            ? w.vms.map((v) => ({
                name: v.vm.name,
                tone: toneOf(v.vm.status),
                live: v.running.length > 0,
                note: v.running.length ? `${v.running.length} running` : "idle",
              }))
            : [{ name: "no VMs yet", tone: "off" as const, note: "" }]
        }
      />
      <Facts
        rows={[
          ["arch", e?.arch ?? "—"],
          ["cores", e?.capacity?.cpu ?? "—"],
          ["memory", e?.capacity?.memoryMiB ? `${Math.round(e.capacity.memoryMiB / 1024)} GB` : "—"],
          ["vm slots", `${w.used} of ${w.slots}`],
          ["paused for", reasons.length ? reasons.join(", ") : "—"],
          ...(e?.snoozedUntil ? ([["snoozed till", `${formatClock(e.snoozedUntil)} · ${e.snoozeMode ?? "drain"}`]] as [string, string][]) : []),
        ]}
      />
      <div className="flex flex-wrap gap-2">
        {status === "snoozed" && canSnooze && (
          <button className="act act-rec" disabled={busy} onClick={() => wake.mutate()}>
            {wake.isPending ? "waking…" : "Wake"}
          </button>
        )}
        {status !== "snoozed" && operator && e && (e.cordoned ? (
          <button className="act act-rec" disabled={busy} onClick={() => resume.mutate()}>
            {resume.isPending ? "resuming…" : "Resume worker"}
          </button>
        ) : (
          <button className="act" disabled={busy} onClick={() => pause.mutate()}>
            {pause.isPending ? "pausing…" : "Pause worker"}
          </button>
        ))}
        {status === "online" && canSnooze && (
          <button className="act" disabled={busy} onClick={() => snooze.mutate()}>
            {snooze.isPending ? "snoozing…" : "Snooze 2h"}
          </button>
        )}
      </div>
      {err && <span className="mono text-[11px] text-[#eda3a3]">{String(err)}</span>}
    </div>
  );
}

function VMDetail({ v, worker }: { v: VMView; worker: WorkerView }) {
  const qc = useQueryClient();
  const principal = usePrincipal();
  const operator = principal?.scopes.includes("operator") ?? false;
  const recycle = useMutation({
    mutationFn: () => api.recycleVM(v.vm.name),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["fleet"] }),
  });
  const raw = (v.vm.raw ?? {}) as Record<string, unknown>;
  const nodeReady = v.node?.status === "ready";
  const ttlLeft = v.ttlLeftMs !== undefined ? formatShort(v.ttlLeftMs) : undefined;
  const what = [
    `A ${v.vm.pool ?? "?"} VM on ${worker.name}.`,
    v.node ? `Its Nomad node is ${v.node.status}${v.running.length ? ` and running ${v.running.length} job${v.running.length === 1 ? "" : "s"}` : ""}.` : "Its Nomad node hasn't registered yet.",
    ttlLeft ? `grove replaces it in ${ttlLeft}, when its TTL runs out.` : "",
  ].join(" ");

  return (
    <div className="detail">
      <Head kind="VM" state={v.vm.status} tone={toneOf(v.vm.status)} name={v.vm.name} what={what} />
      <Stages
        label="VM lifecycle"
        rows={[
          { name: "created by orchard", tone: "teal", what: "Tart clone of the pool's image.", note: v.createdAt ? formatRelativeAge(new Date(v.createdAt).toISOString()) : "" },
          { name: "nomad node joins", tone: v.node ? toneOf(v.node.status) : "off", what: "Enrolled with its own mTLS cert and pool.", note: v.node?.status ?? "waiting" },
          { name: "running jobs", tone: v.running.length ? "teal" : nodeReady ? "grey" : "off", live: v.running.length > 0, what: "Nomad packs this pool's jobs in.", note: `${v.running.length} now · ${v.jobs.length} total` },
          { name: "drain & recycle", tone: "off", what: "Drained, deleted, replaced from fleet.yaml.", note: ttlLeft ? `in ${ttlLeft}` : "no ttl" },
        ]}
      />
      <Facts
        rows={[
          ["worker", worker.name],
          ["pool", v.vm.pool ?? "—"],
          ["image", typeof raw.image === "string" ? raw.image : "—"],
          ["size", `${v.vm.capacity?.cpu ?? "?"} vCPU · ${v.vm.capacity?.memoryMiB ? Math.round(v.vm.capacity.memoryMiB / 1024) : "?"} GB`],
          ["node", v.node ? `${v.node.name} · ${v.node.status}` : "—"],
          ["restarts", typeof raw.restartCount === "number" ? raw.restartCount : "—"],
        ]}
      />
      {operator && (
        <div className="flex flex-wrap gap-2">
          <ConfirmButton
            label={recycle.isPending ? "recycling…" : "Recycle VM"}
            confirmLabel="sure? recycle"
            disabled={recycle.isPending}
            onConfirm={() => recycle.mutate()}
          />
        </div>
      )}
      {recycle.error && <span className="mono text-[11px] text-[#eda3a3]">{String(recycle.error)}</span>}
    </div>
  );
}

function JobDetail({ job, vm }: { job: Job; vm?: VMView }) {
  const qc = useQueryClient();
  const principal = usePrincipal();
  const canCancel = principal?.scopes.includes("operator") || job.request.submittedBy === principal?.id;
  const cancel = useMutation({
    mutationFn: () => api.cancelJob(job.id),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["jobs"] });
      qc.invalidateQueries({ queryKey: ["job", job.id] });
    },
  });
  const terminal = isTerminal(job.status);
  const showLog = job.status === "running";
  const { lines } = useJobLog(job.id, job.status, showLog);
  const tail = lines.slice(-6);

  const where = job.node ?? vm?.vm.name;
  const what =
    job.status === "pending"
      ? job.pendingReason ?? `Waiting for a free slot in the ${job.request.pool} pool.`
      : job.status === "running"
        ? `${job.request.kind} job running on ${where ?? "a VM"} for ${formatJobDuration(job.startedAt, null)}.`
        : job.quarantined
          ? `Quarantined: ${job.failureReason ?? "lost on two different machines"}.`
          : job.failureReason ?? `${job.status} after ${formatJobDuration(job.startedAt, job.finishedAt)}${job.exitCode != null ? `, exit ${job.exitCode}` : ""}.`;

  const finishedTone: Tone | "off" = terminal ? toneOf(job.status) : "off";
  const attempts = job.attempts ?? [];

  return (
    <div className="detail">
      <Head kind="Job" state={job.status} tone={toneOf(job.status)} name={job.id} what={what} />
      <Stages
        label="Job lifecycle"
        rows={[
          { name: "submitted", tone: "teal", what: `by ${job.request.requester ?? "—"}`, note: formatRelativeAge(job.submittedAt) },
          { name: "placed", tone: job.node ? "teal" : job.status === "pending" ? "blue" : "off", live: job.status === "pending", what: job.node ? `on ${job.node}` : "Nomad is looking for room", note: job.node ? "" : "waiting" },
          ...attempts.slice(0, -1).map((a, i) => ({
            name: `attempt ${i + 1} · ${a.outcome}`,
            tone: toneOf(a.outcome === "exited" ? "failed" : a.outcome) as Tone,
            what: a.reason ?? `on ${a.vm}`,
            note: a.finishedAt ? formatRelativeAge(a.finishedAt) : "",
          })),
          { name: "running", tone: job.startedAt ? "teal" : "off", live: job.status === "running", what: shortRepo(job.request.repo) ? `${shortRepo(job.request.repo)}@${job.request.ref ?? "HEAD"}` : job.request.script.split("\n")[0], note: job.startedAt ? formatJobDuration(job.startedAt, job.finishedAt) : "" },
          { name: terminal ? job.status : "finished", tone: finishedTone, what: terminal && job.exitCode != null ? `exit ${job.exitCode}` : undefined, note: job.finishedAt ? formatRelativeAge(job.finishedAt) : "" },
        ]}
      />
      {showLog && (
        <div className="logpane tail">
          <div className="logpane-head">
            <span className="dot live" style={{ background: "var(--accent)" }} />
            <span className="kicker" style={{ fontSize: 10 }}>
              live log
            </span>
            <span className="mono text-[10px] text-[#46545a]">{lines.length} lines</span>
            <Link to={`/jobs/${encodeURIComponent(job.id)}`} className="mono ml-auto text-[10px] tracking-[0.08em]">
              FULL LOG ▸
            </Link>
          </div>
          <div className="py-1.5">
            <LogLines lines={tail} empty="connecting…" />
          </div>
        </div>
      )}
      <Facts
        rows={[
          ["kind", job.request.kind],
          ["pool", job.request.pool],
          ["repo", job.request.repo ? shortRepo(job.request.repo) : "—"],
          ["ref", job.request.ref ?? "—"],
          ["timeout", job.request.timeout ?? "—"],
        ]}
      />
      <div className="flex flex-wrap gap-2">
        <Link to={`/jobs/${encodeURIComponent(job.id)}`} className="act act-rec">
          Open job
        </Link>
        {!terminal && canCancel && (
          <ConfirmButton
            label={cancel.isPending ? "canceling…" : "Cancel"}
            confirmLabel="sure? cancel"
            disabled={cancel.isPending}
            onConfirm={() => cancel.mutate()}
          />
        )}
      </div>
    </div>
  );
}

function Nothing({ model }: { model: FleetModel }) {
  const online = model.workers.filter((w) => workerStatus(w) === "online").length;
  const slots = model.workers.reduce((n, w) => n + w.slots, 0);
  const used = model.workers.reduce((n, w) => n + w.used, 0);
  return (
    <div className="detail">
      <span className="kicker" style={{ fontSize: 10 }}>
        Nothing selected
      </span>
      <span className="mono text-[14px] text-[#eaf4f4]">—</span>
      <span className="text-[13px] leading-[1.6] text-[#9fb0b5]">
        Select a worker to see its VMs, a VM to see the jobs Nomad packed into it, or a queued job to see why it is waiting.
      </span>
      <Facts
        rows={[
          ["workers", `${online} of ${model.workers.length} taking work`],
          ["vm slots", `${used} of ${slots} in use`],
          ["running", model.runningJobs],
          ["queued", model.queue.length],
        ]}
      />
    </div>
  );
}
