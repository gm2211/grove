import { useState } from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { api, ApiError } from "../api/client";
import type { FleetProblem } from "../api/types";
import { Button } from "./Button";

const label: Record<FleetProblem["kind"], string> = {
  "needs-unlock": "keychain locked",
  "boot-loop": "boot loop",
  failed: "failed",
  "not-joined": "never joined Nomad",
};

/** A macOS VM's trouble that unlocking its worker user's keychain may fix. */
function unlockable(p: FleetProblem): boolean {
  return !!p.worker && (p.kind === "needs-unlock" || (p.kind === "boot-loop" && p.pool === "macos"));
}

/**
 * The VMs that can't take jobs, and why: a locked keychain, a boot loop, a failure, or a VM that
 * runs but never registered a Nomad node. Each one is a pool short of capacity, and the reason is
 * otherwise only in `grove status` on the control plane. Renders nothing when every VM is fine.
 *
 * A macOS VM that can't boot for its worker user's locked login keychain can be fixed from here:
 * type that user's password once, and the worker's broker hands it to the worker, which unlocks
 * the keychain. Nothing stores it, so after a reboot the VM shows up here again.
 */
export function StuckVMs({ problems }: { problems?: FleetProblem[] }) {
  const [unlocking, setUnlocking] = useState<string | null>(null);
  const [sent, setSent] = useState<Record<string, boolean>>({});
  if (!problems || problems.length === 0) return null;
  return (
    <section className="needs-you flex flex-col gap-2 px-4 py-3">
      <div className="flex flex-wrap items-baseline gap-x-3 gap-y-1">
        <span className="kicker" style={{ color: "var(--amber)", fontSize: 10 }}>
          stuck
        </span>
        <h2 className="m-0 text-[13.5px] font-medium text-[#eaf4f4]">
          {problems.length === 1 ? "A VM can't take jobs" : `${problems.length} VMs can't take jobs`}
        </h2>
      </div>
      <ul className="m-0 flex list-none flex-col p-0">
        {problems.map((p) => (
          <li
            key={p.vm}
            className="flex flex-col gap-2 border-t py-2 first:border-t-0"
            style={{ borderColor: "#2a2416" }}
          >
            <div className="flex flex-wrap items-center gap-x-4 gap-y-1">
              <span className="badge" data-kind={p.kind}>
                {label[p.kind] ?? p.kind}
              </span>
              <span className="mono text-[12.5px] text-[#eaf4f4]">{p.vm}</span>
              {p.worker && <span className="mono text-[11.5px] text-[#8c9a9e]">on {p.worker}</span>}
              <span className="text-[12px] text-[#c3d1d5]">{p.detail}</span>
              {unlockable(p) && unlocking !== p.worker && (
                <span className="ml-auto">
                  {sent[p.worker!] ? (
                    <span className="text-[12px] text-[#8c9a9e]">password sent; the VM recovers on its next boot</span>
                  ) : (
                    <Button size="sm" variant="primary" onClick={() => setUnlocking(p.worker!)}>
                      Unlock keychain
                    </Button>
                  )}
                </span>
              )}
            </div>
            {unlockable(p) && unlocking === p.worker && (
              <UnlockForm
                worker={p.worker!}
                onDone={() => {
                  setSent((s) => ({ ...s, [p.worker!]: true }));
                  setUnlocking(null);
                }}
                onCancel={() => setUnlocking(null)}
              />
            )}
          </li>
        ))}
      </ul>
    </section>
  );
}

function UnlockForm({ worker, onDone, onCancel }: { worker: string; onDone: () => void; onCancel: () => void }) {
  const qc = useQueryClient();
  const [password, setPassword] = useState("");
  const unlock = useMutation({
    mutationFn: () => api.unlockKeychain(worker, password),
    onSuccess: () => {
      setPassword("");
      onDone();
      qc.invalidateQueries({ queryKey: ["fleet"] });
    },
  });
  return (
    <form
      className="flex flex-wrap items-center gap-2"
      onSubmit={(e) => {
        e.preventDefault();
        if (password) unlock.mutate();
      }}
    >
      <label className="text-[12px] text-[#c3d1d5]" htmlFor={`unlock-${worker}`}>
        Login password of the worker user on <span className="mono">{worker}</span>
      </label>
      <input
        id={`unlock-${worker}`}
        type="password"
        autoComplete="off"
        autoFocus
        className="input min-w-[220px] max-w-[360px] flex-1"
        value={password}
        onChange={(e) => setPassword(e.target.value)}
      />
      <Button size="sm" variant="primary" type="submit" disabled={!password || unlock.isPending}>
        {unlock.isPending ? "Sending…" : "Unlock"}
      </Button>
      <Button size="sm" type="button" onClick={onCancel}>
        Cancel
      </Button>
      <span className="w-full text-[11.5px] text-[#8c9a9e]">
        Kept in memory until that Mac's worker collects it (within a minute), then forgotten. Nothing stores it, so
        after a reboot you unlock again.
      </span>
      {unlock.error && (
        <span className="w-full text-[12px] text-[#eda3a3]">
          {unlock.error instanceof ApiError && unlock.error.status === 403
            ? "Only an operator can unlock a worker's keychain."
            : String(unlock.error)}
        </span>
      )}
    </form>
  );
}
