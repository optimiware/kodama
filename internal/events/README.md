# internal/events

The durable event bus (task T-005): transaction-local event append, outbox → JetStream → inbox delivery, retained observation sequences, quarantine and audited replay, bounded retention and backpressure, and periodic reconciliation. It implements the [event catalog](../../docs/contracts/event-catalog.md); the wire shapes are frozen in [`contracts/events`](../../contracts/events/README.md).

PostgreSQL state and the outbox are authoritative. JetStream provides at-least-once transport; broker duplicate suppression is an optimization and the inbox is the correctness boundary. Nothing here assumes exactly-once external effects.

## Layout

| Package | Contents | Depends on |
| --- | --- | --- |
| `internal/events` | Entities (`Publication`, `InboxEntry`), failure classes, retry/retention policy, replay request/grant/classification, `Publisher` worker, `Reconciler`, consumer-owned ports | `contracts/events/v1`, `internal/domain`. **No pgx, no NATS.** |
| `internal/events/postgres` | `Store`: `Append`, observation reads, outbox claims, inbox processing, quarantine, replay, fences, retention; `Migrations()` | pgx, `internal/storage` |
| `internal/events/jetstream` | `Publisher` (durable ack), `Consumer` (explicit ack), stream/consumer provisioning and validation | `nats.go` |
| `internal/events/eventstest` | Test fixtures: migrated database, broker streams, fault proxy, event builders | test only |

Ports owned by this module (`ports.go`): `TenantSource`, `PublicationStore`, `StatsReader`, `BrokerPublisher`, `Processor`, `SubjectResolver`, plus `ReplayAuthorizer` (`replay.go`). `postgres.Store` implements the first five; `jetstream.Publisher` implements `BrokerPublisher`.

## Wiring (for T-027)

```go
// 1. Migrations, after the base contribution.
manifest, _ := storage.NewManifest(storage.BaseContribution(), postgres.Migrations() /* , others */)
storage.Migrate(ctx, migrationPool, manifest, storage.MigrateOptions{RuntimeRole: role})

// 2. The store: every policy is explicit, none has a default.
store, _ := postgres.NewStore(runtimePool, postgres.Options{
    Backlog:     events.BacklogLimits{MaxEvents: ..., MaxBytes: ...},
    Retry:       events.RetryPolicy{Initial: ..., Max: ..., MaxAttempts: ...},
    Retention:   events.RetentionPolicy{...},          // see the constraints below
    ReplayLease: ..., ReplayBatch: ...,
    Authorizer:  platformReplayAuthorizer,             // nil refuses all operator operations
}, postgres.Binding{Spec: events.ConsumerSpec{Name: "audit-export", Mode: events.ModeHistorical, Effect: events.EffectLocal}, Handle: auditExport})

// 3. Broker: inject the connection (token from the `nats-token` secret file).
cfg := jetstream.Config{Stream: "kodama-events", MaxAge: ..., MaxBytes: ..., MaxMsgs: ..., DuplicateWindow: ..., Replicas: ...}
cfg.ValidateAgainst(retention)                        // inbox must cover the stream's redelivery horizon
pub, _ := jetstream.NewPublisher(nc, cfg);  pub.EnsureStream(ctx)
publisher, _ := events.NewPublisher(store, store, pub, events.PublisherConfig{...})
consumer, _ := jetstream.NewConsumer(nc, cfg, jetstream.ConsumerConfig{Consumer: "audit-export", Durable: "...", ...}, store, store, logger)
consumer.Provision(ctx)

// 4. Workers.
go publisher.Run(ctx)                                  // periodic pass + coalesced wakeups
go consumer.Run(ctx)
rec, _ := events.NewReconciler(store, interval, logger,
    store.RepairReconciliation("audit-export", grace, batch), store.ReplayReconciliation(), store.PruneReconciliation(), /* module reconcilers */)
go rec.Run(ctx)
```

### Producers: append in the business transaction

```go
err := storage.WithTenant(ctx, pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
    // 1. business state, history and audit rows ...
    // 2. the event joins the same transaction:
    _, err := store.Append(ctx, tx, envelope)   // never begins/commits, never calls the broker
    return err
})
publisher.Wake() // optional; the periodic pass repairs a missed wakeup
```

