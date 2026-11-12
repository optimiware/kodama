# Detailed specifications

These specs refine [KODAMA.md](../../KODAMA.md) into implementation contracts. **MUST** identifies a required invariant; **SHOULD** identifies an implementation default that needs a recorded reason to change. Named types and methods describe required semantics; the contract tasks freeze the exact compiling Go declarations before parallel implementation.

| Spec | Scope | Main acceptance criteria |
|---|---|---|
| [S01 — Architecture and contracts](01-architecture.md) | Package catalog, imports, ports, error model | 1, 10, 19, 20 |
| [S02 — Kernel](02-kernel.md) | Ordered lifecycle, outputs, reconciliation, stop barriers | 1, 4, 6, 9, 12, 13, 16 |
| [S03 — Definitions and admission](03-admission.md) | Strict YAML, references, immutable snapshots | 2, 11, 13, 14 |
| [S04 — Persistence and transactions](04-persistence.md) | Relational ownership, RLS, atomic commit units, receipts | 3, 4, 7, 12, 17, 18 |
| [S05 — Identity and authorization](05-identity-authorization.md) | Tokens, roles, verification, evidence matching | 5, 7, 19 |
| [S06 — Shell lifecycles](06-shell-lifecycles.md) | Executions, requests, tasks, inbox, configuration | 4, 16, 17, 18 |
| [S07 — Runtimes, workspaces, and secrets](07-runtime-secrets.md) | Materialization, adapters, keys, logs | 1, 6, 7, 9, 11 |
| [S08 — Operations and events](08-operations-events.md) | Shared router, REST/MCP, stream, NATS, push | 3, 14, 15, 19 |
| [S09 — Userland and optional capabilities](09-userland.md) | Git, client/CLI, agents, recipes, gateways | 5, 8, 10, 14, 20 |
| [S10 — Production and operations](10-production.md) | Environments, isolation, restore, retention | 6, 7, 9, 15, 20 |
| [S11 — Verification matrix](11-verification.md) | Unit obligations, delayed integration, AC trace | 1–20 |

## Contract bundles

| Bundle | Freeze task | Contents | Consumers |
|---|---|---|---|
| C-KERNEL | F02 | Types, state/version rules, reducer facts/intents, runtime/state ports, observation vocabulary | K01–K05 |
| C-DOMAIN | C01 | Shell operation schemas, current-authority contracts, admission inputs, commit-unit ports, errors, event/receipt records | P01–P08, P10–P11, P19 |
| C-ADAPTER | C02 | Store conformance, key/envelope contracts, workload preparation, SDK seams, retention/log ports, installation types | P09, P12–P20, P28 |
| C-PUBLIC | C03 | REST/MCP mapping, client-facing DTOs, verification/report envelopes, Git and agent fixtures | P10–P11, P21–P28 |

Bundles are versioned by a source commit and a digest of their contract files, not by a handwritten claim of compatibility. F02 freezes only the Kernel boundary. C01–C03 happen after the Kernel gate so Shell assumptions do not drive Kernel design.

## Decisions to pin before builders fan out

The product does not specify all encoding and API details. Resolve these in the indicated task; record the choice and test vectors, rather than allowing independent agents to invent incompatible answers.

| Decision | Owner | Required outcome |
|---|---|---|
| Go toolchain, module import path, CI platform | F01 | One supported pinned toolchain and module; reproducible commands |
| Entity ID format; slug/key grammar; state/version representation | F02 | Types and validation, collision/traversal cases, no identity encoded in names |
| Runtime observations and durable effect records | F02 | Explicit unknown vs never-started vs confirmed-stop evidence |
| YAML numeric/string/literal policy; parser limits; installation timeout default | C01 | Reject ambiguous coercion; bound input; enforce 1 minute–30 days |
| Role/operation matrix and public route inventory | C01 | Explicit grants per principal kind, known operation registry, default refusal |
| HMAC canonical encoding and envelope associated data | C01/C02 | Exact byte layout and golden vectors; no inferred concatenation |
| Optional/required output-field semantics | C01 | Container outputs remain optional; request response requirements explicit in the served schema |
| JSON metadata/event size limits and pagination/cursor encoding | C01/C03 | Documented bounded limits, stable ordering, no secret-bearing cursors |
| SQL migration IDs, relational constraints, isolation/lock strategy | C02 | One schema owner; shared atomicity/concurrency cases |
| Supported Podman/Kubernetes/NATS/OIDC/registry/KMS versions | C02 | Pinned client dependencies and fixture API versions |
| OpenAI-compatible subset, MCP version, native-agent provider/tool interface | C03 | Explicit supported subset and deterministic protocol fixtures |

These decisions may narrow syntax to a documented v2 contract but may not add pipeline expressions, DAGs, artifact entities, command-over-NATS, or another execution engine. A product ambiguity that cannot be resolved without changing `KODAMA.md` is a blocker for the affected bundle.
