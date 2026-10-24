# internal/integrations/chatops

Durable outbound ChatOps links (task T-041): project-scoped generic JSON webhook destinations, durable notification delivery over the event bus, bounded retries with a complete attempt history, visible failures with authorized manual replay, and authenticated current-record review links. The HTTP contract, the outbound payload and the receiver conventions are in [`api/chatops.yaml`](../../../api/chatops.yaml).

**Status.** The component is implemented and tested against real PostgreSQL, NATS JetStream and local HTTPS receivers. It is **not wired**: nothing registers its migration, routes, worker or event binding in a server yet (that is T-075/T-046's assembly), and no real gate exists as a link target here (T-046 verifies it).

**What this is not.** It is not interactive approval: a notification carries a link and no authority. There is no Slack/Teams semantics, no approval token, no capability link and no webhook receiver. It does not claim exactly-once delivery to a receiver that does not deduplicate.

## Immutable baseline

- **Subject:** verification and documentation amendment of the existing T-041 component at base commit `b1c63ff2121057533af6edd65e97fabfb1cafb54`; no behavioral, schema, OpenAPI or dependency change. Historical provenance: the component was last amended from `10663fc74318637d932acbc0626fb7100ca4d639` (overflow-safe retry bounds, conflicting-redelivery regression, corrected retention-test assertion). Results recorded against earlier revisions are prior evidence only; fresh candidate-specific results are in the review handoff and are not claimed here, and they are not review approval.
- **Requirements:** AC-08, AC-09, DQ-04, IT-03; supporting AC-03/04/12, IT-02/04/05/09/11 and SD-01–SD-11.
- **Acceptance:** `notification_outage`, `duplicate_delivery`, `forwarded_review_link`, `changed_subject_link`, `webhook_destination_policy`; module portions of AC-A06/A07 and DQ-A03. Real gate authorization, subject applicability and the business loop remain T-075/T-046/T-044.
- **Decisions:** confirmed C-01/03/06/11/16/17; baseline P-01/02/04/05/15/16; ADRs 0002, 0003, 0006, 0007. O-09 selects generic outbound webhooks. O-11 is resolved as an [approved recovery baseline](../../../docs/operations/upgrades-backup-and-recovery.md#approved-retention-and-recovery-baseline) (RPO/RTO/retention objectives to qualify, not measured guarantees here); those windows do not redefine T-005's independently qualified event deduplication horizons, and this task does not qualify them (T-029/T-056). O-18/O-19 select the hosted authority and custody direction; their qualification belongs to T-061, and standalone evidence here does not claim it.
- **Disposition:** conforms within the owned paths (`internal/integrations/chatops/`, `api/chatops.yaml`, `pkg/netpolicy/`); no shared file was edited.

## Layout

| Package | Contents | Depends on |
| --- | --- | --- |
| `internal/integrations/chatops` | `Destination` and `Delivery` entities and their transitions, `Payload`/`Project`, `LinkBuilder`, `Planner` and the consumer spec, `Service` (management use cases), `Worker`, consumer-owned ports (`ports.go`), failure classes (`errors.go`) | `contracts/events/v1`, `internal/domain`, `internal/events` (technology-free types only). **No pgx, no NATS, no `net/http`.** |
| `…/postgres` | `Store` (management, claims, finalization, fence), the event-bus `Binding`, `TxAuthorizer`/`IdentityAuthorizer`, `Migrations()`, `T041_001_chatops.sql` | pgx, `internal/storage`, `internal/events/postgres`, `internal/identity/postgres` |
| `…/webhook` | `Policy` (`chatops.EndpointPolicy`), `Client` (`chatops.Sender`), recipient-bound credential attachment | `pkg/netpolicy`, `net/http`; no SQL |
| `…/http` | handlers, `Routes()`, `Rules()`, generated DTOs, error mapping | `internal/httpapi`, identity route rules |
| `…/chatopstest` | shared fixture and published component evidence: real PostgreSQL stack, real event bus path, receivers, event builders | test support |

`boundaries_test.go` pins these directions, the absence of environment configuration, and the outbound client's security posture.

## Contract

### Destinations

A destination is one project-scoped generic webhook: `name`, an `https` `endpoint` without user-info, query or fragment, an explicit event allow-list, an optional credential **reference**, `enabled`, a `bindingRevision` and an optimistic `version`. The allow-list is exactly `work_item.transitioned`, `gate.requested`, `gate.decided`, `gate.invalidated`, `run.state_changed` and `deployment.state_changed`. Tenant-wide events (no project) and every other type are never forwarded.

`bindingRevision` advances only when the endpoint or the credential reference changes (database trigger and entity agree). A delivery records the revision it was created under: an intent is **never silently redirected** to a new endpoint or credential. At claim time a changed binding fails the intent visibly (`destination_changed`, no send); only an authorized replay re-binds it. A disabled destination gets no new intents, and its queued intents are not selected and resume unchanged when it is enabled again.

### Outbound payload (schema version 1)

A bounded projection, not the event: `schemaVersion`, `notificationId`, `event{id,type,occurredAt,correlationId}`, `tenantId`, `projectId`, `resource{type,id,version}`, a ≤256-byte `summary`, a few classified `attributes` and the `url`. Source content, transcripts, free-form event data and credentials are never forwarded. Fixtures: `testdata/*.event.json` and the byte-exact `testdata/*.payload.json`, checked against the schema in `api/chatops.yaml`. Receivers ignore unknown members; members are only added within a version.

Headers on every attempt: `Content-Type`, `Idempotency-Key` (the stable notification ID, identical across attempts **and replays**), `Kodama-Delivery-Id`, `Kodama-Event-Id`, `Kodama-Event-Type`, `Kodama-Attempt` (total, 1-based), `User-Agent`, and, only for an authenticated destination, `Authorization: Bearer <token>` from the recipient-bound credential source. Incoming API tokens, cookies and caller headers are never forwarded.

### Receipts

A receiver may return a provider receipt in the response header `X-Receipt-Id` (name configurable in `webhook.Config`). One printable-ASCII value of at most 128 bytes is stored next to the HTTP status; anything else is dropped. A missing receipt is never fabricated, and response bodies are never read past a bound or kept.

### Review links

`LinkBuilder` builds `<canonical origin>/api/v1/tenants/{t}/projects/{p}/{collection}/{id}` for work items, gates, runs and deployments from an explicitly configured origin: never from a request `Host`, never from an event-provided URL. A gate URL identifies `P/gates/{id}` only: no token, no impersonation, no embedded decision, no subject. Following it asks the **target route** to authenticate and authorize the follower against the record as it is now. `links_test.go` proves, against a module-local target fixture, that forwarding changes nothing, that a revoked reviewer is refused, that a GET cannot approve, and that a changed or expired record (including invalidation metadata) is what the same URL shows; real gate authorization and subject applicability are T-046/T-044.

## Delivery state machine

| Transition | Rule |
| --- | --- |
| pending → claimed | due; fencing token +1; the attempt record commits **before** the receiver is called |
| claimed → delivered | current token only; final |
| claimed → pending | classified transient failure with attempts left; bounded backoff (a `Retry-After` can lengthen it, never beyond the maximum) |
| claimed → failed | permanent class, or the cycle's attempts are used (`exhausted`) |
| claimed → claimed | lease expired: reclaimed under a new token; the old attempt is closed `abandoned`/uncertain; an expired *last* attempt fails `exhausted` instead of sending again |
| pending/expired → failed | the destination binding changed (`refused`) |
| failed → pending | authorized replay only: reason, `If-Match`, current binding; a new cycle; history preserved |

Unknown outcomes: a timeout, a connection loss after the request was sent, a bad response and a crash after the receiver accepted are recorded `uncertain` and retried with the identical payload and key. A receiver that does not deduplicate on `Idempotency-Key` may apply a notification more than once; ChatOps never claims otherwise. Failure classes are bounded (`unreachable`, `timeout`, `connection_lost`, `bad_response`, `receiver_error`, `rate_limited`, `rejected`, `redirect_refused`, `policy_refused`, `credential_unavailable`, `credential_source_unavailable`, `destination_changed`, `lease_expired`, `internal`); no URL, token, response body or raw transport error is stored or returned. Database triggers back the entity: delivered is final, tokens and counters never decrease, the payload is immutable, attempt history is append-only.

## Transactions and locks

- **Management mutations** (create/update destination, replay): project row (`FOR NO KEY UPDATE`, the same lock the work and identity modules take) → authorization revision (`FOR SHARE`, taken by `identitypg.NewTxAuthority`) → ChatOps rows. A revocation either commits first (and the check refuses) or waits for the mutation to finish (`TestAuthorizationRevocation`). Configuration/replay and their `audit` entry (and the replay metadata row) commit atomically.
- **Event-bus inbox handler:** runs inside T-005's own inbox transaction, in its savepoint: inbox row (T-005) → destination and delivery rows. It takes no project lock and no authorization revision, opens no transaction and calls no DNS, credential source or HTTP. No management mutation takes an inbox row, so the two orders cannot form a cycle.
- **Worker:** claim transaction takes due rows `FOR UPDATE SKIP LOCKED` → destination `FOR SHARE`, verifies eligibility, applies the claim and inserts the attempt, then commits; the send happens outside any transaction; finalization locks one delivery row and is conditioned on the current token (a repeat after an unknown commit outcome is recognized and records nothing twice).
- Reads use the same transaction-bound authorization; an authority outage fails closed as `unavailable`.

## Permissions

An adapter-local mapping to existing identity permissions, not a role: destination configuration (including its reads) and manual replay need `project:settings`; sanitized delivery inspection needs `project:read`. `chatopshttp.Rules()` contributes the coarse route rules; the store repeats the exact permission in each operation's transaction. Non-human actors and unknown permissions are denied. A foreign, cross-project or missing resource is the same 404.

## Retry, claim and HTTP bounds

All are constructor configuration (`WorkerConfig`, `webhook.Config`, `PolicyConfig`, `chatops.Config`); no environment variable exists and none is read. Operational values are T-075's to choose; the tests use short schedules that are not production values.

## Restore and retention

- **Fence.** T-005's consumer fence stops event replay but not deliveries that already exist. `Service.FenceDeliveries` (operator-authorized, reason-bearing, audited) sets a durable per-tenant fence that stops every process from claiming; `ClearDeliveryFence` resumes after the operator has reconciled external effects and fenced old workers. `Worker.Pause` is the process-local half. New intents are still created while fenced (they are durable and wait). A restore must fence before workers restart: a restored worker would otherwise send again to receivers that may have accepted before the backup point.
- **Retention.** Nothing here prunes. Unresolved deliveries and the `(destination, event)` deduplication identity are never deleted opportunistically; the runtime role has no `DELETE` on any table. T-005 keeps its own inbox evidence for the retention it is configured with, and the consumer is classified `ModeHistorical`/`EffectExternalIntent`/`IdempotentBeyondHorizon=false`, so the bus refuses replay beyond its deduplication horizon rather than re-creating an intent. A bounded archive of delivered rows is an installation policy to coordinate with T-005 and the approved recovery baseline (O-11); it is not decided here.

## Assumptions for the integration owner to confirm

1. **Destination policy.** Direct, public HTTPS only; every redirect refused. Private-network delivery is disabled unless the installation supplies `webhook.PrivateEndpoint` allowances: an exact IP and port, scoped to a tenant (optionally a project); never CIDR/wildcard, never unspecified/multicast/link-local (metadata). *T-041 review and the installation owner confirm.*
2. **Credential integration.** The merged `secrets.VersionSource` and gateway/MCP adapters resolve run-pinned credentials; ChatOps must not fabricate a run. `chatops.CredentialSource` is a required, recipient-bound port (`Resolve(scope, ref, Recipient{destination, bindingRevision, host, port})`); without it an authenticated destination is refused at activation and at send. No plaintext credential is stored. *T-075/T-046 confirm the binding; T-061 owns hosted custody qualification.*
3. **Filtering and delivery policy.** The explicit per-destination allow-list above; retry timing, maximum attempts, lease, HTTP bounds and destination limits are constructor configuration. *T-041 confirms the projection; T-075 confirms operational values.*
4. **Receipt convention.** Optional `X-Receipt-Id`, bounded, recorded with the local HTTP status; never fabricated. *T-041 confirms and documents the default.*
5. **Read/replay permissions.** As in [Permissions](#permissions). *T-041/T-010 reviewers confirm.*
6. **Added during implementation.** (a) The project lock is `FOR NO KEY UPDATE`, matching the work/identity modules, so destination mutations briefly serialize with work mutations of the same project. (b) `Retry-After` honors delta-seconds only; the integer is clamped to one hour before conversion to a duration (no overflow) and the wait is capped at the policy maximum. Exponential backoff saturates at the policy maximum before doubling could overflow, so every valid policy yields a positive delay of at most `Max`. (c) Each send uses a fresh, per-send transport with no connection reuse, so the dial-time address rule and any scoped allowance cannot leak across tenants. (d) Delivery IDs are derived from (tenant, project, destination, event) so the receiver key is stable across restore and replay. *Integration owner to confirm.*

## Wiring (for T-075 / T-046) — placeholders, not merged code

```go
// 1. Migrations, after base, events and identity (chatopstest.Contributions is the executable form).
manifest, _ := storage.NewManifest(storage.BaseContribution(), eventspg.Migrations(), identitypg.Migrations() /* … */, chatopspg.Migrations())

// 2. The consumer joins T-005's inbox transaction; give the binding to the event store.
links, _ := chatops.NewLinkBuilder("https://<canonical API origin>")
planner, _ := chatops.NewPlanner(links, nil)
binding, _ := chatopspg.NewBinding(planner)
eventStore, _ := eventspg.NewStore(runtimePool, eventOptions, binding /* , other bindings */)
// …and a jetstream.Consumer named chatops.ConsumerName, as in internal/events/README.md.

// 3. Store, outbound adapter, service, worker.
store, _ := chatopspg.NewStore(chatopspg.Config{DB: runtimePool, Authorizer: chatopspg.IdentityAuthorizer{} /* standalone; hosted supplies its own */})
policy, _ := webhook.NewPolicy(webhook.PolicyConfig{ /* Ports, Hosts, PrivateEndpoints: installation policy */ })
client, _ := webhook.NewClient(webhook.Config{Policy: policy, Credentials: recipientBoundSource /* nil: credential-free only */})
svc, _ := chatops.NewService(chatops.Config{Store: store, Policy: policy, Credentials: recipientBoundSource, Fences: store, Operators: platformOperators, MaxDestinationsPerProject: 20})
worker, _ := chatops.NewWorker(chatops.WorkerConfig{Work: store, Tenants: eventStore, Sender: client, Retry: …, Interval: …, Batch: …, Lease: …, AttemptTimeout: …, Concurrency: …})
go worker.Run(ctx)

// 4. Routes and rules.
h, _ := chatopshttp.NewHandler(svc, cursorCodec)
_ = h.RegisterRoutes(router)                       // prefix /api/v1
for a, r := range chatopshttp.Rules() { rules[a] = r }   // into identityhttp.NewAuthorizer
```

Missing required bindings fail construction (`NewService`, `NewStore`, `NewWorker`, `NewBinding`) or destination activation; none falls back to allow-all or send-anyway.

## Migration order

`T041_001_chatops.sql` is contributed after the base, event and identity migrations (it references `tenants`/`projects` and its authorization reads the identity tables). Once deployed the order must not change; the ledger refuses a reordered or modified migration (`TestMigrationReapply`).

## Running the checks

```sh
make check-task TASK=T-041                 # unit layer: fast, no services, never merge evidence
make check-task TASK=T-041 LAYER=all       # complete scoped qualification (real PostgreSQL and NATS)
go test -race -count=1 ./internal/integrations/chatops/... ./pkg/netpolicy/...
go generate ./internal/integrations/chatops/http && git diff --exit-code -- internal/integrations/chatops/http/chatops.gen.go
```

The service-backed tests use the existing controls and add no environment name: `KODAMA_TEST_POSTGRES_ADMIN_URL`, `KODAMA_NATS_URL`, `KODAMA_REQUIRE_POSTGRES=1`. Databases come from `test/support/postgres`; streams, subjects and durable names carry the `t041` prefix plus a random suffix and are removed on cleanup. Receivers are local `httptest` TLS servers; broker/receiver outages are injected through behaviors, never by stopping a shared service.

| Test | Evidence layer | Covers |
| --- | --- | --- |
| `Test_notification_outage` (`integration/`) | real PostgreSQL + NATS, receiver | AC-08/AC-A06: outbox → JetStream → inbox → delivery survives receiver outage and worker restart; source review event stays committed; history persists; exhausted delivery is visible and replayed manually |
| `Test_duplicate_delivery` (`integration/`) | real PostgreSQL + NATS, receiver | AC-08: concurrent redelivery, broker duplicates and repair → one intent per destination/event; lost response and process failure retry the same key/payload; deduplicating receiver applies one effect; racing workers send once |
| `Test_forwarded_review_link`, `Test_changed_subject_link` (`links_test.go`) | none (module-local target fixture) | AC-09, DQ-04, IT-03 (module portion): a link grants no authority; the same URL shows the changed record. Real authorization/subject behavior: T-046/T-044 |
| `Test_webhook_destination_policy` (`webhook/`) | local HTTP/TLS | destination and redirect restrictions, forbidden resolved addresses, proxy bypass, scoped private allowances |
| `TestDeliveryTransitions`, `TestDestinationEdit`, `TestEndpointShape`, `TestPayloadProjection`, `TestModuleBoundaries` | none | entity rules, fixtures, boundaries |
| `Test_duplicate_delivery_after_retention` (`integration/`) | real PostgreSQL + NATS, receiver | late copies cannot recreate effects after event evidence is pruned; longer retention cannot restore lost evidence; external-intent replay beyond the horizon is refused while authorized replay of retained failed deliveries works |
| `TestWorkerPanicIsUncertain` (`worker_test.go`) | none | a panicking sender is an unknown outcome, never success, never leaks panic text; the retry keeps the exact key and payload |
| `TestRetryDelayBounds` (`delivery_test.go`), `Retry-After` subtests of `TestWebhookBoundsAndClassification` | none / local HTTPS | overflow-safe bounded backoff and numeric `Retry-After` clamping, including near-`Duration`-limit values |
| `TestInboxIntentAtomicity` | real PostgreSQL | partial fan-out rolls back every intent and the inbox completion; reordered and same-version events stay eligible |
| `TestConflictingRedeliveryPreservesOriginalIntent` | real PostgreSQL (real T-005 `Append`/`Process`) | changed content under an existing event ID is quarantined `conflicting_content`; the original pending or completed intent, inbox entry and schedule are unchanged and repair processes only the original content |
| `TestTenantIsolationAndPoolReuse` | real PostgreSQL | non-owner RLS, composite ownership, foreign references, scoped keyset paging, single-connection pool reuse |
| `TestConcurrentClaimsAndReplay`, `TestRestoreFence` | real PostgreSQL | competing workers, expiry and stale finalizers, exhaustion by repeated expiry, duplicate replays, lost commit acknowledgments, restore fence |
| `TestAuthorizationRevocation` | real PostgreSQL + T-010 authority | revocation ordering both ways, missing bindings, outages fail closed |
| `TestCredentialAndDestinationChanges`, `TestWebhookBoundsAndClassification` | local HTTP; PostgreSQL for destination changes | credentials, changed/disabled destinations, bounds, classification, uncertain acceptance |
| `TestMigrationReapply` | real PostgreSQL | ordering, reapplication, grants, constraints and guards |
| `TestHTTPContract` | none (port double) | OpenAPI examples/DTOs, ETags, cursor binding, replay requirements, nondisclosing errors |
| `TestHTTPOverRealStack` (`integration/`) | real PostgreSQL + NATS | the routes over the real router rules, authority, store and worker |
| `TestDialHookBlocksBeforeConnect`, address matrix (`pkg/netpolicy`) | local sockets | actual dial control rejects forbidden connections before listener acceptance |

Component evidence only: it does not qualify production wiring (T-075), a real gate target (T-046/T-044), hosted authority or credential custody (T-061), or restore procedures and recovery objectives (O-11).
