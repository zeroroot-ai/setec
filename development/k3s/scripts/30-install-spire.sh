#!/usr/bin/env bash
# 30-install-spire.sh — install SPIRE on the dev k3s cluster.
#
# setec has one credential source: the SPIFFE Workload API (setec#175).
# The frontend, the node-agent and the operator each take their SVID from
# the SPIRE agent on the node. This script installs the hardened SPIRE
# charts, at the versions that the platform pins, with the dev trust domain
# dev.local. The default ClusterSPIFFEID of the chart gives each Pod
# spiffe://dev.local/ns/<namespace>/sa/<service account>.
#
# Then it mints the SVID of the dev client (35-mint-client-svid.sh).
set -eo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
export KUBECONFIG="${KUBECONFIG:-${ROOT}/kubeconfig}"

SPIRE_REPO="https://spiffe.github.io/helm-charts-hardened"
SPIRE_VERSION="0.28.4"
SPIRE_CRDS_VERSION="0.5.0"
TRUST_DOMAIN="dev.local"

green() { printf '\033[0;32m%s\033[0m\n' "$*"; }

green "Installing the SPIRE CRDs (${SPIRE_CRDS_VERSION})"
helm upgrade --install spire-crds spire-crds --repo "${SPIRE_REPO}" \
    --version "${SPIRE_CRDS_VERSION}" --namespace spire-mgmt --create-namespace --wait

green "Installing SPIRE (${SPIRE_VERSION}), trust domain ${TRUST_DOMAIN}"
helm upgrade --install spire spire --repo "${SPIRE_REPO}" \
    --version "${SPIRE_VERSION}" --namespace spire-mgmt \
    --set global.spire.trustDomain="${TRUST_DOMAIN}" \
    --set global.spire.clusterName=setec-dev \
    --set global.spire.namespaces.create=true \
    --wait --timeout 10m

"$(dirname "${BASH_SOURCE[0]}")/35-mint-client-svid.sh"
