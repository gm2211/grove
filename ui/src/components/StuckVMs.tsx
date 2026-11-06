import type { FleetProblem } from "../api/types";

const label: Record<FleetProblem["kind"], string> = {
  "needs-unlock": "keychain locked",
  "boot-loop": "boot loop",
  failed: "failed",
  "not-joined": "never joined Nomad",
};

/**
 * The VMs that can't take jobs, and why: a locked keychain, a boot loop, a failure, or a VM that
 * runs but never registered a Nomad node. Each one is a pool short of capacity, and the reason is
 * otherwise only in `grove status` on the control plane. Renders nothing when every VM is fine.
 */
export function StuckVMs({ problems }: { problems?: FleetProblem[] }) {
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
            className="flex flex-wrap items-center gap-x-4 gap-y-1 border-t py-2 first:border-t-0"
            style={{ borderColor: "#2a2416" }}
          >
            <span className="badge" data-kind={p.kind}>
              {label[p.kind] ?? p.kind}
            </span>
            <span className="mono text-[12.5px] text-[#eaf4f4]">{p.vm}</span>
            {p.worker && <span className="mono text-[11.5px] text-[#8c9a9e]">on {p.worker}</span>}
            <span className="text-[12px] text-[#c3d1d5]">{p.detail}</span>
          </li>
        ))}
      </ul>
    </section>
  );
}
