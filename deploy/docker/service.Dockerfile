# syntax=docker/dockerfile:1
# Builds one Rollback Guardian service binary into a static, non-root image.
#   docker build -f deploy/docker/service.Dockerfile --build-arg SERVICE=orchestrator .
# SERVICE is one of: orchestrator, investigator, kubernetes-mcp, observability-mcp.
#
# The binaries bind loopback by default (no authentication until Phase 7).
# Loopback inside a container is unreachable from outside its network
# namespace, so the image sets HTTP_ADDR explicitly: every service listens on
# all interfaces of its own container, port 8080. Publish or route that port
# deliberately (for example `docker run -p 127.0.0.1:8091:8080`); it must stay
# on a trusted network. Override with `-e HTTP_ADDR=...` or the HTTP_ADDR
# build arg. Everything else (DATABASE_URL, peer URLs, ...) is supplied by the
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
ARG HTTP_ADDR=:8080
LABEL org.opencontainers.image.title="rollback-guardian-${SERVICE}" \
      org.opencontainers.image.source="https://github.com/belLena81/rollback-guard"
ENV HTTP_ADDR=${HTTP_ADDR}
COPY --from=build /out/app /app
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/app"]
