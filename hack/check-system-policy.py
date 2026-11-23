#!/usr/bin/env python3
"""check-system-policy.py RENDER NAMESPACE: the setec namespace has the default deny and an allow for each component.

The chart has two engines (values.yaml systemPolicy.engine). The checker reads
the objects of the render and judges the engine that it finds: Cilium objects
or plain NetworkPolicy objects. The rules of both engines are the same plan.
The plain engine has its own list at the end of this text.

Reads one rendered chart and fails when, for the cilium engine:
  - no CiliumClusterwideNetworkPolicy selects the namespace by
    k8s:io.kubernetes.pod.namespace with ingress and egress denied by default
    and DNS to kube-dns allowed,
  - a Deployment or DaemonSet of the namespace (except the device plugin,
    which talks to the kubelet over a unix socket) has no CiliumNetworkPolicy
    that selects its component,
  - the operator or the frontend policy has no kube-apiserver egress,
  - the render has no CiliumNetworkPolicy for a kind of Job pod that the
    operator makes at run time (BUILDER_JOBS). A Job is not in the render,
    so the Deployment loop above cannot see it. Without the allow the disk
    builder cannot reach a registry and every launcher Sandbox ends in
    DiskBuildFailed,
  - a policy of those Job pods reaches the world: toEntities world or all,
    a CIDR of the whole address space, or a toFQDNs pattern of "*". Each
    host is named (setec#279),
  - the disk Job policy does not reach the disk registry (the host of
    --disk-repo of the operator),
  - an ingress peer of a namespace policy (fromEndpoints) widens its
    namespace with a matchExpression on k8s:io.kubernetes.pod.namespace.
    Cilium scopes a peer with no namespace label to the namespace of the
    policy, and a matchLabels entry pins one namespace. A Pod label alone is
    not proof: the person who creates a Pod chooses its labels (setec#252).

  For the networkpolicy engine, it fails when:
  - no NetworkPolicy selects every Pod of the namespace (podSelector {}) with
    both policy types, no ingress rule, and egress to port 53 only,
  - a Deployment or DaemonSet of the namespace (except the device plugin) has
    no NetworkPolicy that selects its component,
  - the operator or the frontend policy has no egress to an ipBlock (the API
    server),
  - the render has no NetworkPolicy for a kind of Job pod (BUILDER_JOBS),
  - a rule of any policy has no peer or no port. An empty list of peers or
    ports allows every peer or every port,
  - a rule reaches the world: an ipBlock of the whole address space with no
    except list,
  - a Job policy has no ipBlock that covers the disk registry, when the disk
    registry is an IPv4 address,
  - an ingress peer widens its namespace: a namespaceSelector that does not
    pin one namespace with the label kubernetes.io/metadata.name.

  check-system-policy.py RENDER NS   exit 1 on a finding
"""
import ipaddress
import sys

import yaml

NO_NETWORK = {"device-plugin"}
NEEDS_API = {"operator", "frontend"}
# The component label of the Job pods that the operator makes
# (internal/controller/launcher_disk.go and pool_verify.go).
BUILDER_JOBS = {"disk-builder", "image-verify"}
# The Jobs that push to the disk registry.
DISK_REGISTRY_JOBS = {"disk-builder"}
WORLD_ENTITIES = {"world", "all"}
WORLD_CIDRS = {"0.0.0.0/0", "::/0"}


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


def world_egress(cnp):
    """The egress peers of cnp that reach the world, as text."""
    out = []
    for e in cnp["spec"].get("egress") or []:
        out += [f"toEntities {x}" for x in e.get("toEntities") or [] if x in WORLD_ENTITIES]
        out += [f"toCIDR {c}" for c in e.get("toCIDR") or [] if c in WORLD_CIDRS]
        out += [f"toCIDRSet {c.get('cidr')}" for c in e.get("toCIDRSet") or [] if c.get("cidr") in WORLD_CIDRS]
        for f in e.get("toFQDNs") or []:
            if (f.get("matchPattern") or "").strip() in {"*", "**"}:
                out.append(f"toFQDNs matchPattern {f.get('matchPattern')}")
        if e.get("toEndpoints") == [{}] or any(not ep for ep in e.get("toEndpoints") or []):
            out.append("toEndpoints {}")
    return out


def egress_hosts(cnp):
    """The named hosts and addresses that cnp reaches."""
    hosts = set()
    for e in cnp["spec"].get("egress") or []:
        for f in e.get("toFQDNs") or []:
            hosts.add(f.get("matchName") or f.get("matchPattern"))
        for c in e.get("toCIDR") or []:
            hosts.add(c.split("/")[0])
        for c in e.get("toCIDRSet") or []:
            hosts.add((c.get("cidr") or "").split("/")[0])
        for x in e.get("toEntities") or []:
            hosts.add(f"entity:{x}")
        if e.get("toEndpoints"):
            hosts.add("endpoints")
    return hosts


