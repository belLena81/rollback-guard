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

Optional:

- Helm
- Prometheus stack
- Ollama or a hosted LLM API key

## Demo Workload

Build a tiny Go HTTP service:

- `GET /healthz`: returns healthy unless configured to fail.
- `GET /work`: simulates request handling.
- `GET /metrics`: exposes Prometheus-format counters and latency data.

Use image tags to model rollout versions:

- `demo-api:v1-healthy`
- `demo-api:v2-bad-crashloop`
- `demo-api:v2-bad-error-storm`
- `demo-api:v2-bad-high-latency`

The regression trigger should be a normal Kubernetes deployment update, for example:

```bash
kubectl set image deployment/demo-api demo-api=demo-api:v2-bad-error-storm -n rollback-demo
```

## Minimal Manifests

```text
demo/k8s/
  namespace.yaml
  demo-api-deployment.yaml
  demo-api-service.yaml
  rbac.yaml
demo/images/demo-api/
  main.go
  Dockerfile
demo/scripts/
  setup.sh
  trigger-regression.sh
  reset.sh
```

## Metrics Strategy

Start simple. The Observability MCP server can read fixed metric windows from the demo service directly or through a minimal in-memory fixture. Prometheus is useful later, but should not block the first repeatable demo.

## RBAC Check

The local demo should include a visible least-privilege proof:

```bash
kubectl auth can-i patch deployments \
  --as=system:serviceaccount:rollback-demo:readonly-mcp-sa \
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
make create-incident
make approve-latest-action
make verify
```

The final verification must prove the incident resolved, the rollback succeeded, recovery criteria passed, and the audit timeline contains the full path.
