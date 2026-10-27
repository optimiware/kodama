# internal/pipeline

Pipeline definitions (task T-014): the one structural and semantic validator for
[pipeline documents](../../docs/contracts/pipeline-format.md), the canonical
digest, and immutable registered versions. The HTTP API and the local CLI both call
`Validator`, so a document is valid in both places or in neither.

Status: implemented and tested (validator and HTTP adapter with port fakes; the
PostgreSQL adapter against real PostgreSQL). Nothing wires these routes,
capabilities or ports yet; that is T-027's job, with the
confirmations below. No run, scheduling, CLI command, M2 execution or hosted
integration is claimed.

## Layout

| Path | Purpose |
| --- | --- |
| `parse.go`, `document.go` | Bounded YAML/JSON parsing into a structurally valid `Document` (strict, no aliases/tags/duplicate keys) |
| `validate.go`, `graph.go`, `references.go` | `Validator`: DAG, source/writer ordering, whole-scalar references, typing, images, outputs, gates, capability and reference lookups |
| `canonical.go` | Canonical bytes and `Digest` (frozen by golden fixtures) |
| `document_v2.go`, `validate_v2.go`, `references_v2.go`, `parse_v2.go`, `errors_v2.go` | The `kodama/v1alpha2` (`run_shared`) branch: types, normalization and canonical tree, shape and semantic rules, endpoint expressions, schema-error code mapping, new diagnostic and admission-refusal codes. `document.go`, `validate.go` and `canonical.go` dispatch on `apiVersion`; no `kodama/v1alpha1` branch changed behavior |
| `order.go`, `run_defaults.go` | `ExecutionOrder` (declaration-order topological order, gates included), `EffectiveAccess` (source-backed/source-free defaults) and `EffectiveArgv` (the argv matrix): pure functions for run admission and the runtime adapters |
| `definition.go`, `errors.go` | `Definition` entity, `Candidate`, failure classes, bounded diagnostics |
| `ports.go`, `service.go` | Consumer-owned ports and the `Register`/`Get`/`List` use cases |
| `postgres/` | `Store` (`pipeline.Repository`); the definition table is in the baseline schema (`internal/storage/migrations/001_schema.sql`) |
| `http/` | Routes, DTO/error mapping, generated types (`pipeline.gen.go`) |
| `pipelinetest/` | Test doubles shared by tests and composition tests (in-memory repository, table authority) |
| `../../api/pipeline.yaml` | OpenAPI 3.0.3 fragment with examples |
| `../../contracts/pipeline/` | JSON Schema, fixtures and canonicalization notes |

Dependencies point inward: `http` and `postgres` import `pipeline`; `pipeline`
imports `domain` and the schema package only. Entities and use cases contain no
pgx, net/http, NATS or provider types.

## Entry points

```go
v, err := pipeline.NewValidator(pipeline.Config{
    Capabilities: pipeline.EnabledKinds{pipeline.KindContainer}, // M1: containers only
    Profiles:     profiles,   // immutable-profile lookup (owner: T-027 / profile owner)
    References:   references, // secret/environment reference policy
    // Parse and Limits take explicit bounds; zero values select documented defaults.
})
valid, err := v.ValidateYAML(ctx, scope, actor, data) // or ValidateJSON, or ParseYAML + Validate
svc, err := pipeline.NewService(pipeline.ServiceConfig{Repository: store, Authority: authority, Validator: v})
store, err := pipelinepostgres.NewStore(db, pipelinepostgres.Options{LockTimeout: 0}) // zero = 5s
h, err := pipelinehttp.NewHandler(svc, cursorCodec); err = h.RegisterRoutes(router)
```

* `Validated` exposes `Name()`, `Digest()` and `Canonical()` (a copy). The CLI's
  planned `pipeline validate` calls `Validator`; `pipeline register` calls the API.
* Capabilities are injected, never inferred from the schema: a kind that is not
  enabled is rejected with `kind_disabled` even though the schema can represent it.
  A missing profile or reference port, or a lookup outage, fails closed.

## Ports and actions

| Port | Contract |
| --- | --- |
| `Authority` | `ResolveActor` (verified principal to attributable actor, never from the body) and `Authorize(actor, scope, permission)`. `ErrDenied`/`ErrHidden` refuse; any other error is an outage (503) |
| `Capabilities` | which step kinds the installation executes |
| `ProfileLookup` | resolves an agent profile name; reports whether it is immutable. The profile-version syntax belongs to the profile owner |
| `ReferencePolicy` | may the actor reference this secret/environment; never retrieves values |
| `Repository` | atomic `RegisterOrGet`, scoped `Get`, `List` ordered by `(created_at, id)` |

