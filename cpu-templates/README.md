<!-- SPDX-License-Identifier: Apache-2.0 -->
# Custom CPU templates

The launcher image holds each file here at `/opt/setec/cpu-templates/`. A
SandboxClass names one in `spec.cpuTemplate`, without `.json`. Each machine
of the class then shows the guest the same CPU features, so a snapshot that
one node of the class takes loads on each other node of the class
(`docs/design/runtime.md`).

| File | Instance types | Source |
|---|---|---|
| `m8i.json` | m8i (Intel Xeon 6, Granite Rapids), on a host kernel 5.17 or later | Firecracker v1.17.0, `tests/data/custom_cpu_templates/GNR_TO_T2_6.1.json` (commit `95f868c8e345b1cc8faccd1a3c910b4989dc3f58`), unchanged |

`m8i.json` turns off the CPU features that the T2 template of Firecracker
turns off, so the guest sees one feature set on each Granite Rapids node.
Firecracker uses this template with a host kernel of 5.17 or later, because
an older kernel does not support Intel AMX.

The templates come from Firecracker, Copyright 2017-2020 Amazon.com, Inc.
or its affiliates, under the Apache License 2.0.

To add a template for another instance family, add `<name>.json` here. The
test `TestCPUTemplates_AreValid` in `internal/launcher` checks its format,
and `Dockerfile.launcher` copies each file.
