# Image URL to use all building/pushing image targets
IMG ?= controller:latest

# Get the currently used golang install path (in GOPATH/bin, unless GOBIN is set)
ifeq (,$(shell go env GOBIN))
GOBIN=$(shell go env GOPATH)/bin
else
GOBIN=$(shell go env GOBIN)
endif

# CONTAINER_TOOL defines the container tool to be used for building images.
# Be aware that the target commands are only tested with Docker which is
# scaffolded by default. However, you might want to replace it to use other
# tools. (i.e. podman)
CONTAINER_TOOL ?= docker

# Setting SHELL to bash allows bash commands to be executed by recipes.
# Options are set to exit when a recipe line exits non-zero or a piped command fails.
SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

.PHONY: all
all: build

##@ General

# The help target prints out all targets with their descriptions organized
# beneath their categories. The categories are represented by '##@' and the
# target descriptions by '##'. The awk command is responsible for reading the
# entire set of makefiles included in this invocation, looking for lines of the
# file as xyz: ## something, and then pretty-format the target and help. Then,
# if there's a line with ##@ something, that gets pretty-printed as a category.
# More info on the usage of ANSI control characters for terminal formatting:
# https://en.wikipedia.org/wiki/ANSI_escape_code#SGR_parameters
# More info on the awk command:
# http://linuxcommand.org/lc3_adv_awk.php

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Development