Authorization actions (also the router action names): `pipeline_definition.list`,
`pipeline_definition.register`, `pipeline_definition.read`. The composition root
supplies policy; there is no allow-all default. Authorization runs first, so
deduplication is never a disclosure to an unauthorized caller.

## Model and state transition

There is one creation transition: validated content to a registered immutable
`Definition`. Changed content under a name allocates the next version of that
name; canonically identical content in the same project returns the existing
record and writes nothing. There is no update, delete or editable "latest".
The runtime role has only `SELECT, INSERT` on the table.

## Canonicalization (assumption to confirm)

Deterministic UTF-8 JSON, sorted object keys, array order preserved, plain
decimal integers, documented defaults applied (absent `needs`/`args`/`env`/
`secretFiles`/`outputs`/`inputs` empty, `input.required` false, `workspace`
`{access: none}`, `retry` one attempt), strings otherwise untouched. The digest is
`sha256:` + SHA-256 of those bytes and includes `metadata.name`; it excludes IDs,
ownership, timestamps and the allocated version. See
`contracts/pipeline/README.md` and `contracts/pipeline/v1/fixtures/canonical/`.

## Persistence and recovery

* Table `pipeline_definitions`: tenant/project composite ownership, unique
  `(tenant, project, name, version)` and `(tenant, project, digest)`, composite
  keys `(tenant, project, id)` and `(tenant, project, id, digest)` for future run
  foreign keys, `ENABLE`+`FORCE` RLS, body stored as text (the bytes that were
  hashed).