`Append` validates the envelope, requires the transaction's tenant context to equal the event's tenant, checks project ownership, enforces the backlog limits, and writes the outbox row, the observation record and the counters. A repeated event ID with identical content returns the existing result (`Duplicate`); different content returns `ErrConflict`; an ID that no retained record knows, for an event that occurred before the tenant's durable deduplication boundary (see below), returns `ErrStaleAppend` before anything is written; a full backlog returns `ErrBacklogFull` before anything is written (roll the business change back). It requires READ COMMITTED (the default) and fails otherwise.

**Lock order and cost.** Each replayed event's transaction also takes that admission lock in shared mode first, before the replay request row, inbox rows, and whatever the handler locks (including the backlog and scope rows of an `Append` it performs); fencing and replay admission take it exclusively and then the request rows, so no lock cycle exists between them and a handler's append (live deliveries hold no request row and do not take the lock, so they never delay pruning). Appenders take the tenant's admission advisory lock in shared mode first (it is the lock that inbox pruning, replay admission and fencing take exclusively), then lock the tenant's `event_backlog` row, then the scope's `event_scopes` row, and hold both until the transaction ends. All appenders of one tenant therefore serialize; that is what makes the limits exact and the observation sequence gapless in commit order (a rolled-back append releases its sequence). Keep producer transactions short and do no network calls in them. Publishers and consumers never lock these rows ahead of outbox/inbox rows, so they cannot deadlock with appenders.

### Consumers: inbox plus local effect

`postgres.Binding.Handle` receives `(ctx, tx pgx.Tx, env)` and runs inside the consumer's tenant transaction, in a savepoint, together with the inbox completion. It is adapter wiring: delegate to a domain-oriented port in the owning module rather than spreading pgx into use cases. It must not make network calls. Return `events.Transient(err)` or `events.Permanent(err)`; an unclassified error is retried as transient until `RetryPolicy.MaxAttempts`, then quarantined. A failed handler's writes roll back while the retry/quarantine outcome is recorded durably before the transport acknowledges. External effects (notifications, deployments) must only create durable intents that the owning module reconciles with its own idempotency key.

Current-state consumers (`ModeCurrentState`) reload the authoritative aggregate and guard on versions (`ProcessingMode.Observes`); historical consumers (`ModeHistorical`) process every unique event, including older observations and several events at one version.

### Observation reads (T-024)

```go
obs, wm, err := store.ReadObservations(ctx, tx, events.Scope{TenantID: t, ProjectID: p}, afterSeq, limit)
// err wraps events.ErrCursorExpired when afterSeq is below the retention floor: resync from a snapshot.
wm, err := store.Watermark(ctx, tx, scope) // read in the same REPEATABLE READ tx as the snapshot
```

Sequences are per scope (project, or tenant-wide with an empty project) and gapless in commit order. T-024 owns SSE, `Last-Event-ID` encoding and the HTTP error mapping.

## Delivery state machines

| Entity | Transitions |
| --- | --- |
| `Publication` | pending → claimed (due) · claimed → claimed (lease expired; new fencing token) · claimed → published (current token only) · claimed → pending (classified failure, scheduled retry) |
| `InboxEntry` | pending → completed · pending → pending (transient, attempts remain) · pending → quarantined (permanent or exhausted) · quarantined → pending (authorized replay only) · completed is final |

Unclassified failures are rejected by both entities; stale tokens are `ErrStaleClaim`.

## Replay and recovery

Replay is targeted at one consumer and one scope's observation range, requires an authorized operator (`ReplayAuthorizer`; no authorizer refuses everything), a reason, and is persisted with its audit rows in one transaction. `DryRun` classifies the range (already completed, quarantined, pending, undelivered, beyond horizon, expired) and reports why a replay would be refused. `RequestReplay` refuses:

