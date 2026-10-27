# `internal/secrets` — variables and encrypted typed secrets

Task T-012. Standalone project variables, envelope-encrypted typed secrets, the
declared-reference resolver and the management API, following
[variables and secrets](../../docs/specs/variables-and-secrets.md) and the
[software design](../../docs/specs/software-design.md) (ports and adapters, DDD
entities, consumer-owned ports). The encrypted local store is the **standalone
baseline**. Hosted custody of provider/integration credentials is a separate
decision (O-19, T-061) and is not decided here; a hosted adapter replaces
`VersionSource` without changing the resolver. Production key management (AWS,
GCP and Scaleway KMS, T-028) implements the same `KeyWrapper` port; the only
adapter in this module is the **development** keyfile.

Requirements: VS-01–VS-12 with acceptance VS-A01–VS-A09; supporting SD-01–SD-08,
IT-02–IT-06/IT-11 and the HTTP concurrency conventions. Named acceptance:
`context_swap`, `pinned_rotation`, `revoked_resolution`, `no_plaintext_metadata`,
`precedence`.

## Layout

| Path | Role |
| --- | --- |
| `variable.go`, `secret.go`, `scope_key.go`, `lifecycle.go`, `errors.go` | Entities and their transitions: `Variable`, `Secret`, `SecretVersion`, `ScopeKey`, `ConsumerHandling`; classified errors. No pgx, HTTP or provider types |
| `ports.go` | Consumer-owned ports: `Store`, `Reader`, `Writer`, `IDGenerator`, `KeyWrapper` (`scope_key.go`) |
| `service.go` | Management use cases; all key-provider calls and encryption happen **before** a transaction |
| `configuration.go`, `references.go` | Deterministic input precedence, pinned effective snapshot, `secret://project/<name>` references |
| `resolver.go` | Declared-reference resolution: `AttemptAuthority`, `VersionSource`, `PendingValue` ports |
| `envelope/` | The one envelope codec: HKDF-SHA-256 value keys, AES-256-GCM, bound ownership context |
| `keyfile/` | Development key-wrapping adapter (explicitly selected raw 32-byte KEK files) |
| `postgres/` | Tenant-scoped store, atomic write unit, pin registration, standalone `VersionSource`, keyed idempotent writes; the configuration tables are in the baseline schema (`internal/storage/migrations/001_schema.sql`) |
| `http/` (package `secretshttp`) | Management routes, generated DTOs (`configuration.gen.go` from [`api/configuration.yaml`](../../api/configuration.yaml)) |
| `runtime/` (package `secretsruntime`) | Translation of the runtime contract's `SecretResolveRequest/Response` and error codes; registers nothing |
| `integration/` | Delivery evidence through the merged `kodama step` entrypoint |

## Model

* **Variable**: typed (`string`, `boolean`, `integer`), nonsecret, plaintext. Name
  and type are immutable. A value change creates the next **value version**; the
  **resource version** (the ETag) also counts description edits and deletion.
  Value history is retained in `variable_versions`.
* **Secret**: immutable ID, name and `kind` (`generic`, `git`, `model`, `mcp`,
  `kubeconfig`); monotonically increasing value versions; a separate resource
  version for management concurrency; logical deletion that keeps the name
  reserved. **SecretVersion**: immutable encrypted value, one-way `revoked_at`.
* **ScopeKey**: one random 32-byte key per tenant generation, wrapped by a KEK.
  Rewrap changes the wrapping revision and wrapped bytes only; value ciphertext,
  value versions and pins are untouched. Every wrap ever made is retained with
  its immutable KEK reference. Three integers are never conflated: resource
  version, value version, wrapping revision.
* **Precedence** (`ConfigurationResolver`): pipeline input default → project
  variable of the declared name → explicit run input. Unknown run inputs, type
  mismatches, missing required values and foreign artifacts fail admission;
  unrelated project variables never become inputs. The `EffectiveSnapshot` is
  immutable, carries provenance and pinned variable versions, pins secret
  bindings to exact versions, and never contains plaintext.

## Envelope

HKDF-SHA-256 derives a per-secret-version key from the scope key; AES-256-GCM
seals the value with a fresh random nonce. The key-derivation info binds
installation, tenant, project, secret ID, immutable name and value version; the
additional authenticated data binds those plus the format version, algorithm,
scope key ID and key generation. Changing the ciphertext, the nonce or any bound
field fails authentication (`Test_context_swap`). Persisted per value: format
version, algorithm, nonce, ciphertext, scope key identity; per wrap: wrapped
bytes, wrap algorithm, KEK provider/ID/version. No plaintext or plaintext-derived
fingerprint is stored.

