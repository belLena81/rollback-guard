# Phase 6 Local kind Demo

Phase 6 task 2 puts `demo-api` on a real `kind` cluster, with real RollingUpdate rollouts, the least-privilege RBAC from Phase 3, and a NodePort that Prometheus scrapes. The four Rollback Guardian services run as local processes that reach the cluster through service-account kubeconfigs.

## Layout

| Path | Purpose |
| --- | --- |
| `deploy/kind/cluster.yaml` | Default kind cluster `rollback-guard`. Publishes NodePort `30080` on the Docker host's loopback (`127.0.0.1`). |
| `deploy/kubernetes/kustomization.yaml` | `make deploy` entry point. Includes the namespace, `../rbac`, and the demo-api manifests. |
| `deploy/kubernetes/namespace.yaml` | `rollback-demo` namespace, Pod Security `restricted`. |
| `deploy/kubernetes/demo-api.yaml` | demo-api Deployment (1 replica, `/healthz` probes, image pull policy `Never`) and NodePort Service `30080 -> 8080`. |
| `deploy/rbac/{kustomization,mcp.yaml}` | The two MCP service accounts and Roles (unchanged, pinned by `test/contract/rbac_test.go`). |
| `scripts/demo/*.sh` | Helpers behind the make targets: stable marking, load, SA kubeconfigs, RBAC proof. |

Kustomize emits the `Namespace` before the ServiceAccounts, Roles, and RoleBindings, so applying to a fresh cluster no longer fails on the missing `rollback-demo` namespace. Apply RBAC through `make deploy` (`kubectl apply -k deploy/kubernetes`). `deploy/rbac/mcp.yaml` does not work on its own on a fresh cluster.

## Make targets

```bash
make kind-create      # no-op if the cluster already exists
make images           # task 1: service + demo-api images
make kind-load        # side-load the four demo-api tags into kind
make deploy           # namespace, RBAC, demo-api v1-healthy; waits for the rollout
make demo-v1          # roll to demo-api:v1-healthy, wait, mark its ReplicaSet stable
make generate-load    # GET /work traffic for LOAD_DURATION seconds
make demo-v2-broken   # roll to demo-api:v2-bad-$(DEMO_BROKEN), default error-storm
make rbac-check       # kubectl auth can-i proof for both service accounts
make kubeconfigs      # .demo/kubeconfig-{readonly,rollback} from TokenRequest tokens
make demo-reset       # demo-v1, then prune scaled-down ReplicaSets of earlier rollouts
make phase6-kind-check  # live-cluster test through the real adapter and SA credentials (needs demo-v1 first)
make kind-delete
```

The services, the scripted REST steps, and the end-to-end runs (`make services-up`, `create-incident`, `approve-latest-action`, `verify`, `e2e`, `e2e-decline`) are in [local-demo-env.md](./local-demo-env.md).

| Variable | Default | Notes |
| --- | --- | --- |
| `KIND`, `KUBECTL` | `kind`, `kubectl` | Tool binaries. |
| `KIND_CLUSTER` | `rollback-guard` | |
| `KUBE_CONTEXT` | `kind-$(KIND_CLUSTER)` | Every kubectl call (targets and scripts) is pinned to this context, so the targets never act on whichever cluster happens to be current. |
| `KIND_CONFIG` | `deploy/kind/cluster.yaml` | Override when the Docker daemon is remote and the API server must bind elsewhere. |
| `DEMO_NAMESPACE`, `DEMO_DEPLOYMENT` | `rollback-demo`, `demo-api` | Must match `ALLOWED_TARGETS` and the RBAC `resourceNames`. |
| `DEMO_BROKEN` | `error-storm` | `error-storm`, `high-latency`, or `crashloop`. With crashloop, the rollout is left stalled and the target does not wait. |
| `DEMO_ROLLOUT_TIMEOUT` | `120s` | |
| `DEMO_HOST` | Host from `DOCKER_HOST=tcp://<host>:<port>`, else `localhost` | Where kind publishes NodePorts. |
| `DEMO_URL` | `http://$(DEMO_HOST):30080` | |
| `LOAD_DURATION`, `LOAD_CONCURRENCY` | `60`, `4` | Seconds and parallel curl loops. |
| `DEMO_STATE_DIR` | `.demo` (gitignored) | Holds the SA kubeconfigs (mode `0600`, they contain bearer tokens). |
| `SA_TOKEN_DURATION` | `8h` | TokenRequest lifetime. Rerun `make kubeconfigs` when it expires. |

