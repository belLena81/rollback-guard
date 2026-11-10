#!/usr/bin/env bash
# Manage the containerised agent sandbox. Run from the host.
#
# The sandbox works on a COPY of this checkout (default ../<repo>-sandbox), so
# agents never touch the real working tree. Bring work back with `pull`.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo="$(cd "$here/.." && pwd)"
export SANDBOX_DIR="${SANDBOX_DIR:-$(dirname "$repo")/$(basename "$repo")-sandbox}"
export SANDBOX_UID="${SANDBOX_UID:-$(id -u)}"
export SANDBOX_GID="${SANDBOX_GID:-$(id -g)}"
compose=(docker compose -f "$here/compose.yaml")

init() {
  if [ -d "$SANDBOX_DIR/.git" ]; then return; fi
  echo "Creating sandbox copy at $SANDBOX_DIR"
  mkdir -p "$SANDBOX_DIR"
  # Full copy including .git and gitignored agent files (AGENTS.md, .agents, .claude).
  tar -C "$repo" --exclude=./.idea -cf - . | tar -C "$SANDBOX_DIR" -xf -
}

case "${1:-help}" in
  init) init ;;
  up)
    init
    if [ "${2:-}" = "llm" ]; then compose+=(--profile llm); fi
    "${compose[@]}" up -d --build
    if [ "${2:-}" = "llm" ]; then
      "${compose[@]}" exec ollama ollama pull "${SANDBOX_LLM_MODEL:-qwen2.5:3b}"
    fi
    ;;
  shell) "${compose[@]}" exec dev bash ;;
  claude) shift; "${compose[@]}" exec dev claude "$@" ;;
  exec) shift; "${compose[@]}" exec -T dev "$@" ;;
  migrate) "${compose[@]}" exec -T dev sh -c 'migrate -path migrations -database "$DATABASE_URL" up' ;;
  kind-up)
    "${compose[@]}" exec -T dev sh -c '
      kind get clusters 2>/dev/null | grep -qx rollback-guard ||
        kind create cluster --config .devcontainer/kind-config.yaml
      kind get kubeconfig --name rollback-guard | sed "s#https://0.0.0.0:6443#https://dind:6443#" > "$KUBECONFIG"
      kubectl get nodes'
    ;;
  kind-down) "${compose[@]}" exec -T dev kind delete cluster --name rollback-guard ;;
  sync)
    # Push the current real working tree (not .git) into the sandbox copy.
    tar -C "$repo" --exclude=./.git --exclude=./.idea -cf - . | tar -C "$SANDBOX_DIR" -xf -
    ;;
  pull)
    # Fetch a branch the agent committed in the sandbox into the real repo.
    branch="${2:?usage: sandbox.sh pull <branch>}"
    git -C "$repo" fetch "$SANDBOX_DIR" "$branch:sandbox/$branch"
    echo "Fetched as local branch sandbox/$branch - review with: git diff HEAD...sandbox/$branch"
    ;;
  down) "${compose[@]}" --profile llm down ;;
  destroy)
    "${compose[@]}" --profile llm down -v
    echo "Volumes removed. The sandbox copy at $SANDBOX_DIR was kept; delete it yourself if unwanted."
    ;;
  *)
    cat <<USAGE
usage: sandbox.sh <command>
  up [llm]     build and start the sandbox (llm: also start Ollama and pull a model)
  shell        open a shell in the dev container
  claude ...   run Claude Code inside the sandbox
  exec CMD...  run a command inside the sandbox
  migrate      apply migrations to the sandbox PostgreSQL
  kind-up      create the kind cluster inside the sandbox Docker daemon
  kind-down    delete it
  sync         copy the real working tree (without .git) into the sandbox
  pull BRANCH  fetch a sandbox branch into the real repo as sandbox/BRANCH
  down         stop containers (state kept)
  destroy      stop and delete all sandbox volumes
USAGE
    ;;
esac
