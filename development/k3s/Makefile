# Local k3s + Kata + Setec dev environment.
#
# Usage:
#   make help            # list targets
#   make up              # full bring-up: preflight → k3s → kata → gvisor → pki → setec
#   make up-all          # same as up, plus runc RuntimeClass (dev-only, not for production)
#   make smoke-kata      # verify Pod runs in Firecracker microVM
#   make smoke-setec     # exercise Setec end-to-end with the example client
#   make smoke-cross-cluster   # prove Kind→k3s mTLS gRPC reachability
#   make smoke-integration     # full Gibson→Setec end-to-end (requires Phase D)
#   make kubeconfig      # print KUBECONFIG export line
#   make down            # uninstall k3s + remove PKI/kubeconfig
#
# Scripts in scripts/ are numbered to make ordering obvious.
# Default `up` installs kata-fc + kata-qemu + gvisor (REQ-5.4).
# runc is opt-in via `up-all` — it provides no isolation beyond a plain
# Linux container and must not be used in production clusters.

SHELL := /usr/bin/env bash
.SHELLFLAGS := -eo pipefail -c

ROOT := $(shell pwd)
SCRIPTS := $(ROOT)/scripts
KUBECONFIG_PATH := $(ROOT)/kubeconfig

.PHONY: help
help: ## List targets.
	@awk 'BEGIN {FS = ":.*##"; printf "Targets:\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-22s\033[0m %s\n", $$1, $$2 }' $(MAKEFILE_LIST)

.PHONY: up
up: ## Full bring-up: preflight → k3s → kata (fc+qemu) → gvisor → devmapper → pki → setec.
	$(SCRIPTS)/00-preflight.sh
	$(SCRIPTS)/10-install-k3s.sh
	$(SCRIPTS)/20-install-kata.sh
	$(SCRIPTS)/21-install-gvisor.sh
	$(SCRIPTS)/25-install-devmapper.sh
	$(SCRIPTS)/30-generate-pki.sh
	$(SCRIPTS)/40-install-setec.sh

.PHONY: up-all
up-all: up ## Same as 'up' plus runc RuntimeClass. WARNING: runc is dev-only, no production use.
	$(SCRIPTS)/22-install-runc-profile.sh

.PHONY: smoke-kata
smoke-kata: ## Verify a Pod runs under the kata-fc RuntimeClass (microVM).
	$(SCRIPTS)/50-smoke-kata.sh

.PHONY: smoke-setec
smoke-setec: ## Exercise the Setec example client end-to-end.
	$(SCRIPTS)/60-smoke-setec.sh

.PHONY: smoke-cross-cluster
smoke-cross-cluster: ## Apply client TLS Secret + smoke Job into the existing Kind 'gibson' cluster.
	$(SCRIPTS)/65-smoke-cross-cluster.sh

.PHONY: smoke-integration
smoke-integration: ## Build hello-dev tool image, load to k3s, exercise Gibson→Setec end-to-end.
	$(SCRIPTS)/70-smoke-integration.sh

.PHONY: kubeconfig
kubeconfig: ## Print KUBECONFIG export line for the local k3s cluster.
	@echo "export KUBECONFIG=$(KUBECONFIG_PATH)"

.PHONY: down
down: ## Uninstall k3s and remove dev PKI / kubeconfig.
	$(SCRIPTS)/99-uninstall.sh
