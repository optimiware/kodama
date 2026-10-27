# internal/httpapi

Shared HTTP foundation (task T-004) on `net/http`: no chi, no ORM. It implements
the conventions in [the HTTP contract](../../docs/contracts/http-api.md) once so
product modules do not duplicate them. It contains **no product routes**, no role
policy and no identity provider; authentication, authorization and throttling are
small consumer-owned ports injected by the composition root (T-027).

Status: implemented and tested against real PostgreSQL. Nothing registers these
routes yet; that is T-027's job.

## Layout

| Path | Purpose |
| --- | --- |
| `router.go`, `auth.go`, `rate_limit.go`, `request_id.go` | `NewRouter`, `Route`, `Call`, the three ports, request IDs |
| `errors.go`, `json.go` | `APIError`, `WriteError`, bounded `ReadBody`/`DecodeJSON`, `WriteJSON` |
| `etag.go` | `FormatETag`, `RequireIfMatch`, `CheckVersion` |
| `cursor.go`, `pagination.go` | `NewCursorCodec`, `ParsePage`, `BuildPage`, `ListPage` |
| `idempotency.go` | `ClientKey`, canonical route, `HashRequest`, `StoredResponse`, `Outcome`, `Call.Identity` |
| `common.gen.go`, `generate.go`, `oapi-codegen.yaml` | types generated from `api/common.yaml` |
| `postgres/` | `Idempotency` (keyed transaction); the idempotency table is in the baseline schema (`internal/storage/migrations/001_schema.sql`) |

Dependencies point inward: `postgres` imports `httpapi`, `storage` and `domain`;
`httpapi` imports only `domain` and the standard library.

## Request pipeline

```text
request -> request ID + settings -> mux match (404/405 as envelopes)
        -> Authenticate  (401 invalid_credential, outage 503)
        -> validate {tenantId}/{projectId}/resource identifiers (400)
        -> RateLimiter.Allow, keyed by principal+tenant+project+action (429 Retry-After, outage 503)
        -> Authorizer.Authorize (403 forbidden, hidden 404, outage 503)
        -> handler
```

* Routes are registered only through `Router.Handle(Route{...})`, which rejects a
  missing `Action`, handler or inconsistent wildcard metadata and never panics on
  pattern conflicts. `HandlePublic` is the single, explicit way to serve an
  unauthenticated route (GET/HEAD, no wildcards). The zero `Router` answers 503.
* `NewRouter` fails without an `Authenticator`, `Authorizer` and `RateLimiter`.
  `httpapi.Unlimited()` is a deliberate opt-out, not a default.
* Authorization scope follows the path: no wildcard is principal-level,
  `{tenantId}` tenant-level, `{projectId}` (which requires `{tenantId}`)
  project-level. `Route.ResourceKind`/`ResourceParam` add a resource reference.
  `Target` carries tenant, optional project and resource; it is distinct from
  `domain.Scope`, whose validation requires a project.
* The limiter runs after authentication and before authorization, so a flood of
  denied requests is throttled per verified principal and an unauthenticated
  caller cannot spend any budget. Limiter and authorizer failures fail closed.
* Unknown errors, driver errors and panics become a fixed `500 internal_error`.
  Logs (injected `*slog.Logger`) get the request ID, status, code and the error's
  type chain plus SQLSTATE; never bodies, headers, queries or error text.
* Every response sets `X-Request-ID`, `Cache-Control: no-store` and
  `X-Content-Type-Options: nosniff`.

## Wire conventions (adapter-local choices, frozen by tests and `api/common.yaml`)

* **Request IDs** are server-generated (`req_` + 32 hex). A client-sent
  `X-Request-ID` is ignored.
* **Errors:** `{"error":{"code","message","requestId","details"}}`; `details` is
  always an object. Status map: 400 `invalid_request`/`invalid_cursor`, 401
  `invalid_credential`, 403 `forbidden`, 404 `not_found`, 409
  `idempotency_conflict`, 412 `version_conflict`, 428 `version_required`, 429
  `rate_limited`, 500 `internal_error`, 503 `unavailable`. Oversized bodies are
  413 and a non-JSON content type 415, both with code `invalid_request`; an
  unsupported method is 405 `invalid_request` with `Allow`. Modules add their own
  codes through `NewError(status, code, message)`.
* **JSON:** body limit (`RouterConfig.MaxRequestBytes`, default 1 MiB), a JSON
  media type, exactly one document, unknown fields rejected. Responses are
  serialized and size-checked (`MaxResponseBytes`, default 4 MiB) before anything
  is written.
