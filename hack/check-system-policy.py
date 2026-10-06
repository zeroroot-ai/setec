#!/usr/bin/env python3
"""check-system-policy.py RENDER NAMESPACE: the setec namespace has the default deny and an allow for each component.

Reads one rendered chart and fails when:
  - no CiliumClusterwideNetworkPolicy selects the namespace by
    k8s:io.kubernetes.pod.namespace with ingress and egress denied by default
    and DNS to kube-dns allowed,
  - a Deployment or DaemonSet of the namespace (except the device plugin,
    which talks to the kubelet over a unix socket) has no CiliumNetworkPolicy
    that selects its component,
  - the operator or the frontend policy has no kube-apiserver egress.

  check-system-policy.py RENDER NS   exit 1 on a finding
"""
import sys

import yaml

NO_NETWORK = {"device-plugin"}
NEEDS_API = {"operator", "frontend"}


def judge(docs, ns):
    docs = [d for d in docs if isinstance(d, dict)]
    out = []
    deny = [d for d in docs if d.get("kind") == "CiliumClusterwideNetworkPolicy"
            and ((d["spec"].get("endpointSelector") or {}).get("matchLabels") or {}).get("k8s:io.kubernetes.pod.namespace") == ns
            and (d["spec"].get("enableDefaultDeny") or {}) == {"ingress": True, "egress": True}
            and any("kube-dns" in str(e) for e in d["spec"].get("egress") or [])]
    if not deny:
        out.append(f"no default deny with DNS covers the namespace {ns}")
    cnp = {}
    for d in docs:
        if d.get("kind") == "CiliumNetworkPolicy" and d["metadata"].get("namespace") == ns:
            comp = ((d["spec"].get("endpointSelector") or {}).get("matchLabels") or {}).get("app.kubernetes.io/component")
            cnp[comp] = d
    for d in docs:
        if d.get("kind") in ("Deployment", "DaemonSet") and d["metadata"].get("namespace") == ns:
            comp = (d["spec"]["template"]["metadata"].get("labels") or {}).get("app.kubernetes.io/component")
            if comp in NO_NETWORK:
                continue
            if comp not in cnp:
                out.append(f"{d['kind']}/{d['metadata']['name']} ({comp}) has no CiliumNetworkPolicy, so the default deny cuts it off")
            elif comp in NEEDS_API and not any("kube-apiserver" in (e.get("toEntities") or []) for e in cnp[comp]["spec"].get("egress") or []):
                out.append(f"the {comp} policy has no kube-apiserver egress")
    return out


if __name__ == "__main__":
    path, ns = sys.argv[1], sys.argv[2]
    bad = judge(list(yaml.safe_load_all(open(path))), ns)
    for b in bad:
        print(b)
    sys.exit(1 if bad else 0)