.PHONY: manifests
manifests: controller-gen ## Generate WebhookConfiguration, ClusterRole and CustomResourceDefinition objects.
	"$(CONTROLLER_GEN)" rbac:roleName=manager-role crd webhook paths="./..." output:crd:artifacts:config=config/crd/bases
	# The chart installs the CRDs, so charts/setec/crds is the copy that
	# actually reaches a cluster. It used to be maintained by hand and
	# happened to match; nothing asserted it, so a schema change could ship
	# with the chart serving the old shape. Generate it instead, and the
	# existing "Manifests up-to-date" gate covers the drift for free.
	rm -f charts/setec/crds/*.yaml
	cp config/crd/bases/*.yaml charts/setec/crds/

.PHONY: generate
generate: controller-gen ## Generate code containing DeepCopy, DeepCopyInto, and DeepCopyObject method implementations.
	"$(CONTROLLER_GEN)" object:headerFile="hack/boilerplate.go.txt" paths="./..."

.PHONY: proto
proto: buf ## Generate Go gRPC stubs from api/grpc proto files.
	PATH="$(LOCALBIN):$$PATH" "$(BUF)" generate

.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against code.
	go vet ./...

.PHONY: test
test: manifests generate fmt vet setup-envtest ## Run tests.
# KUBEBUILDER_ASSETS is resolved at RECIPE time, not with $(shell ...) at parse
# time: the parse-time form ran before the setup-envtest prerequisite and
# silently produced an empty (or relative) path, which made envtest fail to exec
# etcd. The suites now fail loudly on that (setec#302), and this resolves it
# correctly in the first place. `abspath` because a relative KUBEBUILDER_ASSETS
# breaks the moment a test binary runs from its own package directory.
	@set -eu; \
	assets="$$("$(ENVTEST)" use $(ENVTEST_K8S_VERSION) --bin-dir "$(LOCALBIN)" -p path)"; \
	case "$$assets" in /*) ;; *) assets="$(CURDIR)/$$assets" ;; esac; \
	[ -x "$$assets/etcd" ] || { echo "KUBEBUILDER_ASSETS=$$assets has no executable etcd; run 'make setup-envtest'" >&2; exit 1; }; \
	KUBEBUILDER_ASSETS="$$assets" go test $$(go list ./... | grep -v /e2e) -coverprofile cover.out

# End-to-end test suite. Gated behind the `e2e` build tag so the tests are
# skipped from `make test` and `go test ./...` on GitHub-hosted runners (which
# have no KVM). Run `make e2e` against a cluster whose nodes expose /dev/kvm,
# with the SETEC_E2E_LAUNCHER_* and SETEC_E2E_DISK_REPO environment set. See
# test/e2e/ for the suite and .github/workflows/e2e.yml for the CI wiring.

# Timeout for the full suite. Each scenario waits up to a few minutes for the
# microVM to boot/exit; the aggregate budget is tuned for a warm host.
E2E_TIMEOUT ?= 30m

# Chart path exposed to the tests via an environment variable.
SETEC_E2E_CHART ?= $(shell pwd)/charts/setec

# Credential guard. internal/credentials owns every mTLS credential in the
# module; internal/credguard is what keeps that true. It walks the whole tree
# — including the separate Go modules under examples/, which the root module's
# tooling cannot otherwise reach — and fails on a hand-built tls.Config, a
# hand-assembled trust pool, a gRPC TLS-credential constructor, or a go-spiffe
# import outside the allow-list in internal/credguard/exemptions.go. It also
# fails on an empty or missing scan root, so it can never pass having examined
# nothing.
#
# It is a Go test, so `make test` and the CI Go suites already run it. This
# target is the way to run it alone, and `check` names it so the guard is
# visible in the gate rather than buried in the test output.
.PHONY: guard-credentials
guard-credentials: ## Fail if an mTLS credential is built outside internal/credentials.
	go test ./internal/credguard/...

.PHONY: e2e
e2e: ## Run the hardware-gated e2e suite (requires KVM nodes).
# This suite is DESTRUCTIVE: it helm-installs a setec release and creates a
# namespace before the first test runs, against whatever kubeconfig context is
# current. It therefore refuses to run unless you name the cluster you mean
# (setec#298):
#
#     SETEC_E2E_CONTEXT=<kubectl context> make e2e     # named cluster
#     SETEC_E2E_ALLOW_ANY_CLUSTER=1 make e2e           # throwaway kind cluster
#
# Both are passed through below; setting neither makes the suite exit with the
# current context named, rather than installing into it.
	SETEC_E2E_CHART="$(SETEC_E2E_CHART)" \
	SETEC_E2E_CONTEXT="$(SETEC_E2E_CONTEXT)" \
	SETEC_E2E_ALLOW_ANY_CLUSTER="$(SETEC_E2E_ALLOW_ANY_CLUSTER)" \
	go test -tags=e2e ./test/e2e/... -v -timeout $(E2E_TIMEOUT)

.PHONY: lint
lint: golangci-lint ## Run golangci-lint linter
	"$(GOLANGCI_LINT)" run

.PHONY: lint-fix
lint-fix: golangci-lint ## Run golangci-lint linter and perform fixes
	"$(GOLANGCI_LINT)" run --fix

# lint-config validates .golangci.yml against a VENDORED copy of the
# golangci-lint JSON schema (hack/lintconfig/), not the one
# `golangci-lint config verify` downloads from golangci-lint.run on every run.
# That download failed the required lint gate on a network blip (setec#260).
# The validator uses the same JSON-schema library golangci-lint does, so the
# verdict is identical; it additionally pins the config `version`, which the
# schema does not cover.
#
# Bumping GOLANGCI_LINT_VERSION to a new MINOR needs a matching schema:
# run `make lint-config-schema-refresh` and commit the new file.
# (recursive `=`: GOLANGCI_LINT_VERSION is defined further down the file)
GOLANGCI_LINT_SCHEMA_VERSION = $(basename $(GOLANGCI_LINT_VERSION))
GOLANGCI_LINT_SCHEMA = hack/lintconfig/golangci.$(GOLANGCI_LINT_SCHEMA_VERSION).jsonschema.json
GOLANGCI_LINT_CONFIG_VERSION = $(patsubst v%,%,$(basename $(GOLANGCI_LINT_SCHEMA_VERSION)))

.PHONY: lint-config
lint-config: ## Verify golangci-lint linter configuration against the vendored schema (offline)
	@test -f "$(GOLANGCI_LINT_SCHEMA)" || { \
		echo "❌ No vendored schema for golangci-lint $(GOLANGCI_LINT_VERSION) at $(GOLANGCI_LINT_SCHEMA)."; \
		echo "   Run: make lint-config-schema-refresh   (then commit the file)"; \
		exit 1; \
	}
	cd hack/lintconfig && go run . \
		-schema "golangci.$(GOLANGCI_LINT_SCHEMA_VERSION).jsonschema.json" \
		-config ../../.golangci.yml \
		-expect-version "$(GOLANGCI_LINT_CONFIG_VERSION)"

.PHONY: lint-config-schema-refresh
lint-config-schema-refresh: ## Re-vendor the golangci-lint JSON schema for the pinned GOLANGCI_LINT_VERSION
	curl -fsSL --retry 3 \
		"https://golangci-lint.run/jsonschema/golangci.$(GOLANGCI_LINT_SCHEMA_VERSION).jsonschema.json" \
		-o "$(GOLANGCI_LINT_SCHEMA)"
	@echo "Vendored $(GOLANGCI_LINT_SCHEMA) — commit it."

##@ Build

.PHONY: build
build: manifests generate fmt vet build-guest-agent ## Build manager binary.
	go build -o bin/manager cmd/main.go

.PHONY: build-guest-agent
build-guest-agent: ## Build the static setec-guest-agent, the /init of the launcher machine (Dockerfile.launcher builds the published one).
	CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o bin/setec-guest-agent ./cmd/setec-guest-agent

.PHONY: run
run: manifests generate fmt vet ## Run a controller from your host.
	go run ./cmd/main.go

# setec publishes each image for linux/amd64 only (docs/design/runtime.md), and
# hack/verify-x86-substrate.sh fails when this file names another platform.
.PHONY: image
image: docker-build ## Uniform-contract alias for docker-build.

.PHONY: docker-build
docker-build: ## Build docker image with the manager.
	$(CONTAINER_TOOL) build -t ${IMG} .

# ast-checks ships the per-declaration read counter behind #116. The version is
# read from go.mod and never written here.
#
# It used to be a second, hand-written pin. #136 bumped the go.mod requirement
# to v0.5.0 and left this line on v0.4.0, so the repo compiled the no-panic test
# against one version of ast-checks and measured .unwired-baseline.txt with
# another. Two pins for one tool is one pin too many: go.mod is the source, so a
# Dependabot bump moves both and there is nothing left to forget.
#
# Pinned, not floating: a version that moved on its own would change the count
# without a commit. := rather than ?=, because an override would put the second
# source back.
UNWIRED_VERSION := $(shell awk '$$1=="github.com/zeroroot-ai/ast-checks"{print $$2; exit}' go.mod)

# unwired-version refuses to run the counter with no version resolved. An empty
# @version means `go run ...@` at whatever the proxy serves, which is the one
# thing the pin exists to prevent, and it would otherwise look like a working
# measurement.
.PHONY: unwired-version
unwired-version:
	@case "$(UNWIRED_VERSION)" in \
	  v*) echo "unwired $(UNWIRED_VERSION) (from go.mod)" ;; \
	  *) echo "::error::no ast-checks version in go.mod, so the unwired baseline has no pinned measurer" >&2; exit 1 ;; \
	esac

.PHONY: lint-unwired
lint-unwired: unwired-version ## Fail if a declaration nothing reads is added (#116). Baseline only shrinks.
	go run github.com/zeroroot-ai/ast-checks/cmd/unwired@$(UNWIRED_VERSION) -dir . -baseline .unwired-baseline.txt

.PHONY: lint-unwired-write
lint-unwired-write: unwired-version ## Re-measure #116 and rewrite the baseline.
	go run github.com/zeroroot-ai/ast-checks/cmd/unwired@$(UNWIRED_VERSION) -dir . -baseline .unwired-baseline.txt -write

# The gate is ast-checks/cmd/crdfields, at the version go.mod pins. It was a
# shell script here and a copy in gibson, and the copies drifted (ast-checks#20).
# Its fixture is the crdfields package's own tests.
.PHONY: check-crd-field-consumers
check-crd-field-consumers: unwired-version ## Fail if a served CRD field has no consumer and no recorded verdict (#121).
	go run github.com/zeroroot-ai/ast-checks/cmd/crdfields@$(UNWIRED_VERSION) -dir . \
	  -types api/v1alpha1 -exempt scripts/crd-field-consumers-exempt.txt -min-served 50

.PHONY: check-runtime-pins
check-runtime-pins: ## Fail if any consumer names a guest kernel or Firecracker version of its own.
	bash scripts/check-runtime-pins.sh --selftest
	bash scripts/check-runtime-pins.sh

.PHONY: check-scaffold-notes
check-scaffold-notes: ## Fail on a kubebuilder scaffold note or on a metrics monitor that skips the TLS check (setec#173).
	bash scripts/check-no-scaffold-notes.sh --selftest
	bash scripts/check-no-scaffold-notes.sh

.PHONY: check-go-comment-spelling
check-go-comment-spelling: ## Fail on British spelling in a Go comment (setec#174).
	bash scripts/check-go-comment-spelling.sh --selftest
	bash scripts/check-go-comment-spelling.sh

.PHONY: check-trust-domain-literal
check-trust-domain-literal: ## Fail on the SPIFFE trust domain of a real install as a literal (setec#169, ADR-0164).
	bash scripts/check-no-trust-domain-literal.sh --selftest
	bash scripts/check-no-trust-domain-literal.sh

.PHONY: check-one-backend
check-one-backend: ## Fail on a second isolation backend: the launcher is the one backend (setec#198).
	bash scripts/check-one-backend.sh --selftest
	bash scripts/check-one-backend.sh

.PHONY: docker-push
docker-push: ## Push docker image with the manager.
	$(CONTAINER_TOOL) push ${IMG}

# Location of the Helm chart. The chart itself is authored in a later task;
# this target is wired up now so CI can exercise it as soon as the chart lands.
HELM_CHART_DIR ?= charts/setec
HELM ?= helm

.PHONY: helm-lint
helm-lint: ## Lint the Setec Helm chart (requires helm CLI on PATH).
	@if [ ! -d "$(HELM_CHART_DIR)" ]; then \
		echo "Helm chart directory $(HELM_CHART_DIR) not present yet; skipping."; \
		exit 0; \
	fi
	@command -v $(HELM) >/dev/null 2>&1 || { \
		echo "helm is not installed; install from https://helm.sh/docs/intro/install/"; \
		exit 1; \
	}
	$(HELM) lint $(HELM_CHART_DIR)

# check: org-contract CI-equivalent gate (gibson#171 slice 1.4 /
# zeroroot-ai/.github#87). Runs the same targets CI executes on every PR.
.PHONY: check
# golangci-lint is deliberately excluded: it type-checks the whole module (~3 GB
# resident, a full core for minutes), and several of these repos share one
# 8-core workstation. CI runs it directly (`go-ci.yml` calls `make lint`), so
# nothing is lost here. Run `make lint` by hand when you want it.
check: test guard-credentials check-runtime-pins check-scaffold-notes check-go-comment-spelling check-trust-domain-literal check-one-backend ## Run the local gate (tests, credential guard, runtime pin guard, scaffold note guard, Go comment spelling guard, trust domain literal guard — run 'make lint' separately).

##@ Dependencies

.PHONY: bootstrap
bootstrap: controller-gen setup-envtest golangci-lint buf ## Install all local build/test/lint tooling (uniform-contract entrypoint).
	go mod download

## Location to install dependencies to
LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p "$(LOCALBIN)"

## Tool Binaries
KIND ?= kind
CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen
ENVTEST ?= $(LOCALBIN)/setup-envtest
GOLANGCI_LINT = $(LOCALBIN)/golangci-lint
BUF ?= $(LOCALBIN)/buf
PROTOC_GEN_GO ?= $(LOCALBIN)/protoc-gen-go
PROTOC_GEN_GO_GRPC ?= $(LOCALBIN)/protoc-gen-go-grpc

## Tool Versions
CONTROLLER_TOOLS_VERSION ?= v0.20.1
BUF_VERSION ?= v1.47.2
PROTOC_GEN_GO_VERSION ?= v1.36.5
PROTOC_GEN_GO_GRPC_VERSION ?= v1.5.1

#ENVTEST_VERSION is the version of controller-runtime release branch to fetch the envtest setup script (i.e. release-0.20)
ENVTEST_VERSION ?= $(shell v='$(call gomodver,sigs.k8s.io/controller-runtime)'; \
  [ -n "$$v" ] || { echo "Set ENVTEST_VERSION manually (controller-runtime replace has no tag)" >&2; exit 1; }; \
  printf '%s\n' "$$v" | sed -E 's/^v?([0-9]+)\.([0-9]+).*/release-\1.\2/')

