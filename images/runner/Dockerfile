# grove-runner: default container image for linux "build"/"agent"/"shell" jobs dispatched to the
# linux pool, and the trusted image a build's `source` step (which holds the clone token) always
# runs in. A build's main step runs this image unless the job names an environment image the pool
# allows (nomad/jobs/build.nomad.hcl, docs/JOBS.md "Job environments"). Jobs get no docker socket.
#
# Supply chain: this image's `source` step holds the private-repo clone token, so what goes into it
# is pinned. The base is a digest (the multi-arch index of ubuntu:24.04, so both platforms resolve
# through it), and Node.js and Go are official tarballs checked against the SHA-256 their projects
# publish, for both architectures. Nothing is piped from the network into a shell. To bump one:
#   base:  the Docker-Content-Digest of registry-1.docker.io/v2/library/ubuntu/manifests/24.04
#          (or `docker buildx imagetools inspect ubuntu:24.04`)
#   Node:  https://nodejs.org/dist/v<version>/SHASUMS256.txt, node-v<version>-linux-{arm64,x64}.tar.gz
#   Go:    https://go.dev/dl/?mode=json&include=all, go<version>.linux-{arm64,amd64}.tar.gz
# The apt packages (Ubuntu's, and gh and docker-ce-cli from their vendors' signed apt repositories)
# are whatever is current at build time.
FROM ubuntu:24.04@sha256:fe81560990fae86dc4743202b2cb4d3df98d0bcf949173cecef15ce1127ef852

ENV DEBIAN_FRONTEND=noninteractive \
    LANG=C.UTF-8 \
    NODE_VERSION=22.23.4

RUN apt-get update -y && apt-get install -y --no-install-recommends \
      ca-certificates \
      curl \
      git \
      gnupg \
      jq \
      python3 \
      python3-pip \
      build-essential \
      unzip \
    && rm -rf /var/lib/apt/lists/*

# GitHub CLI (official apt repo)
RUN mkdir -p -m 0755 /etc/apt/keyrings \
    && curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg -o /etc/apt/keyrings/githubcli-archive-keyring.gpg \
    && chmod go+r /etc/apt/keyrings/githubcli-archive-keyring.gpg \
    && echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" > /etc/apt/sources.list.d/github-cli.list \
    && apt-get update -y \
    && apt-get install -y --no-install-recommends gh \
    && rm -rf /var/lib/apt/lists/*

# Node.js: the official nodejs.org tarball for NODE_VERSION, verified against that release's
# SHASUMS256.txt before it is unpacked. It carries node, npm, npx and corepack; they go to
# /usr/local, with links at /usr/bin where the NodeSource package this replaced had them.
RUN set -eu; \
    arch="$(dpkg --print-architecture)"; \
    case "$arch" in \
      arm64) nodearch=arm64; nodesum=f7a1e486dbc8b286847815b477d4fb5e9fb963ed4489c3e1588853d685f589f9 ;; \
      amd64) nodearch=x64;   nodesum=b8492284c76a54baf58d61668dd0e41ee0fc98adabf9503d30f05d08f55b4406 ;; \
      *) echo "unsupported arch: $arch" >&2; exit 1 ;; \
    esac; \
    curl -fsSL --proto '=https' --tlsv1.2 "https://nodejs.org/dist/v${NODE_VERSION}/node-v${NODE_VERSION}-linux-${nodearch}.tar.gz" -o /tmp/node.tgz; \
    echo "${nodesum}  /tmp/node.tgz" | sha256sum -c -; \
    tar -C /usr/local -xzf /tmp/node.tgz --strip-components=1 --no-same-owner; \
    rm /tmp/node.tgz /usr/local/CHANGELOG.md /usr/local/LICENSE /usr/local/README.md; \
    for tool in node npm npx corepack; do ln -s "/usr/local/bin/$tool" "/usr/bin/$tool"; done; \
    node --version; npm --version

# Go (arch-aware: arm64 workers, amd64 for local/dev builds of this image)
# The tarball is verified against the SHA-256 go.dev lists for it before it is unpacked.
RUN set -eu; \
    GO_VERSION=1.23.4; \
    arch="$(dpkg --print-architecture)"; \
    case "$arch" in \
      arm64) goarch=arm64; gosum=16e5017863a7f6071363782b1b8042eb12c6ca4f4cd71528b2123f0a1275b13e ;; \
      amd64) goarch=amd64; gosum=6924efde5de86fe277676e929dc9917d466efa02fb934197bc2eba35d5680971 ;; \
      *) echo "unsupported arch: $arch" >&2; exit 1 ;; \
    esac; \
    curl -fsSL --proto '=https' --tlsv1.2 "https://go.dev/dl/go${GO_VERSION}.linux-${goarch}.tar.gz" -o /tmp/go.tgz; \
    echo "${gosum}  /tmp/go.tgz" | sha256sum -c -; \
    tar -C /usr/local -xzf /tmp/go.tgz; \
    rm /tmp/go.tgz; \
    /usr/local/go/bin/go version
ENV PATH="/usr/local/go/bin:${PATH}"

# Docker CLI only (no daemon) — used for nested `docker run` against the mounted host socket.
RUN mkdir -p -m 0755 /etc/apt/keyrings \
    && curl -fsSL https://download.docker.com/linux/ubuntu/gpg -o /etc/apt/keyrings/docker.asc \
    && chmod a+r /etc/apt/keyrings/docker.asc \
    && echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/ubuntu noble stable" > /etc/apt/sources.list.d/docker.list \
    && apt-get update -y \
    && apt-get install -y --no-install-recommends docker-ce-cli \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /workspace

LABEL org.opencontainers.image.source="https://github.com/gm2211/grove" \
      org.opencontainers.image.description="Default container image for grove linux build/agent/shell jobs"
