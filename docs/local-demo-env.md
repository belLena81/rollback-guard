# Local Demo Environment

## Goal

Run a repeatable Kubernetes regression demo locally without cloud infrastructure or a registry. The demo should use a real Kubernetes Deployment and real rollback semantics, but keep the workload and metrics simple.

## Required Tools

- Go
- Docker or Podman
- Docker Compose
- `kind`
- `kubectl`
- Make
- PostgreSQL 16 and Prometheus (both provided by the agent sandbox in `.devcontainer/`)

Optional:

- Ollama or a hosted LLM API key (only for `llm_based` or `hybrid` diagnosis)

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
scripts/demo/                 stable marking, load generation, SA kubeconfigs, RBAC proof
```

Details, variables, and the service run order are in [phase6-kind.md](./phase6-kind.md).

## Metrics Strategy

Prometheus scrapes `demo-api` through its NodePort (`30080`), and the Observability MCP queries Prometheus. `demo-api` emits its own `cluster`, `namespace`, `deployment`, and `service` labels, which the queries select on. CPU and memory series for `get_pod_cpu_usage` and `get_pod_memory_usage` are not produced by the demo yet.

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
make kind-create
make images
make kind-load
make deploy
make demo-v1
make generate-load
make demo-v2-broken
```

Creating the incident, approving the rollback, and verifying recovery are done through the REST API for now (see [phase4-a2a.md](./phase4-a2a.md) for the request shape). The scripted `make create-incident`, `make approve-latest-action`, and `make verify` targets are planned with the unattended end-to-end flow and do not exist yet.

The final verification must prove the incident resolved, the rollback succeeded, recovery criteria passed, and the audit timeline contains the full path.
