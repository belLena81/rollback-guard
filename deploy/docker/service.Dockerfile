# syntax=docker/dockerfile:1
# Builds one Rollback Guardian service binary into a static, non-root image.
#   docker build -f deploy/docker/service.Dockerfile --build-arg SERVICE=orchestrator .
# SERVICE is one of: orchestrator, investigator, kubernetes-mcp, observability-mcp.
# Runtime configuration (HTTP_ADDR, DATABASE_URL, ...) is supplied by the
# deployment, never baked into the image.

ARG GO_VERSION=1.26

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION} AS build
ARG SERVICE
ARG TARGETOS
ARG TARGETARCH
RUN test -n "${SERVICE}" || (echo "SERVICE build arg is required" >&2 && exit 1)
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY cmd/ cmd/
COPY internal/ internal/
RUN test -d "cmd/${SERVICE}" || (echo "unknown SERVICE ${SERVICE}" >&2 && exit 1)
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /out/app "./cmd/${SERVICE}"

FROM gcr.io/distroless/static-debian12:nonroot
ARG SERVICE
LABEL org.opencontainers.image.title="rollback-guardian-${SERVICE}" \
      org.opencontainers.image.source="https://github.com/belLena81/rollback-guard"
COPY --from=build /out/app /app
USER 65532:65532
ENTRYPOINT ["/app"]
