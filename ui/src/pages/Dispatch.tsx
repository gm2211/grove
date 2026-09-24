import { useMemo, useState } from "react";
import { useMutation, useQuery } from "@tanstack/react-query";
import { useNavigate } from "react-router";
import { api } from "../api/client";
import { Button } from "../components/Button";
import { Card, PageHeader } from "../components/Page";
import { DispatchIcon, PlusIcon, XIcon } from "../components/Icons";
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
  const operator = principal?.scopes.includes("operator") ?? false;

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

  const kinds: { value: JobKind; label: string; hint: string }[] = [
    { value: "build", label: "Build", hint: "Clone a repo and run a script" },
    { value: "agent", label: "Agent", hint: "Run a coding agent against a repo" },
    ...(operator ? [{ value: "shell" as JobKind, label: "Shell", hint: "Run a script, no checkout" }] : []),
  ];

  return (
    <div className="max-w-3xl">
      <PageHeader
        title="Dispatch a job"
        description="Send a one-off job to a pool of VMs. You'll be taken to its live log."
      />
      <form onSubmit={handleSubmit} className="flex flex-col gap-4">
        <Card title="What to run" bodyClassName="flex flex-col gap-4 p-4">
          <div className="flex flex-col gap-1.5">
            <span className="field-label">Kind</span>
            <div className="grid grid-cols-1 gap-2 sm:grid-cols-3" role="radiogroup" aria-label="Job kind">
              {kinds.map((k) => {
                const active = kind === k.value;
                return (
                  <button
                    key={k.value}
                    type="button"
                    role="radio"
                    aria-checked={active}
                    data-kind={k.value}
                    onClick={() => setKind(k.value)}
                    className="flex flex-col items-start gap-0.5 rounded-lg border px-3 py-2.5 text-left transition-colors"
                    style={{
                      borderColor: active ? "var(--accent)" : "var(--border-strong)",
                      background: active ? "var(--accent-soft)" : "var(--bg-elevated)",
                      boxShadow: active ? "0 0 0 1px var(--accent)" : undefined,
                    }}
                  >
                    <span
                      className="text-[13px] font-semibold"
                      style={{ color: active ? "var(--accent)" : "var(--fg)" }}
                    >
                      {k.label}
                    </span>
                    <span className="text-xs" style={{ color: "var(--fg-muted)" }}>
                      {k.hint}
                    </span>
                  </button>
                );
              })}
            </div>
          </div>

          <label className="flex flex-col gap-1.5">
            <span className="field-label">Pool</span>
            <select className="control" value={effectivePool} onChange={(e) => setPool(e.target.value)}>
              {pools.length === 0 && <option value="">no pools observed</option>}
              {pools.map((p) => (
                <option key={p} value={p}>
                  {p}
                </option>
              ))}
            </select>
          </label>

          {kind !== "shell" && (
            <div className="grid grid-cols-1 gap-4 sm:grid-cols-[1fr_12rem]">
              <label className="flex flex-col gap-1.5">
                <span className="field-label">Repository</span>
                <input
                  className="control mono"
                  placeholder="github.com/gm2211/grove"
                  value={repo}
                  onChange={(e) => setRepo(e.target.value)}
                />
              </label>
              <label className="flex flex-col gap-1.5">
                <span className="field-label">Ref</span>
                <input
                  className="control mono"
                  placeholder="main"
                  value={ref}
                  onChange={(e) => setRef(e.target.value)}
                />
              </label>
            </div>
          )}

          <label className="flex flex-col gap-1.5">
            <span className="field-label">Script</span>
            <textarea
              className="control mono h-36 resize-y leading-relaxed"
              value={script}
              onChange={(e) => setScript(e.target.value)}
            />
          </label>
        </Card>

        <Card title="Options" bodyClassName="flex flex-col gap-4 p-4">
          <div className="flex flex-col gap-1.5">
            <span className="field-label">Environment variables</span>
            {env.map((row, i) => (
              <div key={i} className="flex gap-2">
                <input
                  className="control mono flex-1"
                  placeholder="KEY"
                  aria-label="Variable name"
                  value={row.key}
                  onChange={(e) =>
                    setEnv((rows) => rows.map((r, idx) => (idx === i ? { ...r, key: e.target.value } : r)))
                  }
                />
                <input
                  className="control mono flex-[2]"
                  placeholder="value"
                  aria-label="Variable value"
                  value={row.value}
                  onChange={(e) =>
                    setEnv((rows) => rows.map((r, idx) => (idx === i ? { ...r, value: e.target.value } : r)))
                  }
                />
                <Button
                  type="button"
                  variant="ghost"
                  size="icon"
                  className="!h-[34px] !w-[34px]"
                  title="Remove variable"
                  onClick={() => setEnv((rows) => rows.filter((_, idx) => idx !== i))}
                >
                  <XIcon size={14} />
                </Button>
              </div>
            ))}
            <Button
              type="button"
              size="sm"
              variant="ghost"
              className="self-start"
              onClick={() => setEnv((rows) => [...rows, { key: "", value: "" }])}
            >
              <PlusIcon size={13} />
              Add variable
            </Button>
          </div>

          <label className="flex flex-col gap-1.5">
            <span className="field-label">Timeout</span>
            <input className="control mono !w-32" value={timeout} onChange={(e) => setTimeoutStr(e.target.value)} />
            <span className="field-hint">A Go duration, e.g. 30m, 1h, 1h30m.</span>
          </label>
        </Card>

        {(formError || submit.isError) && (
          <div
            className="rounded-lg border px-3 py-2 text-[13px]"
            style={{
              borderColor: "color-mix(in srgb, var(--status-failed) 40%, transparent)",
              background: "color-mix(in srgb, var(--status-failed) 8%, transparent)",
              color: "var(--status-failed)",
            }}
          >
            {formError ?? String(submit.error)}
          </div>
        )}

        <div className="flex items-center justify-end gap-3">
          <span className="text-xs" style={{ color: "var(--fg-faint)" }}>
            {effectivePool ? `Runs on the ${effectivePool} pool` : "No pool available"}
          </span>
          <Button type="submit" variant="primary" disabled={submit.isPending}>
            <DispatchIcon size={14} />
            {submit.isPending ? "Dispatching…" : "Dispatch job"}
          </Button>
        </div>
      </form>
    </div>
  );
}
