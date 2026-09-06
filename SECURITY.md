# Security policy

## Reporting a vulnerability

**Do not open a public issue.**

Report privately through GitHub Security Advisories:
[Report a vulnerability](https://github.com/zeroroot-ai/setec/security/advisories/new)

## What to expect

| | |
|---|---|
| Acknowledgement | within 3 working days |
| Initial assessment | within 10 working days |
| Fix or mitigation plan | communicated with the assessment |

If you have not heard back within 3 working days, assume the report did not
reach us and escalate through any other channel you have. Silence is a failure
on our side, not a decision.

## Scope

This repository is the sandbox operator

 Kata/Firecracker microVMs used as an untrusted-code execution boundary.:**A sandbox escape is the highest severity we recognise.** Any path from inside a guest to the host, to the node, or to another sandbox is critical regardless of how difficult it is to reach.

## Out of scope

- Findings in a deployment you control that come from your own configuration
- Anything requiring a privileged position we already assume hostile
- Automated scanner output with no demonstrated impact. A CVE in a dependency we do not reach is not a finding; show the path

## Safe harbour

We will not pursue or support legal action against anyone who reports in good
faith under this policy, stays within scope, and does not access, modify or
retain data belonging to anyone else.
