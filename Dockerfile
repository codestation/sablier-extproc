# syntax=docker/dockerfile:1@sha256:87999aa3d42bdc6bea60565083ee17e86d1f3339802f543c0d03998580f9cb89

FROM --platform=$BUILDPLATFORM golang:1.26-alpine3.24@sha256:0178a641fbb4858c5f1b48e34bdaabe0350a330a1b1149aabd498d0699ff5fb2 AS build

ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG REVISION=unknown

ENV GOTOOLCHAIN=local

WORKDIR /src

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY cmd/ ./cmd/
COPY internal/ ./internal/

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build \
      -mod=readonly \
      -trimpath \
      -ldflags="-s -w -X main.version=$VERSION -X main.revision=$REVISION" \
      -o /out/sablier-extproc \
      ./cmd/sablier-extproc

FROM gcr.io/distroless/static-debian13:nonroot@sha256:f7f8f729987ad0fdf6b05eeeae94b26e6a0f613bdf46feea7fc40f7bd72953e6

ARG VERSION=dev
ARG REVISION=unknown

LABEL org.opencontainers.image.title="sablier-extproc" \
      org.opencontainers.image.description="Envoy ext_proc adapter for Sablier" \
      org.opencontainers.image.source="https://github.com/sablierapp/sablier-extproc" \
      org.opencontainers.image.version=$VERSION \
      org.opencontainers.image.revision=$REVISION

COPY --from=build --chown=65532:65532 /out/sablier-extproc /sablier-extproc

USER 65532:65532
ENTRYPOINT ["/sablier-extproc"]