#ENVTEST_K8S_VERSION is the version of Kubernetes to use for setting up ENVTEST binaries (i.e. 1.31)
ENVTEST_K8S_VERSION ?= $(shell v='$(call gomodver,k8s.io/api)'; \
  [ -n "$$v" ] || { echo "Set ENVTEST_K8S_VERSION manually (k8s.io/api replace has no tag)" >&2; exit 1; }; \
  printf '%s\n' "$$v" | sed -E 's/^v?[0-9]+\.([0-9]+).*/1.\1/')

GOLANGCI_LINT_VERSION ?= v2.14.0
.PHONY: controller-gen
controller-gen: $(CONTROLLER_GEN) ## Download controller-gen locally if necessary.
$(CONTROLLER_GEN): $(LOCALBIN)
	$(call go-install-tool,$(CONTROLLER_GEN),sigs.k8s.io/controller-tools/cmd/controller-gen,$(CONTROLLER_TOOLS_VERSION))

.PHONY: setup-envtest
setup-envtest: envtest ## Download the binaries required for ENVTEST in the local bin directory.
	@echo "Setting up envtest binaries for Kubernetes version $(ENVTEST_K8S_VERSION)..."
	@"$(ENVTEST)" use $(ENVTEST_K8S_VERSION) --bin-dir "$(LOCALBIN)" -p path || { \
		echo "Error: Failed to set up envtest binaries for version $(ENVTEST_K8S_VERSION)."; \
		exit 1; \
	}