- **unclassified consumers** (`EffectUnknown`) and ranges beyond the scope's last event (`ErrReplayUnsafe`);
- **events beyond the deduplication horizon** (no inbox entry and recorded before the evidence floor: the later of `RetentionPolicy.Inbox` and the durable boundary below, fixed on the request at admission), unless the request acknowledges it **and** the consumer's owner declared its local effect idempotent; external-intent consumers are never replayed beyond the horizon (`ErrReplayBeyondHorizon`);
- **pruned history** (`ErrReplayRangeExpired`);
- **fenced consumers** (`ErrReplayFenced`): after a database restore an operator calls `FenceConsumer`; replay stays refused until the consumer's owner confirms that external effects were reconciled and old controllers fenced and calls `ClearFence`. A fence set after a request stops the run before the next event's effect. `FenceConsumer` takes the tenant's replay-admission lock before it touches the consumer's active requests, so it is totally ordered with `RequestReplay`: a request admitted earlier is visible to the fence's update (which waits for that request's in-flight event and makes later steps observe the fence), and a request that arrives while the fence is being recorded waits and is then refused as fenced. No request can be admitted between the fence's enumeration and its commit.

`RequestReplay` takes the scope's row lock (the one observation pruning takes `FOR UPDATE`) and the tenant's replay-admission lock before it reads the watermark and classifies, and holds them until the request row commits. A prune therefore either committed first (the request sees the new floor and is refused as expired) or waits and then finds the request pinning its range; the unprocessed observation range **and the inbox evidence of its events** are never pruned while the request is active.

`RunReplay`/`ResumeReplays` process through the same inbox transaction as live delivery: completed events are skipped, quarantined ones reopened, the event IDs are preserved, other consumers are untouched. Every event runs in its own transaction whose first statement validates and renews the runner's claim (fencing token, running state) and then re-checks the restore fence, the consumer's current classification against the one the request was admitted under, and, where no inbox evidence exists, that the event was inside the deduplication horizon at admission (or the request's explicit idempotent-consumer exception applies). A takeover or a fence that commits first is therefore seen before any effect, and one arriving later waits for the event in flight. Progress and counters are recorded in the same transaction, so an interrupted runner resumes exactly where it stopped. A range that was lost despite the pin stops the run with `ErrReplayRangeExpired` instead of completing it; a refused run (fence, unsafe/changed classification, beyond horizon) keeps the request active, pinned, and visible. An authorized replay is the only thing that overrides a pending entry's scheduled retry time. Completion is its own fenced, resumable step: when a runner dies after the last event committed, `next_seq` already exceeds the range and the next runner (`ResumeReplays`) only finalizes, atomically persisting the terminal state and its `event.replay_completed` audit row from the request's durable counters, exactly once. Unsupported-version and malformed deliveries are not replayable events; `DiscardQuarantine` resolves them (audited per quarantine cycle: a resolved entry that an inbox outcome reopens, for example a replay that fails again, starts a new `cycle` with its own audit identity, while a raw redelivery of already resolved poison keeps its resolution). Side-effecting restore/replay stays disabled until its owner supplies reconciliation (O-11 and the restore procedure are not decided here).

## Retention and backpressure

`RetentionPolicy` has no defaults (`MaxClockSkew` included, which must be positive). Validation enforces `Inbox >= BrokerRedelivery`, `Inbox >= ReplayWindow` and `Observation >= ReplayWindow`; `jetstream.Config.ValidateAgainst` requires the stream's `MaxAge` not to exceed `BrokerRedelivery`. `Store.Prune` (also available as `PruneReconciliation`) removes, in bounded batches, published outbox rows, completed inbox entries, the oldest observations (advancing the scope's floor in the same transaction, never past an active replay's next sequence), resolved quarantine records and completed replay requests. Row level security forbids deleting pending outbox rows, non-completed inbox entries, unresolved quarantine and active replays, and column grants make event content immutable. The merged `audit` table is untouched. Pruning an outbox row never removes observation history, and observation pruning never removes deduplication evidence.

**Safe-to-forget boundary for deduplication evidence.** A completed inbox entry is deleted only when its completion is older than `Inbox` **and** its event is not still publishable and not within a broker lifetime: (1) its outbox row is not `pending`/`claimed` (an event consumed from an earlier broker copy, or by repair, before the publisher marked it can still be republished after an outage of any length, starting a new broker lifetime), (2) its `published_at` is older than `BrokerRedelivery`, and (3) no active replay request has it in its unprocessed range. A published outbox row is the persisted record of when the publisher last learned of a broker acceptance, so it is kept until `BrokerRedelivery` has passed (the effective published-outbox retention is `max(PublishedOutbox, BrokerRedelivery)`). Evidence of unpublished events is bounded in practice by the backlog limits, not by time.

**Durable deduplication boundary.** Absence of an inbox entry is never interpreted with the running process's `RetentionPolicy` alone: raising `Inbox` (or `BrokerRedelivery`) later cannot restore evidence an earlier prune deleted. The inbox-pruning transaction therefore first advances the tenant's `event_dedup_boundary` row (`inbox_before` = the completion cutoff used, `broker_before` = the publication cutoff used), committing it together with the delete; a trigger makes both values monotonic, so neither a restart with other options nor a second process can lower them. Every decision uses the later of the durable value and the current policy's cutoff: live delivery and repair (`checkEvidenceHorizon`; repair also stops selecting events recorded before it), and replay admission (the resulting `evidence_floor` is stored on the request, so later pruning, which keeps an active request's evidence, does not refuse a request that was safe when admitted). The boundary is conservative: it moves with every prune, and an event whose origin lies before it needs the explicit idempotent-consumer replay exception. A decreased policy simply takes effect at the next prune. There is no operation to move the boundary backwards; if one is ever needed it must be an explicit migration owned by the operators of that tenant's effects. The admission time used by `checkEvidenceHorizon` is read inside the transaction after the inbox insert (never when the transaction or delivery began), because the uncommitted entry is what holds the event against competing completion and pruning.