In the agent sandbox, `DOCKER_HOST=tcp://dind:2375` gives `DEMO_URL=http://dind:30080`. The pre-created cluster uses `.devcontainer/kind-config.yaml`, so `make kind-create` is a no-op there. To recreate the cluster, pass `KIND_CONFIG=.devcontainer/kind-config.yaml`.

## Stable ReplicaSet

The Kubernetes adapter treats the single ReplicaSet annotated `rollback-guardian.io/stable: "true"` as the rollback target. More than one stable ReplicaSet is an error. `make demo-v1` waits for the v1 rollout to complete, annotates the ReplicaSet whose `deployment.kubernetes.io/revision` equals the Deployment's, and removes the annotation from every other ReplicaSet the Deployment owns (`scripts/demo/mark-stable.sh`). This is an explicit operator action ("this release is verified good"), run with the operator's kubeconfig. Neither MCP service account can annotate ReplicaSets.

Readiness does not prove a release is good, because error-storm and high-latency builds pass `/healthz`. The script therefore takes the image it vouches for (`demo-api:v1-healthy`) and checks three things:

- It refuses unless the current ReplicaSet runs that image with all desired replicas ready.
- It re-reads the Deployment revision after annotating.
- If a rollout started in between, it removes the mark again and fails, leaving no stable ReplicaSet. Rollback Guardian then fails closed.

The annotation is placed directly on the ReplicaSet, never on the Deployment or its Pod template:

- The deployment controller copies Deployment annotations onto each new ReplicaSet, so a broken v2 ReplicaSet would also become "stable".
- Template annotations would change the revision digest.

ReplicaSet annotations that are not on the Deployment survive later rollouts. After a rollback, the stable ReplicaSet is reused and becomes current again, so the next `make demo-v2-broken` can be rolled back the same way.

## Service placement

The orchestrator, investigator, both Kubernetes MCP processes, and the observability MCP run as local processes (`go run` or the `rollback-guardian/*` images), not in the cluster. Reasons:

- PostgreSQL and Prometheus already run beside the cluster (Docker Compose/sandbox), not in it. In-cluster services would need extra networking back to them, plus migrations, Secrets, and Services for five components. None of that is needed to prove the exit gate.
- The Kubernetes boundary that matters is still real. Each Kubernetes MCP process authenticates as its own ServiceAccount through a TokenRequest kubeconfig from `make kubeconfigs`, and the API server enforces `deploy/rbac/mcp.yaml`. The operator's admin kubeconfig is only used by the `make` targets, never by a service.
- demo-api is the only workload that has to be in-cluster, because rollouts and rollbacks must be real.

`make services-up` starts all five in this order with these identities (see [local-demo-env.md](./local-demo-env.md), "Running the services"). The equivalent manual run order (after `make deploy demo-v1 kubeconfigs` and `migrate -path migrations -database "$DATABASE_URL" up`):

```bash
KUBECONFIG=.demo/kubeconfig-readonly KUBERNETES_MCP_MODE=readonly CLUSTER_ID=local go run ./cmd/kubernetes-mcp   # 127.0.0.1:8091
KUBECONFIG=.demo/kubeconfig-rollback KUBERNETES_MCP_MODE=rollback CLUSTER_ID=local \
  DATABASE_URL="$ROLLBACK_MCP_DATABASE_URL" go run ./cmd/kubernetes-mcp                                         # 127.0.0.1:8092
PROMETHEUS_URL="$PROMETHEUS_URL" go run ./cmd/observability-mcp                                                  # 127.0.0.1:8093
DATABASE_URL="$DATABASE_URL" go run ./cmd/investigator                                                            # 127.0.0.1:8094
DATABASE_URL="$DATABASE_URL" go run ./cmd/orchestrator                                                            # 127.0.0.1:8080
```

