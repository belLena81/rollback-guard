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
    # Separate database for the rollback mutation guard integration tests.
    "${compose[@]}" exec -T postgres sh -c \
      'psql -U rollback_guard -d postgres -tc "SELECT 1 FROM pg_database WHERE datname = '"'"'rollback_guard_test'"'"'" | grep -q 1 ||
       psql -U rollback_guard -d postgres -c "CREATE DATABASE rollback_guard_test"'
    if [ "${2:-}" = "llm" ]; then
      "${compose[@]}" exec ollama ollama pull "${SANDBOX_LLM_MODEL:-qwen2.5:3b}"
    fi
    ;;
  shell) "${compose[@]}" exec dev bash ;;
  claude) shift; "${compose[@]}" exec dev claude "$@" ;;
  ask)
    # Delegate a task to Claude Code inside the sandbox, non-interactively.
    #   sandbox.sh ask "<prompt>" [extra claude args, e.g. --resume <session-id>]
    # Edits are auto-accepted and only the allow-listed commands below may run;
    # anything else is refused, since nobody is there to approve it.
    #   sandbox.sh ask [--tier light|standard|heavy] "<prompt>" [extra claude args]
    # The tier picks model and effort (see .devcontainer/README.md, "Model routing").
    shift
    tier="${SANDBOX_TIER:-standard}"
    if [ "${1:-}" = "--tier" ]; then tier="${2:?--tier needs a value}"; shift 2; fi
    # Budgets are hard caps per run (USD), overridable per call with
    # SANDBOX_BUDGET_USD. Tune them from .demo/ask-costs.tsv.
    case "$tier" in
      light)    tier_args=(--model haiku  --effort low);    budget="${SANDBOX_BUDGET_USD:-1}" ;;
      standard) tier_args=(--model sonnet --effort medium); budget="${SANDBOX_BUDGET_USD:-5}" ;;
      heavy)    tier_args=(--model opus   --effort medium); budget="${SANDBOX_BUDGET_USD:-12}" ;;
      *) echo "unknown tier: $tier (light|standard|heavy)" >&2; exit 2 ;;
    esac
    tier_args+=(--max-budget-usd "$budget")
    prompt="${1:?usage: sandbox.sh ask [--tier light|standard|heavy] \"<prompt>\" [--resume <session-id>]}"
    shift
    allowed="Read,Edit,Write,Glob,Grep,Skill,Agent,TodoWrite"
    for c in go gofmt make git golangci-lint govulncheck gopls migrate psql \
             docker kind kubectl curl jq ls cat head tail wc grep rg find sed awk \
             sort uniq diff mkdir cp mv touch echo printf printenv env test true \
             sleep timeout which date basename dirname xargs tee; do
      allowed+=",Bash($c:*)"
    done
    echo "tier: $tier (${tier_args[*]})" >&2
    out="$(mktemp)"
    "${compose[@]}" exec -T dev claude -p "$prompt" "${tier_args[@]}" \
      --permission-mode acceptEdits --allowedTools "$allowed" \
      --output-format json "$@" </dev/null > "$out"
    mkdir -p "$repo/.demo"
    jq -r --arg tier "$tier" '[(now|todate), $tier, .session_id, (.total_cost_usd // 0), (.num_turns // 0), .is_error] | @tsv' "$out" >> "$repo/.demo/ask-costs.tsv"
    jq -r '"session: \(.session_id)\nerror: \(.is_error)\ncost_usd: \(.total_cost_usd // "n/a")  turns: \(.num_turns // "n/a")\n\n\(.result)"' "$out"
    rm -f "$out"
    ;;
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
  push)
    # Mirror a branch of the real repo into the sandbox copy as host/<branch>.
    branch="${2:?usage: sandbox.sh push <branch>}"
    git -C "$SANDBOX_DIR" fetch "$repo" "+$branch:refs/heads/host/$branch"
    echo "Available in the sandbox as host/$branch"
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
  ask [--tier T] PROMPT  delegate a task to the sandbox Claude (allow-listed commands); T = light|standard|heavy
  exec CMD...  run a command inside the sandbox
  migrate      apply migrations to the sandbox PostgreSQL
  kind-up      create the kind cluster inside the sandbox Docker daemon
  kind-down    delete it
  sync         copy the real working tree (without .git) into the sandbox
  pull BRANCH  fetch a sandbox branch into the real repo as sandbox/BRANCH
  push BRANCH  mirror a branch of the real repo into the sandbox as host/BRANCH
  down         stop containers (state kept)
  destroy      stop and delete all sandbox volumes
USAGE
    ;;
esac