* **ETag / If-Match:** the ETag is the quoted integer version (`"7"`). `If-Match`
  must be exactly one strong tag. Missing is 428; weak (`W/"7"`), `*`, lists,
  repeated headers, leading zeros, signs and out-of-range values are 400. A stale
  version is 412. Version comparison must be part of the write itself.
* **Lists:** `{items, nextCursor}`; `nextCursor` is JSON `null` at exhaustion;
  default limit 50; limits outside 1..200 (or repeated/non-numeric) are 400, never
  clamped. Unknown query parameters are rejected by `CheckQuery`.
* **Cursors:** `base64url(0x01 || position) "." base64url(HMAC-SHA256)`, with the
  MAC over a framed `CursorScope` (tenant, project, endpoint pattern, the
  concrete value of every path wildcard such as `{runId}`, normalized filters,
  sort) and the payload. The scope is not in the token, so a copied token
  fails under any other scope. Any failure is 400 `invalid_cursor` without a
  reason. The key is injected (`NewCursorCodec(signingKey, previousKeys...)`, at
  least 32 bytes each; previous keys allow rotation). A cursor proves provenance
  and scope only; every page passes the router's authorization again. Modules must
  use a deterministic sort ending in a unique tie-breaker, read `FetchLimit()`
  rows (limit + 1), and give `BuildPage` a position function returning that
  ordering key for the last returned row.
* **Idempotency key:** `Idempotency-Key`, one header, `[A-Za-z0-9._~:-]{1,128}`.

## Idempotency

Unique key: `(tenant_id, principal, method, canonical_route, key)`. The request
hash is a stored value compared on a match; it is **not** in the key (a unit test
inspects the primary key and indexes). Same hash replays the stored response with
`Idempotent-Replayed: true`; a different hash is `409 idempotency_conflict`.

* **Canonical route:** the registered pattern (including prefix) with each
  wildcard replaced by its validated concrete value, excluding the query string.
  Two projects or resources never share a route. `project_id` is stored for
  ownership (composite FK to `projects`) but is not a key dimension.
* **Request hash:** SHA-256 over a framed query (names sorted, values in order),
  every `If-Match` value and the exact body bytes. Byte-different but
  semantically equal JSON conflicts. Including `If-Match` means a retry after
  success must repeat the original header.
* **Lifecycle:** absent -> transaction-scoped claim -> completed -> (expired ->
  deleted under the same claim by the request reusing the key). The claim is
  `pg_advisory_xact_lock` over a hash of the key tuple, bounded by a
  transaction-local `lock_timeout` (default 10 s, restored before the callback
  runs); a waiter that times out gets `503 unavailable` with `Retry-After`. A row
  exists only once the response is complete, so an unfinished claim is never
  committed and rollback returns the key to absent.
* **Atomicity:** `Execute` runs `Request.Do` and inserts the record in one
  `storage.WithTenant` transaction. Every business, history, audit and outbox row
  required by the mutation must be written by `Do` through the given `tx`
  (T-004 creates no product schema). `Do` must not commit, retain `tx` or make
  network calls. Error, panic or cancellation rolls everything back, including
  the record. An unstorable response (non-2xx, oversized, non-JSON, unsafe
  headers) rolls back and is 500.
* **Success only after commit:** `Execute` returns after commit; the handler
  writes the HTTP success from its `Result` (`WriteStored`). If the commit result
  is unknown (error after the callback and insert succeeded) the result is
  `503 unavailable` wrapping `ErrOutcomeUnknown`, never success, and the callback
  is never rerun; retrying the same key replays if the commit happened and
  executes if not. A lost HTTP response is recovered the same way.
* **Replay authorization:** `Request.AuthorizeReplay` is required. It is called
  after the claim transaction has ended, so no SQL transaction spans an
  authorization call, and also after waiting on a concurrent request. Pass
  `Call.AuthorizeReference`, which re-asks the `Authorizer` with the route action
  about the original route target (including the addressed resource of a
  resource-scoped route), and additionally about the stored target `Reference`
  (the resource named by `Outcome.Target`) when it is nonzero and differs. A
  revoked principal gets 403/404 and never sees the cached body; a matching replay
  does not run `Do` and does not compare resource versions. Header syntax
  validation (for example `RequireIfMatch`) stays in the handler before `Execute`.
  A changed-body conflict returns no stored content.
* **Stored bodies:** a stored response body is empty or exactly one complete JSON
  object or array; anything else (truncated, trailing bytes, a scalar) rolls the
  whole transaction back with a 500, so a malformed success is never committed or
  replayed.
* **Retention:** `Config.Retention` defaults to the **proposed** 24 hours. This is
  a development default, not the production retention decision (O-11). Expired
  rows are replaced on reuse; `PurgeExpired(ctx, tenant, limit)` bounds storage
  and is safe against concurrent reuse.
