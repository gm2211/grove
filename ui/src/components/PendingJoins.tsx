import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api, ApiError } from "../api/client";
import { Button } from "./Button";
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
 * It renders nothing at all when nobody is waiting: this is the Map's normal state, and a
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
    <section className="needs-you flex flex-col gap-2 px-4 py-3">
      <div className="flex flex-wrap items-baseline gap-x-3 gap-y-1">
        <span className="kicker" style={{ color: "var(--amber)", fontSize: 10 }}>
          needs you
        </span>
        <h2 className="m-0 text-[13.5px] font-medium text-[#eaf4f4]">
          {data.length === 1 ? "A Mac is waiting to join" : `${data.length} Macs are waiting to join`}
        </h2>
        <span className="ml-auto text-[12px] text-[#a3b0b4]">Approving mints its worker and device credentials.</span>
      </div>

      <ul className="m-0 flex list-none flex-col p-0">
        {data.map((join) => {
          const expiresInMs = new Date(join.expiresAt).getTime() - Date.now();
          return (
            <li
              key={join.code}
              className="flex flex-wrap items-center gap-x-4 gap-y-1 border-t py-2 first:border-t-0"
              style={{ borderColor: "#3a3220" }}
            >
              <span className="mono text-[12.5px] text-[#eaf4f4]">{join.name}</span>
              <span className="mono text-[11.5px] text-[#a3b0b4]">
                {join.node ? `${join.tailnetIp} (${join.node})` : join.tailnetIp}
              </span>
              <span className="mono text-[12px] tracking-[0.18em] text-[#e6c580]">{join.code}</span>
              <span className="mono text-[11px] text-[#8a979c]">expires in {formatDurationMs(expiresInMs)}</span>
              {join.replacesWorker ? (
                // Approving this hands another machine an existing worker's name and its jobs'
                // credentials, so it is not one click: the CLI asks for --replace.
                <span className="ml-auto text-[11.5px]" style={{ color: "var(--amber)" }}>
                  uses the name of an existing worker from another machine: check the address, then{" "}
                  <span className="mono">grove join approve {join.code} --replace</span>
                </span>
              ) : (
                <Button
                  variant="primary"
                  size="sm"
                  className="ml-auto"
                  disabled={approve.isPending}
                  onClick={() => approve.mutate(join.code)}
                >
                  {approve.isPending && approve.variables === join.code ? "Approving…" : "Approve"}
                </Button>
              )}
            </li>
          );
        })}
      </ul>

      {approve.error ? (
        <p className="m-0 text-xs" style={{ color: "var(--red-text)" }}>
          Could not approve: {String(approve.error)}
        </p>
      ) : null}
    </section>
  );
}
