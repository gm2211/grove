import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api, ApiError } from "../api/client";
import { Button } from "./Button";
import { ServerIcon } from "./Icons";
import { formatDurationMs } from "../lib/format";

/**
 * The Macs that have run `grove setup`, found this control plane over the tailnet, and are now
 * waiting to be let in.
 *
 * Without this panel the only way to approve one is to read the eight-character code off the
 * joining Mac's own terminal and retype it into `grove join approve` on the control plane — which
 * means being at two machines at once, and knowing the command. Here the code is already on
 * screen next to the name and tailnet address it belongs to, so approving is one click from
 * whatever device is showing the UI.
 *
 * It renders nothing at all when nobody is waiting: this is the Fleet page's normal state, and a
 * permanent empty "no pending requests" box would be a box that is almost always wrong to show.
 */
export function PendingJoins() {
  const client = useQueryClient();
  const { data, error } = useQuery({
    queryKey: ["join", "pending"],
    queryFn: api.getPendingJoins,
    refetchInterval: 5000,
    // A read-only or build-scoped token can reach the Fleet page but not this list. That is the
    // expected answer for such a token, not a fault to retry every five seconds.
    retry: (count, err) => !(err instanceof ApiError && (err.status === 401 || err.status === 403)) && count < 2,
  });

  const approve = useMutation({
    mutationFn: (code: string) => api.approveJoin(code),
    onSettled: () => client.invalidateQueries({ queryKey: ["join", "pending"] }),
  });

  if (error instanceof ApiError && (error.status === 401 || error.status === 403)) return null;
  if (!data || data.length === 0) return null;

  return (
    <section
      className="overflow-hidden rounded-xl border"
      style={{
        borderColor: "color-mix(in srgb, var(--status-lost) 45%, var(--border))",
        background: "color-mix(in srgb, var(--status-lost) 6%, var(--bg-elevated))",
      }}
    >
      <div className="flex flex-wrap items-center justify-between gap-x-3 gap-y-1 px-4 pt-3 pb-2">
        <h2 className="flex items-center gap-2 text-sm font-semibold">
          <span className="h-2 w-2 rounded-full" style={{ background: "var(--status-lost)" }} />
          {data.length === 1 ? "A Mac is waiting to join" : `${data.length} Macs are waiting to join`}
        </h2>
        <span className="text-xs" style={{ color: "var(--fg-muted)" }}>
          Approving mints its worker and device credentials.
        </span>
      </div>

      <ul className="flex flex-col px-2 pb-2">
        {data.map((join) => {
          const expiresInMs = new Date(join.expiresAt).getTime() - Date.now();
          return (
            <li
              key={join.code}
              className="flex flex-wrap items-center gap-x-4 gap-y-1 rounded-lg px-2 py-2"
              style={{ background: "var(--bg-elevated)" }}
            >
              <span className="flex items-center gap-2 font-medium">
                <ServerIcon size={15} />
                {join.name}
              </span>
              <span className="mono text-xs" style={{ color: "var(--fg-muted)" }}>
                {join.tailnetIp}
              </span>
              <span
                className="mono rounded-md px-1.5 py-px text-xs font-semibold tracking-widest"
                style={{ background: "var(--bg-inset)" }}
              >
                {join.code}
              </span>
              <span className="text-xs" style={{ color: "var(--fg-faint)" }}>
                expires in {formatDurationMs(expiresInMs)}
              </span>
              <Button
                variant="primary"
                size="sm"
                className="ml-auto"
                disabled={approve.isPending}
                onClick={() => approve.mutate(join.code)}
              >
                {approve.isPending && approve.variables === join.code ? "Approving…" : "Approve"}
              </Button>
            </li>
          );
        })}
      </ul>

      {approve.error ? (
        <p className="px-4 pb-3 text-xs" style={{ color: "var(--status-failed)" }}>
          Could not approve: {String(approve.error)}
        </p>
      ) : null}
    </section>
  );
}
