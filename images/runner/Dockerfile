# grove-runner: default container image for linux "build"/"agent"/"shell" jobs dispatched to the
# linux pool. Docker CLI (not just the daemon) is included deliberately: the Nomad docker driver
# cannot interpolate its `image` config field from dispatch-time meta (hashicorp/nomad#6247), so a
# per-job image override (`meta_optional` field "image" in the parameterized job contract, see
# nomad/jobs/README.md) is honored by nesting a `docker run` against the host's docker socket
# rather than by changing this container's own image at schedule time.
FROM ubuntu:24.04

ENV DEBIAN_FRONTEND=noninteractive \
    LANG=C.UTF-8 \
    NODE_VERSION=22

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

# Node.js (NodeSource)
RUN curl -fsSL https://deb.nodesource.com/setup_${NODE_VERSION}.x | bash - \
    && apt-get install -y --no-install-recommends nodejs \
    && rm -rf /var/lib/apt/lists/*

# Go (arch-aware: arm64 workers, amd64 for local/dev builds of this image)
RUN set -eu; \
    GO_VERSION=1.23.4; \
    arch="$(dpkg --print-architecture)"; \
    case "$arch" in \
      arm64) goarch=arm64 ;; \
      amd64) goarch=amd64 ;; \
      *) echo "unsupported arch: $arch" >&2; exit 1 ;; \
    esac; \
    curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-${goarch}.tar.gz" -o /tmp/go.tgz \
    && tar -C /usr/local -xzf /tmp/go.tgz \
    && rm /tmp/go.tgz
ENV PATH="/usr/local/go/bin:${PATH}"

# Docker CLI only (no daemon) — used for nested `docker run` against the mounted host socket.
RUN mkdir -p -m 0755 /etc/apt/keyrings \
    && curl -fsSL https://download.docker.com/linux/ubuntu/gpg -o /etc/apt/keyrings/docker.asc \
    && chmod a+r /etc/apt/keyrings/docker.asc \
    && echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/ubuntu noble stable" > /etc/apt/sources.list.d/docker.list \
    && apt-get update -y \
    && apt-get install -y --no-install-recommends docker-ce-cli \
    && rm -rf /var/lib/apt/lists/*

# MinIO client (mc), used by nomad/jobs/build.nomad.hcl's run.sh to upload ./artifacts/**
# Set false only for pools without artifact storage; keeps runners without artifact storage buildable
# when the optional upstream MinIO binary distribution is unavailable.
ARG INSTALL_MINIO_CLIENT=true
ARG MINIO_CLIENT_VERSION=RELEASE.2025-08-13T08-35-41Z
RUN set -eu; \
    if [ "$INSTALL_MINIO_CLIENT" = "false" ]; then exit 0; fi; \
    arch="$(dpkg --print-architecture)"; \
    file="mc.linux-${arch}.${MINIO_CLIENT_VERSION}"; \
    base="https://github.com/minio/mc/releases/download/${MINIO_CLIENT_VERSION}"; \
    mkdir /tmp/grove-mc; \
    cd /tmp/grove-mc; \
    curl -fsSL "${base}/${file}" -o "$file"; \
    curl -fsSL "${base}/${file}.sha256sum" -o checksum; \
    sha256sum -c checksum; \
    install -m 755 "$file" /usr/local/bin/mc; \
    cd /; \
    rm -rf /tmp/grove-mc

WORKDIR /workspace

LABEL org.opencontainers.image.source="https://github.com/gm2211/grove" \
      org.opencontainers.image.description="Default container image for grove linux build/agent/shell jobs"
