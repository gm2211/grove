import { useState } from "react";
import { useMutation, useQuery } from "@tanstack/react-query";
import { getServerUrl, getToken, setServerUrl, setToken } from "../lib/settings";
import { api } from "../api/client";
import { Button } from "../components/Button";
import { View, ViewHead } from "../components/Layout";
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
    <View>
      <ViewHead
        kicker="Settings · this browser and the control plane"
        title="Connection and credentials"
        lede="Where this UI sends its requests, and the one control-plane setting an operator can change from here."
      />
      <div className="grid max-w-5xl grid-cols-1 items-start gap-5 lg:grid-cols-2">
        <section className="panel flex flex-col gap-4 p-5">
          <span className="kicker" style={{ fontSize: 10 }}>
            Control plane
          </span>
          {!localBridge && (
            <label className="field">
              <span className="label">grove server URL</span>
              <input className="input" value={url} onChange={(e) => setUrl(e.target.value)} placeholder={window.location.origin} />
            </label>
          )}
          {!localBridge && (
            <label className="field">
              <span className="label">API token</span>
              <input
                type="password"
                className="input"
                value={token}
                onChange={(e) => setTokenInput(e.target.value)}
                placeholder="paste the bearer token from `grove config`"
              />
              <span className="hint">
                Stored in this browser's localStorage only. Every request sends it as{" "}
                <code className="mono text-[#c3d1d5]">Authorization: Bearer &lt;token&gt;</code>.
              </span>
            </label>
          )}
          {localBridge && (
            <p className="m-0 text-[13px] leading-[1.6] text-[#9fb0b5]">
              Connected through <code className="mono text-[#c3d1d5]">grove ui</code>. The device credential stays in macOS Keychain.
            </p>
          )}
          <div className="flex flex-wrap items-center gap-2">
            {!localBridge && (
              <Button variant="solid" onClick={save}>
                Save
              </Button>
            )}
            <Button onClick={() => test.mutate()} disabled={test.isPending}>
              {test.isPending ? "Testing…" : "Test connection"}
            </Button>
            {saved && <span className="mono text-[11px] text-[#7ee7c7]">saved</span>}
          </div>
          {test.isSuccess && (
            <div className="facts">
              {Object.entries(test.data).map(([k, v]) => (
                <div key={k} className="fact">
                  <span className="k">{k}</span>
                  <span className="v" style={{ color: v === true ? "var(--accent)" : v === false ? "var(--red-text)" : undefined }}>
                    {String(v)}
                  </span>
                </div>
              ))}
            </div>
          )}
          {test.isError && <div className="mono text-[11.5px] text-[#eda3a3]">{String(test.error)}</div>}
          {principal && (
            <div className="facts">
              <div className="fact">
                <span className="k">signed in as</span>
                <span className="v">{principal.name}</span>
              </div>
              <div className="fact">
                <span className="k">scopes</span>
                <span className="v">{principal.scopes.join(", ")}</span>
              </div>
            </div>
          )}
        </section>
        <PrivateRepoSourcing operator={operator} />
      </div>
    </View>
  );
}
