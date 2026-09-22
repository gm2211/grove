import { useState, type ReactNode } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api, ApiError } from "../api/client";
import { Button } from "./Button";
import { ConfirmButton } from "./ConfirmButton";

/**
 * Operator control for cloning PRIVATE repositories.
 *
 * Off until someone arms it here (or with `grove github enable`), fleet-wide the moment it is on,
 * and gone the moment it is disabled or its TTL lapses. The token only ever travels upward: the
 * API never returns it, so the field below is always blank on load and the panel shows a
 * fingerprint instead.
 */
export function PrivateRepoSourcing({ operator }: { operator: boolean }) {
  const queryClient = useQueryClient();
  const status = useQuery({
    queryKey: ["github-sourcing"],
    queryFn: api.getGitHubSourcing,
    retry: false,
    // The armed window can lapse on its own, so don't let the panel claim "on" indefinitely.
    refetchInterval: 30_000,
  });

  const [token, setToken] = useState("");
  const [repos, setRepos] = useState("");
  const [ttl, setTtl] = useState("4h");

  const enable = useMutation({
    mutationFn: () =>
      api.enableGitHubSourcing({
        token: token.trim(),
        repos: repos
          .split(",")
          .map((r) => r.trim())
          .filter(Boolean),
        ttl: ttl.trim() || undefined,
      }),
    onSuccess: (next) => {
      setToken("");
      queryClient.setQueryData(["github-sourcing"], next);
    },
  });

  const disable = useMutation({
    mutationFn: api.disableGitHubSourcing,
    onSuccess: (next) => queryClient.setQueryData(["github-sourcing"], next),
  });

  if (!operator) return null;
  if (status.error instanceof ApiError && status.error.status === 503) {
    return (
      <Section>
        <p className="text-xs" style={{ color: "var(--fg-faint)" }}>
          This control plane has no credential store, so jobs can clone public repositories only.
        </p>
      </Section>
    );
  }

  const on = status.data?.enabled ?? false;
  const error = enable.error ?? disable.error;

  return (
    <Section>
      <div className="flex items-center gap-2 text-xs">
        <span
          className="rounded px-1.5 py-0.5 font-medium"
          style={{
            background: on ? "var(--status-success)" : "var(--bg-inset)",
            color: on ? "#fff" : "var(--fg-muted)",
          }}
        >
          {on ? "ON" : "OFF"}
        </span>
        <span style={{ color: "var(--fg-muted)" }}>
          {on
            ? "jobs may clone the private repos below"
            : "jobs can clone public repositories only"}
        </span>
      </div>

      {on && status.data && (
        <dl className="grid grid-cols-[6rem_1fr] gap-x-3 gap-y-1 text-xs" style={{ color: "var(--fg-muted)" }}>
          <dt>hosts</dt>
          <dd className="mono">{(status.data.hosts ?? []).join(", ")}</dd>
          <dt>repos</dt>
          <dd className="mono">{status.data.repos?.length ? status.data.repos.join(", ") : "any repo"}</dd>
          <dt>token</dt>
          <dd className="mono">sha256:{status.data.tokenFingerprint}</dd>
          <dt>expires</dt>
          <dd className="mono">
            {status.data.expiresAt
              ? new Date(status.data.expiresAt).toLocaleString()
              : "never — turn it off when you're done"}
          </dd>
          {status.data.lastUsedAt && (
            <>
              <dt>last used</dt>
              <dd className="mono">{new Date(status.data.lastUsedAt).toLocaleString()}</dd>
            </>
          )}
        </dl>
      )}

      <label className="flex flex-col gap-1 text-xs">
        <span style={{ color: "var(--fg-faint)" }}>GitHub token (write-only — never shown again)</span>
        <input
          type="password"
          autoComplete="off"
          className="mono rounded border px-2 py-1.5 text-sm"
          style={{ borderColor: "var(--border)", background: "var(--bg-elevated)" }}
          value={token}
          onChange={(e) => setToken(e.target.value)}
          placeholder={on ? "paste a token to replace the armed one" : "ghp_… / github_pat_…"}
        />
      </label>

      <div className="flex gap-2">
        <label className="flex flex-1 flex-col gap-1 text-xs">
          <span style={{ color: "var(--fg-faint)" }}>repos (comma-separated, blank = any)</span>
          <input
            className="mono rounded border px-2 py-1.5 text-sm"
            style={{ borderColor: "var(--border)", background: "var(--bg-elevated)" }}
            value={repos}
            onChange={(e) => setRepos(e.target.value)}
            placeholder="gm2211/grove, acme/*"
          />
        </label>
        <label className="flex w-28 flex-col gap-1 text-xs">
          <span style={{ color: "var(--fg-faint)" }}>auto-off after</span>
          <input
            className="mono rounded border px-2 py-1.5 text-sm"
            style={{ borderColor: "var(--border)", background: "var(--bg-elevated)" }}
            value={ttl}
            onChange={(e) => setTtl(e.target.value)}
            placeholder="4h"
          />
        </label>
      </div>

      <div className="flex items-center gap-2">
        <Button
          variant="primary"
          disabled={!token.trim() || enable.isPending}
          onClick={() => enable.mutate()}
        >
          {enable.isPending ? "Enabling…" : on ? "Replace credential" : "Enable"}
        </Button>
        {on && (
          <ConfirmButton
            label={disable.isPending ? "Disabling…" : "Disable"}
            disabled={disable.isPending}
            onConfirm={() => disable.mutate()}
          />
        )}
      </div>

      {error && (
        <div className="text-xs" style={{ color: "var(--status-failed)" }}>
          {String(error)}
        </div>
      )}
      <p className="text-xs" style={{ color: "var(--fg-faint)" }}>
        While this is on, every build/agent job whose repo URL matches gets the credential for its{" "}
        <code className="mono">git clone</code>. Use an <code className="mono">https://</code> repo
        URL — an SSH remote is never given the token.
      </p>
    </Section>
  );
}

function Section({ children }: { children: ReactNode }) {
  return (
    <div
      className="flex flex-col gap-3 rounded border p-3"
      style={{ borderColor: "var(--border)" }}
    >
      <h2 className="text-sm font-semibold">Private repository sourcing</h2>
      {children}
    </div>
  );
}
