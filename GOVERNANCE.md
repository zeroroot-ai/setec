<!-- SPDX-License-Identifier: Apache-2.0 -->
# Governance

Setec is an open-source project. This document explains how decisions are made, how roles are earned, and how to escalate when something goes wrong. It is deliberately light for the v0.1.0 era: one lead maintainer, a clear path to multiple maintainers, and enough structure that a new contributor knows where they stand.

## Roles

### Contributor

Anyone who engages with the project: filing an issue, opening a pull request, reviewing code in a comment, improving docs. Contributors do not need permission to start. Following [CONTRIBUTING.md](./CONTRIBUTING.md) is the only expectation.

### Reviewer

A contributor who has demonstrated sustained, high-quality engagement and who has been granted review rights on the repository. Reviewers:

- Review pull requests in areas where they have context.
- Approve trivial and narrowly-scoped changes under lazy consensus.
- Help triage issues.

Reviewers do not have merge rights. A maintainer must perform the actual merge.

### Maintainer

A reviewer who additionally carries merge rights, release authority, and responsibility for the project's direction. Maintainers:

- Merge pull requests that satisfy the review criteria below.
- Cut releases and tag versions.
- Maintain CI, build, and security infrastructure.
- Respond to security reports (see [SECURITY.md](./SECURITY.md)).
- Mentor contributors and reviewers.

The current list of maintainers lives in [MAINTAINERS](./MAINTAINERS).

## Decision-Making

### Lazy consensus (default)

Most changes to code, docs, and chart templates use lazy consensus. A pull request may be merged once:

1. A maintainer has approved it.
2. CI is green.
3. No other maintainer has requested changes or raised an objection.

If another maintainer raises a concern, the pull request is held until the concern is resolved or withdrawn.

### Explicit consensus

Certain decisions require explicit consensus from all active maintainers before they proceed:

- Backwards-incompatible changes to the CRD API surface.
- Adoption or removal of a core dependency (runtime, build system, package manager).
- Changes to this `GOVERNANCE.md`, to [CONTRIBUTING.md](./CONTRIBUTING.md), or to [CODE_OF_CONDUCT.md](./CODE_OF_CONDUCT.md).
- Adding, removing, or retiring a maintainer.
- Re-licensing or changes to copyright policy.

Explicit consensus means each maintainer has posted an approving comment (or equivalent) on the proposal. Silence does not count.

### Tie-breaking

While the project has a single lead maintainer (the Benevolent Dictator-for-Now model), the lead maintainer resolves deadlocks after at least 14 days of public discussion. The intent of the BDF-Now period is to move fast during the pre-1.0 phase without bike-shedding, not to concentrate authority long-term. Once at least three maintainers are active, tie-breaking transitions to simple majority vote and this section will be replaced accordingly.

## Becoming a Reviewer or Maintainer

There is no application form. The expected path is:

1. Sustained contributions over at least three months, covering code, tests, docs, or review feedback.
2. Nomination by an existing maintainer in a public thread (issue or Discussion).
3. Agreement from at least one additional maintainer (or from the lead maintainer if only one exists).
4. The nominee accepts.

Maintainership is earned, not granted by employer or affiliation. An employer cannot assign a seat. Conversely, leaving an employer does not cost a seat.

## Stepping Down and Emeritus Status

Maintainers may step down at any time by opening a pull request against [MAINTAINERS](./MAINTAINERS). A maintainer who is inactive for six months may be moved to emeritus status by a public proposal from another maintainer, resolved by explicit consensus. Emeritus maintainers are credited in release notes and may return to active status through the normal nomination path.

## Escalation

If you disagree with a decision or experience a conflict:

1. Raise it directly with the maintainer or reviewer involved, on the pull request or issue.
2. If that does not resolve it, open a GitHub Discussion (or issue if Discussions is not enabled) and tag the maintainers group.
3. If the concern involves conduct covered by [CODE_OF_CONDUCT.md](./CODE_OF_CONDUCT.md), contact `conduct@setec.zeroroot.ai` instead. Conduct concerns bypass this escalation chain.

We will make a good-faith effort to respond in public unless privacy is warranted.

## Changes to Governance

Proposed changes to this document require a pull request and follow the explicit-consensus rules above. Minor editorial fixes (typos, link updates) can be handled under lazy consensus.
