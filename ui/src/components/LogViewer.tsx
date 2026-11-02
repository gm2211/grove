import { useEffect, useRef, useState } from "react";
import { streamJobLogs, type LogLine } from "../api/client";
import { isTerminal } from "../lib/fleetModel";

/** Streams a job's log while `enabled`, reconnecting until the job is terminal. */
export function useJobLog(jobId: string, jobStatus: string, enabled = true) {
  const [lines, setLines] = useState<LogLine[]>([]);
  const [connected, setConnected] = useState(false);
  const statusRef = useRef(jobStatus);
  statusRef.current = jobStatus;

  useEffect(() => {
    setLines([]);
    setConnected(false);
    if (!enabled) return;
    return streamJobLogs(jobId, {
      onOpen: () => setConnected(true),
      onError: () => setConnected(false),
      onLine: (entry) => setLines((prev) => [...prev, entry]),
      isTerminal: () => isTerminal(statusRef.current),
    });
  }, [jobId, enabled]);

  return { lines, connected };
}

function clock(ts: string): string {
  const d = new Date(ts);
  return Number.isNaN(d.getTime()) ? "" : d.toLocaleTimeString([], { hour12: false });
}

export function LogLines({ lines, empty = "no output yet" }: { lines: LogLine[]; empty?: string }) {
  if (lines.length === 0) {
    return <div className="mono px-3 py-2 text-[11px] italic text-[#46545a]">{empty}</div>;
  }
  return (
    <>
      {lines.map((entry, i) => (
        <div key={i} className={`logline ${entry.stream === "stderr" ? "err" : ""}`}>
          <span className="t">{clock(entry.ts)}</span>
          <span className="msg">{entry.line}</span>
        </div>
      ))}
    </>
  );
}

export function LogViewer({ jobId, follow, jobStatus }: { jobId: string; follow: boolean; jobStatus: string }) {
  const { lines, connected } = useJobLog(jobId, jobStatus, follow);
  const [paused, setPaused] = useState(false);
  const containerRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    if (paused) return;
    const el = containerRef.current;
    if (el) el.scrollTop = el.scrollHeight;
  }, [lines, paused]);

  const statusLabel = follow
    ? connected
      ? "streaming"
      : jobStatus === "pending"
        ? "waiting for a slot"
        : "connecting"
    : "not following";

  return (
    <div className="logpane min-h-0 flex-1">
      <div className="logpane-head">
        <span className={`dot ${connected ? "live" : ""}`} style={{ background: connected ? "var(--accent)" : "var(--grey)" }} />
        <span className="kicker" style={{ fontSize: 10 }}>
          {statusLabel}
        </span>
        <span className="mono text-[10.5px] text-[#46545a]">{lines.length} lines</span>
        <button className="act sm ml-auto" onClick={() => setPaused((p) => !p)}>
          {paused ? "resume autoscroll" : "pause autoscroll"}
        </button>
      </div>
      <div ref={containerRef} className="h-[420px] overflow-auto py-2 xl:h-auto xl:min-h-[420px] xl:flex-1">
        <LogLines lines={lines} />
      </div>
    </div>
  );
}
