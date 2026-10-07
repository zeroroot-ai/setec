# ADR index

This file lists each architecture decision record (ADR) of the `zeroroot-ai`
organization by number. A code comment cites a record as `ADR-NNNN`. Read the
rule of that number here.

The full records are private. This file is the public part: one line for each
decision.

- A number in the first table is live. A citation of it is correct.
- A number in the second table is retired. Cite the number in its last column.
- A number in neither table matches no record. A citation of it is an error.

The shared workflow `adr-citations.yml` of `zeroroot-ai/.github` checks each
citation in a repo against this file. `gibson` holds the one source copy. Each
other repo keeps a copy at `docs/adr-index.md`.

To update this file after a change of an ADR in `zeroroot-ai/docs`:

1. In `gibson`, run `python3 scripts/gen-adr-index.py <path to docs/adr/README.md>`.
2. Make sure that no rule line reveals a security finding that is still open.
3. Merge the change in `gibson`. Then copy the file into each other repo.

Do not edit the tables by hand.

## Live

| ADR | Rule |
|---|---|
| ADR-0001 | Logout ends the dashboard session and the Zitadel session |
| ADR-0002 | Operators dial the daemon directly over SPIFFE mTLS |
| ADR-0003 | One code path. Fail at boot. Never skip silently. |
| ADR-0004 | One MissionConstraints type, in the SDK proto |
| ADR-0009 | JWT or SPIFFE on every service call. No Kubernetes auth to OpenBao. |
| ADR-0014 | One writer for each Secret |
| ADR-0018 | CodeQL is the only data-flow analysis tool in CI, with one shared workflow and one query set |
| ADR-0023 | The Gibson daemon does not use the Kubernetes API |
| ADR-0024 | OpenBao is the secret store in every environment |
| ADR-0027 | A change lands as one cutover that deletes the old path |
| ADR-0028 | The public API contract has five finished rules |
| ADR-0031 | A pod reads an operator-minted value at startup |
| ADR-0032 | An OpenBao runtime token is periodic and renewable, and the root token is revoked |
| ADR-0033 | The saga runner calls each step, and each step is safe to run again |
| ADR-0034 | The daemon owns LLM provider records and credentials |
| ADR-0035 | CUE is the only mission authoring format |
| ADR-0038 | Eino is the one LLM framework in gibson |
| ADR-0041 | One grant model: FGA tuples, one read RPC, writes through the daemon |
| ADR-0045 | One runtime identity for every component: the Capability Grant JWT |
| ADR-0046 | The FGA model treats agent, tool and plugin principals the same |
| ADR-0052 | setec is the sandbox for mission code |
| ADR-0056 | gibson is the one Go repo for the platform backend and the first-party plugins |
| ADR-0058 | The SDK is the component-development surface only |
| ADR-0059 | The tenant brings the embedding provider |
| ADR-0060 | Billing is one private component with three connection points |
| ADR-0061 | Connector auth is a platform capability (was 0064 until 2026-10-05) |
| ADR-0063 | Mission origination requires a parent mission |
| ADR-0064 | One light brand across the marketing site and the console |
| ADR-0065 | Connector is a component kind, and agents find tools through two meta-tools |
| ADR-0066 | A first-party plugin enrolls with a SPIFFE SVID |
| ADR-0067 | A connector is authorized as a component kind |
| ADR-0068 | One ADR series, with a public index and private records |
| ADR-0070 | A teardown is verified against the AWS account, not Terraform state |
| ADR-0071 | The deployable unit is one umbrella chart, installed after its CRD releases |
| ADR-0072 | A promotion is a chart-version change in one environment directory |
| ADR-0073 | All environments share one workload account |
| ADR-0074 | Self-hosted and SaaS differ by what is deployed, through fail-safe seams |
| ADR-0075 | OpenBao keeps file storage, and Velero backs up its volume |
| ADR-0076 | Two SPIRE admission webhooks are the one accepted fail-open exception |
| ADR-0077 | Public web surfaces are off-cluster, on S3 and CloudFront |
| ADR-0079 | One edge on every substrate |
| ADR-0080 | The release loop |
| ADR-0083 | The substrate contract |
| ADR-0084 | One registry credential path, any registry |
| ADR-0086 | The chart lives in `charts`, the estate lives in `hosted` |
| ADR-0087 | The chart is a guest on the cluster |
| ADR-0088 | Every repo gets every security feature that applies to it |
| ADR-0089 | Elastic License 2.0 everywhere, except the permissive floor |
| ADR-0090 | The chart profiles are a named ladder |
| ADR-0092 | A service connects by cluster DNS and claims the public host |
| ADR-0093 | Zitadel owns users and tenant roles, and a person has one tenant |
| ADR-0094 | Each declaration has a gate that fails when nothing consumes it |
| ADR-0095 | Staging is one permanent k3s instance, and drill is the throwaway |
| ADR-0096 | A Target names no credential, and the job declares the scope |
| ADR-0097 | A component declares itself at check-in, with no manifest file |
| ADR-0101 | The mission brain is an Entity-Component-System |
| ADR-0102 | Entity identity is scope-relative |
| ADR-0104 | The brain runs as a fixed clock-tick loop |
| ADR-0106 | Closed-loop learning from two label sources, through a scheduled trainer |
| ADR-0107 | The knowledge graph is a projection of the World |
| ADR-0108 | Missions run autonomously, bounded up front |
| ADR-0109 | Dispatch is a Timeline side-effect |
| ADR-0110 | Code that the platform starts for a mission always runs in a setec sandbox |
| ADR-0111 | A mission node dispatches to an external component of any kind |
| ADR-0112 | The graph projector is the only writer of the knowledge graph |
| ADR-0113 | Audit evidence is durable in Postgres, and a reader maps it to controls at read time |
| ADR-0114 | A connector runs on ToolHive, behind a ConnectorInstance |
| ADR-0116 | A code-executing agent runs in its own setec sandbox for each mission run |
| ADR-0117 | A first-party tool is a catalog manifest that code generation makes from the tool image |
| ADR-0118 | A first-party mission is a checked-in CUE definition, not a catalog component |
| ADR-0119 | A bank is a pool of long-lived agent sandboxes that one owner holds |
| ADR-0120 | The knowledge graph is the single shared memory |
| ADR-0121 | Agents emit hypotheses, not only facts |
| ADR-0122 | Betting is a self-calibrating prediction market |
| ADR-0123 | A bet settles on evidence or a human verdict, never an LLM |
| ADR-0124 | Taxonomy and ontology are discovered through a safety gate |
| ADR-0126 | A value-of-information planner ranks the moves, and the Decider picks from the top k |
| ADR-0129 | Belief is a relational model over the graph, and bets and reputation are views of it |
| ADR-0131 | A bet settles TRUE only when a pack CEL predicate fires on evidence the agent submits |
| ADR-0132 | A person approves a destructive proof before the agent does the act |
| ADR-0133 | Domain Packs have two tiers: catalog packs and tenant extensions |
| ADR-0134 | Belief inference runs in the daemon as Go code, and Python is the test reference only |
| ADR-0135 | One technique vocabulary lives in the taxonomy, with categories above techniques |
| ADR-0136 | One platform catalog lists first-party components, and each tenant enables its own |
| ADR-0137 | The schema declares which variable an edge feeds, and the trainer learns each strength |
| ADR-0141 | The setec sandbox substrate is x86 with KVM |
| ADR-0142 | A Gibson cluster dials one named setec fleet over mTLS, and the fleet enrolls each cluster |
| ADR-0143 | Until the cutover, setec uses stock runtimes and a node installer |
| ADR-0144 | Warm start is a pool of base snapshots that setec builds |
| ADR-0145 | A restored sandbox must pass five isolation invariants |
| ADR-0146 | A Sandbox is ephemeral or a session, and a session survives a suspend and a node loss |
| ADR-0147 | Storage is a signed image disk, a workspace volume and a snapshot store |
| ADR-0148 | Exec goes through the setec API and the guest agent, and reports a typed exit |
| ADR-0154 | A TypeScript host reaches Gibson through `sdk-ts`: ConnectRPC and the Capability Grant |
| ADR-0155 | Zerocool is a set of host plugins, not a fork of a coding agent |
| ADR-0156 | Zerocool serves `kind=agent` dispatched work from a driver process |
| ADR-0157 | An interactive coding-agent session is a live mission |
| ADR-0158 | The Gibson MCP server is the one tool surface, and a host is an adapter |
| ADR-0161 | HarnessCallbackService carries the knowledge-graph reads |
| ADR-0162 | Each method group on Harness is an embedded interface |
| ADR-0163 | The Timeline is the full history of a tenant World |
| ADR-0164 | One SPIFFE trust domain for each install |
| ADR-0165 | A secure pod is a rule that CI checks |
| ADR-0166 | The setec runtime is a launcher pod |
| ADR-0167 | A mission node reaches only its targets |
| ADR-0168 | CI is the gate, and no review and no signed commit is required |
| ADR-0169 | A fork starts a node from the state of an earlier node |
| ADR-0170 | A rewind starts a new run from a checkpoint |