.PHONY: envtest
envtest: $(ENVTEST) ## Download setup-envtest locally if necessary.
$(ENVTEST): $(LOCALBIN)
	$(call go-install-tool,$(ENVTEST),sigs.k8s.io/controller-runtime/tools/setup-envtest,$(ENVTEST_VERSION))

.PHONY: golangci-lint
golangci-lint: $(GOLANGCI_LINT) ## Download golangci-lint locally if necessary.
$(GOLANGCI_LINT): $(LOCALBIN)
	$(call go-install-tool,$(GOLANGCI_LINT),github.com/golangci/golangci-lint/v2/cmd/golangci-lint,$(GOLANGCI_LINT_VERSION))

.PHONY: buf
buf: $(BUF) $(PROTOC_GEN_GO) $(PROTOC_GEN_GO_GRPC) ## Download buf + protoc-gen-go + protoc-gen-go-grpc locally if necessary.
$(BUF): $(LOCALBIN)
	$(call go-install-tool,$(BUF),github.com/bufbuild/buf/cmd/buf,$(BUF_VERSION))
$(PROTOC_GEN_GO): $(LOCALBIN)
	$(call go-install-tool,$(PROTOC_GEN_GO),google.golang.org/protobuf/cmd/protoc-gen-go,$(PROTOC_GEN_GO_VERSION))
