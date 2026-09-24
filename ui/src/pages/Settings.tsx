import { useState } from "react";
import { useMutation, useQuery } from "@tanstack/react-query";
import { getServerUrl, getToken, setServerUrl, setToken } from "../lib/settings";
import { api } from "../api/client";
import { Button } from "../components/Button";
import { Card, PageHeader } from "../components/Page";
import { LinkIcon } from "../components/Icons";
import { PrivateRepoSourcing } from "../components/PrivateRepoSourcing";

export function SettingsPage() {
  const localBridge = window.location.hostname === "127.0.0.1" || window.location.hostname === "localhost";
  const [url, setUrl] = useState(getServerUrl());
  const [token, setTokenInput] = useState(getToken());
  const [saved, setSaved] = useState(false);

  const test = useMutation({
    mutationFn: api.getHealth,
  });
  const { data: principal } = useQuery({ queryKey: ["whoami"], queryFn: api.whoAmI, staleTime: 60_000, retry: false });
  const operator = principal?.scopes.includes("operator") ?? false;

  function save() {
    setServerUrl(url);
    setToken(token);
    setSaved(true);
    setTimeout(() => setSaved(false), 2000);
  }

  return (
    <div className="max-w-3xl">
      <PageHeader title="Settings" description="How this browser reaches the control plane, and what jobs may clone." />
      <div className="flex flex-col gap-5">
        <Card
          title="Connection"
          description={
            localBridge ? undefined : (
              <>
                Stored in this browser's localStorage only. Every request sends it as{" "}
                <code className="mono">Authorization: Bearer &lt;token&gt;</code>.
              </>
            )
          }
          actions={
            principal && (
              <span className="text-xs" style={{ color: "var(--fg-muted)" }}>
                Signed in as {principal.name}
              </span>
            )
          }
          bodyClassName="flex flex-col gap-4 p-4"
        >
          {!localBridge && (
            <label className="flex flex-col gap-1.5">
              <span className="field-label">grove server URL</span>
              <input
                className="control mono"
                value={url}
                onChange={(e) => setUrl(e.target.value)}
                placeholder={window.location.origin}
              />
            </label>
          )}
          {!localBridge && (
            <label className="flex flex-col gap-1.5">
              <span className="field-label">API token</span>
              <input
                type="password"
                className="control mono"
                value={token}
                onChange={(e) => setTokenInput(e.target.value)}
                placeholder="paste the bearer token from `grove config`"
              />
            </label>
          )}
          {localBridge && (
            <div
              className="flex items-center gap-2 rounded-lg px-3 py-2.5 text-[13px]"
              style={{ background: "var(--bg-inset)", color: "var(--fg-muted)" }}
            >
              <LinkIcon size={14} />
              <span>
                Connected through <code className="mono">grove ui</code>. The device credential stays in the macOS
                Keychain.
              </span>
            </div>
          )}
          <div className="flex flex-wrap items-center gap-2">
            {!localBridge && (
              <Button variant="primary" onClick={save}>
                Save
              </Button>
            )}
            <Button onClick={() => test.mutate()} disabled={test.isPending}>
              {test.isPending ? "Testing…" : "Test connection"}
            </Button>
            {saved && (
              <span className="text-xs" style={{ color: "var(--status-success)" }}>
                Saved
              </span>
            )}
            {test.isSuccess && (
              <span className="mono text-xs" style={{ color: "var(--status-success)" }}>
                reachable: {JSON.stringify(test.data)}
              </span>
            )}
            {test.isError && (
              <span className="text-xs" style={{ color: "var(--status-failed)" }}>
                {String(test.error)}
              </span>
            )}
          </div>
        </Card>
        <PrivateRepoSourcing operator={operator} />
      </div>
    </div>
  );
}
