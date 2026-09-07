import { useEffect, useRef, useState } from "react";
import { streamJobLogs } from "../api/client";
import { Button } from "./Button";

export function LogViewer({ jobId, follow }: { jobId: string; follow: boolean }) {
  const [lines, setLines] = useState<string[]>([]);
  const [paused, setPaused] = useState(false);
  const [connected, setConnected] = useState(false);
  const containerRef = useRef<HTMLDivElement>(null);
  const pausedRef = useRef(paused);
  pausedRef.current = paused;

  useEffect(() => {
    setLines([]);
    setConnected(false);
    if (!follow) return;
    const stop = streamJobLogs(jobId, {
      onOpen: () => setConnected(true),
      onError: () => setConnected(false),
      onLine: (line) => setLines((prev) => [...prev, line]),
    });
    return stop;
  }, [jobId, follow]);

  useEffect(() => {
    if (paused) return;
    const el = containerRef.current;
    if (el) el.scrollTop = el.scrollHeight;
  }, [lines, paused]);

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
          {follow ? (connected ? "streaming" : "connecting…") : "not following"} · {lines.length} lines
        </span>
        <Button onClick={() => setPaused((p) => !p)}>{paused ? "Resume autoscroll" : "Pause autoscroll"}</Button>
      </div>
      <div ref={containerRef} className="mono h-96 overflow-auto px-3 py-2 text-xs leading-relaxed">
        {lines.length === 0 && (
          <div className="italic" style={{ color: "var(--fg-faint)" }}>
            no output yet
          </div>
        )}
        {lines.map((line, i) => (
          <div key={i} className="whitespace-pre-wrap">
            {line}
          </div>
        ))}
      </div>
    </div>
  );
}
