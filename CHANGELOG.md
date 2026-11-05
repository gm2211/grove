# Changelog

All notable changes to grove are documented here.

## Unreleased

- **macOS VMs boot with nobody logged in: `grove worker remember-login`.** A macOS guest needs the
  worker user's login keychain unlocked (Virtualization.framework keeps its host key there), and a
  GUI login, the only way to get that so far, ends at every reboot or crash, so the macOS pool died
  with it until someone logged in again. Run `grove worker remember-login` once as the worker user
  (for example `sudo -u argos -H grove worker remember-login`): it checks the password against that
  user's login keychain, keeps it in `~/.config/grove/credentials/login-keychain` (0600), and the
  worker's wrapper unlocks the keychain inside the worker daemon's own session before Orchard
  starts, and again every 5 minutes. The password goes to `security -i` on stdin, never on argv.
  Verified on a worker Mac: a macOS guest that failed with "Failed to get current host key" booted
  with nobody logged in. `grove doctor`'s `login-session` check accepts it; `--forget` removes it.
  Re-run `grove setup` (or `grove install --role worker`) after upgrading, which rewrites the
  wrapper.

## v0.1.37

- **Install: `--admin` keeps `su` and `sudo` on the terminal.** The root script was piped to
  `su <admin> -c 'sudo sh -e'` on stdin, but macOS `su` reads the password from stdin, so it failed
  at once with `su: Sorry` and never prompted. The script now rides in argv
  (`sudo /bin/sh -e -c '<script>'`, single-quoted for the admin's login shell; it never holds a
  secret) and `su` runs attached, so both `su` and `sudo` prompt for the admin's password. Without
  an interactive stdin, `--admin` fails up front with a clear message.

## v0.1.36

- **Secrets hygiene.** `grove config show` redacts every secret field (`<set, len N>`; fields are
  tagged `secret:"true"` in `internal/config`, so a new one only needs the tag). The Nomad
  bootstrap hint now says `grove config set nomad.token -` (stdin), and `grove config set` warns
  when a secret key's value is given on the command line. `grove install` gains `--token-file`
  (a path, or `-` for stdin) and reads `$GROVE_TOKEN`; `--token` still works but warns.
  `config.yaml`, `secrets.json`, `devices.json` and `github.json` readers now refuse a symlink, a
  non-regular file or a file owned by another user (new `internal/secfile`, same rules as
  `internal/install/credfile.go`) before tightening the mode to 0600.
- **Install: the Orchard controller unit carries its listen/TLS flags; doctor flags drift and a
  bootstrap admin token.** The LaunchAgent / systemd unit ran bare `orchard controller run`, so a
  restart (launchd keeps the arguments of the plist it loaded, so nothing showed until then) would
  have listened on `:6120` on every interface with auto-generated TLS and no host-dirs, breaking
  every worker and grove client that speaks `http://<tailnet-ip>:6120`. The env file's
  `ORCHARD_ADDRESS` was never read by Orchard or launchd. The unit now runs
  `controller run --listen <addr> --insecure-no-tls`, plus `--insecure-allow-host-dirs` only when
  `fleet.yaml` has a `hostXcode` pool, and never carries a token. `grove doctor` adds
  `orchard-controller-unit` (running program/flags vs the rendered unit, from `launchctl print` /
  `systemctl show`) and `orchard-controller-credential` (fails when the running controller's
  environment holds `ORCHARD_BOOTSTRAP_ADMIN_TOKEN`); `grove install` reports both and never
  restarts the controller. docs/OPERATIONS.md has the restart procedure and how to delete the
  `bootstrap-admin` service account.

- **macOS worker: native toolchain installer (execution side).** A shared macOS pool's `build` job
  whose dispatch meta carries `toolchain_json` and `toolchain_hash` (nothing in the control plane
  sets them yet) gets rust, node and/or python installed natively. The fleet startup script creates
  `_grovetool` (UID 7462, no shell, home `/var/empty`, in the pf fence beside the job users, no SSH),
  `/private/var/grove-toolchains` and the root-owned `/usr/local/libexec/grove-toolchain`. The
  `source` task runs that installer as `_grovetool` after the clone token is gone (failure: exit 70);
  it re-validates the spec with `jq`, recomputes the hash, installs with the image's now-pinned,
  checksum-verified mise into a read-only `mise-<version>-<hash>` directory under a lock and a
  20-minute watchdog, and evicts idle entries (24 hours, keeping three, never one in use). `run.sh`
  puts the entry's paths on the job's `PATH` only if they lie inside it. All job templates accept
  the two meta keys; Linux pools ignore them. See docs/OPERATIONS.md "Native toolchains on macOS".

- **Environment jobs can carry a validated native toolchain and fall back to macOS.**
  `environment.toolchain` names exact versions of `rust` (with `clippy`, `rustfmt`, `rust-src`,
  `llvm-tools`), `node` and `python`; the control plane refuses channels, ranges and partial
  versions, re-marshals the spec deterministically and sets `toolchain_json` and `toolchain_hash`
  (sha256) itself, never from a caller's bytes or `meta`. An environment with a toolchain may be
  submitted to, or fall back to, a shared macOS pool with the new `toolchains: {enabled: true}`
  block, and only to one whose job size covers the job's `resources`; an image-only environment
  still never falls back. `fleet.yaml` refuses `toolchains` on Linux and sealed pools.
  `/healthz` features now read `["environment", "toolchain"]`. Spec S22 added; S11 reworded.

## v0.1.35

- **`grove install --admin <account>` runs the pending root steps through an admin account.** From
  a standard user such as `argos` (`ssh -t argos@<mac> grove install --role worker --admin
  gm2211`), grove renders the same root script `--no-sudo` would print, from the live checks, and
  pipes it to `su <account> -c '/usr/bin/sudo /bin/sh -e'`; `su` and `sudo` ask for the admin's
  password on the terminal (no terminal: it says to use `ssh -t`). The plan then runs once more in
  the same invocation, so steps that waited for the root ones apply immediately, and nothing is asked
  when no root step is pending. The account must exist and be in the `admin` group; `--admin` excludes
  `--no-sudo`. `grove setup --admin` works the same way. The account is not remembered in config.
- **`grove setup --user` honours a rootless worker.** The admin's grove read the worker user's
  `config.yaml` itself, could not (their home is mode 750), and treated that as "not rootless", so it
  would install root worker and broker LaunchDaemons over a rootless worker. It now reads the file as
  the worker user (through `sudo -u`), and fails when the setting can't be determined; the install
  path likewise fails on a `config.yaml` it can't read instead of assuming root mode.

## v0.1.34

