# `internal/artifacts` — artifact storage and scoped transfer

Task T-015. Durable metadata and byte transfer for the reports, logs, bundles and
image registrations an attempt produces. Implements the artifact part of the
[runtime envelopes](../../docs/implementation/step-entrypoint.md) (reserve,
upload, complete, download) and the immutable provenance that result, source and
gate consumers rely on, following the [software design](../../docs/specs/software-design.md)
(ports and adapters, DDD entities, consumer-owned ports).

Requirements: IT-03, IT-11, PX-10, DQ-01; supporting SD-01–SD-08 and the tenant
persistence invariants. Named acceptance: `foreign_object`, `digest_mismatch`,
`incomplete_upload`, `expired_url`, `orphan_cleanup` (relevant parts of IT-A04,
PX-A07, DQ-A01, SD-A01–SD-A06). Result finalization and gate decisions remain
their consumers' responsibility: this module answers "is this reference owned,
verified and usable?", nothing more.

Not here: source checkpoint manifests, caller-supplied storage keys or URLs,
production retention values (O-11), hosted identity (O-18/O-19), composition,
central migration registration, configuration names.

## Layout

| Path | Role |
| --- | --- |
| `values.go`, `artifact.go`, `registry.go`, `errors.go` | Entities and values: `Artifact` (identity, tenant/project, producer attempt, declaration, trusted provenance, lifecycle), `Reservation`, `Reference`, `RegistryRef`; classified errors. No pgx, SDK or HTTP types |
| `ports.go` | Consumer-owned ports: `AttemptAuthority`, `ReadAuthority`, `Repository`, `CleanupStore`, `ObjectStore`, `RegistryVerifier`, `Retention`, `IDs` |
| `service.go`, `reserve.go`, `complete.go`, `lookup.go` | Use cases: `Reserve`, `Complete`, `Metadata`, `References`/`Reference`, `Pin`/`Unpin` |
| `reconcile.go` | `Reconciler`: bounded orphan, abandonment, deletion-recovery and retention sweeps under durable leases |
| `postgres/` | `Store` (Repository + CleanupStore), `Migrations()` and `migrations/T015_001_artifacts.sql` |
| `s3/` | S3-compatible `ObjectStore`: scoped presigned transfer, read, seal, delete, under an owned key prefix |
| `oci/` | `Verifier`: digest-addressed registry verification with injected `Policy` and `ProvenanceResolver` |
| `http/` (package `artifactshttp`) | Runtime reserve/complete routes, public metadata route, generated DTOs (`artifacts.gen.go` from [`api/artifacts.yaml`](../../api/artifacts.yaml)) |
| `artifactstest/` | Test environment: migrated database, isolated S3 prefix, fault injection, programmable authorities. Test support only |
| [`../../test/support/s3`](../../test/support/s3/README.md) | Isolated S3 prefixes per test over the development fixture |

## Model

* **Artifact**: immutable identity, tenant/project, producing run/attempt/step,
  name (unique per attempt), kind (`file`, `oci`), role (`output`, `diagnostic`,
  `bundle`), declared digest/size/media type (or the OCI repository), and trusted
  source provenance (repository + commit). Only the lifecycle moves.
* **State** is exactly the frozen runtime enumeration: `reserved` → `usable`
  for files; an OCI registration is created `registered` only after the registry,
  ownership and provenance checks passed. A reserved file is never usable.
* **Disposition** (`active`, `abandoned`, `removed`) is cleanup bookkeeping and
  never a runtime state. `abandoned` closes a never-completed reservation;
  `removed` records a retention deletion and keeps metadata and provenance.
* **Usable** = (`usable` or `registered`) and `active`. `Reference.Usable` is
  what `runtimev1.CheckResult` receives (`artifactshttp.RuntimeRefs`); an
  incomplete artifact is present and unusable, a foreign or absent one is
  omitted.
* File bytes live under server-generated logical keys
  `artifacts/<tenant>/<project>/<artifact>/staging|sealed/<random>`. Callers never
  choose a key, URL or bucket.

## Reservation (`POST /runtime/v1/attempts/{attemptId}/artifacts`)

1. Reauthorize through `AttemptAuthority` — on **every** call, replays included.
   Scope, producer, declared outputs, limits and trusted provenance come from the
   grant, never from the request, the generic router target or a registry.
