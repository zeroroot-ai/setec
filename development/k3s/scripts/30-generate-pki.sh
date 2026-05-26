#!/usr/bin/env bash
# Generate dev CA + frontend server cert + gibson-dev client cert under pki/.
# Idempotent: skip generation if existing certs are still valid for >30 days.

set -eo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PKI="${ROOT}/pki"

mkdir -p "${PKI}"

green() { printf '\033[0;32m%s\033[0m\n' "$*"; }
yellow(){ printf '\033[0;33m%s\033[0m\n' "$*"; }

# Detect host LAN IP for SAN
host_ip="$(ip -4 route get 1.1.1.1 2>/dev/null | awk '{for(i=1;i<=NF;i++) if($i=="src"){print $(i+1); exit}}')"

# 1) CA
if [[ -f "${PKI}/ca.crt" ]] && openssl x509 -checkend 2592000 -noout -in "${PKI}/ca.crt" >/dev/null 2>&1; then
    yellow "Reusing existing CA at ${PKI}/ca.crt (valid > 30 days)"
else
    green "Generating dev CA at ${PKI}/ca.{crt,key}"
    openssl genrsa -out "${PKI}/ca.key" 4096 2>/dev/null
    openssl req -x509 -new -key "${PKI}/ca.key" -days 3650 -sha256 \
        -subj "/CN=Setec Dev CA/O=zeroroot-ai/OU=dev" \
        -out "${PKI}/ca.crt"
    chmod 0600 "${PKI}/ca.key"
fi

# 2) Server cert for setec-frontend
green "Generating server cert (CN setec-frontend.setec-system.svc)"
cat > "${PKI}/server.cnf" <<EOF
[req]
distinguished_name=req_distinguished_name
prompt=no
[req_distinguished_name]
CN=setec-frontend.setec-system.svc
O=zeroroot-ai
[v3]
basicConstraints=CA:FALSE
keyUsage=digitalSignature,keyEncipherment
extendedKeyUsage=serverAuth
subjectAltName=@san
[san]
DNS.1=setec-frontend.setec-system.svc
DNS.2=setec-frontend.setec-system.svc.cluster.local
DNS.3=setec-frontend
DNS.4=host.docker.internal
DNS.5=localhost
IP.1=${host_ip}
IP.2=127.0.0.1
EOF
openssl genrsa -out "${PKI}/server.key" 4096 2>/dev/null
openssl req -new -key "${PKI}/server.key" -config "${PKI}/server.cnf" -out "${PKI}/server.csr"
openssl x509 -req -in "${PKI}/server.csr" -CA "${PKI}/ca.crt" -CAkey "${PKI}/ca.key" \
    -CAcreateserial -days 365 -sha256 \
    -extensions v3 -extfile "${PKI}/server.cnf" \
    -out "${PKI}/server.crt" 2>/dev/null
chmod 0600 "${PKI}/server.key"
rm -f "${PKI}/server.csr" "${PKI}/server.cnf"

# 3) Client cert for tenant gibson-dev
green "Generating client cert (CN gibson-dev)"
cat > "${PKI}/client.cnf" <<EOF
[req]
distinguished_name=req_distinguished_name
prompt=no
[req_distinguished_name]
CN=gibson-dev
O=zeroroot-ai
[v3]
basicConstraints=CA:FALSE
keyUsage=digitalSignature,keyEncipherment
extendedKeyUsage=clientAuth
EOF
openssl genrsa -out "${PKI}/client.key" 4096 2>/dev/null
openssl req -new -key "${PKI}/client.key" -config "${PKI}/client.cnf" -out "${PKI}/client.csr"
openssl x509 -req -in "${PKI}/client.csr" -CA "${PKI}/ca.crt" -CAkey "${PKI}/ca.key" \
    -CAcreateserial -days 365 -sha256 \
    -extensions v3 -extfile "${PKI}/client.cnf" \
    -out "${PKI}/client.crt" 2>/dev/null
chmod 0600 "${PKI}/client.key"
rm -f "${PKI}/client.csr" "${PKI}/client.cnf"

# Verify
openssl verify -CAfile "${PKI}/ca.crt" "${PKI}/server.crt" >/dev/null
openssl verify -CAfile "${PKI}/ca.crt" "${PKI}/client.crt" >/dev/null
green "PKI generated under ${PKI}/"
ls -1 "${PKI}/"
