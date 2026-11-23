#!/usr/bin/env python3
"""check-selectors.py RENDER [--adopt OLD]: the selectors of the chart are disjoint.

Reads one rendered chart and fails when:
  - the selector of a Deployment, StatefulSet or DaemonSet does not match
    the labels of its own Pod template,
  - the selector of a workload matches the Pod template of another workload
    of the same namespace. Such a selector also owns the Pods of the other
    workload, and `kubectl logs deploy/NAME`, a PodDisruptionBudget and a
    dashboard then read the wrong Pods (setec#383),
  - the selector of a Service or a PodDisruptionBudget matches no workload,
    or matches the Pods of more than one workload.

With --adopt OLD (a Deployment manifest of a previous release), it also
fails when the Deployment of the same name in RENDER does not select the Pod
template labels of OLD. After `kubectl delete deployment NAME
--cascade=orphan`, the new Deployment adopts the ReplicaSets and Pods of the
old one only when its selector matches their labels. That is the upgrade
path of the chart README.

  check-selectors.py RENDER [--adopt OLD]   exit 1 on a finding
"""
import sys

import yaml

WORKLOADS = ("Deployment", "StatefulSet", "DaemonSet")


def matches(selector, labels):
    """True when a label selector (matchLabels, matchExpressions) selects labels."""
    for k, v in (selector.get("matchLabels") or {}).items():
        if labels.get(k) != v:
            return False
    for e in selector.get("matchExpressions") or []:
        key, op, vals = e["key"], e["operator"], e.get("values") or []
        if op == "In" and labels.get(key) not in vals:
            return False
        if op == "NotIn" and key in labels and labels[key] in vals:
            return False
        if op == "Exists" and key not in labels:
            return False
        if op == "DoesNotExist" and key in labels:
            return False
    # An empty selector selects nothing for a workload and everything for a
    # policy. Here it is a finding of the callers, so it does not match.
    return bool(selector.get("matchLabels") or selector.get("matchExpressions"))


def ident(d):
    return f"{d['kind']}/{d['metadata'].get('namespace', '')}/{d['metadata']['name']}"


def template_labels(d):
    return (((d["spec"].get("template") or {}).get("metadata") or {}).get("labels")) or {}


def judge(docs, old=None):
    docs = [d for d in docs if isinstance(d, dict)]
    out = []
    workloads = [d for d in docs if d.get("kind") in WORKLOADS]
    for w in workloads:
        sel = w["spec"].get("selector") or {}
        if not matches(sel, template_labels(w)):
            out.append(f"{ident(w)}: its selector does not match its own Pod template")
        for x in workloads:
            if x is w or x["metadata"].get("namespace") != w["metadata"].get("namespace"):
                continue
            if matches(sel, template_labels(x)):
                out.append(f"{ident(w)}: its selector also matches the Pods of {ident(x)}")
    for d in docs:
        kind = d.get("kind")
        if kind == "Service":
            sel = {"matchLabels": d["spec"].get("selector") or {}}
        elif kind == "PodDisruptionBudget":
            sel = d["spec"].get("selector") or {}
        else:
            continue
        if not (sel.get("matchLabels") or sel.get("matchExpressions")):
            if kind == "Service" and d["spec"].get("type") == "ExternalName":
                continue
            out.append(f"{ident(d)}: it has no selector")
            continue
        ns = d["metadata"].get("namespace")
        hit = [w for w in workloads if w["metadata"].get("namespace") == ns and matches(sel, template_labels(w))]
        if not hit:
            out.append(f"{ident(d)}: its selector matches the Pods of no workload")
        elif len(hit) > 1:
            out.append(f"{ident(d)}: its selector matches the Pods of {len(hit)} workloads: " + ", ".join(ident(h) for h in hit))
    if old is not None:
        new = [w for w in workloads if w["kind"] == old["kind"] and w["metadata"]["name"] == old["metadata"]["name"]
               and w["metadata"].get("namespace") == old["metadata"].get("namespace")]
        if not new:
            out.append(f"the render has no {ident(old)} to adopt the Pods of the old release")
        elif not matches(new[0]["spec"].get("selector") or {}, template_labels(old)):
            out.append(f"{ident(new[0])}: its selector does not match the Pod labels of the old release, "
                       "so it would not adopt the orphaned ReplicaSets")
    return out


if __name__ == "__main__":
    args = sys.argv[1:]
    old = None
    if "--adopt" in args:
        i = args.index("--adopt")
        old = yaml.safe_load(open(args[i + 1]))
        del args[i : i + 2]
    bad = judge(list(yaml.safe_load_all(open(args[0]))), old)
    for b in bad:
        print(b)
    sys.exit(1 if bad else 0)
