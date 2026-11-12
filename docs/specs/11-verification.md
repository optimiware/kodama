# S11 — Verification strategy and acceptance trace

**Unit-test every functional package before integrating any real component.** Unit completion and system qualification are distinct evidence states.

## 1. Test levels and gates

| Level | Allowed proof | First phase |
|---|---|---|
| U0 | Format/vet, compile, imports, ownership, contract hashes, static fixture and Markdown/JSON/config checks | F01 onward |
| U1 | Pure rules, deterministic fakes, temp filesystem, in-process transport messages, fuzz seed/property cases | Kernel/package work |
| U2 | Race detector and deterministic concurrent port/transaction schedules | Kernel/package work |
| R1 | Real PostgreSQL/NATS/Process/Podman/Git/OIDC/registry/KMS component contracts | **After G2** |
| R2 | Composed Shell/Kernel/public clients, cross-boundary races and outages | **After G2** |
| A1 | End-to-end product ACs and optional-capability independence | After G3 |
| O1 | Production isolation, provider qualification, resilience, restore/retention objectives | After G3 |

Before G2, tests do not launch processes/containers or contact external services. Fake `RoundTripper`, SDK interfaces, SQL seam and request recorders are permitted; socket-backed stacks and real databases are not. Testing pure helper packages within a capability is still a unit test. A Kernel assembled entirely through deterministic injected fakes is the G1 unit subject.

Before F01 creates the Go module, this repository ships no product code and no development harness. Documentation/configuration changes are verified with static Markdown link, JSON parse, task-DAG and configuration checks; a transient ignored checker may be used as evidence but is not committed source. F01 introduces the normal Go module and the `go` baseline; it does not rebuild the former harness.

Test commands are introduced by F01; planned baseline:

```sh
go vet ./...
go test ./...
go test -race ./internal/kernel/...
```

Run targeted package commands per card while building. G1/G2 run full applicable U0/U1 and affected U2 on the exact assembled source revision. Integration tests live in explicit later-phase packages/build tags so ordinary unit commands remain hermetic. No skipped future acceptance test is counted as evidence.

## 2. Required fixtures

- Fake clock with explicit advance; no time-based sleeps.
- Versioned atomic fake state with conflict/rollback injection, snapshots that survive a new controller instance.
- Runtime fake with stable resources, never-started proof, unknown/absent/running/exited evidence, lost response before/after effect, stop barriers and workspace loss.
- Principal/authority/key/registry/broker/HTTP/command fakes driven by frozen contracts.
- Immutable input fixtures for token encoding, approval matching, operation envelopes and definition parser behavior.
- Barrier-controlled concurrency schedules, seeds and minimal reproduction for fuzz failures.

Fakes implement only their contract. They must not return success for every effect or import real implementations to conceal missing boundaries. Builders can propose stronger cases; weakening a frozen expected invariant requires contract review.

## 3. Acceptance map

Every AC requires both its local cases and the later real proof.

| AC | Unit cases / owning packages | Delayed system proof |
|---|---|---|
| 1 Kernel independence | K-INDEPENDENT, K-ORDER; Kernel/script/runtime adapters | I02, I04 ordinary images/logs, no runner/Shell dependency in Kernel |
| 2 Immutable admission | A-PARSE/SNAPSHOT/INPUT; P04/P20 | I04 API-only/inline runs and edit immunity |
| 3 Idempotency | S-START, E-RECEIPT/PUSH; P05–P08/P12/P13 | I02, I04, I06 duplicate interfaces/events/config-edit replay |
| 4 Request settlement | K-REQUEST/DEADLINE, S-REQUEST; P03/P05 | I02, I04 real assignment/version/deadline races and restart |
| 5 Protected integration | Exact approval matrix, Git rules/journal; P03/P23 | I03, I05 forbidden evidence, rules, atomic ref updates |
| 6 Recovery | K-RECOVER/STOP/WORKSPACE; runtime adapters | I02, I04, Q02 real uncertainty, restart, missing workspace |
| 7 Identity | MAC/OIDC/current-authority/origin/broker cases; P02/P03/P18/P19/P22 | I02, I03, Q02 live invalidation and tenant isolation |
| 8 Delivery evidence | Output retries/recipe fixtures; K03/P27 | I06, Q01 digest publication/test/promotion without rebuild |
| 9 Cleanup | K-CLEANUP; P14–P16/P28 | I02, Q02/Q03 attributable pending cleanup and recovery |
| 10 Optional capabilities | Public-only imports/disabled-capability configs; P10/P11/P22–P28 | I06, Q01 plain/Git-free/gateway-free, external/native agents |
| 11 Materialization | A-ENV/runtime/secret snapshots; P09/P14–P17 | I02, I04, Q02 real files/permissions/renewal/isolation |
| 12 Outputs | K-OUTPUT, output operation races; K03/P05/P13 | I02, I04 HTTP versus settlement and success-only visibility |
| 13 Ordered flow | K-ORDER/DEADLINE, A-PARSE/REFERENCE; K01/K02/P04 | I04 unsupported syntax/preparation errors, persisted deadlines |
| 14 Integrations | E-INGRESS/PUSH/BROKER, service fixtures; P07/P08/P18/P26 | I03, I06 scopes, delayed reports, trigger exclusions/queue |
| 15 Broker independence | Fake-outage domain scenarios; K05/P07/P08/P18 | I04, I06, Q02 broker down during progression/reply/deadline/trigger |
| 16 Planned waits | K-REQUEST/WORKSPACE, S-REQUEST/INBOX | I04, I05 no workload/credential, same workspace/execution |
| 17 Handoffs | S-HANDOFF, checkpoint restoration; P06/P24/P25 | I04, I06 atomic reply/queue, stop ordering, missing refs |
| 18 Task progress | S-PROGRESS/EDIT; P06/P12/P13 | I04 old-event immunity and operator retry history |
| 19 Authorization/verification | Operation matrix, exact matching, secrecy; P03/P21 | I03, I05, Q02 local/remote parity and fresh revocation |
| 20 Userland Git | Import boundary, rules, journal/retention; P23 | I03, I05, Q03 real journal replay/retention/restore (there is no Userland HTTP commit-retention discovery; Shell-managed `refs/kodama/keep/<sha>` enforces retention) |

## 4. Evidence and gate validity

Record task ID, source/base SHA, contract revisions, environment/toolchain, exact commands/exit codes, relevant test names, findings, and deferred real proofs. No wall-clock date or narrative “tests pass” substitutes for a revision.

Contract or implementation changes invalidate affected evidence. G2 is evaluated on one source revision containing all functional packages; package-only green branches are insufficient. The gate runs unit/static checks, not system assembly. A missing operation, placeholder implementation, unsupported stub on a required path, skipped invariant, or unreviewed contract change blocks G2.

After G2, a discovered package defect goes back to its owning card, gets a failing unit reproducer where feasible, and refreshes affected unit and integration evidence. Integration cannot create an unplanned subsystem under the label “glue.”

## 5. Efficiency

During builds, run targeted U0/U1/U2. Run the full available unit/static baseline and required race checks on each candidate source before landing on `master`; gates additionally validate aggregate evidence and their required suites. Repeat only for changed code/contracts/base, failures or unresolved concerns. Share reviewed conformance vectors, not mutable test state. Shard independent package/scenario families; use isolated resource namespaces for real tests later.

Cache toolchains and dependencies. Never trade a missing negative/race/recovery case for a coverage percentage. Deterministic unit checks target seconds per package; measure actual time rather than weakening correctness to meet an arbitrary budget.
