# Template materialised by 65-smoke-cross-cluster.sh.
# DO NOT EDIT the generated file — it's gitignored and overwritten on each run.
apiVersion: v1
kind: Secret
metadata:
  name: setec-client-tls
  namespace: gibson
  labels:
    setec.zeroroot.ai/dev-only: "true"
type: Opaque
data:
  ca.crt: __CA_B64__
  client.crt: __CLIENT_CRT_B64__
  client.key: __CLIENT_KEY_B64__