**Completion never precedes a retained origin.** The timestamps that bound an event's lifetime come from different clocks: `outbox.created_at` from the appending process, `event_observations.recorded_at` from PostgreSQL, completion from the consuming process. Every later decision about a missing entry (the evidence-horizon check, repair backfill, replay classification, `Append`'s boundary check) compares one of those origins with the persisted deduplication boundary, which pruning derives from `completed_at`. An entry is therefore stored as completed at the latest of the consumer's clock, the event's `occurredAt`, and the retained outbox and observation origins read in the completing transaction; a pruned entry implies that every origin that survives it lies before the boundary, whatever the skew between the three clocks. `TestCompletionNeverPrecedesRetainedOrigin` skews the database or appender clock against the consumer, prunes between the unclamped completion and the origin, and requires one effect on live delivery, repair and replay classification.

**Event content is immutable per inbox entry.** The inbox row's `payload_digest` is read under the row lock before any duplicate, backoff, retry, replay or handler decision (and, unlocked, before the project-scope check so a changed project cannot masquerade as a scope error). A delivery of the same `(tenant, consumer, eventId)` with different content is quarantined as `conflicting_content` under a key derived from its payload hash, leaving the original entry, its schedule and its quarantine record untouched and never running a handler; a replay that meets a retained observation whose content differs from the entry counts the event as failed the same way. `TestConflictingRedeliveryKeepsOriginalContent` covers pending and completed entries, changed resource, payload, project and actor values, and the original's later repair.

**Re-appending is recognized by any surviving evidence.** The retention lifetimes are independent, so a consumer's inbox entry can outlive the event's outbox row and observation. `Append`'s idempotency lookup therefore also consults the inbox (`event_inbox.payload_digest`, kept after the payload is cleared on completion): while any consumer's entry exists, an identical re-append returns the existing result (`Duplicate`, sequence 0) and creates nothing, and different content under the ID is `ErrConflict`. Without this, the re-append would give the event a fresh `created_at`/`recorded_at` that postdates its completion; once the inbox entry was pruned, that fresh observation would be the only record, and delivery or repair, which take its recording time as the origin, would admit the event as unprocessed and repeat the effect. Each event thus keeps one origin for as long as any record of its processing exists, and after the last one is pruned the boundary check below refuses the retry. Consequence: a consumer that never received an event cannot be given it by a re-append once another consumer's entry exists (its history is bounded by observation/broker retention anyway).

