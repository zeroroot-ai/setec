<!-- SPDX-License-Identifier: Apache-2.0 -->
# Developer Notes

Internal conventions for Setec contributors. These are deliberately narrow; most development guidance lives in [CONTRIBUTING.md](../CONTRIBUTING.md).

## Naming conventions

| Thing                              | Canonical form    | Notes                                                   |
|------------------------------------|-------------------|---------------------------------------------------------|
| Project name (prose, docs, titles) | `Setec`           | Capital S, lowercase rest. Never `SETEC`, never `setec`. |
| CRD API group                      | `setec.zeroroot.ai`        | Lowercase; matches `api/v1alpha1` Go types.             |
| Go module path                     | `github.com/zeroroot-ai/setec` | Lowercase; GitHub org convention.             |
| Helm release name (examples)       | `setec`           | Lowercase, matches DNS naming rules.                    |
| Default namespace                  | `setec-system`    | Lowercase with hyphen.                                  |
| Operator binary (in prose)         | `setec-operator`  | The container image and the product name in sentences.  |
| Operator binary (on disk)          | `bin/manager`     | Kubebuilder scaffold default; preserved for tooling.    |
| Environment variable prefix        | `SETEC_*`         | Upper snake case by convention.                         |

### Why two names for the operator binary

Kubebuilder scaffolds the controller binary as `cmd/main.go` producing `bin/manager`. Renaming that path would ripple through Makefile targets, CI workflows, and Dockerfile stages without user-visible benefit. The authoritative name in prose, marketing, and container-image tags is `setec-operator`; the path on disk stays `bin/manager`. When writing new docs or release notes, always say "the `setec-operator` image" or "the Setec operator".

### Environment variables

Environment variables follow upper snake case with a `SETEC_` prefix, e.g. `SETEC_E2E_CHART`. This is standard Unix convention and does not conflict with the prose rule above.

## Where other conventions live

- Commit messages: see [CONTRIBUTING.md](../CONTRIBUTING.md#commit-conventions).
- CRD authoring: see [docs/crd-reference.md](./crd-reference.md).
- Metrics naming: see [docs/observability.md](./observability.md).
- Release engineering: see `charts/setec/Chart.yaml` for version, `CHANGELOG.md` for the current release state.
