import { useState, type ReactNode } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api, ApiError } from "../api/client";
import { Button } from "./Button";
import { ConfirmButton } from "./ConfirmButton";
import { StatusPill } from "./StatusPill";

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
        <p className="m-0 text-[13px] text-[#9fb0b5]">
          This control plane has no credential store, so jobs can clone public repositories only.
        </p>
      </Section>
    );
  }

  const on = status.data?.enabled ?? false;
  const error = enable.error ?? disable.error;

  return (
    <Section on={on}>
      <p className="m-0 text-[13px] leading-[1.6] text-[#9fb0b5]">
        {on
          ? "Jobs may clone the private repositories below."
          : "Jobs can clone public repositories only. Arm a token to let them clone private ones, for a while."}
      </p>

      {on && status.data && (
        <div className="facts">
          <Row k="hosts" v={(status.data.hosts ?? []).join(", ")} />
          <Row k="repos" v={status.data.repos?.length ? status.data.repos.join(", ") : "none, enable again with repos"} />
          <Row k="token" v={`sha256:${status.data.tokenFingerprint}`} />
          <Row
            k="expires"
            v={status.data.expiresAt ? new Date(status.data.expiresAt).toLocaleString() : "never, turn it off when you're done"}
          />
          {status.data.lastUsedAt && <Row k="last used" v={new Date(status.data.lastUsedAt).toLocaleString()} />}
        </div>
      )}

      <label className="field">
        <span className="label">GitHub token · write-only</span>
        <input
          type="password"
          autoComplete="off"
          className="input"
          value={token}
          onChange={(e) => setToken(e.target.value)}
          placeholder={on ? "paste a token to replace the armed one" : "ghp_… / github_pat_…"}
        />
      </label>

      <div className="flex gap-3">
        <label className="field flex-1">
          <span className="label">repos</span>
          <input className="input" value={repos} onChange={(e) => setRepos(e.target.value)} placeholder="gm2211/grove, acme/*" />
        </label>
        <label className="field w-28">
          <span className="label">auto-off</span>
          <input className="input" value={ttl} onChange={(e) => setTtl(e.target.value)} placeholder="4h" />
        </label>
      </div>

      <div className="flex items-center gap-2">
        <Button
          variant="primary"
          disabled={!token.trim() || !repos.trim() || enable.isPending}
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

      {error && <div className="mono text-[11.5px] text-[#eda3a3]">{String(error)}</div>}
      <p className="m-0 text-[12px] leading-[1.55] text-[#5c696e]">
        While this is on, every build/agent job whose repo URL matches gets the credential for its{" "}
        <code className="mono">git clone</code>, and nothing else does. Use an <code className="mono">https://</code> repo
        URL: an SSH remote is never given the token.
      </p>
    </Section>
  );
}

function Row({ k, v }: { k: string; v: string }) {
  return (
    <div className="fact">
      <span className="k">{k}</span>
      <span className="v">{v}</span>
    </div>
  );
}

function Section({ children, on }: { children: ReactNode; on?: boolean }) {
  return (
    <section className="panel flex flex-col gap-4 p-5">
      <div className="flex items-baseline gap-[9px]">
        <span className="kicker" style={{ fontSize: 10 }}>
          Private repository sourcing
        </span>
        {on !== undefined && <StatusPill status={on ? "running" : "exited"} label={on ? "on" : "off"} />}
      </div>
      {children}
    </section>
  );
}
