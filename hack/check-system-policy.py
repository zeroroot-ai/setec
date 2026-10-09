#!/usr/bin/env python3
"""check-system-policy.py RENDER NAMESPACE: the setec namespace has the default deny and an allow for each component.

Reads one rendered chart and fails when:
  - no CiliumClusterwideNetworkPolicy selects the namespace by
    k8s:io.kubernetes.pod.namespace with ingress and egress denied by default
    and DNS to kube-dns allowed,
  - a Deployment or DaemonSet of the namespace (except the device plugin,
    which talks to the kubelet over a unix socket) has no CiliumNetworkPolicy
    that selects its component,
  - the operator or the frontend policy has no kube-apiserver egress,
  - the render has no CiliumNetworkPolicy for the Job pods that the operator
    makes at run time (BUILDER_JOBS), or that policy has no egress to the
    world on port 443. A Job is not in the render, so the Deployment loop
    above cannot see it. Without the allow the disk builder cannot reach a
    registry and every launcher Sandbox ends in DiskBuildFailed,
  - an ingress peer of a namespace policy (fromEndpoints) widens its
    namespace with a matchExpression on k8s:io.kubernetes.pod.namespace.
    Cilium scopes a peer with no namespace label to the namespace of the
    policy, and a matchLabels entry pins one namespace. A Pod label alone is
    not proof: the person who creates a Pod chooses its labels (setec#252).

  check-system-policy.py RENDER NS   exit 1 on a finding
"""
import sys

import yaml

NO_NETWORK = {"device-plugin"}
NEEDS_API = {"operator", "frontend"}
# The component label of the Job pods that the operator makes
# (internal/controller/launcher_disk.go and pool_verify.go).
BUILDER_JOBS = {"disk-builder", "image-verify"}


def selected_components(cnp):
    sel = cnp["spec"].get("endpointSelector") or {}
    comps = set()
    comp = (sel.get("matchLabels") or {}).get("app.kubernetes.io/component")
    if comp:
        comps.add(comp)
    for e in sel.get("matchExpressions") or []:
        if e.get("key") == "app.kubernetes.io/component" and e.get("operator") == "In":
            comps.update(e.get("values") or [])
    return comps


def reaches_world_443(cnp):
    for e in cnp["spec"].get("egress") or []:
        if "world" in (e.get("toEntities") or []) and any(
                p.get("port") == "443" for tp in e.get("toPorts") or [] for p in tp.get("ports") or []):
            return True
    return False


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
    for job in sorted(BUILDER_JOBS):
        covering = [d for d in docs if d.get("kind") == "CiliumNetworkPolicy" and d["metadata"].get("namespace") == ns
                    and job in selected_components(d)]
        if not covering:
            out.append(f"the Job pods of the operator ({job}) have no CiliumNetworkPolicy, so the default deny cuts them off")
        elif not any(reaches_world_443(d) for d in covering):
            out.append(f"the policy of the {job} Job pods has no egress to the world on port 443")
    for comp, d in cnp.items():
        for rule in d["spec"].get("ingress") or []:
            for peer in rule.get("fromEndpoints") or []:
                if any(e.get("key") == "k8s:io.kubernetes.pod.namespace" for e in peer.get("matchExpressions") or []):
                    out.append(f"the {comp} policy admits a peer from more than one namespace: {peer}")
    return out


if __name__ == "__main__":
    path, ns = sys.argv[1], sys.argv[2]
    bad = judge(list(yaml.safe_load_all(open(path))), ns)
    for b in bad:
        print(b)
    sys.exit(1 if bad else 0)
