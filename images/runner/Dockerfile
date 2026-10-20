# grove-runner: default container image for linux "build"/"agent"/"shell" jobs dispatched to the
# linux pool. grove serve runs each job in a fresh container of this image (or the job's own
# image) inside a sealed Linux VM; see internal/sandbox.
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

# Docker CLI only (no daemon). Sealed pools mount no Docker socket, so it is inert unless a job
# brings its own daemon; kept so existing runner images and derived images keep building.
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
