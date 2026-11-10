# Agent sandbox image for Rollback Guardian.
# Everything an agent needs to build, lint, test, and run integrations, with
# no access to the host Docker daemon or host files outside /workspace.
FROM docker:29-cli AS dockercli
FROM node:24-bookworm-slim AS node
FROM ghcr.io/astral-sh/uv:latest AS uv

FROM golang:1.26-bookworm

ARG USER_UID=1000
ARG USER_GID=1000
ARG GOLANGCI_LINT_VERSION=v2.10.1

RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates curl git jq less make postgresql-client procps ripgrep sudo unzip \
    && rm -rf /var/lib/apt/lists/*

# Docker CLI + compose plugin (talks to the dind sidecar, never the host daemon).
COPY --from=dockercli /usr/local/bin/docker /usr/local/bin/docker
COPY --from=dockercli /usr/local/libexec/docker/cli-plugins /usr/local/libexec/docker/cli-plugins

# Node (for Claude Code and npx-launched MCP servers) and uv (for uvx-launched ones).
COPY --from=node /usr/local/bin/node /usr/local/bin/node
COPY --from=node /usr/local/lib/node_modules /usr/local/lib/node_modules
RUN ln -s ../lib/node_modules/npm/bin/npm-cli.js /usr/local/bin/npm \
 && ln -s ../lib/node_modules/npm/bin/npx-cli.js /usr/local/bin/npx
COPY --from=uv /uv /uvx /usr/local/bin/

# Go toolchain helpers used by the Makefile gates and the harness.
ENV GOBIN=/usr/local/bin
# GOTOOLCHAIN=auto: some tools (gopls) require a newer Go than the project pins.
RUN GOTOOLCHAIN=auto go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@${GOLANGCI_LINT_VERSION} \
 && GOTOOLCHAIN=auto go install -tags postgres github.com/golang-migrate/migrate/v4/cmd/migrate@latest \
 && GOTOOLCHAIN=auto go install golang.org/x/tools/gopls@latest \
 && GOTOOLCHAIN=auto go install golang.org/x/vuln/cmd/govulncheck@latest \
 && GOTOOLCHAIN=auto go install sigs.k8s.io/kind@latest \
 && go clean -cache -modcache
ENV GOBIN=

RUN curl -fsSLo /usr/local/bin/kubectl \
      "https://dl.k8s.io/release/$(curl -fsSL https://dl.k8s.io/release/stable.txt)/bin/linux/amd64/kubectl" \
 && chmod +x /usr/local/bin/kubectl

RUN npm install -g @anthropic-ai/claude-code && npm cache clean --force

RUN groupadd -g ${USER_GID} dev \
 && useradd -m -u ${USER_UID} -g ${USER_GID} -s /bin/bash dev \
 && mkdir -p /workspace /home/dev/.claude /home/dev/.kube /home/dev/.cache /home/dev/go \
 && chown -R dev:dev /workspace /home/dev

USER dev
ENV GOPATH=/home/dev/go \
    GOCACHE=/home/dev/.cache/go-build \
    PATH=/home/dev/go/bin:/usr/local/go/bin:/usr/local/bin:/usr/bin:/bin
WORKDIR /workspace
CMD ["sleep", "infinity"]
