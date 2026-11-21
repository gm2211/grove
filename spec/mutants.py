#!/usr/bin/env python3
"""Puts each bug the models were written for back into a scratch copy of grove, one at a time,
and requires the conformance tests to fail on it. If a mutant survives, the tests no longer tie
the code to the model, and spec/check.sh fails.

usage: spec/mutants.py REPO TRACES_DIR

Each mutant replaces one exact snippet. If the snippet is gone (the code was rewritten), the
script fails too: update the mutant to the new code rather than dropping it.
"""
import os
import shutil
import subprocess
import sys
import tempfile

MUTANTS = [
    {
        "name": "Engage records a lapsed snooze's pause as external",
        "file": "internal/snooze/snooze.go",
        "old": "len(st.Reasons(worker, now)) == 0 && st.Get(worker).Snooze == nil {",
        "new": "len(st.Reasons(worker, now)) == 0 {",
        "test": "TestSnoozeConformsToModel",
    },
    {
        "name": "Wake clears the snooze before resuming (a failed resume leaves a pause with no reason)",
        "file": "internal/snooze/snooze.go",
        "old": """	still = otherReasons(st, worker, now)
	if len(still) == 0 {
		if err := p.ResumeWorker(ctx, worker); err != nil {
			return false, nil, err
		}
	}
	if err := st.ClearSnooze(worker); err != nil {
		return false, nil, err
	}""",
        "new": """	still = otherReasons(st, worker, now)
	if err := st.ClearSnooze(worker); err != nil {
		return false, nil, err
	}
	if len(still) == 0 {
		if err := p.ResumeWorker(ctx, worker); err != nil {
			return false, nil, err
		}
	}""",
        "test": "TestSnoozeConformsToModel",
    },
    {
        "name": "the Expirer clears a lapsed snooze before resuming",
        "file": "internal/snooze/snooze.go",
        "old": """				if known && w.SchedulingPaused {
					if err := e.Orchard.ResumeWorker(ctx, name); err != nil {
						errs = append(errs, fmt.Errorf("resume %s after snooze: %w", name, err))
						continue
					}
				}
				if err := e.Store.ClearSnooze(name); err != nil {
					errs = append(errs, err)
				}""",
        "new": """				if err := e.Store.ClearSnooze(name); err != nil {
					errs = append(errs, err)
				}
				if known && w.SchedulingPaused {
					if err := e.Orchard.ResumeWorker(ctx, name); err != nil {
						errs = append(errs, fmt.Errorf("resume %s after snooze: %w", name, err))
						continue
					}
				}""",
        "test": "TestSnoozeConformsToModel",
    },
    {
        "name": "worker resume goes through while the worker is snoozed",
        "file": "internal/server/handlers_workers.go",
        "old": "if e := s.opts.Pauses.Get(name); e.Active(time.Now()) {",
        "new": "if e := s.opts.Pauses.Get(name); false && e.Active(time.Now()) {",
        "test": "TestSnoozeConformsToModel",
    },
    {
        "name": "worker pause pauses Orchard without recording the operator reason",
        "file": "internal/server/handlers_workers.go",
        "old": "if s.opts.Pauses != nil {\n\t\tif err := s.opts.Pauses.SetOperator(",
        "new": "if false {\n\t\tif err := s.opts.Pauses.SetOperator(",
        "test": "TestSnoozeConformsToModel",
    },
    {
        "name": "Wake doesn't hold the worker's lock",
        "file": "internal/snooze/snooze.go",
        "old": "func Wake(ctx context.Context, p Pauser, st *Store, worker string, now time.Time) (resumed bool, still []string, err error) {\n\tdefer st.Lock(worker)()\n",
        "new": "func Wake(ctx context.Context, p Pauser, st *Store, worker string, now time.Time) (resumed bool, still []string, err error) {\n",
        "test": "TestS1_WritersHoldTheWorkersLock",
    },
    {
        "name": "reconcile without the job's lock (two reruns of one lost job)",
        "file": "internal/dispatch/service.go",
        "old": "func (s *service) reconcile(ctx context.Context, rec *record) (*Job, error) {\n\tdefer s.lockJob(rec.ID)()\n",
        "new": "func (s *service) reconcile(ctx context.Context, rec *record) (*Job, error) {\n",
        "test": "TestS24_ConcurrentReconcilesRerunALostJobOnce",
    },
    {
        "name": "Cancel without the job's lock (a racing reconcile un-cancels the job)",
        "file": "internal/dispatch/service.go",
        "old": "func (s *service) Cancel(ctx context.Context, id string) error {\n\tdefer s.lockJob(id)()\n",
        "new": "func (s *service) Cancel(ctx context.Context, id string) error {\n",
        "test": "TestS24_CancelRacingAReconcileStaysCanceled",
    },
    {
        "name": "Cancel overwrites a finished job's outcome",
        "file": "internal/dispatch/service.go",
        "old": "\tif isTerminal(rec.Status) {\n\t\t// It already ended: its outcome stands",
        "new": "\tif false && isTerminal(rec.Status) {\n\t\t// It already ended: its outcome stands",
        "test": "TestS24_CancelOfAFinishedJobKeepsItsOutcome",
    },
    {
        "name": "a Dispatch error is taken as proof nothing was dispatched",
        "file": "internal/dispatch/service.go",
        "old": "\t\ts.notDispatched(ctx, id, req, name, err)\n",
        "new": "",
        "test": "TestS24_RefusedDispatchLeavesNoCopy",
    },
    {
        "name": "the reconcile sweep stops looking for a refused dispatch's late copy",
        "file": "internal/dispatch/service.go",
        "old": "\t\ts.sweepNotDispatched(ctx, time.Now().UTC())\n",
        "new": "",
        "test": "TestS24_RefusedDispatchLeavesNoCopy",
    },
    {
        "name": "two submits with one idempotency key both dispatch",
        "file": "internal/dispatch/service.go",
        "old": "\t\tdefer s.lockJob(\"idempotency\\x00\" + req.SubmittedBy + \"\\x00\" + req.IdempotencyKey)()\n",
        "new": "",
        "test": "TestS24_ConcurrentSubmitsWithOneKeyDispatchOnce",
    },
]