* **Table:** tenant FK, `(tenant_id, project_id)` FK, `ENABLE`+`FORCE` RLS with a
  tenant policy, owner policy for administration, runtime grants `SELECT, INSERT,
  DELETE` only (completed records are immutable). Body <= 1 MiB (adapter default
  256 KiB), allowlisted headers (`ETag`, `Location`, `Content-Location`).

## Integrating a module (T-027 and module tasks)

1. Build one `Router` with the injected ports and register each route with an
   `Action` (and a resource reference where relevant).
2. For edits: `v, err := httpapi.RequireIfMatch(r)`, pass `v` to the use case and
   compare it in the write (`UPDATE ... WHERE id=$1 AND version=$2`; zero rows then
   classified with `CheckVersion` or not-found). Return the new ETag.
3. For idempotent creates/decisions: `body := ReadBody`, `id := call.Identity(r,
   body)`, then `idem.Execute(ctx, postgres.Request{Identity: id, AuthorizeReplay:
   call.AuthorizeReference, Do: ...})` and `WriteStored`. Return a
   `httpapi.Outcome` whose `Target` names the created resource.
4. For lists: `CheckQuery`, `codec.ParsePage(r, call.CursorScope(filters, sort))`,
   query `FetchLimit()` rows in a unique order, `BuildPage`.
5. Nothing to register for migrations: the idempotency table is part of the
   baseline schema (`internal/storage/migrations/001_schema.sql`) that
   `storage.BaseContribution()` applies. Inject cursor keys, retention, size
   bounds and the logger from the configuration; T-027 owns the setting names.
6. Reference `api/common.yaml` components from the module's OpenAPI fragment;
   T-027 assembles and publishes the combined document.

## Generated types

`api/common.yaml` is OpenAPI 3.0.3 (the pinned generator reads 3.0; the runtime
fragment is 3.1). Types only, no router or embedded spec:

```sh
go generate ./internal/httpapi/...
git diff --exit-code -- internal/httpapi/common.gen.go   # drift check
```

`generate.go` runs `go run github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@v2.4.1`.
The tool is resolved in its own module graph; it and its dependencies are not in
`go.mod`/`go.sum`, and the generated file imports only the standard library.
Generated names (`Error`, `ErrorEnvelope`, `Page`, `Limit`, ...) are reserved in
this package, so hand-written helpers use different names (`ListPage`,
`ClientKey`, `BadRequestError`, ...).

## Tests and checks

```sh
go test -race -count=1 ./internal/httpapi/...               # PostgreSQL tests skip without a URL
export KODAMA_TEST_POSTGRES_ADMIN_URL='postgres://postgres:<password>@127.0.0.1:<port>/postgres?sslmode=disable'
export KODAMA_REQUIRE_POSTGRES=1                            # skipped DB tests become failures
go test -race -count=1 ./internal/httpapi/...
make test-task TASK=T-004
```

| Test | Covers |
| --- | --- |
| `Test_idempotency_changed_body` (postgres) | AC-02, AC-A02: hash compared, not keyed; one effect |
| `Test_stale_edit` (postgres) | AC-03, AC-A02: atomic version check, 412/428 |
| `Test_cursor_scope` | AC-04, AC-A03, IT-A04: copied/tampered cursors, reauthorization |
| `Test_replay_after_revocation` (postgres) | IT-03, IT-A04: replay authorization, no disclosure |
| `Test_idempotency_concurrent`, `_expiry`, `_rollback_and_retry`, `_lost_response` (postgres) | exactly-once, expiry races, rollback, unknown commit |
| `Test_idempotency_tenant_isolation` (postgres) | SD-04, IT-02/IT-04/IT-05: RLS, grants, pooled context, composite FK, reapply |
| `Test_transport_denials`, `Test_etag_validation`, `Test_pagination_and_throttling`, `Test_json_bounds`, `Test_common_contract` | AC-03/AC-11, deny by default, schema/example/generated-type alignment |

Representative create/edit/list handlers and their fixture tables live only in
`*_test.go`; they are not product routes.

## Requirement mapping

AC-02 (idempotency, stored hash), AC-03 (expected versions, stable codes, request
IDs), AC-04 (scope/filter/sort-bound cursors, reauthorization), AC-11 (scoped
limiter interface, `Retry-After`), IT-03 (authorization on every path, including
cursors and replays), IT-A04 reference disclosure, SD-01/SD-03/SD-04 (ports in the
consumer, pgx only in the adapter, tenant/RLS/ownership preserved).

## Not implemented here

SSE retention/resynchronization (AC-06), product routes and OpenAPI, role policy,
identity provider, hosted authority mapping (O-18/O-19), the production retention
decision (O-11), CLI. No NATS, S3 or Git service is used.
