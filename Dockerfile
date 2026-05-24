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
FROM golang:1.26 AS builder
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
FROM gcr.io/distroless/static-debian12:nonroot
ARG CMD=manager

WORKDIR /

COPY --from=builder /out/${CMD} /entrypoint

# Run as the distroless "nonroot" user/group (65532:65532).
USER 65532:65532

ENTRYPOINT ["/entrypoint"]
