<!-- SPDX-License-Identifier: Apache-2.0 -->
# Launch tweet thread

Six tweets. First is the headline. Middle four walk the story. Last is the ask.

## 1. Headline

> We just shipped Setec v0.1.0: a Kubernetes operator that runs workloads inside Firecracker microVMs via Kata Containers. Apache 2.0, cloud-agnostic, self-hostable. `kubectl apply -f sandbox.yaml` and you have hardware isolation.
>
> https://github.com/zeroroot-ai/setec

## 2. The problem

> Agents writing code, CI running untrusted branches, fuzzers eating CPU. All three need isolation, not permission lists. Containers share a kernel. Setec gives each workload a microVM, addressed through a single CRD.

## 3. Cold starts

> The node-agent keeps a pool of paused Firecracker VMs ready. When a Sandbox matches one, we restore the paused state instead of booting. Observed sub-100ms P50 cold-start on a prepared bare-metal host for pool-claimed sandboxes.

## 4. Three examples

> Three reference programs ship with it, each under 200 lines:
>
> - `ai-code-exec` runs LLM-generated Python.
> - `ci-sandbox` runs `npm test` on an untrusted branch.
> - `sec-research` runs AFL++ with `network.mode: none`.
>
> Pick the shape that matches your use case.

## 5. What it isn't

> Setec does not ship its own runtime. It binds to Kata + Firecracker installed by `kata-deploy`. The API is `v1alpha1` and will change. The pre-warm pool is node-local. We'd rather ship honest limits than hide them.

## 6. Ask

> Try it, open issues, send PRs. Non-trivial changes go through `.spec-workflow/` for visible design. Governance is in `GOVERNANCE.md`; security disclosure is in `SECURITY.md`.
>
> Repo: https://github.com/zeroroot-ai/setec
> Getting started: https://github.com/zeroroot-ai/setec/blob/main/docs/getting-started.md

## Notes for the maintainer

- Tweet 1 runs close to the character limit; trim repo-URL suffix if needed.
- If benchmark numbers from the smoke test differ materially from "sub-100ms P50", update tweet 3.
- Post the thread immediately after the HN submission so sharing between the two feels coherent.
