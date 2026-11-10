# Local Demo Environment

## Goal

Run a repeatable Kubernetes regression demo locally without cloud infrastructure or a registry. The demo uses a real Kubernetes Deployment and real rollback semantics, but keeps the workload and metrics simple. `make e2e` runs the whole Phase 6 exit gate unattended.

## Required Tools

- Go
- Docker or Podman, with Docker Compose
- `kind`, `kubectl`
- Make, bash, `curl`, `jq`
- `migrate` (golang-migrate CLI), for `make services-up`
- PostgreSQL 16 and Prometheus: provided by the agent sandbox (`.devcontainer/`), or by `make compose-up` elsewhere

Optional:

- Ollama or a hosted LLM API key (only for `llm_based` or `hybrid` diagnosis)
- A Jaeger instance for the trace check (`make compose-up COMPOSE_PROFILES="--profile tracing"`)

## Demo Workload

`cmd/demo-api` (code in `internal/demoapi`) is a tiny Go HTTP service:

- `GET /healthz`: healthy except in crashloop mode.
- `GET /work`: simulates request handling; only this route is counted in the metrics.
- `GET /metrics`: Prometheus-format request counter and latency histogram.

One binary backs every image tag; `DEMO_MODE` selects the behaviour:

- `demo-api:v1-healthy`
- `demo-api:v2-bad-crashloop`
- `demo-api:v2-bad-error-storm`
- `demo-api:v2-bad-high-latency`

The regression trigger is a normal Kubernetes deployment update, wrapped by `make demo-v2-broken`:

```bash
kubectl set image deployment/demo-api demo-api=demo-api:v2-bad-error-storm -n rollback-demo
```

## Layout

```text
cmd/demo-api/                 entry point
internal/demoapi/             workload implementation
deploy/docker/                demo-api and service Dockerfiles
deploy/kind/cluster.yaml      kind cluster config
deploy/kubernetes/            namespace, demo-api Deployment and Service (kustomize)
deploy/rbac/mcp.yaml          the two MCP service accounts and Roles
deploy/compose/               PostgreSQL, Prometheus, optional Jaeger for make compose-up
scripts/demo/                 stable marking, reset, load, SA kubeconfigs, RBAC proof,
                              service lifecycle, REST steps, end-to-end runs
test/e2e/                     the end-to-end flows as a Go test (build tag e2e)
```

Details of the kind targets and variables are in [phase6-kind.md](./phase6-kind.md).

## Dependencies: sandbox or Compose

In the agent sandbox, PostgreSQL and Prometheus already run and `DATABASE_URL`, `PROMETHEUS_URL`, `DOCKER_HOST`, and `KIND_CONFIG` are set. Nothing else is needed.

