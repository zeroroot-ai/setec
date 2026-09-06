#!/usr/bin/env bash
# Apply the dev client TLS Secret + cross-cluster smoke Job into the existing
# Kind 'gibson' cluster, then wait for the Job to complete.
#
# Prerequisites:
#   - Kind cluster 'gibson' is running and current kube context can reach it
#   - PKI exists under ../pki/ (run 30-generate-pki.sh first)
#
# The Job dials the Setec frontend at the host's LAN IP (the NodePort
# exposure on the k3s cluster). Kind Pods can reach the host LAN directly
# via the Docker bridge — no Kind cluster config changes required.

set -eo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PKI="${ROOT}/pki"
KIND_CONTEXT="${KIND_CONTEXT:-kind-gibson}"
NODEPORT="${SETEC_NODEPORT:-30051}"
HOST_IP="$(ip -4 route get 1.1.1.1 2>/dev/null | awk '{for(i=1;i<=NF;i++) if($i=="src"){print $(i+1); exit}}')"
SETEC_ADDR="${SETEC_ADDR:-${HOST_IP}:${NODEPORT}}"

green() { printf '\033[0;32m%s\033[0m\n' "$*"; }
red()   { printf '\033[0;31m%s\033[0m\n' "$*"; }

[[ -f "${PKI}/ca.crt" && -f "${PKI}/client.crt" && -f "${PKI}/client.key" ]] || {
    red "FAIL: dev PKI missing — run scripts/30-generate-pki.sh first"
    exit 1
}
kubectl --context="${KIND_CONTEXT}" get ns gibson >/dev/null 2>&1 || {
    red "FAIL: namespace 'gibson' not found in context ${KIND_CONTEXT} — is the Gibson chart deployed?"
    exit 1
}

# Materialise the Secret manifest from the template
GEN="${ROOT}/manifests/gibson-kind/setec-client-tls.generated.yaml"
sed \
    -e "s|__CA_B64__|$(base64 -w0 < "${PKI}/ca.crt")|" \
    -e "s|__CLIENT_CRT_B64__|$(base64 -w0 < "${PKI}/client.crt")|" \
    -e "s|__CLIENT_KEY_B64__|$(base64 -w0 < "${PKI}/client.key")|" \
    "${ROOT}/manifests/gibson-kind/setec-client-tls.yaml.tpl" > "${GEN}"
green "Generated ${GEN}"

green "Applying TLS Secret to ${KIND_CONTEXT}/gibson"
kubectl --context="${KIND_CONTEXT}" apply -f "${GEN}"

green "Applying smoke Job (deletes any prior run first)"
kubectl --context="${KIND_CONTEXT}" -n gibson delete job setec-smoke-cross-cluster --ignore-not-found
kubectl --context="${KIND_CONTEXT}" -n gibson delete configmap setec-smoke-source --ignore-not-found

# Substitute SETEC_ADDR into the job manifest (template uses __SETEC_ADDR__).
JOB_RENDERED=$(mktemp)
sed "s|__SETEC_ADDR__|${SETEC_ADDR}|" "${ROOT}/manifests/gibson-kind/setec-smoke-job.yaml" > "${JOB_RENDERED}"
green "Dialling Setec at ${SETEC_ADDR}"
kubectl --context="${KIND_CONTEXT}" apply -f "${JOB_RENDERED}"
rm -f "${JOB_RENDERED}"

green "Waiting for Job to complete (up to 5m)..."
if ! kubectl --context="${KIND_CONTEXT}" -n gibson wait --for=condition=Complete --timeout=5m job/setec-smoke-cross-cluster 2>/dev/null; then
    red "FAIL: smoke Job did not complete. Pod logs:"
    kubectl --context="${KIND_CONTEXT}" -n gibson logs -l job-name=setec-smoke-cross-cluster --tail=200 2>&1 || true
    echo '--- events ---'
    kubectl --context="${KIND_CONTEXT}" -n gibson get events --field-selector involvedObject.name=setec-smoke-cross-cluster --sort-by=.lastTimestamp 2>&1 || true
    exit 1
fi

green "PASS: cross-cluster Job completed. Logs:"
kubectl --context="${KIND_CONTEXT}" -n gibson logs -l job-name=setec-smoke-cross-cluster --tail=50