The specification names the platform's runtime secret-storage envelope as the
reference but fixes only its algorithms and bound fields. The byte framing in
[`envelope/codec.go`](envelope/codec.go) is this module's choice, frozen by a
deterministic vector in `codec_test.go`. The pinned upstream document (`optimiweb/platform`
revision `05dec0d81a15f9c3bcac826508f0342761343d38`, `docs/specs/16-secrets.md`) was **not
retrievable from this environment**, so no comparison was made and **no
compatibility with platform rows is claimed**; no rows are copied between the systems.

## Wiring (for the composition root, T-027)

Migrations are the squashed baseline only: the configuration tables, their
grants and the run-pin constraint are part of
`internal/storage/migrations/001_schema.sql` and `002_grants.sql`, applied
by `storage.BaseContribution()`. This module contributes none:

```go
manifest, err := storage.NewManifest(storage.BaseContribution())
```

Construction (names are the module's exports; the key provider and installation
ID come from explicit configuration, and startup fails if either is missing):

```go
provider, err := keyfile.New(keyfile.Config{Development: true, Current: "v1", Files: files}) // dev only
store, err := secretspg.NewStore(secretspg.Config{DB: runtimePool, Events: secretspg.EventsFrom(eventStore), IDs: secrets.RandomIDs{}})
svc, err := secrets.NewService(secrets.ServiceConfig{InstallationID: id, Store: store, Wrapper: provider, IDs: secrets.RandomIDs{}})
keyed, err := secretspg.NewKeyed(idempotency, store)
h, err := secretshttp.NewHandler(secretshttp.Config{Service: svc, Keyed: keyed, Cursors: cursors, HashKey: hashKey})
err = h.RegisterRoutes(router)                       // under the router's /api/v1 prefix

source, err := secretspg.NewVersionSource(store, provider, id)
resolver, err := secrets.NewResolver(attemptAuthority, source)   // attemptAuthority: T-020/T-021
adapter, err := secretsruntime.NewAdapter(resolver)               // wired to the runtime route by T-021/T-027
resolution, err := secrets.NewConfigurationResolver(store, store, artifactOwnership)  // run admission (T-020)
```

Management routes (relative to `/api/v1`, `P=/tenants/{tenantId}/projects/{projectId}`):
`GET, POST P/variables`; `GET, PATCH, DELETE P/variables/{id}`; `GET, POST P/secrets`;
`GET, DELETE P/secrets/{id}`; `GET, POST P/secrets/{id}/versions`;
`POST P/secrets/{id}/versions/{version}/revoke`. Authorization actions (injectable
through `secretshttp.Config.Actions`): `variable.list|read|create|update|delete`,
`secret.list|read|create|delete`, `secret.version.list|create|revoke`. There is no
plaintext-read route and no public rewrap route; administrative rewrap is
`Service.RewrapScopeKey`. Secret creation and new versions require `Idempotency-Key`
(and, for versions, `If-Match`); edits and deletes require `If-Match`; revocation is
idempotent by state. Module error codes: `name_conflict`, `resource_deleted`.
`HashKey` (at least 32 bytes, supplied outside the database) keys the idempotency
fingerprint of requests that carry a secret value, so the stored request hash is
not an offline-guessable fingerprint of the value (`Test_no_plaintext_metadata`).
T-027 owns its configuration name. Events: only the merged `configuration.changed` (resource type `configuration`);
revocation and rewrap are mapped to `updated` with the precise operation in the audit
metadata.

## Obligations of the execution module (T-020/T-021/T-027)

* **Pin registration**: call `Store.RegisterPins(ctx, tx, PinRequest)` in the same
  tenant transaction that creates the run, with the snapshot's secret bindings. It
  locks the secret row and the version row `FOR SHARE`, so registration is totally
  ordered against deletion and revocation (`Test_concurrent_mutations`). It
  refuses a deleted secret or revoked version (`ErrDeleted`, `ErrRevoked`) and is
  idempotent per consumer. T-020 adds the foreign key of `secret_pins.consumer_id`
  to its run table.
* **Pin release / cancellation guard**: call `Store.ReleasePins` in the transaction
  that ends or cancels a run or attempt. Resolution finalization locks the pin row,
  so a resolution either commits (and is audited) before the release or finds the
  pin released and releases nothing. No network call happens in these transactions.
* **Admission order**: resolve the configuration (`ConfigurationResolver`, which
  pins the current non-revoked version of each referenced secret), then register the
  pins in the run's transaction. A secret revoked or deleted in between makes
  registration fail, which fails admission rather than pinning a blocked version.
* **Attempt authority**: provide `secrets.AttemptAuthority`, returning the
  authenticated attempt's tenant, project, run and declared bindings, or
  `ErrAttemptInactive` / `ErrAuthorityMissing`. It is consulted twice per
  resolution (before and after decryption). With no adapter wired, resolution is
  unavailable; there is no permissive default.