def main() -> int:
    repo, traces = os.path.abspath(sys.argv[1]), os.path.abspath(sys.argv[2])
    failed = False
    with tempfile.TemporaryDirectory() as tmp:
        work = os.path.join(tmp, "grove")
        shutil.copytree(repo, work, ignore=shutil.ignore_patterns(".git", ".lake", "node_modules", "_apalache-out"))
        env = dict(os.environ, GROVE_SNOOZE_TRACES=traces)
        for m in MUTANTS:
            path = os.path.join(work, m["file"])
            with open(path) as f:
                original = f.read()
            if original.count(m["old"]) != 1:
                print(f"MUTANT GONE  {m['name']}: its snippet is no longer in {m['file']} exactly once; update spec/mutants.py")
                failed = True
                continue
            with open(path, "w") as f:
                f.write(original.replace(m["old"], m["new"]))
            try:
                r = subprocess.run(
                    ["go", "test", "-count=1", "-run", "^" + m["test"] + "$", "./internal/conformance"],
                    cwd=work, env=env, capture_output=True, text=True, timeout=600,
                )
            finally:
                with open(path, "w") as f:
                    f.write(original)
            if r.returncode == 0:
                print(f"SURVIVED     {m['name']}: {m['test']} still passes with the bug back in")
                failed = True
            elif "--- FAIL" not in r.stdout:
                print(f"BROKE BUILD  {m['name']}: the mutant didn't compile or the test didn't run\n{r.stdout}{r.stderr}")
                failed = True
            else:
                print(f"killed       {m['name']} ({m['test']})")
    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(main())
