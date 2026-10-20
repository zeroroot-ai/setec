#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright 2026 Zero Root AI
#
# The federation exit test (setec#251, ADR-0164). One kind cluster runs
# three SPIRE servers, each with its own trust domain:
#
#   a  the install. Its caller is on the allow list of the fleet.
#   b  the setec fleet. Its frontend serves the callers.
#   c  an install that nobody enrolled. It trusts the fleet, but the fleet
#      has no bundle of c.
#
# The chart of setec makes the federation of the fleet with a
# (frontend.clients[].federation). The test makes the other half, the
# federation of a and c with b, because that half belongs to the install
# (Gibson), not to setec.
#
# It proves two things. The caller of a reaches the frontend of b and gets
# an application answer. The caller of c is refused in the TLS handshake.
#
# Environment: FRONTEND_IMAGE and CLIENT_IMAGE name the images that
# kind has loaded. Run it on a cluster that you can delete.
set -euo pipefail

: "${FRONTEND_IMAGE:?set FRONTEND_IMAGE, e.g. setec-frontend:fed}"
: "${CLIENT_IMAGE:?set CLIENT_IMAGE, e.g. setec-fedclient:fed}"
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
WORK="$(mktemp -d)"

SPIRE_REPO=https://spiffe.github.io/helm-charts-hardened
SPIRE_VERSION=0.28.4
SPIRE_CRDS_VERSION=0.5.0
# The trust domains. No other place holds one.
TD_A=a.test
TD_B=b.test
TD_C=c.test
FLEET_NS=setec-system
FLEET_RELEASE=setec

diagnose() {
  echo "::group::diagnostics"
  kubectl get pods -A -o wide || true
  kubectl get clusterfederatedtrustdomains -o yaml || true
  kubectl get clusterspiffeids -o wide || true
  for x in a b c; do
    kubectl -n "spire-$x" logs "spire-$x-server-0" -c spire-server --tail=80 || true
    kubectl -n "spire-$x" logs -l app.kubernetes.io/name=server -c spire-controller-manager --tail=60 || true
  done
  kubectl -n spire-a logs -l app.kubernetes.io/name=agent --tail=40 || true
  kubectl -n spire-b logs -l app.kubernetes.io/name=agent --tail=40 || true
  kubectl -n "$FLEET_NS" logs deploy/"$FLEET_RELEASE"-frontend --tail=80 || true
  kubectl -n "$FLEET_NS" describe deploy/"$FLEET_RELEASE"-frontend || true
  echo "::endgroup::"
}
trap 'rc=$?; if [ "$rc" -ne 0 ]; then diagnose; fi; exit "$rc"' EXIT

# install_spire <letter> <trust domain> <health port of the agent>
# The agents use the host network, so each one needs a port of its own.
install_spire() {
  local x="$1" td="$2" port="$3"
  kubectl create namespace "spire-$x" --dry-run=client -o yaml | kubectl apply -f -
  helm upgrade --install "spire-$x" spire --repo "$SPIRE_REPO" --version "$SPIRE_VERSION" \
    --namespace "spire-$x" \
    --set global.spire.trustDomain="$td" \
    --set global.spire.clusterName="fed-$x" \
    --set global.spire.namespaces.server.name="spire-$x" \
    --set global.spire.namespaces.system.name="spire-$x" \
    --set spiffe-csi-driver.enabled=false \
    --set spiffe-oidc-discovery-provider.enabled=false \
    --set spire-server.federation.enabled=true \
    --set "spire-agent.socketPath=/run/spire/agent-sockets-$x/spire-agent.sock" \
    --set "spire-agent.healthChecks.port=$port" \
    --wait --timeout 10m
}

# register_federation <letter> <trust domain of the peer> makes the default
# registration entries of the SPIRE <letter> list the peer under
# federatesWith, so each workload gets the bundle of the peer. SPIRE refuses
# the entry until it holds that bundle, so this runs after the federation.
register_federation() {
  helm upgrade "spire-$1" spire --repo "$SPIRE_REPO" --version "$SPIRE_VERSION" \
    --namespace "spire-$1" --reuse-values \
    --set "spire-server.controllerManager.identities.clusterSPIFFEIDs.default.federatesWith={$2}" \
    --wait --timeout 10m
}

# bundle <letter> prints the SPIFFE bundle of the trust domain of the server.
bundle() {
  kubectl -n "spire-$1" exec "spire-$1-server-0" -c spire-server -- \
    /opt/spire/bin/spire-server bundle show -format spiffe
}

# federate <letter> <peer letter> <peer trust domain> <peer bundle file>
# makes the ClusterFederatedTrustDomain by which the SPIRE of <letter> trusts the peer.
federate() {
  local x="$1" y="$2" ytd="$3" file="$4"
  jq -n --arg name "fed-$x-to-$y" --arg td "$ytd" --arg class "spire-$x-spire-$x" \
    --arg url "https://spire-$y-server.spire-$y.svc.cluster.local:8443" \
    --arg id "spiffe://$ytd/spire/server" --rawfile bundle "$file" '{
      apiVersion: "spire.spiffe.io/v1alpha1", kind: "ClusterFederatedTrustDomain",
      metadata: {name: $name},
      spec: {trustDomain: $td, className: $class, bundleEndpointURL: $url,
             bundleEndpointProfile: {type: "https_spiffe", endpointSPIFFEID: $id},
             trustDomainBundle: $bundle}}' | kubectl apply -f -
}

