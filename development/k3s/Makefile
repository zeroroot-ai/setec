# Local k3s + Setec dev environment. Each Sandbox is a Firecracker machine
# in a launcher Pod.
#
# Usage:
#   make help            # list targets
#   make up              # full bring-up: preflight → k3s → disk registry → spire → setec
#   make smoke-setec     # exercise Setec end-to-end with the example client
#   make smoke-cross-cluster   # prove Kind→k3s mTLS gRPC reachability with the dev client SVID
#   make smoke-integration     # full Gibson→Setec end-to-end (requires Phase D)
#   make kubeconfig      # print KUBECONFIG export line
#   make down            # uninstall k3s + remove pki/ and kubeconfig + stop the disk registry
#
# Scripts in scripts/ are numbered to make ordering obvious.

SHELL := /usr/bin/env bash
.SHELLFLAGS := -eo pipefail -c

ROOT := $(shell pwd)
SCRIPTS := $(ROOT)/scripts
KUBECONFIG_PATH := $(ROOT)/kubeconfig

.PHONY: help
help: ## List targets.
	@awk 'BEGIN {FS = ":.*##"; printf "Targets:\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-22s\033[0m %s\n", $$1, $$2 }' $(MAKEFILE_LIST)

.PHONY: up
up: ## Full bring-up: preflight → k3s → disk registry → spire → setec.
	$(SCRIPTS)/00-preflight.sh
	$(SCRIPTS)/10-install-k3s.sh
	$(SCRIPTS)/20-install-disk-registry.sh
	$(SCRIPTS)/30-install-spire.sh
	$(SCRIPTS)/40-install-setec.sh

.PHONY: smoke-setec
smoke-setec: ## Exercise the Setec example client end-to-end.
	$(SCRIPTS)/60-smoke-setec.sh

.PHONY: smoke-cross-cluster
smoke-cross-cluster: ## Apply the dev client SVID Secret + smoke Job into the existing Kind 'gibson' cluster.
	$(SCRIPTS)/65-smoke-cross-cluster.sh

.PHONY: smoke-integration
smoke-integration: ## Build hello-dev tool image, load to k3s, exercise Gibson→Setec end-to-end.
	$(SCRIPTS)/70-smoke-integration.sh

.PHONY: kubeconfig
kubeconfig: ## Print KUBECONFIG export line for the local k3s cluster.
	@echo "export KUBECONFIG=$(KUBECONFIG_PATH)"

.PHONY: down
down: ## Uninstall k3s and remove pki/ and kubeconfig.
	$(SCRIPTS)/99-uninstall.sh