$(PROTOC_GEN_GO_GRPC): $(LOCALBIN)
	$(call go-install-tool,$(PROTOC_GEN_GO_GRPC),google.golang.org/grpc/cmd/protoc-gen-go-grpc,$(PROTOC_GEN_GO_GRPC_VERSION))
	@test -f .custom-gcl.yml && { \
		echo "Building custom golangci-lint with plugins..." && \
		$(GOLANGCI_LINT) custom --destination $(LOCALBIN) --name golangci-lint-custom && \
		mv -f $(LOCALBIN)/golangci-lint-custom $(GOLANGCI_LINT); \
	} || true

# go-install-tool will 'go install' any package with custom target and name of binary, if it doesn't exist
# $1 - target path with name of binary
# $2 - package url which can be installed
# $3 - specific version of package
define go-install-tool
@[ -f "$(1)-$(3)" ] && [ "$$(readlink -- "$(1)" 2>/dev/null)" = "$(1)-$(3)" ] || { \
set -e; \
package=$(2)@$(3) ;\
echo "Downloading $${package}" ;\
rm -f "$(1)" ;\
GOBIN="$(LOCALBIN)" ./scripts/go-retry.sh go install $${package} ;\
mv "$(LOCALBIN)/$$(basename "$(1)")" "$(1)-$(3)" ;\
} ;\
ln -sf "$$(realpath "$(1)-$(3)")" "$(1)"
endef

define gomodver
$(shell go list -m -f '{{if .Replace}}{{.Replace.Version}}{{else}}{{.Version}}{{end}}' $(1) 2>/dev/null)
endef
