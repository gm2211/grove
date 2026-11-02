import { useMemo, useState } from "react";
import { useMutation, useQuery } from "@tanstack/react-query";
import { useNavigate } from "react-router";
import { api } from "../api/client";
import { Button } from "../components/Button";
import { View, ViewHead } from "../components/Layout";
import type { JobKind } from "../api/types";

// Same shape grove's server accepts via time.ParseDuration ("30m", "2h", "90s", "1h30m", ...) —
// checked client-side just to catch an obviously-malformed value before submitting; the server is
// the source of truth (see internal/dispatch/duration.go).
const DURATION_RE = /^\d+(?:\.\d+)?(ns|us|µs|ms|s|m|h)$/;

interface EnvRow {
  key: string;
  value: string;
}

export function DispatchPage() {
  const navigate = useNavigate();
  const { data: fleet } = useQuery({ queryKey: ["fleet"], queryFn: api.getFleet, refetchInterval: 5000 });
	const { data: principal } = useQuery({ queryKey: ["whoami"], queryFn: api.whoAmI, staleTime: 60_000 });
	// Raw shell jobs need operator or the explicit dispatch:shell scope; plain "dispatch" is build/agent only.
	const canShell = principal?.scopes.some((s) => s === "operator" || s === "dispatch:shell") ?? false;

  const pools = useMemo(() => {
    const set = new Set<string>();
    for (const vm of fleet?.vms ?? []) if (vm.pool) set.add(vm.pool);
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
    const trimmedTimeout = timeout.trim();
    if (trimmedTimeout && !DURATION_RE.test(trimmedTimeout)) {
      setFormError(`timeout must look like "30m" or "2h" (got ${JSON.stringify(trimmedTimeout)})`);
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
      // Sent as the raw text ("30m", "2h", ...) — never converted to nanoseconds. The server
      // parses this with the same rules as Go's time.ParseDuration; see
      // internal/dispatch/duration.go.
      timeout: trimmedTimeout || undefined,
      requester: "ui",
    });
  }

  return (
    <View>
      <ViewHead
        kicker="Dispatch · one job into one pool"
        title="Send a job to the fleet"
        lede="It waits in its pool's queue until Nomad finds room in a VM, then streams its log here. Builds and agent sessions clone the repo first; shell jobs just run the script."
      />
      <form onSubmit={handleSubmit} className="panel flex max-w-3xl flex-col gap-5 p-5 sm:p-6">
        <div className="grid grid-cols-1 gap-5 sm:grid-cols-2">
          <div className="field">
            <span className="label">kind</span>
            <div className="seg self-start">
              {(["build", "agent", ...(canShell ? ["shell"] : [])] as JobKind[]).map((k) => (
                <button
                  key={k}
                  type="button"
                  className="systab"
                  data-on={kind === k ? "1" : "0"}
                  data-kind={k}
                  onClick={() => setKind(k)}
                >
                  {k}
                </button>
              ))}
            </div>
          </div>
          <label className="field">
            <span className="label">pool</span>
            <select className="input" value={effectivePool} onChange={(e) => setPool(e.target.value)} name="pool">
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
          <div className="grid grid-cols-1 gap-5 sm:grid-cols-[minmax(0,2fr)_minmax(0,1fr)]">
            <label className="field">
              <span className="label">repo</span>
              <input
                className="input"
                name="repo"
                placeholder="https://github.com/gm2211/grove"
                value={repo}
                onChange={(e) => setRepo(e.target.value)}
              />
            </label>
            <label className="field">
              <span className="label">ref</span>
              <input className="input" name="ref" placeholder="main" value={ref} onChange={(e) => setRef(e.target.value)} />
            </label>
          </div>
        )}

        <label className="field">
          <span className="label">script</span>
          <textarea className="input h-32" name="script" value={script} onChange={(e) => setScript(e.target.value)} />
        </label>

        <div className="field">
          <span className="label">env</span>
          {env.map((row, i) => (
            <div key={i} className="flex gap-2">
              <input
                className="input flex-1"
                placeholder="KEY"
                value={row.key}
                onChange={(e) =>
                  setEnv((rows) => rows.map((r, idx) => (idx === i ? { ...r, key: e.target.value } : r)))
                }
              />
              <input
                className="input flex-1"
                placeholder="value"
                value={row.value}
                onChange={(e) =>
                  setEnv((rows) => rows.map((r, idx) => (idx === i ? { ...r, value: e.target.value } : r)))
                }
              />
              <Button type="button" size="icon" className="h-[38px] w-[38px]" title="remove" onClick={() => setEnv((rows) => rows.filter((_, idx) => idx !== i))}>
                ✕
              </Button>
            </div>
          ))}
          <Button type="button" size="sm" className="self-start" onClick={() => setEnv((rows) => [...rows, { key: "", value: "" }])}>
            + add var
          </Button>
        </div>

        <label className="field">
          <span className="label">timeout</span>
          <input className="input w-36" name="timeout" value={timeout} onChange={(e) => setTimeoutStr(e.target.value)} />
          <span className="hint">A Go duration: 30m, 1h, 1h30m.</span>
        </label>

        {formError && <div className="mono text-[12px] text-[#eda3a3]">{formError}</div>}
        {submit.isError && <div className="mono text-[12px] text-[#eda3a3]">{String(submit.error)}</div>}

        <div className="flex items-center gap-3 border-t border-[#171d21] pt-5">
          <Button type="submit" variant="solid" disabled={submit.isPending}>
            {submit.isPending ? "Dispatching…" : "Dispatch"}
          </Button>
          <span className="stat">
            into <b>{effectivePool || "—"}</b> as <b>{kind}</b>
          </span>
        </div>
      </form>
    </View>
  );
}