Elsewhere, start them with Compose after the kind cluster exists (Prometheus joins kind's Docker network and scrapes demo-api on the kind node, `rollback-guard-control-plane:30080`):

```bash
make kind-create
make compose-up                                   # add COMPOSE_PROFILES="--profile tracing" for Jaeger
export DATABASE_URL='postgres://rollback_guard:rollback_guard@127.0.0.1:5432/rollback_guard?sslmode=disable'
export PROMETHEUS_URL=http://127.0.0.1:9090
# with Jaeger:
export OTEL_EXPORTER_OTLP_ENDPOINT=http://127.0.0.1:4318 TRACE_QUERY_URL=http://127.0.0.1:16686
make compose-down                                 # when done
```

`deploy/compose/compose.yaml` publishes ports on `COMPOSE_BIND_ADDR` (default `127.0.0.1`); `POSTGRES_PORT`, `PROMETHEUS_PORT`, `JAEGER_OTLP_PORT`, and `JAEGER_UI_PORT` override the host ports. The credentials are throwaway local values. Its Prometheus uses the same scrape setup as the sandbox's (job `demo-api`, 5s interval, no relabeling); `deploy/compose/prometheus.yml` is copied into a small locally built image instead of bind-mounted, so the stack also works against a remote Docker daemon. Against a remote daemon such as the sandbox's `dind`, set `COMPOSE_BIND_ADDR=0.0.0.0` (only on a private network) and reach the ports at the daemon's host.

## Running the services

`make services-up` builds the four service binaries into `bin/demo` (gitignored), applies the migrations to `DATABASE_URL`, writes the service-account kubeconfigs (`make kubeconfigs`), and starts the five processes in dependency order, waiting for each `/health/ready`:

| Process | Address | Identity / dependencies |
| --- | --- | --- |
| kubernetes-mcp-readonly | `127.0.0.1:8091` | `.demo/kubeconfig-readonly` (readonly ServiceAccount) |
| kubernetes-mcp-rollback | `127.0.0.1:8092` | `.demo/kubeconfig-rollback` (rollback ServiceAccount), `ROLLBACK_MCP_DATABASE_URL` (default `DATABASE_URL`) |
| observability-mcp | `127.0.0.1:8093` | `PROMETHEUS_URL` |
| investigator | `127.0.0.1:8094` | `DATABASE_URL`, readonly and observability MCP; `DIAGNOSIS_STRATEGY` (default `rule_based`) and `LLM_*` pass through |
| orchestrator | `127.0.0.1:8080` | `DATABASE_URL`, all three MCP servers, investigator |

Each process gets only its own variables (`env -i`), so the operator's admin `KUBECONFIG` never reaches a service. `METRICS_WINDOW` (default `30s` for the demo, see below), `OTEL_EXPORTER_OTLP_ENDPOINT`, `OTEL_TRACES_SAMPLE_RATIO`, `LOG_LEVEL`, `A2A_TASK_TIMEOUT`, `RECOVERY_TIMEOUT`, and `RECOVERY_POLL_INTERVAL` are passed through when set. Pid files and logs are in `.demo/pids` and `.demo/logs`. A process that exits or is not ready within `SERVICES_READY_TIMEOUT` (60s) stops the start-up, prints its last log lines, and stops what was already started.

```bash
make services-status     # pid, state, /health/ready code; non-zero if any is down
make services-down       # SIGTERM in reverse order, SIGKILL after SERVICES_STOP_TIMEOUT (15s)
```

`services-up` is idempotent: running processes are left alone. Rebuilt binaries take effect after `make services-down services-up`. The rollback MCP should use the dedicated database role described in `docs/phase3-mcp.md`; set `ROLLBACK_MCP_DATABASE_URL` to it.

The demo sets `METRICS_WINDOW=30s` (the domain minimum) for every process, so a regression shows in the error rate within seconds and recovery is visible about half a minute after the rollback. With the 5-minute default the same flow works but takes minutes.

## Metrics Strategy

Prometheus scrapes `demo-api` through its NodePort (`30080`), and the Observability MCP queries Prometheus. `demo-api` emits its own `cluster`, `namespace`, `deployment`, and `service` labels, which the queries select on. CPU and memory series for `get_pod_cpu_usage` and `get_pod_memory_usage` are not produced by the demo yet.

The error-rate query has no value when there is no traffic at all (`docs/phase6-kind.md`, "Verification"), so recovery cannot pass without load: during the recovery wait a no-traffic window is a failed read and the wait continues. `make e2e` keeps load running throughout, and its verify step also checks live traffic.

## RBAC Check

`make rbac-check` runs the full least-privilege proof. A single example:

```bash
kubectl auth can-i patch deployments \
  --as=system:serviceaccount:rollback-demo:rollback-guardian-kubernetes-readonly \
  -n rollback-demo
```

Expected result: `no`.

## Demo Sequence

```bash
make kind-create images kind-load deploy   # once
make services-up
make demo-reset            # v1-healthy, stable mark, prune old ReplicaSets
make generate-load &       # LOAD_DURATION seconds of GET /work
make demo-v2-broken        # v2-bad-error-storm
make create-incident       # symptom = current error rate from Prometheus
make approve-latest-action # approves the API's proposal; executes, watches, verifies
make verify
```

### Scripted REST steps

| Target | Script | What it does |
| --- | --- | --- |
| `create-incident` | `create-incident.sh` | Reads the demo-api error rate over `METRICS_WINDOW` from Prometheus (the observability MCP's query) and posts an `http_error_rate` incident with threshold `INCIDENT_THRESHOLD` (0.1). `INCIDENT_OBSERVED` and `INCIDENT_STARTED_AT` override the symptom. Sends a fresh sampled `traceparent` and records the incident ID and trace ID in `.demo/last-incident`. |
| `approve-latest-action` | `approve-latest-action.sh` | Reads the latest incident (or `INCIDENT_ID`), takes `proposal.actionId` and `proposal.proposalHash` verbatim from the API, and posts them to `/approve`. It never computes or guesses a hash; with no proposal it stops. The orchestrator executes, watches the rollout, and verifies recovery inside the request (`API_TIMEOUT`, default 900s, the server write timeout). |
| `verify` | `verify.sh` | Asserts the outcome from the API, the cluster, and the trace backend (below). `VERIFY_EXPECT=declined` checks the decline scenario. Prints every check; exits 1 if any failed. |

`verify` (expect `resolved`) checks:

- incident `resolved`; diagnosis `rollback` with evidence references;
- the approval is a human approval of the exact proposal (action ID and hash), and the `rollback.proposal_created` event carries that hash;
- execution `succeeded` from the proposal's current to its target revision;
- recovery criteria passed: verification passed at the target revision, all replicas ready and available, error rate below the symptom threshold (with `VERIFY_REQUIRE_TRAFFIC=1`, which `e2e` sets, also that demo-api is serving traffic);
- the audit timeline: sequence numbers `1..n`, and in order `incident.created`, `status_changed→investigating`, `evidence_collected` (deployment, metric), `diagnosis_completed` (rollback), `policy.evaluated` (allowed), `rollback.proposal_created`, `→awaiting_approval`, `rollback.approved`, `→approved`, `→executing`, `rollback.execution_completed` (succeeded), `→verifying`, `rollback.verification_completed` (passed), `→resolved`. Event kinds are those in `internal/domain/event.go`; "resolved" is a `status_changed` event, not its own kind;
- the cluster: the rollout is complete, exactly one ReplicaSet is marked stable, it carries the Deployment's current revision and has ready replicas, and the Deployment runs `STABLE_IMAGE` (`demo-api:v1-healthy`);
- the trace check (below).

### Trace check

When `OTEL_EXPORTER_OTLP_ENDPOINT` is set (the services export spans), `verify` reads the incident's trace back from `TRACE_QUERY_URL` (a Jaeger query API; missing, it is a FAIL) by the trace ID that `create-incident` sent and requires:

- spans `POST /v1/incidents` and `investigation.dispatch` (orchestrator), `investigation.execute` (investigator), `mcp tool get_deployment` (kubernetes-mcp-readonly), and `mcp tool get_http_error_rate` (observability-mcp);
- one connected tree: exactly one span whose parent is not in the trace (the REST server span; the client's own span is not exported);
- for the approval's separate trace, `mcp tool execute_approved_rollback` on kubernetes-mcp-rollback.

Without `OTEL_EXPORTER_OTLP_ENDPOINT` it prints `trace check SKIPPED (not passed)` with the reason, and the final line says `trace check SKIPPED`. A skipped trace check is never reported as passed.

Both modes also check that sequence numbers are `1..n`, timestamps never go backwards, there is no `incident.failed`/`expired`/`canceled` event, and every evidence ID the conclusion references was collected in the timeline; the resolved mode requires exactly one proposal, approval, execution, and verification event.

### Unknown rollback outcome

If the rollback request may or may not have reached the cluster and could not be reconciled, `/approve` answers `503` with error `rollback_outcome_unknown` and `Retry-After` (see `api/openapi.yaml`). Nothing was recorded as failed; the incident stays `executing`. `approve-latest-action` exits 3 and prints the retry command. Retrying the same approval reuses its idempotency key, so the rollback MCP's ledger mutates at most once, and the retry reconciles the attempt and then verifies recovery. The same retry resumes an incident left `verifying` by an interrupted request. Do not create a new incident or roll back by hand.

## End-to-end runs

```bash
make e2e            # exit gate (services-up first, automatically)
make e2e-decline    # policy-decline scenario
make e2e-test       # both flows as a Go test against the running stack
```

`make e2e` (`scripts/demo/e2e.sh rollback`) runs, and times, each step: check the five services, `demo-reset`, start load for the whole scenario, wait for a healthy baseline (traffic, error rate below threshold), `demo-v2-broken` (error-storm), wait until Prometheus shows the error rate at or above `INCIDENT_THRESHOLD`, `create-incident`, wait for `awaiting_approval`, `approve-latest-action`, `verify` (with `VERIFY_REQUIRE_TRAFFIC=1`). Any failure stops the run with `e2e: FAIL during step '<step>'` and a non-zero exit. `E2E_BREACH_TIMEOUT` and `E2E_INVESTIGATION_TIMEOUT` (180s each) bound the waits.

`make e2e-test` runs `test/e2e` (`-tags=e2e`): `TestE2ERollback` and `TestE2EDecline` drive the same flows (they call `make demo-reset` and `make demo-v2-broken`, generate load in-process, and assert from the API, the cluster through `kubectl`, and the trace backend). Missing configuration fails the test rather than skipping it. `make lint` covers the `e2e` tag.

### Decline scenario

`make e2e-decline` shows the policy visibly declining an unsafe action (`docs/design.md`). It rolls out `v2-bad-crashloop`. With `maxUnavailable: 0` and one replica, the crashing pod never becomes ready, so the rollout stalls and the old v1 pod keeps serving: users see no errors. An alert then reports an error spike (`DECLINE_REPORTED_ERROR_RATE`, default 0.5) that the live metrics do not show.

The investigation decides from its own evidence. The rule diagnoser finds the current error rate below the threshold and recommends `no_action` at confidence 0.40. The policy denies on `rollback_recommended`, `minimum_confidence`, and `current_metric_threshold_breached`; the reported symptom alone cannot make an action eligible. The incident ends `declined` with no proposal, approval, or execution and no `rollback.*` event, and `verify declined` checks that the Deployment's generation is unchanged and that it was not rolled back. The policy decision's per-rule results are stored in `policy_decisions.rules` but not exposed by the API yet, so `verify declined` reads them read-only with `psql` (when `DATABASE_URL` is set; `e2e-decline` requires it) and requires `current_metric_threshold_breached` failed and both `rollout_temporally_correlated` and `rollout_recent` passed. A decline caused by a stale rollout timestamp therefore cannot pass as this scenario.

Why crashloop rather than a below-threshold error storm: it is the realistic case the rules are built for (a broken rollout that is not hurting users must not be rolled back automatically), it needs no artificial thresholds, and it exercises a different evidence shape (stalled rollout, healthy metric) from the rollback scenario (completed rollout, breached metric).

### Reset and repeatability

Every run starts with `make demo-reset`: `demo-v1` (roll to v1-healthy, mark its ReplicaSet stable), then `scripts/demo/prune-replicasets.sh` deletes the scaled-down ReplicaSets of earlier rollouts (only ones the Deployment controls, scaled to zero, neither current nor stable).

The prune is needed because Kubernetes reuses an old ReplicaSet when a Deployment returns to a Pod template it has run before, and Rollback Guardian dates a rollout by the current ReplicaSet's creation time. Without the prune, the second `demo-v2-broken` would look like a rollout from the first run, outside `RULE_CORRELATION_WINDOW` (10m), and the policy would decline it as not temporally correlated. That direction is fail-safe, but it is a real limitation for re-deployed templates; see `DEVELOPMENT_PLAN.md`.

The final verification proves the incident resolved, the rollback succeeded, recovery criteria passed, and the audit timeline contains the full path.

## Variables

| Variable | Default | Used by |
| --- | --- | --- |
| `ORCHESTRATOR_URL` | `http://127.0.0.1:8080` | REST scripts, e2e |
| `PROMETHEUS_URL` | environment | create-incident, e2e, verify, services |
| `DATABASE_URL`, `ROLLBACK_MCP_DATABASE_URL` | environment; the latter defaults to the former | services-up, migrate-up |
| `METRICS_WINDOW` | `30s` | services and the scripts' Prometheus queries |
| `INCIDENT_THRESHOLD` | `0.1` | create-incident, e2e |
| `INCIDENT_ID` | last created (`.demo/last-incident`) | approve-latest-action, verify |
| `VERIFY_EXPECT` | `resolved` | verify (`resolved` or `declined`) |
| `STABLE_IMAGE` | `demo-api:v1-healthy` | verify |
| `OTEL_EXPORTER_OTLP_ENDPOINT`, `TRACE_QUERY_URL` | unset (trace check skipped) | services, verify |
| `SERVICES_BIN_DIR` | `bin/demo` | services-build, services-up |
| `MIGRATE` | `migrate` | migrate-up |
| `COMPOSE`, `COMPOSE_FILE`, `COMPOSE_PROFILES` | `docker compose`, `deploy/compose/compose.yaml`, empty | compose-up/down |

The kind variables (`KUBE_CONTEXT`, `DEMO_NAMESPACE`, `DEMO_URL`, ...) are listed in [phase6-kind.md](./phase6-kind.md).

## Verification

```bash
make services-up
make e2e
make e2e-decline
make e2e                 # repeatable: each run resets first
make e2e-test
make services-down
```
