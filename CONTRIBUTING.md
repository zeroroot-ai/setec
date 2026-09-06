<!--
SPDX-License-Identifier: Apache-2.0
-->

# Contributing to Setec

Thanks for your interest in contributing. Setec is a Kubernetes-native
operator for microVM isolation, and we want it to be approachable for
contributors of every level — from a typo fix to a new CRD feature.

This document covers what you need to know before sending a pull
request. If anything is unclear, please open an issue or a discussion
rather than guessing.

## Development setup

### Prerequisites

- Go 1.25+ (the `go.mod` `toolchain` directive is authoritative)
- `make`
- `kubectl` 1.28+
- `helm` 3.13+
- Docker or Podman (for building container images)
- `git` with DCO sign-off configured (see below)

For full end-to-end testing you also need a bare-metal Linux host with
KVM enabled and Kata Containers installed. The unit and envtest suites
run fine on any workstation.

### Clone and build

```bash
git clone https://github.com/zeroroot-ai/setec.git
cd setec
make generate        # regenerate deepcopy code
make manifests       # regenerate CRD manifests
make proto           # regenerate gRPC stubs
make build           # build the operator binary
```

### Run tests

```bash
make test            # unit + envtest suites (downloads envtest binaries)
make lint            # golangci-lint
make helm-lint       # lint the Helm chart
make helm-template   # render Helm templates with default values
```

End-to-end tests are gated behind the `e2e` build tag because they
require a real Kata + Firecracker environment. Run them on a prepared
bare-metal host with:

```bash
make e2e
```

## Commit conventions

Setec follows the
[Conventional Commits](https://www.conventionalcommits.org/) style.
The first line looks like:

```
<type>(<scope>): <subject>
```

Common types: `feat`, `fix`, `docs`, `chore`, `ci`, `test`, `refactor`.
Examples:

- `feat(api): add preWarmPoolSize to SandboxClass`
- `fix(controller): avoid requeue loop when snapshot is missing`
- `docs(quickstart): clarify KVM prerequisite`
- `test(e2e): un-skip pool cold-start scenario`

Keep the subject under 72 characters. Explain the motivation in the
body if the change is non-obvious.

## Pull request process

1. Fork the repository and create a topic branch from `main`.
2. Make your change in focused commits (see above).
3. Sign off every commit (see DCO below).
4. Run `make test`, `make lint`, and `make helm-lint` locally.
5. Push the branch and open a pull request against `main`. The PR
   template will auto-populate; fill in every section.
6. The default reviewers are listed in
   [.github/CODEOWNERS](.github/CODEOWNERS); they will be requested
   automatically. A maintainer approval plus passing CI is required to
   merge.
7. Squash-merge is the default. Keep the squash subject line in
   Conventional-Commits form.

### CI requirements

Your PR must pass:

- `make test` — unit + envtest
- `make lint` — golangci-lint
- `make helm-lint` — chart lint
- The link checker and the examples-build job (added in Phase 4)

Red CI blocks merge. Maintainers will help you diagnose if the failure
looks unrelated to your change.

## Developer Certificate of Origin (DCO)

Setec uses the
[Developer Certificate of Origin](https://developercertificate.org/).
By signing off, you assert that you wrote the contribution or have the
right to contribute it under Apache 2.0.

Add the sign-off automatically with:

```bash
git commit -s -m "feat: your change"
```

This appends a `Signed-off-by: Your Name <you@example.com>` trailer.
Every commit in a PR must have this trailer — GitHub's DCO check
enforces it. You can backfill existing commits with `git commit --amend
-s` (for the tip) or `git rebase --signoff <base>` (for a range).

We do not require a CLA.

## Proposing larger changes

Anything beyond a small fix or documentation tweak starts with a
specification, not code. Setec uses a lightweight
[spec-workflow](https://github.com/zeroroot-ai/setec/tree/main/.spec-workflow)
directory pattern:

1. Open an issue describing the problem.
2. Draft a requirements doc, a design doc, and a task list under
   `.spec-workflow/specs/<feature-name>/`.
3. Discuss and iterate on the specs before writing code.
4. Implement the tasks one at a time, with reviewable commits.

This keeps review focused on decisions rather than on diffs of code
that has already been written. For examples of what a spec looks like,
see the existing `setec-mvp`, `setec-multitenant`, `setec-snapshots`,
and `setec-launch` specs in the private spec workflow.

## Testing expectations

- **Unit tests.** Every new package gets tests. Keep coverage above
  85% for new code.
- **envtest.** Controller logic is exercised against a real API
  server started by `sigs.k8s.io/controller-runtime/pkg/envtest`. New
  controller branches get envtest coverage.
- **E2E.** Features that cross the operator/node-agent/Firecracker
  boundary earn a scenario in `test/e2e/`. Use `t.Skip()` with a
  clear reason if the scenario requires capabilities the GitHub runner
  does not have.

## Documentation

Any user-facing change updates the relevant doc in `docs/`. The PR
template has a checklist — tick the boxes.

## Getting help

- **Questions and discussions:** GitHub Discussions (enabled once the
  repository is public).
- **Bugs and feature requests:** GitHub Issues (use the templates).
- **Security reports:** see [SECURITY.md](SECURITY.md). Never open a
  public issue for a vulnerability.

Thank you for making Setec better.
