# syntax=docker/dockerfile:1.7

# Build-time knob: which binary this image wraps. Default is the
# Phase 1 operator "manager"; the release workflow also builds
# "node-agent" and "frontend" from this same Dockerfile.
#
#   docker build --build-arg CMD=manager   .
#   docker build --build-arg CMD=node-agent .
#   docker build --build-arg CMD=frontend   .
#
# ----------------------------------------------------------------------------
# Build stage
# ----------------------------------------------------------------------------
# Base images are mirror-sourced and digest-pinned for reproducibility
# (RESTRUCTURE-QUALITY-BARS §1). Toolchain pinned to go 1.26.4 to match
# go.mod / .tool-versions and the rest of the platform (gibson#777). To
# refresh, mirror the new tag in zeroroot-ai/.github mirror-list.yaml,
# then re-resolve the digest with:
#   docker buildx imagetools inspect ghcr.io/zeroroot-ai/mirror/golang:<tag> --format '{{.Manifest.Digest}}'
# --platform=$BUILDPLATFORM: the build stage always runs natively on the
# build host and cross-compiles via TARGETOS/TARGETARCH (CGO is disabled),
# so multi-arch builds (linux/amd64,linux/arm64 — setec#132) never emulate
# the Go toolchain. The distroless runtime stage below is a multi-arch
# index, and it has no RUN steps, so no QEMU is needed anywhere.
FROM --platform=$BUILDPLATFORM ghcr.io/zeroroot-ai/mirror/golang:1.26.6@sha256:640a234f4bea3e399c056b7b8f9c667c4939befae8db2f14e9785e16eccd4205 AS builder
ARG TARGETOS
ARG TARGETARCH
ARG CMD=manager
ARG CMD_PATH=""

WORKDIR /workspace

# Cache module downloads.
COPY go.mod go.mod
COPY go.sum go.sum
RUN go mod download

# Copy the rest of the source tree. .dockerignore narrows this.
COPY . .

# Produce a fully static binary. CGO is disabled to satisfy the
# distroless/static image, which contains no libc. CMD_PATH allows
# manager (cmd/main.go) to use the legacy path while node-agent and
# frontend use cmd/<name>/main.go.
RUN set -eux; \
    if [ -n "${CMD_PATH}" ]; then \
      SOURCE="${CMD_PATH}"; \
    elif [ "${CMD}" = "manager" ]; then \
      SOURCE="cmd/main.go"; \
    else \
      SOURCE="./cmd/${CMD}"; \
    fi; \
    CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
      go build -trimpath -ldflags="-s -w" -o /out/${CMD} ${SOURCE}

# ----------------------------------------------------------------------------
# Runtime stage
# ----------------------------------------------------------------------------
# Distroless static on Debian 12, nonroot by default (UID/GID 65532).
# Mirror-sourced + digest-pinned (mirror dest: distroless-static-debian12).
FROM ghcr.io/zeroroot-ai/mirror/distroless-static-debian12:nonroot@sha256:a9329520abc449e3b14d5bc3a6ffae065bdde0f02667fa10880c49b35c109fd1
ARG CMD=manager

WORKDIR /

COPY --from=builder /out/${CMD} /entrypoint

# Run as the distroless "nonroot" user/group (65532:65532).
USER 65532:65532

ENTRYPOINT ["/entrypoint"]