## Retired

| ADR | What it decided | Cite this number |
|---|---|---|
| ADR-0005 | Shared Argo resources live in one `platform-primitives` Application | None. No ADR holds this rule. |
| ADR-0006 | A root Argo Application excludes raw manifests | None. No ADR holds this rule. |
| ADR-0007 | An agent merges its own PR and never polls | None. No ADR holds this rule. |
| ADR-0008 | Register the Zitadel Service name as a trusted domain | ADR-0092 |
| ADR-0010 | The kind cluster runs every component | None. No ADR holds this rule. |
| ADR-0011 | A saga step skips only on the tenant spec | ADR-0003 |
| ADR-0012 | Nobody writes to a datastore by hand | None. No ADR holds this rule. |
| ADR-0013 | Fix the cause, never disable a gate | None. No ADR holds this rule. |
| ADR-0015 | No `go.work`, no `replace`, no submodules | None. No ADR holds this rule. |
| ADR-0016 | Bringup passes three gates in order | ADR-0083, ADR-0087 |
| ADR-0017 | The SDK holds no infrastructure clients and never imports gibson | ADR-0058 |
| ADR-0019 | All repos stay on v0 until a deliberate 1.0 | None. No ADR holds this rule. |
| ADR-0020 | The nil-guard walker flags only receiver-field checks | None. No ADR holds this rule. |
| ADR-0021 | A 60 percent coverage floor for each package | None. No ADR holds this rule. |
| ADR-0022 | Argo ServerSideDiff needs a controller flag and an app option | None. No ADR holds this rule. |
| ADR-0025 | An OSS SDK and a private `platform-sdk` | ADR-0058 |
| ADR-0026 | All Go services build clients through one private library | None. No ADR holds this rule. |
| ADR-0029 | Five named CI checks prove reproducible builds | None. No ADR holds this rule. |
| ADR-0030 | A package stays in the SDK when customer code needs it | ADR-0058 |
| ADR-0036 | A Capability Grant is always required | ADR-0045 |
| ADR-0037 | Customer RPCs on two SDK services, operator RPCs on one | ADR-0058 |
| ADR-0039 | Tenant admin services live in the OSS SDK | ADR-0058 |
| ADR-0040 | Tenant administration is split into focused services | ADR-0058 |
| ADR-0042 | Connect by cluster name, claim the public origin | ADR-0092 |
| ADR-0043 | Every principal is an FGA tuple | ADR-0093, ADR-0045 |
| ADR-0044 | The dashboard can write only the Tenant CR | ADR-0058 |
| ADR-0047 | A connector is an MCP bridge plugin | ADR-0065 |
| ADR-0048 | A connector is a generic bridge plugin on the SDK | ADR-0065 |
| ADR-0049 | The MCP bridge is a plugin runtime mode | ADR-0065 |
| ADR-0050 | Three license tiers | ADR-0089 |
| ADR-0051 | Rejected: one gibson instance for each tenant | None. No ADR holds this rule. |
| ADR-0053 | Dissolve `platform-sdk` and `platform-clients` into gibson | ADR-0056 |
| ADR-0054 | A license for each tier, no CLA, no `ee/` | ADR-0089 |
| ADR-0055 | Rejected: an AGPL core with an `ee/` split | None. No ADR holds this rule. |
| ADR-0057 | Rejected: an `ee/` registration seam | None. No ADR holds this rule. |
| ADR-0062 | Never used | None. No ADR holds this rule. |
| ADR-0069 | Staging and prod are destroyed to zero cost | ADR-0083, ADR-0095 |
| ADR-0078 | A plain Kubernetes cluster is the self-hosted target | ADR-0090, ADR-0083 |
| ADR-0081 | gVisor runs on the EKS workload nodes | ADR-0166, ADR-0083 |
| ADR-0082 | A recreate restores only a named backup | ADR-0083 |
| ADR-0085 | The chart is Apache-2.0, the images are not | ADR-0089 |
| ADR-0091 | Staging is one EC2 instance with k3s | ADR-0095 |