# run_client <letter> <expect> <window> <trust domain of the fleet>
# runs the client Pod in the domain <letter> and waits for its verdict.
run_client() {
  local x="$1" expect="$2" window="$3" ns="client-$1"
  local server_id="spiffe://$TD_B/ns/$FLEET_NS/sa/$FLEET_RELEASE-frontend"
  kubectl create namespace "$ns" --dry-run=client -o yaml | kubectl apply -f -
  kubectl -n "$ns" create serviceaccount client --dry-run=client -o yaml | kubectl apply -f -
  kubectl -n "$ns" delete pod fed-client --ignore-not-found --wait=true
  kubectl apply -f - <<POD
apiVersion: v1
kind: Pod
metadata:
  name: fed-client
  namespace: $ns
  labels:
    app: fed-client
spec:
  serviceAccountName: client
  restartPolicy: Never
  containers:
    - name: client
      image: $CLIENT_IMAGE
      imagePullPolicy: Never
      args:
        - --spiffe-socket=/run/spire/agent-sockets-$x/spire-agent.sock
        - --addr=$FLEET_RELEASE-frontend.$FLEET_NS.svc.cluster.local:50051
        - --server-spiffe-id=$server_id
        - --expect=$expect
        - --window=$window
      volumeMounts:
        - name: spire
          mountPath: /run/spire/agent-sockets-$x
          readOnly: true
  volumes:
    - name: spire
      hostPath:
        path: /run/spire/agent-sockets-$x
        type: Directory
POD
  local phase=""
  for _ in $(seq 1 240); do
    phase="$(kubectl -n "$ns" get pod fed-client -o jsonpath='{.status.phase}' 2>/dev/null || true)"
    [ "$phase" = Succeeded ] || [ "$phase" = Failed ] && break
    sleep 5
  done
  kubectl -n "$ns" logs fed-client --tail=-1 || true
  [ "$phase" = Succeeded ]
}

echo "::group::SPIRE servers"
helm upgrade --install spire-crds spire-crds --repo "$SPIRE_REPO" --version "$SPIRE_CRDS_VERSION" \
  --namespace spire-crds --create-namespace --wait
install_spire a "$TD_A" 9982
install_spire b "$TD_B" 9983
install_spire c "$TD_C" 9984
echo "::endgroup::"

bundle a > "$WORK/a.json"
bundle b > "$WORK/b.json"
bundle c > "$WORK/c.json"

# The install a and the install c trust the fleet.
federate a b "$TD_B" "$WORK/b.json"
federate c b "$TD_B" "$WORK/b.json"

# The fleet trusts the enrolled install a, through the chart of setec. c is
# not enrolled, so the chart makes no federation for it.
jq -n --arg tdb "$TD_B" --arg tda "$TD_A" --rawfile bundle "$WORK/a.json" \
  --arg image "${FRONTEND_IMAGE%%:*}" --arg tag "${FRONTEND_IMAGE##*:}" '{
    webhook: {enabled: false}, replicas: 0, podDisruptionBudget: {enabled: false},
    credentials: {spiffe: {
      trustDomain: $tdb, className: "spire-b-spire-b",
      socketPath: "/run/spire/agent-sockets-b/spire-agent.sock"}},
    frontend: {enabled: true, image: {repository: $image, tag: $tag, pullPolicy: "Never"},
      clients: [{name: "install-a",
        spiffeID: ("spiffe://" + $tda + "/ns/client-a/sa/client"),
        federation: {
          bundleEndpointURL: "https://spire-a-server.spire-a.svc.cluster.local:8443",
          bundleEndpointProfile: "https_spiffe",
          endpointSPIFFEID: ("spiffe://" + $tda + "/spire/server"),
          trustDomainBundle: $bundle}}]},
    systemPolicy: {frontendCallers: [
      {namespace: "client-a", podLabels: {app: "fed-client"}},
      {namespace: "client-c", podLabels: {app: "fed-client"}}]}}' > "$WORK/fleet-values.json"

helm upgrade --install "$FLEET_RELEASE" "$ROOT/charts/setec" --namespace "$FLEET_NS" --create-namespace \
  -f "$ROOT/hack/chart-launcher-values.yaml" -f "$WORK/fleet-values.json"
kubectl -n "$FLEET_NS" rollout status deploy/"$FLEET_RELEASE"-frontend --timeout=300s

# Each workload gets the bundle of its peer. The fleet lists a, the installs
# list b.
register_federation a "$TD_B"
register_federation b "$TD_A"
register_federation c "$TD_B"

echo "::group::the caller in the federated domain a reaches the fleet in b"
run_client a accepted 8m0s
echo "::endgroup::"

echo "::group::the caller in the domain c, which the fleet does not trust, is refused"
run_client c refused 1m30s
echo "::endgroup::"

echo "PASS: the fleet in $TD_B accepted $TD_A and refused $TD_C"
