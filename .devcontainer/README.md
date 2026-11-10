# Agent Sandbox

A containerised environment where coding agents can build, test, and run real integrations without touching the real checkout, the host Docker daemon, or any real infrastructure.

## What runs

| Service | Purpose | Reachable from `dev` as |
| --- | --- | --- |
| `dev` | Go 1.26, golangci-lint, gopls, govulncheck, migrate, kind, kubectl, psql, Node, uv, Claude Code | - |
| `postgres` | PostgreSQL 16 with throwaway credentials | `$DATABASE_URL` |
| `dind` | Isolated Docker daemon for testcontainers and the kind cluster | `$DOCKER_HOST` |
| `prometheus` | Metrics backend for the observability MCP | `$PROMETHEUS_URL` |
| `ollama` (optional) | Free local OpenAI-compatible LLM for `llm_based` / `hybrid` diagnosis | `$LLM_BASE_URL` |

Isolation model:

- The agent works on a **copy** of the repo (default `../rollback-guard-sandbox`, override with `SANDBOX_DIR`). The copy includes `.git` and the gitignored agent files.
- Containers and the kind cluster live inside `dind`. The host Docker socket is not mounted.
- No ports are published to the host.
- `dind` is a privileged container, which is required for Docker-in-Docker. Outbound internet is open (Go modules, npm, model API). If you need egress restrictions, add a firewall like the one in Anthropic's reference devcontainer.

## Use

```bash
./.devcontainer/sandbox.sh up          # build, create the copy, start services
./.devcontainer/sandbox.sh migrate     # apply migrations to the sandbox database
./.devcontainer/sandbox.sh kind-up     # optional: kind cluster inside dind
./.devcontainer/sandbox.sh up llm      # optional: Ollama + model (multi-GB download)
./.devcontainer/sandbox.sh claude      # run Claude Code inside the sandbox
./.devcontainer/sandbox.sh shell       # or a plain shell
```

First run of Claude Code inside the sandbox: log in with `/login` (the login is kept in the `claude-home` volume), or export `ANTHROPIC_API_KEY` on the host before `up`. Approve the project MCP servers from `.mcp.json` when prompted.

Because the container is the security boundary, running `sandbox.sh claude --dangerously-skip-permissions` there is a reasonable trade-off for unattended work. Do not use that flag on the host.

## Getting work in and out

```bash
./.devcontainer/sandbox.sh sync            # copy the real working tree (not .git) into the sandbox
./.devcontainer/sandbox.sh pull my-branch  # fetch a sandbox branch into the real repo as sandbox/my-branch
git diff HEAD...sandbox/my-branch          # review before merging
```

Nothing flows back to the real repo unless you run `pull` and merge.

## Stop and clean up

```bash
./.devcontainer/sandbox.sh down      # stop, keep state
./.devcontainer/sandbox.sh destroy   # stop and delete all volumes (database, caches, cluster, login)
```

`destroy` leaves the sandbox copy directory in place; remove it yourself.

## MCP servers for the agent (`.mcp.json`)

| Server | Why | Notes |
| --- | --- | --- |
| `gopls` | Type-aware Go navigation, references, diagnostics, rename, vulncheck | Built into the image |
| `context7` | Current docs for the A2A, MCP, pgx, client-go, and OpenAI SDKs | Free; fetched with `npx` on first use |
| `postgres` | Schema inspection, `EXPLAIN`, index health against the sandbox database | Restricted (read-only) mode; fetched with `uvx` |
| `kubernetes` | Inspect the sandbox kind cluster | `--read-only`; needs `kind-up` first |

These are developer tools for the agent. They are unrelated to, and must never be copied into, the product's own narrow MCP servers.

GitHub work uses the `gh` CLI on the host rather than an MCP server.
