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
FROM --platform=$BUILDPLATFORM ghcr.io/zeroroot-ai/mirror/golang:1.26.8@sha256:9d2f36f06329b2a141b9db99ffa32765cf695ee57b813ca29e245e8670bcbfff AS builder
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

# setec-keepalive only: file capabilities on the binary itself, so the
# kernel grants them at execve() regardless of how the container
# runtime's own process setup handles a non-root init process
# (setec#91). Kata Containers' guest-side agent sets
# CapBnd correctly from securityContext.capabilities.add but leaves
# CapPrm/CapEff/CapAmb empty for a non-root container — verified
# against a real kata-fc Pod via /proc/self/status — so a plain
# Kubernetes capability grant never reaches this binary's actual
# process; a file capability does, because it is the kernel's own
# execve() that grants it, not the container runtime's setup code.
# libcap2-bin's setcap only needs to run natively on the build platform
# (a file capability is architecture-independent metadata on the
# target binary, not machine code), so this needs no QEMU regardless
# of TARGETARCH.
RUN if [ "${CMD}" = "setec-keepalive" ]; then \
      apt-get update; \
      apt-get install -y --no-install-recommends libcap2-bin; \
      rm -rf /var/lib/apt/lists/*; \
      setcap 'cap_sys_admin,cap_dac_override+ep' /out/${CMD}; \
      getcap /out/${CMD}; \
    fi

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
