import { useEffect, useRef, useState } from "react";
import { streamJobLogs, type LogLine } from "../api/client";

const TERMINAL_STATUSES = ["success", "failed", "canceled", "lost"];

/**
 * Terminal-style log pane. Always dark, in both themes, so output reads like the shell it came
 * from; stderr lines are tinted and marked in the gutter.
 */
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
  const dotColor = follow && connected ? "var(--status-success)" : "var(--log-faint)";

  return (
    <div
      className="flex flex-col overflow-hidden rounded-xl border"
      style={{ borderColor: "var(--border)", background: "var(--log-bg)", color: "var(--log-fg)" }}
    >
      <div
        className="flex items-center justify-between gap-3 border-b px-4 py-2 text-xs"
        style={{ borderColor: "rgb(255 255 255 / 0.07)", color: "var(--log-faint)" }}
      >
        <span className="flex items-center gap-2">
          <span className="h-1.5 w-1.5 rounded-full" style={{ background: dotColor }} />
          {statusLabel}
          <span>·</span>
          <span className="tabular">{lines.length} lines</span>
        </span>
        <button
          type="button"
          onClick={() => setPaused((p) => !p)}
          className="rounded-md px-2 py-1 font-medium transition-colors hover:bg-white/10"
          style={{ color: "var(--log-fg)" }}
        >
          {paused ? "Resume autoscroll" : "Pause autoscroll"}
        </button>
      </div>
      <div ref={containerRef} className="mono h-[28rem] overflow-auto py-2 text-xs leading-relaxed">
        {lines.length === 0 && (
          <div className="px-4 italic" style={{ color: "var(--log-faint)" }}>
            no output yet
          </div>
        )}
        {lines.map((entry, i) => (
          <div
            key={i}
            className="flex gap-4 px-4 hover:bg-white/5"
            style={entry.stream === "stderr" ? { color: "#f5b35c", background: "rgb(245 179 92 / 0.06)" } : undefined}
          >
            <span className="tabular w-8 shrink-0 text-right select-none" style={{ color: "var(--log-faint)" }}>
              {i + 1}
            </span>
            <span className="min-w-0 whitespace-pre-wrap break-words">{entry.line}</span>
          </div>
        ))}
      </div>
    </div>
  );
}
