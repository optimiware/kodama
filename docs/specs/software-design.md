# Software design

**Scope:** M1 onward, within one Go module and one server process. Vertical capability packages use lightweight ports/adapters and domain-driven design (DDD) entities at meaningful boundaries. Preserve working packages and refine concrete seams; shared foundation libraries in [P-16](../decisions.md#implementation-baseline) are confirmed. Module-specific ports remain proposed until their owning task validates them.

## Model and responsibilities

- **Domain:** module-owned entities such as work items, workspaces, runs, and gates have stable identity and behavior that protects their invariants and valid state transitions. Use value objects for validated values and aggregate boundaries where consistency rules justify them. Persistence rows and HTTP/runtime DTOs are adapter representations, not the definition of business behavior.
- **Application:** use cases coordinate entities, authorization, tenant scope, transaction boundaries, and side effects through narrow ports. Cross-entity rules such as WIP capacity still require transactional concurrency controls.
- **Ports:** small Go interfaces describe the operations a use case needs from persistence or external systems. Define them beside the consuming use case, with domain-oriented inputs, results, and explicit failure semantics. Entity-specific repository ports are appropriate when needed; a universal entity repository or service framework is not the goal.
- **Adapters:** HTTP/CLI/event handlers invoke use cases; PostgreSQL, Kubernetes, Git, NATS, S3, model, and MCP adapters implement outbound ports. Translate driver/provider types and errors at these boundaries.
- **Composition:** `internal/app` constructs concrete adapters and injects them into use cases. Dependencies point inward toward application/domain behavior. Domain entities stay independent of transport, database drivers, and provider SDKs.

Keep entities and ports within the owning module's paths. `internal/domain` remains limited to shared IDs, scope, versions, and errors. Use the [package layout](../implementation/repository-layout.md) rather than introducing global entity, repository, or adapter registries.

A capability is a cohesive package, not a separately versioned Go module or a mandatory stack of layers. Introduce an interface only for an actual consumer boundary or testable external effect. Prefer direct values/functions and existing concrete adapters where sufficient. Do not add a dependency-injection framework, universal repository, generic unit of work, or interface for every type. `internal/app` explicitly selects constructors, authority profiles, transaction adapters, routes, migrations, and worker lifetimes; it contains no competing business policy.

HTTP uses `net/http`. Add chi only after the standard-library mux is shown to be insufficient. Do not add GORM or another generic ORM. Logs use `log/slog`. Product YAML uses `go.yaml.in/yaml/v3`. OpenAPI generation uses `oapi-codegen`. These match platform and are not a license to import the platform module.

Hosted adapters preserve the [platform ownership boundary](platform-integration.md). A local domain entity or SQL projection cannot become an independent authority for core membership, entitlements, or credentials. Follow [design conformance](../implementation/design-conformance.md) to pin the relevant design sources and record intentional changes before dependent implementation proceeds.

## Requirements

| ID | Requirement |
| --- | --- |
| SD-01 | Modules SHOULD use hexagonal ports/adapters with inward dependencies and composition-root injection. |
| SD-02 | Stateful business concepts SHOULD be modeled as DDD entities with identity, invariants, and explicit transitions; value objects and aggregates SHOULD reflect actual domain rules. |
| SD-03 | Ports SHOULD be small, consumer-owned, and domain-oriented; infrastructure types and behavior SHOULD remain in adapters. |
| SD-04 | Persistence adapters MUST preserve tenant predicates, PostgreSQL RLS, ownership constraints, and transaction/outbox atomicity required by the owning specifications. |
| SD-05 | Reviews SHOULD check entity behavior and dependency direction; tests SHOULD cover domain rules independently and adapter semantics against real dependencies where required. |
| SD-06 | Implementation changes MUST identify their owning requirements, decisions, immutable design references, affected ports, and verification evidence. |
| SD-07 | A deliberate design change MUST update its authoritative contract and affected fixtures/examples/tasks together; material unresolved deviations MUST block dependent work and approval. |
| SD-08 | Conformance evidence MUST identify the candidate and baseline it assessed; a changed candidate or baseline MUST receive fresh applicable evidence before progression. |
| SD-09 | Capability packages MUST remain within one Go module, with explicit composition and small consumer-owned seams; additional layers/interfaces SHOULD be justified by a concrete boundary. |
| SD-10 | Each cross-capability seam MUST identify its authority owner, transaction boundary, failure/replay semantics, wiring owner, and contract fixtures before dependent assembly. |
| SD-11 | Component validation MUST prove domain/use-case policy with infrastructure-free Go unit tests. Real adapter semantics MUST have named checkpoint owners; assembly/checkpoint validation MUST verify actual transaction/authorization/provider behavior before declaring the milestone accepted. Unit doubles do not certify SQL/RLS or production operation. |
| SD-12 | `internal/app` MUST select every outbound adapter from one required environment (`development`, `test` or `production`); domain and use-case packages MUST NOT read it. `production` MUST refuse development and test adapters, and an unset or unknown environment MUST stop startup. |
| SD-13 | A port with more than one adapter MUST have one exported contract suite that every adapter runs. An in-memory adapter MUST NOT merge without it. |
| SD-14 | In-memory adapters MUST preserve tenant scoping, uniqueness and conflict outcomes, version checks and all-or-nothing updates, and MUST share one transaction across cross-module joins. They MUST NOT be offered as evidence for SQL, RLS, locking or lock order. |
| SD-15 | A new port SHOULD be small enough to describe in one sentence, SHOULD have no more than about six methods and MUST NOT carry one backend's vocabulary; a consumer needing more splits it into several ports. Existing wider ports are preserved and split only by their owning task. |

## Authorization, transactions, and recovery

Application use cases establish authorized tenant/project scope before invoking persistence ports. PostgreSQL adapters use the validated `WithTenant` transaction boundary and non-owner runtime role required by [identity and RBAC](identity-tenancy-and-rbac.md). Domain validation complements database constraints, RLS, and locking; it does not replace them.

Port contracts preserve the [transaction boundaries](../implementation/database-schema.md#transaction-boundaries): state/history/audit/outbox commit together where required, and external network calls happen outside SQL transactions. Adapters expose conflicts, retries, and unknown outcomes so use cases can apply the owning module's recovery rules. An in-memory entity transition alone does not establish a durable outcome.

**Run resources.** Under the `run_shared` model ([run-resources contract](../contracts/run-resources.md)) execution owns a small companion port beside `execution.Runtime` for run scratch, preparation, services and the run network boundary (`Ensure`, `Observe`, `Logs`, `Stop`, `Remove`, `Capabilities`; at most six methods per SD-15). It keeps provider types and vocabulary in adapters, is wired explicitly by `internal/app` from the selected backend, and does not widen the six-method attempt port. Resource intent, UID/fingerprint binding, readiness/stop/removal evidence, reservations and deadlines persist with tenant predicates/RLS and audit/outbox in the same transactions as the run state that justifies them; runtime calls stay outside SQL transactions; a resource record is not a new public aggregate or business status. Capability growth uses a named set separate from the `uint8` attempt `Guarantees`; the entity/value-object boundaries are T-112's to freeze and validate in review.

Execution's `Env`/`Store.Update` and PostgreSQL peer bindings join work admission, workspace reservations, secret pins and events. Results adds session-guarded execution/workspace decorators. An independent callback transaction can escape rollback/reverse lock order; undecorated results can miss current grant checks. Cancellation revokes grants within its transaction; gate consumption checks current subject/authority. The [seam table](../implementation/repository-layout.md#package-seam-contracts) records these obligations. T-062 supplies execution/results bindings, T-072 unit-tests gate wiring and T-073 qualifies actual SQL behavior before M2 acceptance. Merging unit-tested code does not certify its adapters or the milestone.

## Acceptance

| ID | Scenario | Requirements |
| --- | --- | --- |
| SD-A01 | Module review verifies that transport/provider dependencies stay in adapters and concrete dependencies are injected at composition; any deviation has a recorded rationale | SD-01, SD-03, SD-05 |
| SD-A02 | Domain tests exercise accepted and rejected transitions and value validation without PostgreSQL, Kubernetes, or an HTTP server | SD-02, SD-05 |
| SD-A03 | PostgreSQL integration checks verify tenant isolation, pooled context, concurrent invariants, and rollback of the required state/history/audit/outbox changes through the persistence port | SD-04, SD-05 |
| SD-A04 | Adapter integration checks preserve the owning specification's conflict, duplicate, and unknown-outcome behavior through the port | SD-03, SD-05 |
| SD-A05 | Review handoff identifies the requirement/design baseline and port owners; an undocumented competing core policy or unresolved contract conflict blocks approval | SD-06, SD-07 |
| SD-A06 | A changed candidate/design baseline cannot reuse the old conformance report as current evidence; an accepted contract change is accompanied by updated examples/checks | SD-07, SD-08 |
| SD-A07 | Package review preserves the single module and demonstrates a concrete consumer for each added port; composition explicitly constructs enabled capabilities and fails closed for missing bindings | SD-01, SD-03, SD-09 |
| SD-A08 | A seam handoff records authority/transaction owners, lock order, conflict/unknown-outcome replay, constructors and fixtures; a missing join becomes an ordered bounded prerequisite before activation | SD-04, SD-10 |
| SD-A09 | Component unit checks prove policy through injected interfaces; explicit integration checkpoints prove real RLS/atomicity, rollback, revocation races and durable replay through selected adapters before milestone acceptance | SD-04, SD-05, SD-11 |
| SD-A10 | An unset or unknown `KODAMA_ENV` refuses to start. Under `production`, each of the in-memory store, Process runtime, development KEK and OIDC fixture is refused at construction. | SD-12 |
| SD-A11 | The memory and PostgreSQL adapters of each ported module pass the same `Contract`; the unit gate runs the memory side and the `test` environment runs both. | SD-13 |
| SD-A12 | An in-memory `Update` that fails after writing several tables leaves none of them changed, a peer joined through the same transaction rolls back with its caller, and no `Tx` reads another tenant's rows. | SD-14 |

Module tasks apply these checks where relevant; integration tasks verify the assembled boundaries using the [test layers](../implementation/development-and-testing.md#test-layers).

## Environments

Ports are the same in every environment; only the adapters differ. `development` composes in-memory adapters over one transaction kernel, `test` composes real services on Podman and `production` composes Kubernetes ([ADR 0009](../adr/0009-environments.md), [environments](../implementation/environments.md)). In-memory adapters prove entity and use-case policy quickly and are held to their PostgreSQL twin by one contract suite; they never replace the checkpoints SD-11 names. Keep each port small: `execution.Runtime` has six methods and no notion of Jobs, containers or processes.

## Decisions

The preference is recorded in [C-16](../decisions.md#confirmed-direction) and [ADR 0006](../adr/0006-hexagonal-domain-design.md). Exact entity/aggregate boundaries and port signatures are owned by the module task and validated in review; no new provider decision is needed to adopt this preference.