2. Output role needs a declared output of the same kind; a bundle needs a writer.
3. Existing name: identical immutable declaration → 200 and fresh instructions;
   different → `409 idempotency_conflict`; a closed (abandoned/removed)
   reservation → `409 idempotency_conflict`. New → 201.
4. Aggregate file/byte/diagnostic limits are enforced atomically under a
   per-attempt advisory lock in the same transaction as the insert.
5. Upload instructions are generated after the transaction, bounded by
   `min(UploadTTL, grant.ExpiresAt)`, and the latest issued expiry is recorded
   durably **before** the instruction exists. They are never stored or replayed.
   A completed file's replay keeps the frozen "reserved + upload" shape and never
   reopens or modifies usable content.
6. An unknown commit outcome is `503 unavailable`, never success; the retry
   observes the committed reservation.

OCI: `registry` is parsed (`host[:port]/repo`, tag discarded, no scheme or
credentials), authorized by the injected `oci.Policy`, the digest-addressed
manifest is fetched and hashed (never the tag or a digest header), and trusted
provenance is resolved by the injected `ProvenanceResolver`. Only a verified
registration is stored, as `registered` with `upload: null`. A replay does not
re-verify immutable content. Registry content is never deleted by this module.

## Completion (`POST .../artifacts/{artifactId}/complete`)

1. Reauthorize; load the artifact **for this attempt** (another attempt, project
   or tenant is `404`); compare the completion with the reservation (disagreement
   is `422 digest_mismatch`). A usable artifact returns the same immutable result.
2. `BeginCompletion` takes a fenced lease (claim token + generation) and durably
   records the candidate sealed destination **before any object is written**. A
   live lease elsewhere is `503` (`ErrInProgress`); an expired lease is taken over
   with a higher generation.
3. Outside any transaction: read the staging object into bounded scratch while
   hashing, require the declared size (`409 artifact_incomplete` otherwise) and
   digest (`422 digest_mismatch`), then write the verified bytes to the sealed
   destination (checksum-bound, `If-None-Match: *`). ETags and caller metadata are
   never evidence. Scratch files are removed on every path. Object calls are
   bounded by the lease deadline.
4. Reauthorize again; `CommitUsable` re-checks token, generation, live lease and
   the winner flag on the exact candidate in one transaction with the audit
   record. A fenced-out or expired holder commits nothing. After an unknown commit
   outcome the durable state is re-read: success only if the artifact is usable.

A still-valid staging PUT can only ever reach the staging key; usable content is
the separate sealed object, so staging overwrites cannot change verified
downloads.

## Public metadata (`GET /api/v1/tenants/{t}/projects/{p}/artifacts/{id}`)

Representation and errors are in [`api/artifacts.yaml`](../../api/artifacts.yaml).
`download=true` issues a short-lived GET instruction only for sealed, active file
content, after `ReadAuthority` (tenant/project/resource, diagnostic visibility)
allowed the artifact; an absent, foreign and hidden artifact are the same `404`.

## Cleanup, orphans and retention

Everything the module can write is recorded in `artifact_objects` before the
write (`staging`, `candidate`), with the latest instant a credential or lease
could still write it (`writable_until`). `Reconciler.Sweep` per tenant:

* abandons idle reservations whose latest transfer/lease expired more than
  `AbandonAfter` ago (disabled when zero);
