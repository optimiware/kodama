# Events: what the memory adapter does not model

`Mem`, `InProcessBroker` and `OpenMem` run `Contract` and `BrokerContract` with no database or broker (SD-13). They are flagged development-only (`DevelopmentOnly() bool`); `internal/app` refuses them under `production` (SD-12).

Modeled: the transaction-local `Append` over the caller's `memdb.Tx` (identity and content conflict, per-scope gapless sequences, backlog limits, future-event bound, tenant check), publication claims with fencing tokens, leases, failure classes and retry schedule, publication stats, subject tokens, the inbox (one effect per consumer and event, backoff, retries, quarantine, content conflict, scope mismatch), replay with dry-run classification (an interrupted replay releases its range and a retry resumes through the inbox, skipping what completed), payload copies at every append, claim, read and delivery boundary, and message-ID suppression and redelivery in the broker.

Deliberately not modeled:

- **Retention and the deduplication horizon.** Nothing is pruned, so `ErrStaleAppend`, `ErrReplayBeyondHorizon`, `ErrReplayRangeExpired` and late-copy refusal never occur. `postgres.Store.Prune` has no memory twin.
- **Project ownership.** Append and delivery do not check that the project belongs to the tenant; memory holds no project table. The PostgreSQL foreign keys and RLS own that proof.
- **Locks and isolation.** Writers are serialized by `memdb`; there is no `SKIP LOCKED`, admission lock or lock order.
- **Savepoints.** A failing handler discards its `Update` and the failure is recorded in a second `Update`. Between the two another delivery can run; the second records nothing if the entry changed.
- **Restore fences, repair passes, quarantine discard, persisted replay state across restarts (an interrupted replay is resumed only by repeating the request) and the history tables** (`event_inbox_attempts`, `outbox_attempts`, audit rows).
- **Restart persistence.** Replay requests, subject tokens and messages vanish with the process. `TenantIDs` lists tenants that were added or appended to, including one whose appending update later rolled back.
- **Broker durability.** The in-process broker keeps accepted message IDs forever (no duplicate window), delivers only on `Pump` and has no `AckWait` clock.
