# internal/observation

Authorized durable observation streams (task T-024): the project and run event routes that let clients observe work and runs without raw NATS access. They serve server-sent events or a JSON snapshot-plus-cursor from retained PostgreSQL observations, under current authorization, with scoped opaque cursors, bounded buffering and explicit resynchronization.

Requirements: AC-04, AC-06, IT-03, IT-11 (supporting AC-12, IT-02/04/05, SD-01–08). Contracts: [HTTP API](../../docs/contracts/http-api.md#runtime-and-observation), [event catalog](../../docs/contracts/event-catalog.md), [`api/observation.yaml`](../../api/observation.yaml). Decisions C-01, C-03, C-06, C-11, C-16, C-17 and P-01, P-02, P-04, P-05, P-16 are respected; O-11 (production retention) and O-18 (hosted authority selection) stay open and are not selected here. Baseline commit `e2276a26eef576cabf3120099f0fe8003359d0f3`.

**PostgreSQL observations are authoritative.** The event store's retained `event_observations` rows and their gapless per-project sequence are the public continuation. NATS delivery and broker sequence are never read, so a broker outage, a slow client or a restart cannot change what a cursor means.

## Layout

| Package | Contents | Depends on |
| --- | --- | --- |
| `internal/observation` | `Scope` (tenant, project, optional run, normalized type filter), the projection allowlist, the `Session` entity (positions that only move forward, terminal closure), `Service` (`Open`, `Snapshot`), consumer-owned ports (`Log`, `State`, `Access`, `Sink`) | `contracts/events/v1`, `internal/domain`. **No pgx, net/http, NATS or identity.** |
| `internal/observation/postgres` | `Store` implementing `Log` and `State`: bounded durable reads and atomic snapshot-plus-watermark over the event store; the work-state binding; the required run-state seam | pgx, `internal/storage`, `internal/events` |
| `internal/observation/authorization` | `Adapter`: `Access` over `identity.Authority` (family → `work_item:read` / `run:read`) | `internal/identity` |
| `internal/observation/http` | Routes, SSE and JSON handlers, cursor wiring, error mapping, the authorization-rule contribution, generated DTOs | `internal/httpapi`, `internal/identity/http` (rule type) |
| `internal/observation/observationtest` | Test fixtures: migrated database, test-only run tables, event builders, SSE client, in-memory ports | test only |

## Projection

Only these event types are forwarded, as an explicit allowlist in `scope.go`; anything else is never sent, whatever was retained:

| Family | Event types | Permission | Run-stream matching |
| --- | --- | --- | --- |
| work item | `work_item.created`, `.updated`, `.transitioned` | `work_item:read` | excluded |
| run | `run.created`, `run.state_changed` | `run:read` | the run's own `resource.id` |
| step attempt | `step_attempt.state_changed` | `run:read` | the typed `runId` payload member (never a correlation or causation ID) |

Tenant-wide events (empty scope key) never appear in a project stream. Snapshots carry the same families as narrow current-state entries (`type`, `id`, `version`, `state`, plus `kind` or `runId`/`stepId`), not the owners' full resources.

## Behavior

- **Open.** Validate the scope; ask current access for every family; verify the project (and run) exists in the tenant and project; validate a resume position against the retained floor (`410 cursor_expired`) and the committed high-water mark (`400 invalid_cursor`); without a resume position, acquire the atomic snapshot. All of this happens before the response is committed.
- **Snapshot.** Project/run existence, every resource binding and the event watermark are read in one read-only `REPEATABLE READ` transaction, so the cursor is exactly the position the state reflects. A state above the configured bound fails (`503 snapshot_too_large`) and never yields a cursor; a failing or missing binding is `503`/a configuration error, never an empty success. Access is checked before the read and again before release.
- **Backlog → live.** Bounded pages are read from the durable log (each page one short transaction; no connection is held while writing to a client), then the same log is polled. There is no separate subscription, so there is no handoff race. A page and its watermark come from one snapshot; an empty page below the high-water mark is treated as an inconsistent log and fails closed.
- **Authorization.** Access is rechecked for each forwarded event immediately before it is handed to the sink, and periodically while idle. Hidden or denied-for-all ends the stream; an outage ends it and is never access; a denied family while another stays observable only excludes that family's events. The standalone authority is never a fallback for a selected hosted profile (that is `identity.SelectAuthority`'s guarantee, exercised in `authorization_test.go`).
- **Positions.** `Session` tracks *scanned* (read and decided), *emitted* (handed to the client and flushed) and the start, all monotonic. Pages whose events were all excluded produce a `checkpoint` frame so a quiet filtered stream resumes near the head. Resume is `Last-Event-ID`; event frame ids are cursors.
- **Slow clients.** Frames collect in a bounded buffer written under a per-write deadline; a client that does not accept data within it is disconnected (`ErrSlowClient`), never served with dropped events, and resumes from its last cursor. Memory per stream is bounded by the page size/byte bound plus the buffer bound.
- **Retention loss midstream.** Ends with a final `resync_required` frame (no `id`, so `Last-Event-ID` does not advance) when writable.
- **Delivery.** At-least-once at the network boundary: a reconnect may repeat an event the client already saw. Event IDs are stable; clients deduplicate by ID. Exactly-once is not claimed.

