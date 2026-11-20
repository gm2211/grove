# Security

## Reporting a vulnerability

Please report it privately: open a draft advisory at
<https://github.com/gm2211/grove/security/advisories/new>. Don't open a public issue for something
that lets a job, a worker or a tailnet device do more than its credential allows.

Useful in a report: the grove version (`grove version`), the role of the machine involved (control
plane, worker, client), and the smallest steps that show the problem.

## What grove trusts

- The control plane and its operator credential: full control of the fleet.
- A worker Mac: its own VMs and the credentials of the jobs it runs, and nothing of another worker's.
- A job: its own VM, and placeholders in place of real credentials.
- A release: only a build the release workflow of this repository made from a commit on `main`.
  `grove serve` and the worker's broker check that build provenance (a Sigstore attestation) before
  they install an update, as well as the release's checksums.

The design and the properties grove checks are in [ARCHITECTURE.md](ARCHITECTURE.md) and
[spec/README.md](spec/README.md).

## Repository settings this relies on

Self-update trusts `main`, so `main` should be protected: pull requests only, CI required, no
force-push, and review from code owners (`.github/CODEOWNERS`). A tag ruleset limiting who can push
`v*` tags is a second line; the release workflow already refuses a tag whose commit is not on `main`.
