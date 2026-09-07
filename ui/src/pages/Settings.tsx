import { useState } from "react";
import { useMutation } from "@tanstack/react-query";
import { getServerUrl, getToken, setServerUrl, setToken } from "../lib/settings";
import { api } from "../api/client";
import { Button } from "../components/Button";

export function SettingsPage() {
  const [url, setUrl] = useState(getServerUrl());
  const [token, setTokenInput] = useState(getToken());
  const [saved, setSaved] = useState(false);

  const test = useMutation({
    mutationFn: api.getHealth,
  });

  function save() {
    setServerUrl(url);
    setToken(token);
    setSaved(true);
    setTimeout(() => setSaved(false), 2000);
  }

  return (
    <div className="max-w-lg">
      <h1 className="mb-4 text-lg font-semibold">Settings</h1>
      <div className="flex flex-col gap-4">
        <label className="flex flex-col gap-1 text-xs">
          <span style={{ color: "var(--fg-faint)" }}>grove server URL</span>
          <input
            className="mono rounded border px-2 py-1.5 text-sm"
            style={{ borderColor: "var(--border)", background: "var(--bg-elevated)" }}
            value={url}
            onChange={(e) => setUrl(e.target.value)}
            placeholder={window.location.origin}
          />
        </label>
        <label className="flex flex-col gap-1 text-xs">
          <span style={{ color: "var(--fg-faint)" }}>API token</span>
          <input
            type="password"
            className="mono rounded border px-2 py-1.5 text-sm"
            style={{ borderColor: "var(--border)", background: "var(--bg-elevated)" }}
            value={token}
            onChange={(e) => setTokenInput(e.target.value)}
            placeholder="paste the bearer token from `grove config`"
          />
        </label>
        <div className="flex items-center gap-2">
          <Button variant="primary" onClick={save}>
            Save
          </Button>
          <Button onClick={() => test.mutate()} disabled={test.isPending}>
            {test.isPending ? "Testing…" : "Test connection"}
          </Button>
          {saved && <span style={{ color: "var(--status-success)" }}>saved</span>}
        </div>
        {test.isSuccess && (
          <div style={{ color: "var(--status-success)" }} className="text-xs">
            reachable: {JSON.stringify(test.data)}
          </div>
        )}
        {test.isError && (
          <div style={{ color: "var(--status-failed)" }} className="text-xs">
            {String(test.error)}
          </div>
        )}
        <p className="text-xs" style={{ color: "var(--fg-faint)" }}>
          Stored in this browser's localStorage only. Every request sends it as{" "}
          <code className="mono">Authorization: Bearer &lt;token&gt;</code>.
        </p>
      </div>
    </div>
  );
}