## Error behavior (before the response is committed)

| Condition | Response |
| --- | --- |
| unsupported/empty `type`, unknown query member, unacceptable `Accept`, `Last-Event-ID` with the JSON representation | `400 invalid_request` |
| malformed, tampered, foreign-scope, ahead-of-head or ambiguous (two headers) cursor | `400 invalid_cursor` (identical for every cause) |
| missing credential | `401 invalid_credential` |
| in-tenant caller without a permitted family | `403 forbidden` |
| hidden tenant/project/run, missing project or run | `404 not_found` (identical bodies) |
| cursor older than the retained history | `410 cursor_expired` |
| authority, database or binding outage | `503 unavailable` with `Retry-After` |
| complete snapshot above the bound | `503 snapshot_too_large` |

A writer without per-write deadlines or flushing is refused with `500` before any header is committed.

## Integration (for the composition root and T-027)

Nothing here edits `internal/app`; wiring is the integration owner's:

```go
// 1. Required bindings. Work is merged; Runs is the execution module's seam
//    (current run/step-attempt state plus scoped run lookup) and has no
//    production implementation yet: NewStore fails without it.
store, err := obspg.NewStore(runtimePool, eventStore /* *events/postgres.Store */, obspg.Bindings{
    Work: obspg.WorkResources{},
    Runs: executionBinding, // implements obspg.RunBinding, uses only the supplied tx
})
// 2. Authorization over the installation's selected authority.
access, _ := obsauth.NewAdapter(selectedAuthority /* identity.SelectAuthority(...) */)
// 3. The use cases; every bound is explicit (no production defaults here).
svc, _ := observation.NewService(observation.Config{Log: store, State: store, Access: access, Options: observation.Options{
    PageSize: ..., MaxPageBytes: ..., PollInterval: ..., HeartbeatInterval: ..., AccessInterval: ..., MaxSnapshotResources: ...,
}})
// 4. HTTP: the cursor codec is the router's shared one (signing key injected).
h, _ := obshttp.NewHandler(svc, cursorCodec, obshttp.HandlerOptions{WriteTimeout: ..., MaxBufferedBytes: ...})
_ = h.RegisterRoutes(router)
// 5. Merge the route rules into the authorizer's map: a missing rule denies.
rules := identityhttp.DefaultRules()
for a, r := range obshttp.Rules() { rules[a] = r }
```

Deployment notes: `http.Server.WriteTimeout` must not cut these responses (the handler sets a per-write deadline through `http.ResponseController`, which replaces the server-wide one for the connection); graceful shutdown should cancel request contexts (for example `BaseContext`) or streams hold `Shutdown` until clients leave; the response-size bound (`MaxResponseBytes`) must exceed the largest permitted snapshot; the retention period remains the event store's (`events.RetentionPolicy.Observation`), and this module selects none.

## Tests

Real PostgreSQL only (isolated databases through `test/support/postgres`; `KODAMA_REQUIRE_POSTGRES=1` makes a missing database a failure); no NATS, S3 or Git. Resource prefix `t024`.

| Test | Location | Covers |
| --- | --- | --- |
| `Test_snapshot_stream_race` | `integration/snapshot_stream_race_test.go` | atomic snapshot vs concurrent commits, rollback, filtered gaps |
| `Test_cursor_expiry` | `integration/cursor_expiry_test.go` | real pruning, floor boundary, snapshot resync, midstream loss |
| `Test_foreign_cursor` | `integration/foreign_cursor_test.go` | scope-bound cursors, nondisclosure |
| `Test_slow_client` | `integration/slow_client_test.go` | write bound, disconnect, isolation from other clients, resume |
| `Test_current_authorization` | `integration/authorization_test.go` | revocation, outage, no fallback, snapshot release |
| `Test_filtered_resume` | `integration/resume_test.go` | run filtering, checkpoints, reconnect, handler restart |
| `Test_tenant_pool_cleanup` | `postgres/isolation_test.go` | RLS, predicates, pooled connections, cancellation |
| `Test_snapshot_binding_contract`, `Test_durable_read_snapshot` | `postgres/snapshot_test.go` | one transaction, required bindings, bounds |
| `Test_session_transitions` and projection/service tests | `session_test.go`, `projection_test.go`, `service_test.go` | entity rules, service paths with in-memory ports |
| `Test_http_contract` | `http/contract_test.go` | OpenAPI examples, DTOs, framing, headers, errors, writers |

Run: `go generate ./internal/observation/http/...`, `go test -race -count=1 ./internal/observation/...`, `go vet ./internal/observation/...`.