- **Switching a root worker to `--rootless` retires its LaunchDaemons.** Before, `grove install
  --role worker --rootless` on a Mac whose worker and broker were root LaunchDaemons only loaded the
  LaunchAgents beside them, so two `orchard worker run` and two `grove broker` processes ran. The
  plan now has a root step, `retire-worker-daemons` (before `worker-agents`), that boots
  `com.grove.orchard-worker` and `com.grove.broker` out of the system domain and removes their
  plists; it touches nothing else (`com.grove.pf-vm` stays, so `--firewall` keeps the VM fence).
  `worker-agents` loads no agent, and writes no plist, while those daemons exist: it says so and
  waits, and the re-run after an admin ran the printed block loads them. The `pmset disablesleep`
  the root install set is left alone.

## v0.1.33

- **An enrolment counts only when the guest script itself confirms it.** Each enrolment exec now
  carries a random nonce on stdin, and grove takes the exec as an enrolment only when the guest
  prints `grove: enrolled <nonce>` as a line of its own. Before, matching the bare line was enough,
  and the script's own source holds that line: an exec answered before the guest had booted came
  back with the command echoed and exit 0, grove logged "enrolled", and the VM waited out its
  30-minute enrolment timeout, was restarted, and did the same again, so no shared VM ever joined
  Nomad. A false confirmation is now an error, retried on the next tick.
- **A build job can name its environment.** `JobRequest.environment.image` runs a `build` job's
  script in a digest-pinned image (`<registry>/<repo>@sha256:...`, no tags) instead of the pool's
  runner image. The control plane decides: the pool must allow the repository in fleet.yaml
  `environments.allow` (exact repository or a `/` prefix, explicit registry), or the submit is a
  400. Not for macOS or sealed pools, and a job with an environment never falls back to macOS. The
  image reaches Nomad as meta `task_image`, set on every build dispatch (environment, else the
  pool's runner image); the `source` step that holds the clone token always runs in the runner
  image. `/healthz` reports `features: ["environment"]`. The Linux worker's docker plugin now
  garbage-collects unused images after 24h. Details: docs/JOBS.md "Job environments".
- **Security: a caller's `meta.image` no longer reaches Nomad.** It used to be copied into the
  dispatch meta (ignored by the templates, but a trap for any template that read it). Caller meta
  now only round-trips in `grove_meta_json`.
- **Linux VMs no longer restart the moment their startup script finishes.** Orchard runs a startup
  script by typing `set -e`, the script and `exit` into a login shell. On that exit, bash runs
  `~/.bash_logout`, whose `clear_console` fails without a terminal on Ubuntu, and with `-e` still
  on, every Linux script that got to its end exited 1. With Nomad mTLS, the script only gets to its
  end once the VM is enrolled, so each enrolment was followed within two seconds by a "failed"
  VM, an Orchard restart onto a fresh clone (no certificate, no key), a 30-minute wait and,
  after three of those, a rebuild: no Linux node ever registered. The generated script now ends
  with `set +e` on Linux guests. Their startup script changes, so every Linux VM is recreated once.
- **`grove serve` logs its version when it starts** (`grove serve: listening … version=…`). A
  control plane that had a new binary installed but kept running an older `grove serve` (which
  enrolled a VM restarted by Orchard only 30 minutes later, not on the next tick) looked the same
  as an upgraded one. Re-run `grove install --role control-plane` after upgrading, which restarts
  it, or restart the service yourself (docs/OPERATIONS.md "Upgrading").

## v0.1.32

- **The control plane's own worker gets a broker credential.** Workers elsewhere get one from join
  approval, but the control plane's worker is installed with `grove install --role worker` and
  never joins, so its broker had no credential and never synced: jobs there couldn't use
  credentials, and `grove doctor` showed `✗ broker`. On the control-plane host, that install now
  issues the credential itself (scope `broker` only, named after the Orchard worker) when none is
  stored. Never on `--dry-run`.

## v0.1.31

- **The control-plane UI is a live map now.** The queue, worker Macs, their VMs and the jobs running
  in each VM sit in columns left to right, in the order a job travels, with a detail column for whatever is selected: its
  lifecycle, a live log tail for a running job, and the actions you'd take there (pause, snooze,
  wake, recycle, cancel). The look follows Argos: dark, mono labels, a serif headline per view.
  Jobs, job detail, Dispatch and Settings were restyled to match; `/fleet` links go to the map.
- **README: new screenshot, a walkthrough GIF, and both diagrams redrawn dark**, with the
  credential broker added to "Where grove runs" and the broker flow drawn in the same style.
- **A VM that Orchard restarted is enrolled into Nomad again.** Orchard's OnFailure restart boots a
  fresh clone under the same VM UID, without the certificate the last enrolment wrote, and grove
  remembered enrolments by UID alone, so the restarted VM waited up to 30 minutes for a new one.
  Enrol now tells boots apart by UID and restart count, and it re-enrols a VM whose Nomad client
  hasn't registered 5 minutes after its enrolment.
- **Snoozing a Mac again just after a snooze lapsed no longer leaves it paused for good.** In the
  window between a snooze lapsing and the expirer clearing it, a new snooze recorded the existing
  pause as external, so the Mac stayed paused after the new snooze ended. `Engage` now counts a
  recorded snooze, lapsed or not, as grove's own pause. Found by replaying the snooze model's
  traces against the code.
- **`spec/check.sh` now ties the snooze model to the code.** It replays 1,000 sampled model traces
  against the real snooze, API and health code (`TestSnoozeConformsToModel`) and puts six old bugs
  back one at a time (`spec/mutants.py`), failing unless a conformance test catches each.

## v0.1.30

- **The worker's LaunchDaemons get their own security session (`SessionCreate`).** A daemon with
  none had `tart run` attached to the console user's login session, where it checked in as a
  foreground app: that is what flashed in the Dock of whoever was logged in at a worker Mac, and it
  put job-facing VM processes inside someone else's session. Re-run `grove setup --user <user>`
  (one sudo) after upgrading: the installed plist changes. This does not unlock the worker user's
  keychain, so macOS VMs still need that user logged in (`login-session`).

## v0.1.29

