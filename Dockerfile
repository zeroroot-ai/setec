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
# Base images are mirror-sourced and digest-pinned for reproducibility.
# The toolchain is whatever the FROM line below
# names, which must match go.mod and .tool-versions (gibson#777). The version
# is deliberately not repeated here: this comment said 1.26.4 while the FROM
# line said 1.26.8, and before that 1.26.6, so the prose copy only ever drifted.
# check-go-toolchain.sh (.github#22) is what actually holds them together. To
# refresh, mirror the new tag in zeroroot-ai/.github mirror-list.yaml,
# then re-resolve the digest with:
#   docker buildx imagetools inspect ghcr.io/zeroroot-ai/mirror/golang:<tag> --format '{{.Manifest.Digest}}'
# --platform=$BUILDPLATFORM: the build stage always runs natively on the
# build host and compiles for TARGETOS/TARGETARCH (CGO is disabled). setec
# publishes each image for linux/amd64 only (ADR-0141), so the two are the
# same on a CI runner. The runtime stage has no RUN steps.
FROM --platform=$BUILDPLATFORM ghcr.io/zeroroot-ai/mirror/golang:1.27.1@sha256:e0174e51e81218523251d85d248a90d24c3d5e81543b4f07a5d66229397db190 AS builder
# The builder image carries exactly the Go that go.mod names, and the org
# guard (check-go-toolchain.sh, .github#22) fails a PR where they differ.
# GOTOOLCHAIN=local makes a mismatch fail the build instead of downloading a
# toolchain, so the pinned base is the toolchain that built the binary.
ARG GOTOOLCHAIN=local
ENV GOTOOLCHAIN=${GOTOOLCHAIN}
ARG TARGETOS
ARG TARGETARCH
ARG CMD=manager
ARG CMD_PATH=""

WORKDIR /workspace

# Cache module downloads.
COPY go.mod go.mod
COPY go.sum go.sum
# Through the retry wrapper: proxy.golang.org resets an in-flight HTTP/2
# stream from time to time, and Go reports that as a build failure. It is the
# same wrapper the Makefile uses for `go install`, and it still never retries a
# checksum mismatch. It is COPYed separately so this layer's cache key stays
# go.mod + go.sum + the wrapper, and does not become the whole tree.
COPY scripts/go-retry.sh scripts/go-retry.sh
RUN ./scripts/go-retry.sh go mod download

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
FROM ghcr.io/zeroroot-ai/mirror/distroless-static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
ARG CMD=manager

WORKDIR /

COPY --from=builder /out/${CMD} /entrypoint

# Apache-2.0 §4(a) requires that every recipient of a distribution gets a
# copy of the License, and a published image is a distribution. /licenses
# is the OCI convention. Last in the stage, so it cannot bust the cache
# of the layers above it.
COPY LICENSE /licenses/LICENSE

# Run as the distroless "nonroot" user/group (65532:65532).
USER 65532:65532

ENTRYPOINT ["/entrypoint"]
