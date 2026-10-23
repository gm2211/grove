# CI: run a repository's jobs on every push and pull request

`grove serve` can watch GitHub repositories and act like GitHub Actions on your own machines:
every new commit on a watched branch, and every new head commit of an open pull request, runs the
jobs in that commit's `.grove.yml`. Each job's result shows up on the commit and the pull request
as a check, `grove/<job>`, that links to the job in grove's UI.

grove asks GitHub for changes; GitHub never contacts grove. There's no webhook and the control
plane needs no public address. The trade-off is that a push is noticed up to one poll interval
late (30 s by default).

## 1. Add `.grove.yml` to the repository

At the root of the repository:

```yaml
jobs:
  test:
    pool: linux          # a pool from fleet.yaml
    timeout: 30m         # optional; Go duration
    env: { CI: "1" }     # optional
    script: |
      make test
  lint:
    pool: linux
    script: make lint
```

- Each job runs as a grove `build` job of that exact commit, in parallel with the others. The
  script runs with `bash -eo pipefail` in the checkout, and anything it puts in `./artifacts/` is
  kept like any build job's artifacts.
- Unknown keys are an error, so a typo is reported on the commit rather than ignored.
- A commit without `.grove.yml` runs nothing and gets no check.
- A `.grove.yml` that can't be parsed is reported as a failed `grove` check, with the reason.

## 2. Give grove a token that can report results

The watcher uses the same token as private-repo sourcing, armed with `grove github enable`
([OPERATIONS.md](OPERATIONS.md#letting-jobs-clone-private-repos-and-turning-it-back-off)). It also
needs to read the repository and write statuses, so give the fine-grained token these
permissions on the watched repositories:

| Permission | Access | Why |
|---|---|---|
| Contents | Read | read branch heads and `.grove.yml`; clone private repositories |
| Pull requests | Read | list open pull requests |
| Commit statuses | Read and write | post each job's result |

```bash
grove github enable --repo owner/name    # no --ttl: CI stops when the token disarms
```

A watched repository that the armed token doesn't cover is skipped. `grove serve` logs one
warning naming it. If the token expires or is disarmed, CI stops until it's re-armed.

## 3. Name the repositories in `config.yaml`

On the control plane:

```yaml
ci:
  interval: 30s            # optional
  repos:
    - repo: owner/name
      branches: [main]     # new commits on these branches run CI
      pullRequests: true   # so does each new head commit of an open pull request
    - repo: owner/other
      host: ghe.example.com   # GitHub Enterprise; omit for github.com
      branches: [main, release]
```

Restart `grove serve` to pick up changes.

## What happens, and what doesn't

- **Each commit runs once.** grove remembers the commits it has seen in `ci.json`, next to
  `jobs.json`, for 14 days. Every job carries the idempotency key
  `grove-ci:<repo URL>@<sha>:<job>`, so a restart never starts a job twice.
- **The first poll of a new repository** runs the current head of each watched branch and of
  each open pull request.
- **Pull requests from forks never run.** Their `.grove.yml` is someone else's script, and it
  would run on your machines.
- **A job that can't be started** (an unknown pool, for example) is retried on the next two polls.
  After that it's reported as a failed check that names the reason.
- **Results:** waiting for a VM and running both show as pending. Passing shows as success. A
  failed script shows as failure, with its exit code. A canceled or lost job shows as error.
- **Rerunning:** submit the job again through the API or CLI with a new idempotency key. A
  "re-run" button is not built yet.
- **API rate limits:** each poll costs about one request per watched branch plus one for pull
  requests. grove sends them as conditional requests, and GitHub doesn't count an unchanged
  answer against the limit.
