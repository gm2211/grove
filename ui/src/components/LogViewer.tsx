import { useEffect, useRef, useState } from "react";
import { streamJobLogs, type LogLine } from "../api/client";
import { Button } from "./Button";

const TERMINAL_STATUSES = ["success", "failed", "canceled", "lost"];

export function LogViewer({ jobId, follow, jobStatus }: { jobId: string; follow: boolean; jobStatus: string }) {
  const [lines, setLines] = useState<LogLine[]>([]);
  const [paused, setPaused] = useState(false);
  const [connected, setConnected] = useState(false);
  const containerRef = useRef<HTMLDivElement>(null);
  const pausedRef = useRef(paused);
  pausedRef.current = paused;
  const statusRef = useRef(jobStatus);
  statusRef.current = jobStatus;

  useEffect(() => {
    setLines([]);
    setConnected(false);
    if (!follow) return;
    const stop = streamJobLogs(jobId, {
      onOpen: () => setConnected(true),
      onError: () => setConnected(false),
      onLine: (entry) => setLines((prev) => [...prev, entry]),
      isTerminal: () => TERMINAL_STATUSES.includes(statusRef.current),
    });
    return stop;
  }, [jobId, follow]);

  useEffect(() => {
    if (paused) return;
    const el = containerRef.current;
    if (el) el.scrollTop = el.scrollHeight;
  }, [lines, paused]);

  const statusLabel = follow
    ? connected
      ? "streaming"
      : jobStatus === "pending"
        ? "waiting for allocation…"
        : "connecting…"
    : "not following";

  return (
    <div
      className="flex flex-col overflow-hidden rounded-lg border"
      style={{ borderColor: "var(--border)", background: "var(--bg-inset)" }}
    >
      <div
        className="flex items-center justify-between border-b px-3 py-1.5 text-xs"
        style={{ borderColor: "var(--border)", color: "var(--fg-muted)" }}
      >
        <span>
          {statusLabel} · {lines.length} lines
        </span>
        <Button onClick={() => setPaused((p) => !p)}>{paused ? "Resume autoscroll" : "Pause autoscroll"}</Button>
      </div>
      <div ref={containerRef} className="mono h-96 overflow-auto px-3 py-2 text-xs leading-relaxed">
        {lines.length === 0 && (
          <div className="italic" style={{ color: "var(--fg-faint)" }}>
            no output yet
          </div>
        )}
        {lines.map((entry, i) => (
          <div
            key={i}
            className="whitespace-pre-wrap"
            style={entry.stream === "stderr" ? { color: "var(--status-lost)" } : undefined}
          >
            {entry.stream === "stderr" && <span style={{ opacity: 0.7 }}>[stderr] </span>}
            {entry.line}
          </div>
        ))}
      </div>
    </div>
  );
}
