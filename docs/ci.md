# Continuous Integration

`.github/workflows/ci.yml` runs on pull requests to `master`, on pushes to `master` and to `phase*/**` branches, and on manual dispatch. A newer push to the same ref cancels the older run (never on `master`); a branch push and its pull request share one concurrency group. Every job has `contents: read` only.

Jobs fail fast, cheapest first:

```text
lint -> unit, contract, vuln, manifests -> integration, images -> e2e
        (e2e needs only unit, contract and manifests, so it runs beside integration and images)
```

| Job | What it runs | What it gates |
| --- | --- | --- |
| `lint` | `LINT_TAGS` equals the Makefile's; `make fmt-check vet`; `go mod tidy` leaves no diff; golangci-lint v2.10.1 (the sandbox version) with `--build-tags=integration,kind,e2e`; actionlint on the workflows | Formatting, vet and lint on every file including the tagged suites that `go test ./...` skips; a tidy module; valid workflows |
| `unit` | `make test-race` with a coverage profile; uploads the `coverage` artifact (profile, per-function report, HTML) and prints the total in the job summary | Unit, contract and in-process lifecycle tests under the race detector |
| `contract` | `make contract-test` | MCP tool catalogs, OpenAPI, RBAC manifest: the protocol shapes and the narrow-tool invariant |
| `vuln` | `govulncheck ./...` | Known vulnerabilities reachable from the code, on the newest patch of the Go version in `go.mod` |
| `manifests` | `kubectl kustomize` of `deploy/kubernetes` and `deploy/rbac` piped to `kubeconform -strict` (Kubernetes 1.37.0 schemas); `docker compose config` | The deployed manifests are schema-valid |
| `integration` | `make integration` with a PostgreSQL 16 service container (database `rollback_guard_test`) exposed as `ROLLBACK_GUARD_POSTGRES_TEST_DSN` | Phase 3-5 suites on testcontainers and the Postgres adapter suite. `postgres-integration` fails on any skipped test and requires named tests, so an unset DSN or a dropped build tag cannot pass |
| `images` | `docker buildx` of the four service images and four demo-api tags (matrix, no push, GitHub Actions cache) | Every Dockerfile builds. The matrix mirrors `images-services` and `DEMO_VARIANTS` in the Makefile |
| `e2e` | kind v0.33.0 with the pinned node image, `make images-demo-api kind-load deploy rbac-check phase6-kind-check`, `make compose-up` with Jaeger, `make e2e`, `make e2e-decline` | The Phase 6 exit gate on a fresh cluster (below). 45-minute timeout. A run whose `verify` reports the trace check as skipped fails (the job requires the "including the trace check" line). On failure or cancellation it uploads the run logs, `.demo/logs`, `.demo/last-incident` and a diagnostics bundle (service status, compose logs, cluster state) as `e2e-logs` |

The e2e artifact never includes `.demo/kubeconfig-*`, which hold service-account bearer tokens.

## Pinning

- Every third-party action is pinned to a full commit SHA with its version in a comment. Dependabot is not configured; bump a pin by resolving the new tag's commit and editing the comment with it.
- `.github/actions/setup-go` reads `major.minor` from `go.mod` and installs the newest patch release of it with module and build caches. Plain `go-version-file` would install `1.26.0` exactly, whose standard library `govulncheck` can flag.
- Tool versions are in the workflow's top-level `env`: golangci-lint, actionlint, govulncheck, kubeconform, migrate, kubectl (with its SHA-256), kind and the kind node image (with its digest).

## Reproduce locally

Run the commands in the job's order from the repository root. They are the same Makefile targets the jobs call.

```bash
# lint
make fmt-check vet && go mod tidy && git diff --exit-code -- go.mod go.sum
golangci-lint run --build-tags=integration,kind,e2e ./...
go run github.com/rhysd/actionlint/cmd/actionlint@v1.7.12

# unit, contract, vuln
make test-race GOFLAGS="-coverprofile=coverage.out -covermode=atomic"
make contract-test
go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...

# manifests
for d in deploy/kubernetes deploy/rbac; do
  kubectl kustomize "$d" | go run github.com/yannh/kubeconform/cmd/kubeconform@v0.8.0 -strict -summary -kubernetes-version 1.37.0 -
done
docker compose -f deploy/compose/compose.yaml config --quiet

# integration (Docker; ROLLBACK_GUARD_POSTGRES_TEST_DSN must name a database containing "test")
make integration

# images
make images

# e2e (needs kind, kubectl, migrate, psql, jq; see local-demo-env.md)
make kind-create images-demo-api kind-load deploy rbac-check phase6-kind-check
make compose-up COMPOSE_PROFILES="--profile tracing"
export DATABASE_URL='postgres://rollback_guard:rollback_guard@127.0.0.1:5432/rollback_guard?sslmode=disable'
export PROMETHEUS_URL=http://127.0.0.1:9090 OTEL_EXPORTER_OTLP_ENDPOINT=http://127.0.0.1:4318 TRACE_QUERY_URL=http://127.0.0.1:16686
make e2e e2e-decline
make services-down compose-down
```

In the agent sandbox, PostgreSQL and Prometheus already exist, so `compose-up` is not needed for the first stages. The sandbox cluster is long-lived rather than fresh; delete and recreate it (`make kind-delete kind-create`) for a fresh-cluster run.

## What the workflow cannot prove

- The e2e services use the owner database role for the rollback MCP (`ROLLBACK_MCP_DATABASE_URL` unset, as in the local demo), so the restricted role from `docs/phase3-mcp.md` is not exercised.
- Only `postgres-integration` fails on a skipped test. The phase 3-5 suites and `phase6-kind-check` have no skips today, but are not guarded against a future one.

- The workflow was validated with actionlint and its commands were run in the sandbox. It has not yet run on a GitHub-hosted runner; the first run may need adjustments for runner differences (action inputs, Docker network names, timing).
- `e2e` runs `make e2e` and `make e2e-decline`; `make e2e-test` (the same flows as a Go test) is not run, to avoid executing each flow twice.
- Dependabot, branch protection, required status checks and caching limits are repository settings, not part of this file.