**Re-appending a fully pruned event is refused.** Once an event was published, consumed, and its outbox row, observation and inbox entry were all pruned, no retained record recognizes its ID, and an `Append` retry would otherwise create a fresh unpublished outbox row whose delivery `checkEvidenceHorizon` admits (an unpublished outbox row means "never processed"), repeating the effect. `Append` therefore also checks, after its idempotency lookup finds nothing, that the event's `occurredAt` is not before the tenant's durable `inbox_before` boundary, and otherwise fails with `ErrStaleAppend`. This works without keeping identity records forever: a retry carries the identical envelope and so the identical `occurredAt`, and an inbox completion is stamped no earlier than the event's `occurredAt` (the later of the processing time and `occurredAt`, so a producer whose clock runs ahead cannot make an event complete "before it happened"), so the pruning that forgot the evidence (completion before the boundary) necessarily moved the boundary past `occurredAt` too, whatever the clock skew. `Append` refuses an event whose `occurredAt` is later than its own clock plus `RetentionPolicy.MaxClockSkew` with `ErrFutureEvent`, which bounds how long an event can extend its own evidence; `MaxClockSkew` is explicit configuration, like every retention value. Only the durable boundary is used (a larger policy cannot lower it), and the verdict is exact because appends hold the admission lock shared until their transaction ends while the boundary is advanced under the exclusive lock: a prune waits for in-flight appends, whose unpublished outbox rows then protect their events. Assumptions: producers emit `occurredAt` from a clock that is not ahead of the appender's by more than `MaxClockSkew` (enforced) and not behind by more than the inbox retention; an event ID is never reused for a different occurrence (a different envelope under a pruned ID is indistinguishable from a new event; the contract requires unique IDs); a legitimately new event whose `occurredAt` predates the boundary (a producer lagging by more than the inbox retention) is refused and must be re-issued with a current timestamp.

