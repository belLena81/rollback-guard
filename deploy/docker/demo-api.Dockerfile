# syntax=docker/dockerfile:1
# Demo workload image. One binary backs every demo tag; the failure mode is
# baked in as a default via DEMO_MODE and can still be overridden at runtime.
#   docker build -f deploy/docker/demo-api.Dockerfile \
#     --build-arg DEMO_MODE=error-storm --build-arg DEMO_VERSION=v2 \
#     -t demo-api:v2-bad-error-storm .
# `make images` builds all four tags from docs/local-demo-env.md.

ARG GO_VERSION=1.26

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION} AS build
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY cmd/ cmd/
COPY internal/ internal/
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /out/demo-api ./cmd/demo-api

FROM gcr.io/distroless/static-debian12:nonroot
ARG DEMO_MODE=healthy
ARG DEMO_VERSION=v1
LABEL org.opencontainers.image.title="demo-api" \
      org.opencontainers.image.source="https://github.com/belLena81/rollback-guard" \
      io.rollback-guardian.demo-mode="${DEMO_MODE}"
ENV DEMO_MODE=${DEMO_MODE} \
    DEMO_VERSION=${DEMO_VERSION} \
    HTTP_ADDR=:8080
COPY --from=build /out/demo-api /demo-api
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/demo-api"]