* **Revocation follow-up**: revocation records pending `terminate_consumer` and
  `rotate_issued_credential` effects for every unreleased pin. Execution and the
  issuer-rotation procedure read them with `Store.PendingHandling` (restart-safe,
  paged by ID) and acknowledge them with `Writer.AcknowledgeHandling`. The module does
  not cancel runs or revoke cloud credentials, and the records never claim that
  delivered bytes were recalled.

## Assumptions for the integration owner to confirm

1. **Management payloads**: a validated `kind` plus a bounded (1–131072 bytes),
   write-only base64 `value`; provider-specific credential schemas stay with the
   consumers, and no generic connection resource exists.
2. **Envelope framing** is this module's codec (see above); T-028 consumes the
   `KeyWrapper` interface.
3. **Deletion and retention**: logical deletion and retention reporting only, no
   physical purge. Deleted names are never reused; pinned versions stay retained and
   resolvable (unless revoked) for their runs; backup retention is always reported
   `unresolved`. Production windows remain with O-11/T-029/T-056.
4. **Execution seams** (`AttemptAuthority`, pin registration/release, consumer
   termination) are injected contracts; T-020/T-021/T-027 confirm and wire them.
5. **Event vocabulary**: revocation and rewrap use `configuration.changed` with
   change `updated` (rewrap is tenant-scoped, resource ID = scope key ID, version =
   wrapping revision).
6. **Secret version creation requires `If-Match`** (the secret's ETag as last read)
   because the value version is bound into the ciphertext before the transaction
   starts; a concurrent rotation makes the loser fail with `412 version_conflict`.

## Limits stated plainly

* Plaintext lifetime is bounded (buffers are cleared after use, request bodies are
  cleared after the handler) but Go gives no complete heap erasure: the base64
  response string of a resolution and JSON-decoded strings cannot be erased.
* File exclusion (VS-11) means secrets are delivered as separate protected files
  outside the source and output roots, removed afterwards, and refused when found in
  a source change or reachable from an output path. What a command prints into its
  diagnostics, and redaction of it, is a separate control and is not claimed here.
* VS-10 has **development** restore evidence only (encrypted rows restored into a
  fresh database and decrypted with separately retained development KEK files; a
  missing key fails closed). It does not qualify a production KMS.
* The standalone store says nothing about hosted custody (O-19/T-061).

## Tests

Database tests use the shared helpers (`KODAMA_TEST_POSTGRES_ADMIN_URL`,
`KODAMA_REQUIRE_POSTGRES=1`) and isolated databases.

```sh
go generate ./internal/secrets/... && git diff --exit-code -- internal/secrets/http/configuration.gen.go
go test -race -count=1 ./internal/secrets/...
make test-task TASK=T-012
```

| Test | Package | Covers |
| --- | --- | --- |
| `Test_context_swap` | `envelope` | VS-06, VS-A04 |
| `Test_precedence` | `secrets` | VS-02, VS-A01 |
| `Test_declared_attempt_authority` | `secrets` | VS-03, VS-A02 |
| `Test_domain_transitions` | `secrets` | VS-01, VS-07, VS-08, VS-12 entity rules |
| `Test_key_provider_failure` | `keyfile` | VS-09, VS-A07 classification, no fallback |
| `Test_pinned_rotation` | `postgres` | VS-07, VS-A05 |
| `Test_revoked_resolution`, `Test_resolution_revocation_race` | `postgres` | VS-08, VS-A06, VS-03 |
| `Test_crypto_failure_atomicity`, `Test_stored_ciphertext_tampering` | `postgres` | VS-09, VS-A07, VS-06 at rest |
| `Test_concurrent_mutations` | `postgres` | uniqueness, expected versions, pin/delete and pin/revoke order |
| `Test_configuration_atomicity` | `postgres` | state, audit, event and keyed response commit together |
| `Test_deletion_retention` | `postgres` | VS-12, VS-A09 |
| `Test_development_restore` | `postgres` | VS-10 (development only), VS-A08 first half |
| `Test_tenant_isolation`, `Test_migration_reapply` | `postgres` | IT-02–IT-06/IT-11, schema, grants, guards |
| `Test_no_plaintext_metadata` | `secretshttp` | VS-04, VS-A03 |
| `Test_idempotent_configuration_write` | `secretshttp` | replay, conflict, revoked replay, unknown commit |
| `Test_configuration_contract` | `secretshttp` | OpenAPI, DTOs, ETags, cursors, error mappings |
| `Test_runtime_translation` | `secretsruntime` | runtime DTO and error-code mapping |
| `Test_secret_file_exclusion` | `integration` | VS-05, VS-11, VS-A08 second half |