**Late copies are refused where they would act, not fenced where they are sent.** No publisher-side rule can bound the last broker acceptance: cancelling a `Publish` context removes only the response waiter, not a message the NATS client buffered during a reconnect or already handed to the network, so a publisher that timed out can still land its old copy after another publisher published, consumers completed, and retention forgot the evidence (and the broker's duplicate window passed). A fetched delivery held by a stalled consumer, or an item selected by a stalled repair worker, is the same hazard on the consuming side; `MaxAge` does not revoke messages already in client memory. The durable guard is therefore inside the transaction that would create an inbox entry and run the effect (live delivery and repair; replay has its own admission checks): when the insert actually creates the entry, `checkEvidenceHorizon` must show that the entry cannot have been pruned, mirroring the prune rule from durable facts only (never from when the message was fetched or selected):

- the outbox row is unpublished, or was published within `BrokerRedelivery` (pruning is forbidden): the missing entry means "never processed";
- otherwise the entry can only be missing because it was pruned if the event is older than the inbox cutoff (a completion is never earlier than the event's origin: the outbox row's `created_at`, or the retained observation's `recorded_at` once that row is gone); a younger event is admitted, an older one or one of unknown origin is **refused**.

A refusal rolls the insert back before any effect; `Process` quarantines the delivery durably as `beyond_horizon` (acknowledged after the record is committed, kept for an operator, never retried forever, and a resolved entry stays resolved on further copies); `Repair` skips the item and counts it in `RepairResult.Refused`. An operator may still replay such an event with the explicit idempotent-consumer exception. Because every fact the check reads is monotone, the verdict cannot flip back to "admit" after a prune. Clock assumption: `created_at` and the consumer's decision use the store's injected clock, as pruning does; keep the clocks of publisher, consumer and pruning processes in agreement within the margin you accept. Events must reach the broker through `Append` (an event with no outbox row and no observation is refused as unknown). The publisher also never sends a claim at or after its persisted lease expiry (`Claim.LeaseExpiresAt`; counted in `PublishResult.Expired`) and caps each send at the remaining lease, which only reduces how often a stale copy is sent.

**A replayed event resolves its quarantine record whenever it completes.** An authorized replay reopens a quarantined inbox entry and records the replay and its operator on the quarantine record (`replay_id`, `replay_by`) in the same transaction. If the handler then fails transiently, the entry is a pending retry and the record stays open; whichever path later completes the effect (scheduled retry, `Repair`, ordinary delivery or another replay) resolves the record as `replayed` for that operator in the completing transaction, so a committed effect never leaves an unresolved failure and retention can clean the record up. A record with no replay provenance is never resolved implicitly. `TestQuarantineResolvedWhenReplayedEventCompletesLater` covers quarantine, replay, transient failure, restart, repair or delivery, completion and retention.

**Existing JetStream consumers are validated, not adopted blindly.** `Provision` reuses a durable consumer only if it is a pull consumer with explicit acknowledgment, the adapter's filter, `DeliverAll`, instant replay, and the configured ack wait and max ack pending, with unlimited broker redelivery. It refuses (`ErrInvalidConfiguration`, consumer untouched) a push consumer or delivery subject, `HeadersOnly` (which would deliver empty payloads that look malformed), other start positions, backoff overrides, in-memory state, inactivity expiry, paused or prioritized consumers, replica counts differing from the stream, and request limits below the fetch settings.

**Existing JetStream streams are validated the same way.** `EnsureStream` and `Provision` reuse a stream only if its limits match and nothing else changes what it stores or whether a publish is acknowledged. They refuse (`ErrInvalidConfiguration`, stream untouched, no consumer created) `NoAck` (publishes would time out while the message is stored, so the outbox could never drain), a `SubjectTransform` (stored subjects would lose the tenant token and type that the consumer filter and trusted tenant resolution rely on), mirrors or sources, sealed streams, discard-new, per-subject message limits, per-message size limits below the largest envelope, consumer-count limits, message counters, asynchronous persistence, and stream-level consumer inactivity or ack caps. `TestStreamRefusesBehaviorChangingSettings` pre-creates each variant on real JetStream.

**Broker redelivery is unlimited.** `ConsumerConfig` has no `MaxDeliver`: the adapter creates consumers with unlimited redelivery and refuses an existing durable with a finite limit. A finite limit would make the broker stop delivering a message the processor could not settle durably (for example a poison message while PostgreSQL is down); a malformed or unsupported raw delivery has no observation or inbox entry, so nothing could recover it before stream expiry. The inbox attempt policy, which runs after a durable outcome exists, alone decides when to quarantine. `Test_quarantine_database_outage` withholds the database's ability to persist a quarantine, requires repeated redelivery without settlement, then restores it and requires durable quarantine before acknowledgment.

**Poison is quarantined, never retried forever.** Under the trusted subscription tenant, `Process` checks that the envelope's project exists for that tenant before it touches the inbox; an absent or foreign-owned project is quarantined as `scope_mismatch` (the project is never a foreign key there), as are other scope violations. Untrusted text that PostgreSQL `text` cannot hold (NUL, invalid UTF-8) is sanitized with `events.SafeText` in every stored diagnostic (handler, broker and decoder errors) or omitted from display metadata (best-effort event type); the original bytes live only in the `bytea` payload. Both guarantee the quarantine transaction itself cannot fail on content, so the message is acknowledged after durable quarantine instead of being redelivered indefinitely.

**Unresolved inbox work is self-contained.** A pending or quarantined inbox entry keeps the delivered payload on the entry (cleared at completion), so `Repair` can re-drive a scheduled retry long after the broker copy and the observation history expired; observation retention only governs API cursors and replay ranges. A pending entry with a scheduled retry is not run by ordinary deliveries, duplicates or repair before its time (the handler is not invoked and the attempt count is unchanged); `Process` answers `OutcomeRetry` with the remaining delay. Production values and RPO/RTO are operational policy (O-11), not chosen here.

## Observability

`Store.PublicationStats` reports pending/claimed counts, backlog bytes, oldest unpublished age, total attempts, next retry and the last failure class/message per tenant; `outbox_attempts` and `event_inbox_attempts` keep durable retry/failure history; `Publisher.Counters` and `jetstream.Consumer.Counters` are process-local. Payloads are never logged and diagnostics are bounded to 512 bytes.

## Running the checks

```sh
go test -race -count=1 ./contracts/events/... ./internal/events/...
```

Pure tests need nothing. The acceptance and adapter tests need real PostgreSQL and NATS: set the existing controls, as for `internal/storage`:

```sh
export KODAMA_TEST_POSTGRES_ADMIN_URL='postgres://postgres:<password>@127.0.0.1:<port>/postgres?sslmode=disable'
export KODAMA_NATS_URL='nats://<token>@127.0.0.1:<port>'   # or without user-info with the token in /secrets/nats-token
export KODAMA_REQUIRE_POSTGRES=1                          # fail instead of skip when either is missing
```

No new environment names exist. Databases come from `test/support/postgres`; streams, subject namespaces and durable names use the `t005` prefix plus a random suffix and are removed on cleanup. Broker outages are injected through a test-local TCP proxy, never by stopping the shared service.

| Test | Covers |
| --- | --- |
| `Test_publish_ack_crash` | PX-05, AC-08: crash between broker ack and publication mark; reclaim; stable ID; effect once beyond broker suppression |
| `Test_duplicate_consumer` | PX-05, AC-08: concurrent and redelivered duplicates, crash after local commit, rollback of failed effects |
| `Test_reordered_events` | PX-05, PX-A03: newer-before-older, same-version events, lost wakeup, periodic reconciliation |
| `Test_broker_outage` | PX-05, PX-A03, AC-08, AC-A06: outage, retry history, atomic backlog rejection, drain after recovery |
| `Test_poison_scope_and_text` | PX-05: foreign/nonexistent project and escaped-NUL poison quarantine durably before ack, no handler, no endless redelivery |
| `TestStalePublisherCannotRepublishAfterRetention` | PX-05, AC-08: a publisher stalled past its lease sends nothing after another publisher published, consumers completed and retention pruned |
| `TestLateBufferedPublicationIsHarmless` | PX-05, AC-08: a publication buffered across a reconnect lands after another publisher published, consumers completed and evidence was pruned; the copy is quarantined, effects stay single |
| `TestPausedAdmissionCannotRepeatPrunedEffect`, `TestRetentionIncreaseDoesNotReadmitPrunedEffects`, `TestBoundaryDoesNotBlockAdmittedReplay` | PX-05, AC-08: a delivery stalled before its inbox insert and resumed after completion and pruning is refused (observation retained); a larger `Inbox` after a restart never readmits pruned effects in live delivery, repair or replay admission; an admitted replay keeps its admission horizon |
| `TestPausedDeliveryCannotRerunPrunedEffect`, `TestRepairRevalidatesEvidenceHorizon`, `TestEvidenceHorizonBoundary` | PX-05, AC-08: a fetched delivery or selected repair item resumed after completion elsewhere and pruning is refused; the boundary matrix |
| `TestReplayFinalizationResumes` | AC-08: crash between the last event and finalization completes exactly once after restart |
| `Test_quarantine_replay` | PX-05, AC-08: durable quarantine before ack, scoped dry-run, audited replay surviving restart |
| `TestReplayRecoveryGuard` | PX-05, PX-A03: beyond-horizon, restore fence, unknown external outcomes |
| `TestDeliveryTransitions`, `TestAppendAtomicity`, `TestTenantIsolation`, `TestClaimFencing`, `TestObservationCommitOrder`, `TestRetentionBackpressure`, `TestMigrationReapply`, `TestStreamProvisioning` | state transitions, atomicity, tenant isolation (PostgreSQL and NATS subjects), fencing, cursors, retention, migrations, provisioning |

AC-06 here is the foundation (retained, gapless cursors, expiry and watermarks); the SSE/`Last-Event-ID`/`410 cursor_expired` behavior is T-024. AC-08 here is durable delivery with retry/failure visibility; actual notification destinations are T-041.

## Requirement and baseline mapping

Requirements: PX-05, AC-06, AC-08; supporting SD-01 to SD-08 and the tenant persistence invariants IT-02, IT-04 and IT-05. Decisions: C-01, C-03, C-11, C-16 and P-01, P-02, P-05, P-16; ADR 0002 and ADR 0006. The catalog's payload field spellings are proposed by `contracts/events/v1` and confirmed by the producer modules; O-11 (retention periods, RPO/RTO) is not resolved by this implementation.