Default URLs and `ALLOWED_TARGETS=local/rollback-demo/demo-api/demo-api` already line up (see `docs/phase3-mcp.md` and `docs/phase4-a2a.md`). The rollback MCP should use the dedicated database role described in `docs/phase3-mcp.md`.

Every process binds loopback by default, which is all the demo needs when the five processes share one host: they reach each other on `127.0.0.1`, and Prometheus scrapes only demo-api (through the NodePort), not the services. A process that must be reachable from another network namespace needs an explicit `HTTP_ADDR`:

- The `rollback-guardian/*` images set `HTTP_ADDR=:8080` themselves. Publish the port on the host's loopback and point the peers at it, for example `docker run -p 127.0.0.1:8091:8080 -e KUBERNETES_MCP_MODE=readonly ... rollback-guardian/kubernetes-mcp:dev`. Containers on a shared Docker network use the container names instead (`KUBERNETES_READ_MCP_URL=http://kubernetes-mcp-readonly:8080/mcp`, `INVESTIGATOR_PUBLIC_URL=http://investigator:8080`, and so on).
- For a `go run` process that a container has to call (for example a Prometheus container scraping `/metrics`), bind the one interface that container can reach, such as the Docker bridge address (`HTTP_ADDR=172.17.0.1:<port>`), not `0.0.0.0`. `/metrics` shares its listener with the service's other routes, so do not do this for the orchestrator (`/v1` approve) or the rollback Kubernetes MCP (`execute_approved_rollback`): scrape those from the host, or through a loopback-published port.

Only the orchestrator authenticates (`AUTH_MODE=github`, `docs/phase7-auth.md`); the investigator and MCP servers, and the orchestrator in the default `local` mode, are unauthenticated, so any non-loopback address must stay on a trusted network. A service that listens beyond loopback logs a warning at start-up.

Check a running process with `curl -s 127.0.0.1:8091/health/live`, `/health/ready` (`200 {"status":"ok"}` or `503 {"status":"not_ready"}`), and `/metrics`.

## Metrics

Prometheus scrapes demo-api through the NodePort as a static target (job `demo-api`). demo-api emits its own `cluster`, `namespace`, `deployment`, and `service` labels. They don't collide with the target labels (`job`, `instance`), so Prometheus keeps them as-is and no relabeling or `honor_labels` is needed. Do not add relabel rules that rename them. The Prometheus adapter selects series by exactly these labels.

The Deployment runs one replica on purpose. Scrapes through a NodePort are load-balanced across pods, and pods share one label set, so two pods would interleave counters behind one target and corrupt `rate()`. Scaling up requires per-pod discovery (in-cluster Prometheus or `kubernetes_sd_configs`).

## Verification

```bash
make images kind-load deploy demo-v1
curl -sS "$DEMO_URL/work"
make generate-load
make demo-v2-broken generate-load
make rbac-check
make phase6-kind-check
make lint test-race contract-test
```

The error-rate query that the observability MCP issues for a 5m window is:

```promql
(sum(rate(rollback_guardian_http_requests_total{cluster="local",namespace="rollback-demo",deployment="demo-api",service="demo-api",code=~"5.."}[5m0s]))
   or (sum(rate(rollback_guardian_http_requests_total{cluster="local",namespace="rollback-demo",deployment="demo-api",service="demo-api"}[5m0s])) * 0))
/ (sum(rate(rollback_guardian_http_requests_total{cluster="local",namespace="rollback-demo",deployment="demo-api",service="demo-api"}[5m0s])) > 0)
```

It returns `0` after v1 load (no 5xx series: the numerator falls back to 0) and rises toward the configured `DEMO_ERROR_RATE` (default `0.8`) after `make demo-v2-broken`. Without any traffic in the window it returns no sample, which the adapter reports as an invalid result, never as a `0` error rate: an error ratio without requests says nothing, and a `0` would let recovery verification pass on a service that serves nothing.
