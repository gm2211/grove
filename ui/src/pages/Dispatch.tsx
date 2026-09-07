import { useMemo, useState } from "react";
import { useMutation, useQuery } from "@tanstack/react-query";
import { useNavigate } from "react-router";
import { api } from "../api/client";
import { Button } from "../components/Button";
import { parseDurationToNanos } from "../lib/format";
import type { JobKind } from "../api/types";

interface EnvRow {
  key: string;
  value: string;
}

export function DispatchPage() {
  const navigate = useNavigate();
  const { data: fleet } = useQuery({ queryKey: ["fleet"], queryFn: api.getFleet, refetchInterval: 5000 });

  const pools = useMemo(() => {
    const set = new Set<string>();
    for (const vm of fleet?.vms ?? []) if (vm.labels?.pool) set.add(vm.labels.pool);
    for (const n of fleet?.nodes ?? []) if (n.labels?.pool) set.add(n.labels.pool);
    return Array.from(set).sort();
  }, [fleet]);

  const [kind, setKind] = useState<JobKind>("build");
  const [pool, setPool] = useState("");
  const [repo, setRepo] = useState("");
  const [ref, setRef] = useState("main");
  const [script, setScript] = useState("make test && make build");
  const [timeout, setTimeoutStr] = useState("1h");
  const [env, setEnv] = useState<EnvRow[]>([{ key: "", value: "" }]);
  const [formError, setFormError] = useState<string | null>(null);

  const effectivePool = pool || pools[0] || "";

  const submit = useMutation({
    mutationFn: api.submitJob,
    onSuccess: (job) => navigate(`/jobs/${job.id}`),
  });

  function handleSubmit(e: React.FormEvent) {
    e.preventDefault();
    setFormError(null);
    if (!effectivePool) {
      setFormError("pool is required");
      return;
    }
    if (!script.trim()) {
      setFormError("script is required");
      return;
    }
    const envObj: Record<string, string> = {};
    for (const row of env) {
      if (row.key.trim()) envObj[row.key.trim()] = row.value;
    }
    submit.mutate({
      kind,
      pool: effectivePool,
      repo: kind === "shell" ? undefined : repo || undefined,
      ref: kind === "shell" ? undefined : ref || undefined,
      script,
      env: Object.keys(envObj).length > 0 ? envObj : undefined,
      timeout: parseDurationToNanos(timeout),
      requester: "ui",
    });
  }

  return (
    <div className="max-w-2xl">
      <h1 className="mb-4 text-lg font-semibold">Dispatch a job</h1>
      <form onSubmit={handleSubmit} className="flex flex-col gap-4">
        <div className="grid grid-cols-2 gap-4">
          <label className="flex flex-col gap-1 text-xs">
            <span style={{ color: "var(--fg-faint)" }}>kind</span>
            <select
              className="rounded border px-2 py-1.5 text-sm"
              style={{ borderColor: "var(--border)", background: "var(--bg-elevated)" }}
              value={kind}
              onChange={(e) => setKind(e.target.value as JobKind)}
            >
              <option value="build">build</option>
              <option value="agent">agent</option>
              <option value="shell">shell</option>
            </select>
          </label>
          <label className="flex flex-col gap-1 text-xs">
            <span style={{ color: "var(--fg-faint)" }}>pool</span>
            <select
              className="rounded border px-2 py-1.5 text-sm"
              style={{ borderColor: "var(--border)", background: "var(--bg-elevated)" }}
              value={effectivePool}
              onChange={(e) => setPool(e.target.value)}
            >
              {pools.length === 0 && <option value="">no pools observed</option>}
              {pools.map((p) => (
                <option key={p} value={p}>
                  {p}
                </option>
              ))}
            </select>
          </label>
        </div>

        {kind !== "shell" && (
          <div className="grid grid-cols-2 gap-4">
            <label className="flex flex-col gap-1 text-xs">
              <span style={{ color: "var(--fg-faint)" }}>repo</span>
              <input
                className="mono rounded border px-2 py-1.5 text-sm"
                style={{ borderColor: "var(--border)", background: "var(--bg-elevated)" }}
                placeholder="github.com/gm2211/grove"
                value={repo}
                onChange={(e) => setRepo(e.target.value)}
              />
            </label>
            <label className="flex flex-col gap-1 text-xs">
              <span style={{ color: "var(--fg-faint)" }}>ref</span>
              <input
                className="mono rounded border px-2 py-1.5 text-sm"
                style={{ borderColor: "var(--border)", background: "var(--bg-elevated)" }}
                placeholder="main"
                value={ref}
                onChange={(e) => setRef(e.target.value)}
              />
            </label>
          </div>
        )}

        <label className="flex flex-col gap-1 text-xs">
          <span style={{ color: "var(--fg-faint)" }}>script</span>
          <textarea
            className="mono h-32 resize-y rounded border px-2 py-1.5 text-sm"
            style={{ borderColor: "var(--border)", background: "var(--bg-elevated)" }}
            value={script}
            onChange={(e) => setScript(e.target.value)}
          />
        </label>

        <div className="flex flex-col gap-1 text-xs">
          <span style={{ color: "var(--fg-faint)" }}>env</span>
          {env.map((row, i) => (
            <div key={i} className="flex gap-2">
              <input
                className="mono flex-1 rounded border px-2 py-1 text-sm"
                style={{ borderColor: "var(--border)", background: "var(--bg-elevated)" }}
                placeholder="KEY"
                value={row.key}
                onChange={(e) =>
                  setEnv((rows) => rows.map((r, idx) => (idx === i ? { ...r, key: e.target.value } : r)))
                }
              />
              <input
                className="mono flex-1 rounded border px-2 py-1 text-sm"
                style={{ borderColor: "var(--border)", background: "var(--bg-elevated)" }}
                placeholder="value"
                value={row.value}
                onChange={(e) =>
                  setEnv((rows) => rows.map((r, idx) => (idx === i ? { ...r, value: e.target.value } : r)))
                }
              />
              <Button
                type="button"
                onClick={() => setEnv((rows) => rows.filter((_, idx) => idx !== i))}
              >
                ✕
              </Button>
            </div>
          ))}
          <Button type="button" className="self-start" onClick={() => setEnv((rows) => [...rows, { key: "", value: "" }])}>
            + add var
          </Button>
        </div>

        <label className="flex flex-col gap-1 text-xs">
          <span style={{ color: "var(--fg-faint)" }}>timeout (e.g. 30m, 1h)</span>
          <input
            className="mono w-32 rounded border px-2 py-1.5 text-sm"
            style={{ borderColor: "var(--border)", background: "var(--bg-elevated)" }}
            value={timeout}
            onChange={(e) => setTimeoutStr(e.target.value)}
          />
        </label>

        {formError && <div style={{ color: "var(--status-failed)" }}>{formError}</div>}
        {submit.isError && <div style={{ color: "var(--status-failed)" }}>{String(submit.error)}</div>}

        <Button type="submit" variant="primary" disabled={submit.isPending} className="self-start">
          {submit.isPending ? "Dispatching…" : "Dispatch"}
        </Button>
      </form>
    </div>
  );
}