def disk_registry_host(docs):
    """The host of --disk-repo of the operator, or None."""
    for d in docs:
        if d.get("kind") != "Deployment":
            continue
        for c in d["spec"]["template"]["spec"].get("containers") or []:
            for a in c.get("args") or []:
                if a.startswith("--disk-repo="):
                    return a.split("=", 1)[1].split("/")[0].split(":")[0]
    return None


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
    disk_host = disk_registry_host(docs)
    for job in sorted(BUILDER_JOBS):
        covering = [d for d in docs if d.get("kind") == "CiliumNetworkPolicy" and d["metadata"].get("namespace") == ns
                    and job in selected_components(d)]
        if disk_host is None:
            continue  # no launcher: the operator makes no Job
        if not covering:
            out.append(f"the Job pods of the operator ({job}) have no CiliumNetworkPolicy, so the default deny cuts them off")
            continue
        for d in covering:
            for w in world_egress(d):
                out.append(f"the policy {d['metadata']['name']} lets the {job} Job pods reach the world ({w})")
        hosts = set().union(*(egress_hosts(d) for d in covering))
        if job in DISK_REGISTRY_JOBS and disk_host not in hosts:
            out.append(f"the policy of the {job} Job pods does not reach the disk registry {disk_host}")
    for comp, d in cnp.items():
        for rule in d["spec"].get("ingress") or []:
            for peer in rule.get("fromEndpoints") or []:
                if any(e.get("key") == "k8s:io.kubernetes.pod.namespace" for e in peer.get("matchExpressions") or []):
                    out.append(f"the {comp} policy admits a peer from more than one namespace: {peer}")
    return out


def np_components(np):
    comp = ((np["spec"].get("podSelector") or {}).get("matchLabels") or {}).get("app.kubernetes.io/component")
    return {comp} if comp else set()


def np_world(np):
    """The egress peers of np that reach the world, as text."""
    out = []
    for e in np["spec"].get("egress") or []:
        for t in e.get("to") or []:
            blk = t.get("ipBlock") or {}
            if blk.get("cidr") in WORLD_CIDRS and not blk.get("except"):
                out.append(f"ipBlock {blk['cidr']}")
    return out


def np_reaches(np, addr):
    """True when an egress ipBlock of np covers the IPv4 address addr."""
    ip = ipaddress.ip_address(addr)
    for e in np["spec"].get("egress") or []:
        for t in e.get("to") or []:
            blk = t.get("ipBlock") or {}
            if not blk.get("cidr"):
                continue
            net = ipaddress.ip_network(blk["cidr"], strict=False)
            if ip.version == net.version and ip in net and not any(
                    ip in ipaddress.ip_network(x, strict=False) for x in blk.get("except") or []
                    if ipaddress.ip_network(x, strict=False).version == ip.version):
                return True
    return False


def judge_np(docs, ns):
    docs = [d for d in docs if isinstance(d, dict)]
    out = []
    pols = [d for d in docs if d.get("kind") == "NetworkPolicy" and d["metadata"].get("namespace") == ns]
    deny = []
    for d in pols:
        spec = d["spec"]
        ports = [p.get("port") for e in spec.get("egress") or [] for p in e.get("ports") or []]
        if (spec.get("podSelector") == {} and set(spec.get("policyTypes") or []) == {"Ingress", "Egress"}
                and not spec.get("ingress") and ports and set(ports) == {53}):
            deny.append(d)
    if not deny:
        out.append(f"no default deny with DNS covers the namespace {ns}")
    by_comp = {}
    for d in pols:
        for comp in np_components(d):
            by_comp[comp] = d
    for d in docs:
        if d.get("kind") in ("Deployment", "DaemonSet") and d["metadata"].get("namespace") == ns:
            comp = (d["spec"]["template"]["metadata"].get("labels") or {}).get("app.kubernetes.io/component")
            if comp in NO_NETWORK:
                continue
            if comp not in by_comp:
                out.append(f"{d['kind']}/{d['metadata']['name']} ({comp}) has no NetworkPolicy, so the default deny cuts it off")
            elif comp in NEEDS_API and not any(
                    (t.get("ipBlock") or {}).get("cidr") for e in by_comp[comp]["spec"].get("egress") or [] for t in e.get("to") or []):
                out.append(f"the {comp} policy has no egress to the API server (an ipBlock)")
    disk_host = disk_registry_host(docs)
    for job in sorted(BUILDER_JOBS):
        if disk_host is None:
            continue  # no launcher: the operator makes no Job
        covering = [d for d in pols if job in np_components(d)]
        if not covering:
            out.append(f"the Job pods of the operator ({job}) have no NetworkPolicy, so the default deny cuts them off")
            continue
        for d in covering:
            for w in np_world(d):
                out.append(f"the policy {d['metadata']['name']} lets the {job} Job pods reach the world ({w})")
        try:
            ipaddress.ip_address(disk_host)
        except ValueError:
            continue
        if job in DISK_REGISTRY_JOBS and not any(np_reaches(d, disk_host) for d in covering):
            out.append(f"the policy of the {job} Job pods does not reach the disk registry {disk_host}")
    for d in pols:
        name = d["metadata"]["name"]
        for direction, key in (("egress", "to"), ("ingress", "from")):
            for i, rule in enumerate(d["spec"].get(direction) or []):
                if not rule.get(key):
                    out.append(f"the policy {name}: {direction} rule {i} has no peer, so it allows every peer")
                if not rule.get("ports"):
                    out.append(f"the policy {name}: {direction} rule {i} has no port, so it allows every port")
                for peer in rule.get(key) or []:
                    sel = peer.get("namespaceSelector")
                    if direction == "ingress" and sel is not None and "k8s:" not in str(sel) and not (
                            (sel.get("matchLabels") or {}).get("kubernetes.io/metadata.name")):
                        out.append(f"the policy {name} admits a peer from more than one namespace: {peer}")
    return out


def judge_any(docs, ns):
    docs = [d for d in docs if isinstance(d, dict)]
    if any(str(d.get("kind")).startswith("Cilium") for d in docs):
        return judge(docs, ns)
    return judge_np(docs, ns)


if __name__ == "__main__":
    path, ns = sys.argv[1], sys.argv[2]
    bad = judge_any(list(yaml.safe_load_all(open(path))), ns)
    for b in bad:
        print(b)
    sys.exit(1 if bad else 0)
