#!/usr/bin/env bash
# 35-mint-client-svid.sh — mint the X509-SVID of the dev client.
#
# The dev client is the enrolled client "dev" of the frontend
# (values-local.yaml): spiffe://dev.local/ns/gibson/sa/gibson-daemon. The
# smoke scripts dial the frontend with this SVID from outside the cluster,
# so the SPIRE server mints it, and no agent serves it. It writes:
#
#   pki/client.crt  the SVID
#   pki/client.key  its private key
#   pki/ca.crt      the trust bundle of dev.local
#
# An SVID lives a few hours. Each smoke script runs this first.
set -eo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
export KUBECONFIG="${SETEC_K3S_KUBECONFIG:-${ROOT}/kubeconfig}"
PKI="${ROOT}/pki"
CLIENT_ID="${SETEC_DEV_CLIENT_ID:-spiffe://dev.local/ns/gibson/sa/gibson-daemon}"
mkdir -p "${PKI}"

out="$(kubectl -n spire-server exec spire-server-0 -c spire-server -- \
    /opt/spire/bin/spire-server x509 mint \
    -socketPath /tmp/spire-server/private/api.sock \
    -spiffeID "${CLIENT_ID}" -ttl 6h)"

# The output holds PEM blocks in order: the SVID certificate, its private
# key, then the root CAs of the trust domain.
printf '%s\n' "${out}" | awk -v crt="${PKI}/client.crt" -v key="${PKI}/client.key" -v ca="${PKI}/ca.crt" '
    /-----BEGIN .*PRIVATE KEY-----/ { inkey = 1; seenkey = 1 }
    /-----BEGIN CERTIFICATE-----/   { incert = 1 }
    inkey  { print > key }
    incert { if (seenkey) print > ca; else print > crt }
    /-----END .*PRIVATE KEY-----/   { inkey = 0 }
    /-----END CERTIFICATE-----/     { incert = 0 }
'
chmod 0600 "${PKI}/client.key"
for f in client.crt client.key ca.crt; do
    [[ -s "${PKI}/${f}" ]] || { echo "FAIL: the minted SVID has no ${f}" >&2; exit 1; }
done
openssl verify -CAfile "${PKI}/ca.crt" "${PKI}/client.crt" >/dev/null
printf '\033[0;32m%s\033[0m\n' "Minted the SVID of ${CLIENT_ID} into ${PKI}/"