* promotes **unreferenced, non-winner** objects whose `writable_until + StagingGrace`
  passed to durable deletion intent (staging of usable/closed artifacts; sealed
  candidates that are not the live claim's destination), leases a batch under a
  token, deletes (a missing object is success), then acknowledges only while the
  lease is held. Timeouts leave the work pending; a restarted or second worker
  converges; a renewed transfer reopens the staging record so a repeat sweep
  catches a late write;
* never touches the winner, objects of a live claim, unrecorded objects, or any
  key outside `artifacts/` in the adapter's prefix.

Retention is a mechanism only: with no `Retention` injected usable content is
never deleted. `Pin`/`Unpin` serialize with deletion by row locks (`FOR SHARE` vs
`FOR UPDATE`); a pinned artifact is never removed and a removed one cannot be
pinned. The database refuses to schedule a winner for deletion unless retention
already marked the artifact `removed`. Production periods are O-11 and are
injected, never defaulted here.

## Database

`T015_001_artifacts.sql`: `artifacts`, `artifact_objects`, `artifact_pins`.
Composite `(tenant_id, project_id)` ownership to `projects`; forced RLS with
owner and tenant policies; runtime grants without DELETE (except releasing a pin)
and `UPDATE` only on lifecycle columns; triggers keep identity, declaration,
provenance, seal and verification evidence immutable and the lifecycle one-way;
`UNIQUE (tenant_id, project_id, attempt_id, name)` is the replay key. The
execution tables do not exist yet, so there is no foreign key to attempts:
**T-020 adds `(tenant_id, project_id, attempt_id)` → attempts and
`(tenant_id, project_id, run_id)` → runs in its migration contribution.** Audit
rows (`artifact.reserved|registered|completed|abandoned|removed`) commit with
the transition and hold identifiers only. No artifact event type exists in the
event catalog, so none is emitted.

## Wiring (T-027 and the execution/integration owners)

```go
manifest, _ := storage.NewManifest(storage.BaseContribution(), /* ... */ artifactspg.Migrations())
store, _ := artifactspg.NewStore(artifactspg.Config{DB: runtimePool})
objects, _ := arts3.NewStore(arts3.Config{Client: s3Client, TransferClient: signerClient, Bucket: bucket, Prefix: prefix, MaxTransfer: ttlCap})
verifier, _ := oci.NewVerifier(oci.Config{Policy: registryPolicy, Provenance: provenanceResolver})
svc, _ := artifacts.NewService(artifacts.ServiceConfig{
    Repository: store, Authority: attemptAuthority, Reader: readAuthority, Objects: objects, Registry: verifier,
    UploadTTL: ..., DownloadTTL: ..., ClaimLease: ...,
})
rec, _ := artifacts.NewReconciler(artifacts.ReconcilerConfig{Store: store, Objects: objects, Retention: nil /* until O-11 */, Policy: policy})
go rec.Run(ctx, interval, logErr)

rt := artifactshttp.NewRuntime(svc, artifactshttp.DefaultActions())  // router prefix "/runtime/v1"
pub := artifactshttp.NewPublic(svc, artifactshttp.DefaultActions())  // router prefix "/api/v1"
// consumers: svc.References(scope, attempt, ids) -> artifactshttp.RuntimeRefs -> runtimev1.CheckResult
```

Injected authorities the owners must supply (missing wiring denies):

| Port | Supplied by | Contract |
| --- | --- | --- |
| `AttemptAuthority.Grant(caller, attempt)` | T-016/T-020/T-027 | Current scope, run/step, declared outputs, limits, writer flag, trusted source provenance, authority expiry; `ErrAuthorityMissing`, `ErrAttemptInactive`, `ErrNotFound` for a token that does not address the attempt |
| `ReadAuthority.AuthorizeRead(caller, scope, subject)` | T-010/T-027 | Tenant/project/resource access including diagnostic visibility; `ErrNotFound` hides |
| `oci.Policy`, `oci.ProvenanceResolver` | execution/integration | Tenant/project → registry endpoint + credential; trusted provenance of content produced by the attempt |
| `Retention` | operations (O-11) | Optional; absent means nothing usable is deleted |

## Error mapping

| Condition | Module error | Wire |
| --- | --- | --- |
| malformed request | `ErrInvalid` | 400 `invalid_request` |
| no/invalid credential | `ErrAuthorityMissing` | 401 `invalid_credential` |
| revoked/cancelled/expired attempt | `ErrAttemptInactive` | 403 `attempt_revoked` |
| missing, foreign, hidden | `ErrNotFound` | 404 `not_found` |
| changed metadata / closed reservation | `ErrIdempotencyConflict` | 409 `idempotency_conflict` |
| missing, truncated or wrong-size upload | `ErrIncomplete` | 409 `artifact_incomplete` |
| undeclared output, limit, unverifiable or foreign registration | `ErrOutputInvalid` (and subtypes) | 422 `output_invalid` |
| declared or actual digest disagreement | `ErrDigestMismatch` | 422 `digest_mismatch` |
| dependency failure, unknown outcome, lease held | `ErrUnavailable` | 503 `unavailable` + `Retry-After` |
| anything else | — | 500 `internal_error`, no detail |

## Assumptions the integration owner confirms

* Operational policy stays external: transfer lifetimes, grace periods, limits and
  retention eligibility are injected; destructive retention is off without a
  policy (O-11, T-029/T-056).
* Runtime authority is injected: the module neither verifies tokens nor edits
  execution tables; T-016/T-020/T-027 confirm `AttemptAuthority`.
* OCI authorization and provenance are adapter-local ports, verified by digest;
  registry annotations and the caller's spelling establish nothing.
* The public representation is module-owned (`api/artifacts.yaml`); download
  instructions are issued only with `download=true`.
* Existing runtime codes are reused for unspecified details: size/missing →
  `artifact_incomplete`, digest disagreement (declared or actual) →
  `digest_mismatch`, invalid or unverifiable OCI registration → `output_invalid`.
  Bundles count against a service-level bound (`BundleBytes`, `BundleFiles`), not
  the attempt's output limits.

## Provider limitations

LocalStack signs and serves presigned URLs but **skips signature validation by
default** (`S3_SKIP_SIGNATURE_VALIDATION=1`): with that default expiry, key and
method binding are not enforced by the provider. `Test_expired_url` detects this
with a canary and skips only the enforcement subtests (reporting why); its
module-owned bounds (never sign beyond the requested expiry or the transfer cap,
checksum binding, renewal) still run. Qualification of expiry and tampering was
run against LocalStack 4.14.0 started with `S3_SKIP_SIGNATURE_VALIDATION=0` and
the fixture access key `test` with secret `test` (LocalStack validates only
that pair, so a fixture whose `s3-secret-key` differs could not pass signature
validation if it were enabled). A provider with real IAM enforcement is a
production qualification item, not claimed here.

## Tests and checks

```sh
export KODAMA_TEST_POSTGRES_ADMIN_URL=postgres://postgres:<pw>@127.0.0.1:<port>/postgres?sslmode=disable
export KODAMA_S3_ENDPOINT=http://127.0.0.1:4566 KODAMA_S3_BUCKET=kodama-t015 KODAMA_S3_PREFIX=t015/
export KODAMA_SECRETS_DIR=<dir with s3-access-key and s3-secret-key>
export KODAMA_REGISTRY_URL=http://127.0.0.1:5000        # optional: real registry test
export KODAMA_REQUIRE_POSTGRES=1 KODAMA_REQUIRE_S3=1 KODAMA_REQUIRE_REGISTRY=1
go test -race -count=1 ./internal/artifacts/... ./test/support/s3/...
go generate ./internal/artifacts/http/... && git diff --exit-code -- internal/artifacts/http/artifacts.gen.go
make test-task TASK=T-015
```

| Test | Covers |
| --- | --- |
| `Test_foreign_object` (`http`) | IT-03, IT-11, DQ-01: completion, metadata, download and trusted lookup with copied foreign IDs |
| `Test_digest_mismatch` (`http`) | PX-10, DQ-01: declared and actual mismatch, misleading metadata, never usable |
| `Test_incomplete_upload` (`http`) | PX-10, PX-A07: absent/truncated/wrong-size; diagnostics stay readable |
| `Test_expired_url` (`s3`) | Provider enforcement of expiry, method/key binding, checksum, renewal |
| `Test_orphan_cleanup` | Abandoned uploads, stranded sealing objects, survivors, convergence, retention and pins |
| `TestArtifactTransitions` | Entity transitions, immutable provenance, role/kind rules, bounds |
| `TestReservationConcurrency` (`postgres`) | One identity, atomic aggregate limits |
| `TestCompletionRecovery`, `TestStagingOverwriteCannotChangeUsableArtifact` | Fencing, response loss, crash after sealing, takeover, overwrite races |
| `TestTenantPersistence`, `TestMigrationReapply` (`postgres`) | RLS, grants, pooled context, composite ownership, immutable columns, rollback |
| `TestOCIVerification` (`oci`) | Real registry, hostile responses, redirects, provenance, outages |
| `TestRuntimeContractCompatibility`, `TestPublicMetadataContract` (`http`) | Frozen runtime fixtures, OpenAPI/DTO alignment, authorization order |
| `TestModuleBoundaries`, `TestReservationLifecycle`, `TestMetadataAndReferences` | Dependency direction; service rules; scoped lookup |

PostgreSQL, S3 and registry tests skip without their fixtures
(`KODAMA_REQUIRE_*=1` turns a missing fixture into a failure). The acceptance
tests are not satisfied by skips: run them with the fixtures present.