* Registration runs in one `storage.WithTenant` transaction: lock the project
  row (`FOR NO KEY UPDATE`, bounded by `LockTimeout`), recheck the digest,
  allocate the version, insert the definition and one audit row
  (`pipeline_definition.registered`, resource type `pipeline_definition`; actor
  type `service` is stored as the audit table's `system`). A duplicate writes no
  version and no audit row; an audit failure or cancellation rolls the definition
  back. No network call happens inside the transaction.
* Failure classes: validation (400 syntax / 422 semantic), denied/hidden, conflict
  (a digest match with different content is never returned as success),
  unavailable. An uncertain commit is `ErrUnavailable` wrapping
  `ErrOutcomeUnknown`, never success and never retried internally. Because
  registration is content-addressed, the caller's retry converges on one
  definition.
* Registration emits **no event**: neither the event catalog nor
  `contracts/events/v1` defines one, and this module does not misuse another
  aggregate's event. The audit row is the record. Add an event only through the
  event-catalog owner.

## HTTP (relative to `/api/v1`)

| Route | Action | Result |
| --- | --- | --- |
| `GET /tenants/{t}/projects/{p}/pipeline-definitions` | list | page ordered by `(created_at, id)`, optional `name` filter, scope-bound cursors |
| `POST /tenants/{t}/projects/{p}/pipeline-definitions` | register | JSON document in, `201` new version / `200` identical existing; `ETag` = version, `Location` |
| `GET /tenants/{t}/projects/{p}/pipeline-definitions/{id}` | read | the immutable definition, canonical content and digest |

No `If-Match` or `Idempotency-Key` is required. No update, delete or
validation-only route exists. Errors: syntax `400 invalid_request`, semantic
`422 invalid_definition` (both with `details.diagnostics`: stable code, document
path, constant message, never document content), conflict `409 definition_conflict`,
outage or unknown outcome `503 unavailable`.

## Tests

```sh
go test -race -count=1 ./internal/pipeline/... ./contracts/pipeline/...
export KODAMA_TEST_POSTGRES_ADMIN_URL='postgres://postgres:<password>@127.0.0.1:<port>/postgres?sslmode=disable'
export KODAMA_REQUIRE_POSTGRES=1   # skipped PostgreSQL tests become failures
make test-task TASK=T-014
go generate ./internal/pipeline/... && git diff --exit-code -- internal/pipeline/http/pipeline.gen.go
```

| Test | Covers |
| --- | --- |
| `Test_cycle` | PX-01: self/multi-node cycles; valid diamond |
| `Test_unknown_output` (`references_test.go`) | PX-01/PX-02: undeclared/nonancestor outputs, implicit `commit`, reference types |
| `Test_parallel_writers` (`graph_test.go`) | PX-02/PX-14: incomparable (including conditional) writers, source selection |
| `Test_conditional_gate` | AC-12: `when` and executable-only gate fields rejected; gates unavailable under M1 |
| `Test_mutable_image` | AC-12: tag-only and malformed digests rejected |
| `Test_duplicate_definition` (postgres) | PX-14: identical YAML/JSON converge, one audit, new content a new version |
| `Test_parser_strictness`, `Test_canonical_digest`, `Test_definition_rules`, `Test_contract_cases` | strict parsing, goldens, rule table, shared fixtures |
| `Test_capability_and_reference_policy`, `Test_service_*` | fail-closed capability, profile, secret and authority outages |
| `Test_concurrent_registration`, `Test_registration_recovery`, `Test_pipeline_tenant_isolation`, `Test_pipeline_migrations` (postgres) | concurrency, rollback/recovery, RLS and grants, ordering |
| `Test_pipeline_http_contract` (http) | 201/200, ETag/Location, validator parity, denial after revocation, cursors, no mutation routes |
| `Test_pipeline_contract_fixtures` (`contracts/pipeline/v1`) | schema alternatives and fixtures |

## Assumptions for the integration owner to confirm

1. **Canonicalization** as above is frozen by golden fixtures; confirm before consumers adopt the digest.
2. **Limits, capabilities, profile and reference adapters** are injected. The module invents no setting names or profile-version syntax. T-027 wires `EnabledKinds{KindContainer}` for M1 and the real profile/secret-policy adapters.
3. **No registration event** (see above).
4. **Parser bounds** (`256 KiB`, depth 16, 20000 nodes) are library options, not installation settings.
5. **Scope of validation**: definition-side only. Run-input resolution, source-commit ownership, secret-version pinning, output finalization, runtime filesystem/symlink containment of `/kodama/outputs` and gate applicability are runtime responsibilities of their owning tasks.
6. **Wiring**: T-027 has no migration to register for this module (the definition table is part of the
   baseline schema, `internal/storage/migrations/001_schema.sql`, applied by `storage.BaseContribution()`);
   it builds the cursor codec and provides `Authority`, routes and the router `Authorizer` policy for the three
   actions.

## kodama/v1alpha2 (run_shared), delivered by T-110, not enabled

The format is specified by [`pipeline-format-v1alpha2.md`](../../docs/contracts/pipeline-format-v1alpha2.md)
and described in [`contracts/pipeline`](../../contracts/pipeline/README.md). Facts the next
tasks rely on:

* **Opt-in, off by default.** `Config.EnableV1Alpha2` (zero value `false`) makes a
  `v1alpha2` document fail `Validate` with `format_disabled` (semantic stage, no lookup, nothing
  persisted by `Service.Register`). `Validator.FormatEnabled(apiVersion)` reports the choice.
  Enabling registration is not enabling execution: run admission refuses the format
  (the T-118 guard in `internal/execution`) until T-112/T-116 deliver complete resource
  capability checks.
* **Reading is independent of the flag.** `ParseYAML`/`ParseJSON`, `NewValidatedFromCanonical`
  and `Validated.Document` always read a `v1alpha2` document, so stored definitions stay
  readable on an installation that never enabled or later disables registration.
* **Stages.** Schema and shape findings (ranges, ports, probes, storage, edges, egress names,
  `workspace_from_removed`, `invalid_*`, `duplicate_port`, probe/edge `unknown_port` and
  `unknown_service`) are syntax-stage. Expression, edge-to-endpoint, secret and `needs`-graph
  findings are semantic. The same code is reported at the same path whichever path found it:
  `Validate` re-runs the shape checks for a `Document` built in code.
* **Canonical form** fills contract-constant defaults only. Absent step `workspace`,
  `spec.runWorkspace`, service `command`, `environment` and `when` stay absent; `args` absent
  and `[]` are one value; `services: {}` and an absent `services` are two spellings kept distinct.
* **Typed documents.** `Spec` gained `TimeoutSeconds`, `RunWorkspace`, `Services`, `Network`; `Step`
  gained `Egress`. The service type is `RunService` (the use case keeps the name `Service`). A
  `kodama/v1alpha1` document that sets any of them is refused, never silently reduced.
* **Pure helpers.** `ExecutionOrder`, `EffectiveAccess` and `EffectiveArgv` implement the contract
  tables; the `Admission*` constants spell the admission refusal codes execution returns.
* **Execution codec.** `execution.EncodeStep` persists only the `v1alpha1` step fields. A run must
  not be admitted from a `v1alpha2` definition until T-112 extends the codec and the guard together.
* **Constant v2 messages.** A `v1alpha2` validation replaces the messages of the shared
  duplicate-step, cycle, parallel-writer, unknown-input/step/output, not-ancestor and
  type-mismatch findings with constant text; the `v1alpha1` messages are unchanged. A typed
  `Scalar` whose type is not string, boolean or integer, and storage paths that overlap on a
  component boundary anywhere in the list, are refused.