- **A rootless worker can have the VM firewall: `grove install --role worker --firewall`.** The
  `host-firewall` step (the `com.grove.pf-vm` pf anchor) no longer forces the whole worker onto
  LaunchDaemons: with `--firewall`, a rootless install keeps its LaunchAgents (which run in the
  user's session, so macOS VMs get its keychain) and adds only the firewall, whose one root step is
  printed for an admin. Once installed, later rootless runs keep it current without the flag, and
  `grove doctor` checks it instead of warning.
- **`grove worker ls` and `grove status` work on a device that only has a server token** (#133).
  With `server.url` set and no Orchard or Nomad config (a laptop set up to snooze its own Mac),
  both now go through `GET /api/v1/fleet`: `status` prints the server check, worker, VM and node
  counts, snoozes and boot loops; `worker ls` reports the API's error instead of silently falling
  back to "orchard.url is not configured".
- **A macOS VM that can't boot because its worker user isn't logged in is retried hourly** (#132).
  Virtualization.framework's "Failed to get current host key" fails every rebuild the same way
  until that user has a login session, so grove no longer recreates it every 10 minutes (and churns
  tart and the Dock on that Mac); it retries once an hour, names the cause in the rebuild reason,
  and `grove status` and `grove doctor` list it as "needs login".

## v0.1.28

- **Pausing, snoozing and waking a worker no longer race.** Two writers of a worker's pause state
  (a snooze, `grove worker resume`, the snooze expirer, the health monitor) could interleave and
  leave a snoozed Mac taking jobs, or a resumed Mac paused forever with no recorded reason. Every
  writer now holds that worker's lock, and a lapsing snooze is cleared only after Orchard accepts
  the resume, so a failed resume is retried on the next pass.
- **A job queued on a snoozed pool no longer fails just after the snooze ends.** For up to a minute
  after a snooze ended, dispatch could fail the job as "every worker is paused". It now waits.
- **A VM gets its pool's Nomad identity only on a worker that pool's selector matches.** Before, a
  worker Mac outside a pool could create a VM of that pool on itself and be enrolled into it, with
  that pool's jobs and secrets.
- **`spec/`: 20 properties of grove, checked on every push.** Quint models checked by TLC, Lean 4
  proofs and CodeQL secret-flow queries, tied to the Go code by conformance tests; `spec/check.sh`
  runs them and CI runs it as the `spec` job. spec/README.md lists each property and its result.

- **An enrolment counts only once the VM confirms it.** An Orchard exec whose stream closed without
  an exit status read as exit 0, and Enrol took any exit 0 as done, then left the VM alone for up
  to 30 minutes while it still waited for its certificate. Exec now reports a missing exit status
  as an error, Enrol also requires the guest script's `grove: enrolled` line, and serve logs each
  VM it enrols.

## v0.1.27

- **`grove doctor` warns when the worker user has no login session (`login-session`).** A macOS VM
  can't boot without one: Virtualization.framework's Secure Enclave host key needs the user's
  unlocked login keychain, so with nobody logged in as the worker user every macOS VM failed with
  `VZErrorDomain -9 "… Failed to get current host key"` and was recreated every 10 minutes. Linux
  VMs are unaffected, so it is a warning. docs/INSTALL.md says how to keep the session (fast user
  switching or auto-login).

## v0.1.26

- **`grove doctor` on a worker or client no longer fails on checks that only matter on the control
  plane.** `orchard-controller` and `nomad` pass as "not used here" when config.yaml has no endpoint
  for them and the machine doesn't run `grove serve` (no `server.listen`): a worker joined through
  `grove setup` reaches the control plane through `server.url` only. `brew-trust:hashicorp/tap` is
  audited only on the control plane, the one role that installs Nomad (trust is per user, so a
  worker user was flagged for an admin's tap it never uses). A control plane missing either endpoint
  still fails.

- **`grove serve` says why it won't enrol a VM.** A running VM that waits for its Nomad identity
  but doesn't match what the fleet spec would create (other worker, sealed pool, drifted spec) was
  skipped silently, so it looked like enrolment was never attempted. serve now logs each such VM
  and the reason (every 10 minutes while it holds), and a plan's `spec changed` delete names the
  field that differs.
- **A shared VM that never enrolled in Nomad is deleted right away.** Its shutdown script used to
  poll a Nomad client that never started until the 2h30m shutdown timeout, which held the Orchard
  worker's sync loop, so no other VM on that Mac was created, reported running or deleted. The
  script now exits at once when the VM has no `enrolled` marker.
- **A fleet failure that persists is logged again every 10 minutes.** `grove serve` logged an
  enrolment or roll-call error only when it changed, so one that kept failing the same way went
  quiet after its first line. That read like a fix while a VM sat waiting for its Nomad identity.

## v0.1.25

- **The worker install no longer fails with `Bootstrap failed: 5: Input/output error` on a Mac whose
  daemons were already running.** launchd finishes a `bootout` after it returns, so the root step
  that reloads the Orchard worker, broker and VM firewall LaunchDaemons now waits (up to 10s) until
  each is unloaded before bootstrapping it, and retries the bootstrap once. Before, the script
  stopped there and left the broker unloaded.

## v0.1.24

- **`grove install --role worker --rootless` installs a worker with no root at all.** For a user with no sudo (such as a dedicated `argos` standard user), the worker and broker become that user's own LaunchAgents in `~/Library/LaunchAgents` (`worker-agents`, replacing `worker-daemons`; the same jobs, loaded into `gui/<uid>`, an older same-label agent replaced in place, verified running), `com.grove.keepawake` runs `/usr/bin/caffeinate -ims` in place of `pmset` (an existing caffeinate agent counts as done), and `host-firewall` and `softnet-root` are listed as `[skip]` with what is lost: VMs on this Mac are not fenced off the host's other ports and LAN, and isolated network pools are unavailable. No sudo block is printed and the install exits 0. It is selected by `--rootless`, by `worker.rootless: true` in `config.yaml` (written by the install, so re-runs stay rootless), or automatically when the user isn't an admin and the worker isn't already installed as LaunchDaemons (`--rootless=false` opts out). `retire-launchagents` keeps the agents it installed, and `grove doctor` checks agents instead of daemons and reports the missing firewall as a warning. LaunchAgents only run while the user is logged in at the Mac, so it needs auto-login or a FileVault unlock; see docs/INSTALL.md, "A worker with no root".

- **The test suite no longer changes the host's DNS settings.** Fleet tests that ran the macOS VM startup script with `/bin/sh` called the real `networksetup -setdnsservers` on the developer's machine, resetting every network service (Tailscale's included). Every such test now gets a PATH directory whose host-mutating binaries (`networksetup`, `pfctl`, `launchctl`, `dscl`, `sudo` and the rest) fail loudly unless the test fakes them, and a guard test rejects any test that runs a shell without it.

- **A worker no longer needs the admin's Homebrew to get Tart.** When Tart is missing and Homebrew
  belongs to another user, `grove install --role worker` installs the pinned Tart 2.41.1 release
  tarball (SHA-256 checked; bundle id, signature and team verified before it is linked) to
  `~/.local/lib/grove/tart.app` and `~/.local/bin/tart`, and the `brew-trust:openai/tools` step
  passes instead of failing. That grove-managed bundle is also a source for the headless Tart copy,
  after Homebrew's and before the `tart` on PATH, so a re-run heals without `tart` being on PATH.
  The doctor's `headless-tart` wording no longer names Homebrew only.

## v0.1.23

Also covers v0.1.21 and v0.1.22, which were tagged without a changelog section.

- **`grove install --role worker` heals a worker whose Tart is not from Homebrew.** The
  `tart-headless-app` step took its source only from `brew --prefix tart`, so a worker running as
  its own user on another user's Homebrew, with Tart installed as the upstream `tart.app`, failed
  it. It now falls back to the bundle the `tart` on PATH resolves into (never grove's own copy or
  shim), with the same bundle-id, signature and team checks, and re-makes the copy when either
  source changes. The `brew-trust:<tap>` steps (openai/tools, hashicorp/tap) are satisfied without
  asking Homebrew when the binaries the tap gates (tart, softnet, nomad) are already on PATH.

- **`grove install` no longer piles up duplicate backups.** `backup-legacy-worker` skips a file a
  timestamped backup already holds byte-for-byte, creates the backup directory only when it saves
  something, and prunes `YYYYMMDD-HHMMSS` backup directories that only repeat an older one (the
  oldest stays; other names such as `manual-*` are never touched). A re-run reports `[ok]`.

- **`grove install` downloads Orchard instead of building it, so a Mac without Go can be a worker.**
  Every grove release now also carries `orchard_<os>_<arch>.tar.gz` for darwin/linux on
  arm64/amd64: the Orchard fork at the pinned `OrchardForkCommit`, built by the release workflow
  (plain Go, no cgo) and listed in the release's `checksums.txt`. `grove install` (worker and
  control plane) fetches the asset of its own version, verifies its SHA-256 and installs it; it
  builds from source only when there is no asset (a dev build, or a release from before this)
  and Go is present, and otherwise says what to install. Takes effect with the first release cut
  after this change.

- **Snooze a worker: a timed pause that resumes by itself, with jobs queuing instead of failing.**
  `grove worker snooze [--for 2h | --until 18:00] [--now] [--note ...]` (and `wake`, MCP
  `grove_snooze_worker`/`grove_wake_worker`, a Wake button in the UI) pauses the Mac you work on
  for up to 24 hours. Its VMs drain and are removed, then rebuilt when the snooze ends. Workers
  of a pool are interchangeable, so a snoozed one never holds work: jobs go to whichever other
  worker frees up first, wait for the snooze only when no other worker could run them (accepted
  and `pending` with `pendingReason` and `queuedUntil`, never refused), and a snooze can never
  get a queued job lost to the 30 minute pending timeout. `--now` requeues build/shell jobs
  elsewhere (agent jobs keep running, reported as `leftRunning`). Sealed pools stop and rerun the
  job on another machine as an `evicted` attempt, which is not a loss. Control-plane state in
  `pauses.json` records why a worker is paused (snooze, health, operator), so a snooze ending
  never undoes another pause; `grove worker resume` no longer ends a snooze. New scope
  `worker:snooze:<worker>`, granted to a Mac when it joins. **Fixed docs:** a paused worker's VMs
  were always drained and deleted, not left running. Deploying this needs a `grove serve`
  restart; to snooze from the Mac itself, issue the scoped credential (docs/OPERATIONS.md).

- **The worker runs Tart as a background app, so "Tart" no longer flashes in the Dock.**
  Homebrew's `tart.app` is a foreground app that macOS checks into the logged-in user's session on
  every `tart run`. `grove install --role worker` now makes `~/Applications/Grove Tart.app`, a
  copy of it (verified: bundle id, signature, Cirrus Labs team) with `LSUIElement` set and
  re-signed ad hoc with only the virtualization entitlement, and puts it first on the worker
  wrapper's `PATH` through a shim; the wrapper exits 78 if it is missing. Bridged networking is
  unavailable in the copy (grove never uses it). Re-run the install after `brew upgrade tart`;
  `grove doctor` gains a `headless-tart` check; `grove uninstall` removes the copy. Deploying this
  needs `grove install --role worker` on each worker Mac.

## v0.1.20

- **`grove install --role worker` heals an existing worker in place.** It keeps the worker's
  Orchard name and labels (from its recorded identity, grove's wrapper, a hand-written
  `run-worker.sh`, or an old LaunchAgent) instead of renaming it to the hostname, so a Mac Studio
  running `--name studio --labels host=studio` no longer orphans the pools that select
  `host: studio`. It re-renders the current launch shape (wrapper with the guest-agent transport,
  LaunchDaemons), then removes what that supersedes: the old LaunchAgent, `run-worker.sh`,
  `worker-bootstrap` (its token moves to the credential file), and another account's earlier
  per-user stack for the same worker, all backed up first under
  `~/.config/grove/backups/<timestamp>/`. A second run with nothing to change touches and restarts
  nothing. New `--name`/`--labels`/`--force`: a rename on the control plane host rewrites
  `fleet.yaml`'s `workerSelector`s and deletes the old Orchard worker record; elsewhere it refuses
  and prints the exact change. launchd plists now refuse a non-absolute program (a bare
  `nomad`/`grove` exits 78). `grove doctor` gains a `worker-shape` check that points at the re-run.

- **Jobs that can never be placed fail in minutes, not after 30.** When a job's pool shows no
  eligible Nomad node for 3 minutes and the fleet reconciler knows why (enrolment erroring, every
  VM of the pool boot-looping, no worker matching the pool), the job ends `failed` with that
  reason (e.g. `pool linux has no ready node: enrollment failing: enrol linux-studio-0: ... 403`).
  The 30-minute `no allocation` timeout remains for busy fleets. `grove status` and `grove doctor`
  now report a Nomad token that is not a management token (403) and, per worker, VMs that are
  boot-looping, with counts and last start.
- **`grove setup --user` works on an admin-owned Homebrew.** A worker user no longer needs to own
  Homebrew to trust a tap that is already tapped (trust is per user); grove only taps when the tap
  is missing. A `brew trust` that records the trust but exits non-zero (it can't refresh Homebrew's
  API data on someone else's prefix) now counts as trusted.
- **`grove uninstall --role worker` removes a worker from its Mac.** One root script (one sudo, or
  one block for an admin) boots out and removes the Orchard worker, broker and VM firewall
  LaunchDaemons and flushes the `com.apple/grove-vm` pf anchor (`--restore-sleep` also re-enables
  sleep); then the worker user's credentials, wrapper, staged plists, Orchard app, `orchard`
  symlink and logs go. `config.yaml` stays unless `--purge`. `--user <name>` does it from an admin
  account for a standard worker user, rendering the root script itself; `--dry-run` lists the
  steps, and a re-run after a full uninstall is a no-op. It prints the `orchard` and `grove access`
  commands that make the control plane forget the worker.
- **An admin sets a worker up for a standard user in one command: `grove setup --user argos`.**
  Run from an admin account, it asks for the admin's sudo password once (and keeps the sudo
  session alive through the join-approval wait), joins, runs the worker install as `argos` through
  `sudo -n -u argos` with the join result and registry token on stdin (never argv), then shows and
  runs the root script and waits for the worker. No `su`, no block to paste, no second run. The
  admin's grove renders that root script itself from `argos`'s name and home, the grove binary and
  its own join result; nothing `argos`'s process prints or stages becomes root script text, and the
  staged plists stay SHA-256 pinned.
- **Re-running `grove setup --user` repairs instead of re-joining.** When the user's worker is
  already installed (its root-owned LaunchDaemons say so), setup skips the join, keeps the existing
  credentials, re-checks every step, and runs only the root steps that fail their check (none, and
  no sudo beyond the first prompt, if all is well). `--rejoin` joins again; a `--server` that
  differs from the installed control plane needs it.
- **Root-owned plist copies stay private until they pass their hash check.** The root script now
  copies each staged LaunchDaemon plist in with mode 0600 and makes it 0644 only after its SHA-256
  matches, so a staged plist that is a symlink to a root-only file can no longer expose that file's
  contents, even briefly.

## v0.1.19

- **Joining a worker Mac is one command with at most one password prompt.** `grove setup` and
  `grove install --role worker` used to print the privileged steps (`disable-sleep`,
  `worker-daemons`, `host-firewall`, and `softnet-root`) as separate `[manual]` sudo commands for an
  admin to paste, then needed a re-run. They are now collected into ONE root script that runs
  through a single `sudo` when the user is an admin on a terminal and answers yes (`--yes` skips the
  question; without a terminal it needs cached sudo credentials). If sudo isn't possible or is
  declined (a standard user such as `argos`, no terminal, or the new `--no-sudo`), grove prints the
  same script as one copy-pasteable `sudo /bin/sh -e <<'GROVE_ROOT_STEPS'` block for an admin, and
  you re-run the same grove command afterwards (`grove setup` stops there instead of waiting for a
  worker that can't be online yet). The script uses only absolute system binaries and carries no
  secret; the host-firewall plist is now SHA-256 pinned after the root-owned copy, like the
  daemons'. `grove doctor` remediations point at re-running setup instead of printing sudo lines.
- **A second macOS user can run the worker install (`su - argos`).** The `brew-trust:<tap>` check
  now asks `brew tap-info` with `HOMEBREW_NO_AUTO_UPDATE=1` and `HOMEBREW_NO_INSTALL_FROM_API=1`,
  so a user who does not own Homebrew no longer fails on its lock files and mistakes a trusted tap
  for an untrusted one. When a tap does need tapping or trusting and Homebrew belongs to another
  user, the step stops with the exact `brew tap X && brew trust X` to run as that user, instead of
  a permission error halfway through. `registry-login` no longer fails when `tart login` cannot
  use the Keychain ("User interaction is not allowed", as in a `su -` shell): it warns and keeps
  the login in the 0600 credentials file the worker daemon reads. Other `tart login` errors, such
  as rejected credentials, still fail. `grove doctor` accepts that file as the registry login.

## v0.1.18

- **Worker Macs serve jobs with nobody logged in.** The Orchard worker and the credential broker
  ran as per-user LaunchAgents, so a worker Mac did nothing after a reboot until someone logged
  in, and `grove setup` over `su - <user>` had no GUI session to load them into. They are now
  LaunchDaemons with `UserName` set to the installing user: they start at boot, and a dedicated
  standard user such as `argos` can run the worker (verified: Tart boots a macOS guest from a
  `UserName` daemon with no GUI session). The install stages the plists and a new privileged
  `worker-daemons` step, printed for an admin like the VM firewall, installs them root-owned into
  `/Library/LaunchDaemons`, checks they run as the worker user, loads them and removes the old
  LaunchAgents (`retire-launchagents` removes any that remain). The install refuses to run as
  root. Re-run `grove install --role worker` on each worker and do the printed `worker-daemons`
  command; `grove doctor` has a new `worker-daemon` row and checks the broker's daemon.
  The worker's bootstrap credential, the broker's credential and the registry login move from the
  login Keychain, which a boot-time daemon can't unlock, to 0600 files under
  `~/.config/grove/credentials` (re-running the install migrates them; the Keychain items are left
  in place and `grove broker` falls back to its item). Tart gets the registry login from
  `TART_REGISTRY_USERNAME`/`TART_REGISTRY_PASSWORD`, set by the worker's wrapper from that file. Restart a worker's
  services with `sudo launchctl kickstart -k system/com.grove.<name>`. `grove setup` from a shell
  with no Keychain warns instead of failing.

- **macOS VMs replace admin/admin again.** The startup script's first block changed `admin`'s
  password with `dscl . passwd /Users/admin <new>`. `admin` holds the image's SecureToken, and
  macOS (26.6 here) refuses even root that change without the old password (`eDSAuthFailed`), so
  the image password stayed, the block failed closed, and Orchard restarted the VM every few
  seconds. The line now names the old password too (still on stdin, never argv).

- **Re-running the worker install keeps the worker's name.** `grove install --role worker` named
  the worker after the hostname, so it renamed a worker installed under another name. That
  includes a Mac whose worker LaunchAgent predates grove's wrapper, which takes its name
  from `--name` inline. Its pools' `workerSelector` then matched nothing. The install now keeps
  the name already installed (from the wrapper, else the old LaunchAgent), and `--name` sets it
  explicitly.

## v0.1.17

- **Workers reach their VMs without Local Network permission.** On macOS 15+ the worker's SSH to
  a VM (startup script, exec, enrolment) needs Local Network permission, which grove's worker
  Macs keep off, so every startup script failed with `no route to host` and Orchard restarted
  the VM every few minutes. The worker now runs Orchard (gm2211/orchard#9) with
  `--guest-agent-transport`, which carries those connections through `tart exec` and the Tart
  Guest Agent on the VM's control socket instead of the network. Re-run `grove install --role
  worker` on each worker Mac to pick it up. The Linux image installs `netcat-openbsd` and checks
  that the guest agent is enabled.

- **The control-plane install no longer takes the control plane down.** Its launchd plists and
  systemd units named `nomad` and `grove` bare, and launchd/systemd start jobs with a PATH that
  has neither `~/.local/bin` nor Homebrew, so Nomad exited 78 and `grove serve` never started.
  Units now name the absolute path (this shell's PATH, then `~/.local/bin`, then where the
  install puts it). Relaunching a service also waits for launchd to finish unloading it before
  `launchctl bootstrap`, which failed with "5: Input/output error" on a still-loaded job.

- **`grove registry login` logs an installed worker in to the image registry.** `pbpaste | grove
  registry login --user <github-username>` runs `tart login` with the token over stdin (or from
  `$GROVE_REGISTRY_TOKEN`, never an argument) and logs in again even for the same user, so it is
  also how a token is rotated. Re-running `grove install --role worker` for that would skip the
  login once the marker matched.

- **Re-running `grove install --role worker` keeps the installed worker.** Without
  `--controller`/`--token` it reuses the controller URL in the existing wrapper and the credential
  already in Keychain. Before, it rewrote the wrapper with an empty controller and restarted the
  worker, which then couldn't reach Orchard. With no worker installed, it now refuses and asks for
  `--controller`.

## v0.1.16

- **Credentials can expire, and a local client can use a Unix socket.** `grove access issue
  --expires-in 180d` (`expiresIn` over HTTP) issues a credential that stops working after that
  long; `grove access list` and `GET /api/v1/whoami` show when. `server.socket` and
  `server.socketUsers` make `grove serve` also listen on a Unix socket that only those macOS
  users can connect to (checked against the peer the kernel reports), with a bearer token still
  required on every request. This is how Argos on the control-plane Mac talks to grove.

- **Every worker runs the credential broker.** Approving a Mac's `grove setup` now also issues it
  a credential with only the `broker` scope, named after the worker. Setup keeps it in Keychain
  and loads a `com.grove.broker` LaunchAgent running `grove broker`, which reads it from there.
  Rerunning setup replaces it and revokes the old one. `grove doctor` gets a `broker` row, and the
  broker's `/healthz` now says whether its last sync worked.

- **Jobs can carry credentials they never see.** `JobRequest.credentials` (`kind`, `env`, `token`)
  gives the job a `gph1_…` placeholder under `env`, keeps the token in the control plane's
  memory only, and serves it to the broker on the worker the job lands on, until the job ends.
  `PUT /api/v1/jobs/{id}/credentials` puts fresh tokens behind the same placeholders. Refused on
  sealed pools. `grove broker --listen` now takes an IP only; the port is always 3128.

- **Credential broker (not wired into dispatch yet).** `grove broker` runs on a worker Mac as the
  proxy its job VMs use. A job gets a placeholder (`gph1_` and 256 random bits) instead of a real
  token; on requests to GitHub, Anthropic or OpenAI/ChatGPT the broker swaps it for the real token
  in the `Authorization`/`x-api-key` header only, and masks that token (raw, URL- or JSON-escaped,
  base64) in the response. Everything else is tunnelled, to public addresses only. Entries live in
  memory, come from the control plane (`GET /api/v1/broker/entries`, a credential with only the new
  `broker` scope, named after the worker) and die when the job ends or a 10-minute lease runs out.
  Dispatch, the VM side and the worker install follow in later changes.

- **grove only listens on the tailnet.** `grove serve` refuses to start when `server.listen` is a
  wildcard (`0.0.0.0`, `:6120`) or resolves to anything but loopback or a Tailscale address, and
  `grove doctor` has a `grove-listen` row for it. Fix with `grove config set server.listen
  <tailnet-ip>:6120`.

- **No authentication configured now means refused, not open.** An API server with no token,
  device store or Tailscale auth answers every `/api/v1` request with 503 instead of treating the
  caller as the operator. `grove serve` always has a device store, so a normal install is
  unaffected; this closes the path where a relocated or half-configured control plane runs open.

- **Pull-request builds of the runner image get no package token.** `images.yml` builds a PR's
  `images/runner` without pushing and with `contents: read` only; `packages: write` is now
  limited to the push-to-main job.

- **`allowDockerSocket` is gone.** Mounting the VM's docker socket gave a job root on the guest,
  and with it a way past the network fence. Every Linux job now runs in its pool's `runnerImage`;
  the per-job `image` meta is ignored on every pool. A `fleet.yaml` that still sets
  `allowDockerSocket` is refused at load: delete the line. The Linux worker image also turns off
  Docker host volumes (takes effect when the image is rebuilt).

- **macOS clones are fenced too.** A macOS job's `source` task stays root (to hide the clone token
  from the job) but runs every git command as a new unprivileged `_grovesrc` user, which the
  guest's pf fence keeps off home networks alongside the job user. The token reaches git's
  credential helper through a file only `_grovesrc` can read, removed when the task ends. A macOS
  VM made before this release fails its clones until it is recreated with the new startup script.

- **Jobs reach the internet, never home.** Every shared pool's guest now fences its jobs off
  private, tailnet (CGNAT) and link-local addresses: Linux job containers in `DOCKER-USER` (the
  sealed pools' fence, plus DNS to the guest's resolvers), macOS jobs with a pf anchor on the job
  user, reloaded at boot. The guest's own Nomad client and Tailscale are unaffected. There is no
  opt-out. The startup script changes, so every shared VM is recreated once on upgrade.

- **Worker Macs no longer authenticate by Tailscale identity.** A shared pool's jobs reach the
  control plane through their host Mac's network, so they arrived as the Mac's identity, which
  could carry its owner's operator grant. grove now refuses Tailscale identity auth from any
  Orchard worker's Mac (by MagicDNS name, failing closed until the worker list loads); workers use
  their Keychain device credential. `server.tailscale.trustWorkerHosts: true` restores the old
  behaviour. docs/OPERATIONS.md no longer suggests granting a user login `operator`.

- **A web page can no longer act as your machine's Tailscale identity.** State-changing API
  requests a browser sends from another site (`Sec-Fetch-Site`, or an `Origin` that doesn't match
  `Host`) are refused with 403, and tailnet-identity auth is only accepted on the control plane's
  own names (an IP, a MagicDNS name, `localhost`, or `server.url`'s host), so a DNS-rebinding page
  can't read or dispatch either. The CLI, argos and the same-origin UI are unaffected.

- **Idempotency keys are per submitter.** Another caller reusing a key gets its own job instead of
  being handed (or blocking) the original caller's.

- **Elastic pools: VMs follow the queue.** A pool with `maxPerWorker` (and optional
  `minPerWorker`) gets one more VM while its jobs wait unplaced, on the roomiest matching worker
  (Orchard VM slots, cores, memory, and at most 2 macOS VMs per Mac). If no worker has room, an
  idle VM of another elastic pool with no waiting jobs is swapped out for it, at most once per
  worker every 15 minutes. VMs idle for 15 minutes above the minimum are removed. `grove fleet
  plan` and the serve log give the reason for each change. Pools without `maxPerWorker` keep
  exactly `perWorker`. See ARCHITECTURE.md "Elastic pools".

- **Linux jobs fall back to macOS.** A job a shared Linux pool can't place moves to the shared
  `macos` pool: after 20 seconds when the pool has no node at all, after 3 minutes when its nodes
  are busy. The job keeps its id and gains `fallback: {from, to, at, reason}`. Jobs whose last 3
  runs on macOS all failed (same kind, pool and repo) stop falling back for 7 days and wait for
  Linux. Opt out per job with `pinPool: true`, per pool with fleet.yaml `fallback: none`. See
  docs/JOBS.md "Falling back to macOS".

- **A boot-looping VM is rebuilt too**, not only a failed one: a VM Orchard has restarted 3 or
  more times (`fleet.BootLoopRestarts`) is deleted and recreated, at most once per 10 minutes.

- **A failed `tailscale up` no longer stops a shared-pool VM's startup script.** Under `set -eu` an
  expired or revoked `tailscale.authKey` ended the script before Nomad restarted with the VM's
  meta, so the VM's Nomad node had no `meta.pool`, took no job, and Orchard restarted the VM
  forever (40,426 times on one Linux worker). The guest now logs the failure and carries on;
  it reaches the control plane through its host's NAT anyway. This changes the generated Linux
  startup script, so existing Linux VMs are recreated once.

- **A job marked lost for never being placed now stops its Nomad job.** It used to stay queued in
  Nomad and run, unwatched, as soon as a matching node joined. `grove serve` also stops the
  leftover Nomad jobs of such lost jobs once at startup.

- **A failed VM is rebuilt instead of holding its slot forever.** Orchard's restart policy never
  touches a VM whose start failed, and the reconciler treated it as up to date, so a pool's only
  VM could sit "failed" for weeks while every job for that pool ended "no allocation within 30m".
  The reconciler now deletes and recreates a VM that has been failed for 10 minutes
  (`fleet.FailedVMRetry`), at most once per 10 minutes, and logs Orchard's failure message as the
  reason.

- **Down Nomad nodes are purged.** Each VM boot registers a new Nomad node and the old one stayed
  "down", so a VM stuck restarting left thousands behind (and `/fleet` fetches every node).
  `grove serve` now purges nodes that stay down for 10 minutes; a live node simply re-registers.
  `grove status` shows nodes by status (`3 (down 1, ready 2)`) instead of one total.

- **A Nomad 403 from the node-token sweep is logged once an hour, with the fix**, instead of every
  30 seconds. It means `nomad.token` in `config.yaml` isn't a management token; per-VM node
  tokens are then never revoked.

- Sealed jobs (`grove sandbox run`) no longer depend on the guest's `timeout(1)`. `run.sh` keeps
  the time limit itself (TERM at the limit, KILL 10 seconds later, exit 124) and runs the script in
  its own process group, so a cancel reaches it exactly once and reports the job's own exit
  status. uutils' `timeout`, Ubuntu 26.04's default, exits 124 or dies leaving the job running
  when it is TERMed; this also fixes CI's `TestRunMacOS_Cancel` on `ubuntu-latest`.

- **Tailnet callers no longer need a grove token.** A request with no `Authorization` header from
  a Tailscale address is authenticated by `tailscale whois` on its peer address and gets the scopes
  the tailnet policy grants it through the `github.com/gm2211/grove` app capability, or that
  `server.tailscale.grants` in `config.yaml` gives its tag or login. Bearer tokens and device
  credentials keep working unchanged, and a token, when sent, always wins. Off with
  `server.tailscale.disabled: true`. See docs/OPERATIONS.md "Tailscale identity instead of tokens".

- **Named job secrets.** `JobRequest.secrets` was reserved and rejected; it now resolves names an
  operator set with `grove secret set NAME --allow tag:argos` (or `PUT /api/v1/secrets/{name}`)
  into the job's environment at dispatch. The submitter never handles the value and job history
  keeps only names. A non-operator may name only secrets granted to its id or Tailscale tags. See
  docs/OPERATIONS.md "Named secrets for jobs".

- **MinIO is gone: build artifacts are kept in a plain directory on the control plane**
  (`artifacts.dir` in `config.yaml`, default `~/.local/state/grove/artifacts`, next to `jobs.json`).
  Only the control plane ever read or wrote artifacts, so there is no longer an artifact service,
  port, bucket or credential: `grove sandbox run` stores what it copies out of a sealed VM
  (files 0600 in 0700 directories, written by atomic rename, paths confined to the directory), and
  `grove serve` serves and expires them exactly as before (15 min / 1 h retention, same API).
  `grove install --role control-plane` no longer installs MinIO or its client; re-running it on an
  existing control plane stops and removes the old MinIO service, drops the `artifacts.endpoint`/
  `bucket`/`accessKey`/`secretKey` keys from `config.yaml` and sets `artifacts.dir`. Until then an
  old `config.yaml` still loads and `grove serve` just warns that those keys are ignored. MinIO's
  data under `~/.config/grove/minio` is left for you to delete (artifacts are not migrated). Shared
  (non-sealed) pools' build template no longer tries to upload with `mc`, and the runner image no
  longer ships `mc`: those builds don't collect artifacts for now. See docs/OPERATIONS.md
  "Artifacts".

- macOS pools can be sealed too (`network: isolated` on the `macos` pool). The VM runs on Softnet
  with the host blocked, public DNS on every network service, and its Nomad client and Tailscale
  turned off, exactly like a sealed Linux VM. With no container runtime in the guest, `grove
  sandbox run --os darwin` runs the job as the unprivileged `_grovejob` user in a fresh run
  directory, and every sealed macOS VM runs **one job only**: once the job is over, grove deletes
  the VM and the fleet reconciler recreates it fresh from the image, so expect a VM boot before
  each job. Timeouts, cancellation, lost VMs, logs and build artifacts work as on sealed Linux
  pools. See docs/OPERATIONS.md "Isolating a pool's network".

- Build artifacts now expire: `grove serve` deletes a job's artifacts 15 minutes after it succeeds
  and 1 hour after it fails, is canceled or is lost (`artifacts.retention.success`/`.failure` in
  `config.yaml`). Expired jobs report `artifactsExpiredAt`. The first sweep after upgrading deletes existing
  artifacts that are already past those windows. See docs/JOBS.md "Artifact retention".

## v0.1.15

- **Security:** re-running `grove setup` on a Mac no longer leaves its earlier device credentials
  valid. Once the Mac is back online with its new credential, grove revokes the older setup
  credentials with the same name. Operator-issued credentials (`grove access issue`) are untouched.

- Re-running `grove install --role control-plane` restarts Nomad when its config changed, so a new
  config takes effect.

## v0.1.13

- **Security:** the control plane's Nomad runs with ACLs on. `grove install --role control-plane`
  bootstraps them and stores the management token in `config.yaml` (`nomad.token`); re-running it
  migrates an install that had ACLs off (restarts Nomad once). Each VM the fleet reconciler creates
  gets its own node-only Nomad token, in its shutdown script's environment only, so it can drain
  itself on recycle; the token is revoked once the VM is gone. The VM's Nomad client enforces ACLs
  too. Existing VMs pick this up on their next recycle. See docs/OPERATIONS.md "Nomad ACLs and node
  tokens".

- **Security:** job history no longer keeps `env` values: the stored job (and every job the API
  returns) lists each key with the value `[redacted]`. `secrets` is rejected until grove can
  resolve named secrets, instead of being accepted and silently ignored.
- **Security:** the private-repo clone token no longer reaches a job's script. build and agent
  jobs clone in a separate `source` prestart task that alone receives the token (in the dispatch
  payload, never `env_json`), passes it to git only through its own environment, and deletes it
  before `main` starts. A failed clone still shows in the job's logs and exit code. Scripts that
  relied on run.sh's `gh auth setup-git` must set up git auth themselves.
- **Security:** macOS jobs no longer run as root. The fleet startup script creates a hidden,
  passwordless `_grovejob` account (home `/private/var/grove-job`) and the job's `main` task runs
  as it.
- **Security:** `grove github enable` and `PUT /api/v1/github/sourcing` require at least one
  repository; a credential armed earlier with no repositories named clones nothing until it is
  re-armed. Only `https://` repo URLs are accepted for build/agent jobs, refs may not start with
  `-`, and env keys must be shell variable names.
- **Security:** `grove install` no longer installs anything it downloaded unverified. Nomad
  (Linux, now pinned to 2.0.7 instead of "latest"), MinIO (Linux, `RELEASE.2025-09-06T17-38-46Z`),
  the MinIO client and Homebrew's installer (pinned commit instead of `HEAD`) are checked against
  SHA-256 digests embedded in grove and refused on mismatch. `scripts/install.sh` verifies the
  Linux grove tarball against the release's `checksums.txt` (`GROVE_VERSION` pins a release) and
  pins Homebrew's installer the same way. See docs/INSTALL.md "Verified downloads".
- **Security:** the control plane's artifact credential in `config.yaml` is now a MinIO user
  limited to the `grove` bucket (`grove-artifacts` policy), not MinIO's root credential. Re-running
  `grove install --role control-plane` migrates an existing install and reloads `grove serve`.
- **Security:** `grove setup` shows the control plane it discovered and asks for confirmation, or
  a choice when several answer, instead of joining the first peer on :6130. Unattended runs need
  `--server URL`, or `--yes` when exactly one control plane answers.
- `fleet.yaml` pools take `network: isolated`, which seals their VMs: Softnet with the host Mac
  blocked, public DNS, a fence for job containers, and no Nomad client, Tailscale login or
  credential inside. A sealed VM reaches the public internet and nothing at home. Its pool's jobs
  run from the control plane instead (`grove sandbox run`, whose Nomad client the control-plane
  install now enables): it clones there, pushes the checkout into a fresh container in a sealed VM,
  streams its logs, polls it, and pulls its artifacts out. A job whose VM stops answering is
  reported lost. Linux pools only. See docs/OPERATIONS.md "Isolating a pool's network".
- `grove install --role worker --softnet` installs Softnet and prints the one-time command that
  lets it become root; `grove doctor` reports whether it can.
- `grove config set <key> -` reads the value from stdin, so secrets stay out of shell history.

## v0.1.11

- Fleet page lists the Macs waiting to join, with an **Approve** button beside each, so enrolling
  a Mac no longer means reading its code off one machine's terminal and retyping it on another
  (`GET /api/v1/join/pending`, operator scope).
- `GET /join` serves an unauthenticated page with the install commands, this control plane's
  address, and a QR code of its own link (`GET /join/qr.svg`) — so a Mac that has never met grove
  can be told what to run without anyone typing a tailnet address.

## v0.1.2

- README overhaul: screenshots, real command examples, Mermaid flow diagrams (`docs/FLOW.md`).
- `grove install` trusts third-party Homebrew taps (`brew trust`) before installing; `grove doctor`
  reports untrusted taps.
- Tart images built and published for the first time: `ghcr.io/gm2211/grove-linux-worker`
  (Ubuntu arm64, Nomad 2.0.5, Docker 29.8, Tailscale) and `ghcr.io/gm2211/grove-macos-worker`
  (macOS Sequoia + Xcode 16.4, Nomad, Tailscale). Template fixes: macOS disk ≥ 140 GB base,
  Homebrew on PATH in Packer's non-login SSH shell.

## v0.1.1

Fixes found by running the whole stack live (single-host smoke stack: `orchard dev --synthetic`,
`nomad agent -dev`, `grove serve`) after v0.1.0:

- `grove serve` now runs the fleet reconciler loop, so VMs deleted by Orchard on TTL expiry are
  recreated (`--fleet-reconcile-interval`, `--no-fleet-reconcile`; `GET /api/v1/fleet/reconcile`).
- `JobRequest.timeout` accepts a duration string (`"30m"`) or a number of seconds — never
  nanoseconds. Sub-second timeouts are rejected.
- Log streaming: fixed a race in the Nomad client that dropped buffered log frames; `--follow`
  waits for the allocation, streams, and exits with the job's exit code; follow on a finished job
  returns immediately instead of hanging.
- Jobs whose Nomad job vanished, or that never got an allocation within 30m, are marked `lost`
  with a reason; pending jobs surface Nomad's blocked-evaluation reason.
- Orchard VM labels are scheduler selectors: pool/host are derived from the VM name and worker;
  drift is detected field-by-field. Without this every VM stayed `pending`.
- `run.sh` uses a portable timeout (macOS has no GNU `timeout`); macOS Nomad clients get
  `cpu_total_compute` set (Apple Silicon fingerprints ~24 MHz otherwise).
- Job resources come from `fleet.yaml` pool defaults (`jobCPU`/`jobMemory`); docker-socket
  mounting is opt-in per pool (`allowDockerSocket`).
- UI streams logs via fetch + NDJSON with the bearer header (EventSource sent the token in the URL).
- Install: `--roles` passed per value, Tart from `openai/tools`; grove is its own Homebrew tap.

## v0.1.0

First usable cut of grove: turn Macs (and a Linux box) you already own into a private CI/agent
cloud, reachable only over Tailscale.

**What works today**

- Fleet reconciler that keeps Orchard VMs matching `fleet.yaml`, including per-VM TTL and
  shutdown-hook support.
- Parameterized Nomad jobs for `build` / `agent` / `shell` kinds — dispatch, status, and log
  streaming (plain, SSE, and NDJSON) end to end.
- Job reconciliation against Nomad detects orphaned and stuck jobs: a dispatched Nomad job that no
  longer exists (e.g. after a Nomad server restart with a wiped data dir) or one that's sat pending
  longer than a configurable timeout is now marked `lost` with an explanatory `failureReason`,
  instead of staying `pending` forever. A background sweep (`grove serve`, every 30s by default)
  reconciles every non-terminal job so this doesn't depend on something polling `Get`.
- Pending-placement diagnostics: a pending job's status now surfaces *why* Nomad hasn't placed it
  yet (constraint filtered, resources exhausted, node class filtered, or no nodes available),
  derived from Nomad's own blocked-evaluation metrics — in `grove jobs get`, the HTTP API, and the
  job detail page in the UI.
- HTTP API (`/api/v1/...`) with token auth, plus an embedded React/Vite control-plane UI (fleet
  view, job list, job detail with live logs).
- MCP server (stdio) so coding agents can dispatch and follow grove jobs directly.
- `grove install` role bootstrap planner (`worker` / `control-plane` / `client`) and `grove doctor`
  health checks.
- Agent-orchestrator executor contract — see [docs/ORCHESTRATOR.md](docs/ORCHESTRATOR.md).

**Not yet**

- Only live-tested on a single-host smoke stack — not yet run against a real multi-Mac fleet.
- Tart VM images aren't published to `ghcr.io` yet; build your own from `images/`.
- Per-dispatch CPU/memory sizing (`JobRequest.Resources`) is validated against pool defaults but
  not yet applied to the running job's actual resource allocation — see
  [docs/JOBS.md](docs/JOBS.md) "Per-job resource sizing".
