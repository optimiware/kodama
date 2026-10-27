# `internal/workspace` — Git workspaces and controlled publication

Task T-013. Commits are the durable checkpoints; there is no shared writable
filesystem. Implements the [workspace protocol](../../docs/specs/workspaces.md)
(WS-01–WS-12, WS-A01–WS-A08) with supporting WM-11, SD-01–SD-08 and IT-02–IT-05.

Status: implemented and tested against real PostgreSQL and real Git over HTTPS.
Nothing registers these routes or adapters yet; that is T-027's job
(see *Integration owner* below).

## Layout

| Path | Purpose |
| --- | --- |
| `workspace.go`, `publication.go`, `retention.go` | Entities: `Workspace`, `Publication` receipt, `RetainedRef`; explicit transitions, no I/O |
| `ports.go`, `errors.go` | Consumer-owned ports (store, authorization, peer modules, Git) and sentinel errors |
| `service.go`, `provision.go`, `sync.go`, `writer.go`, `publish.go`, `reconcile.go`, `archive.go`, `fence.go`, `retained_refs.go` | Use cases coordinating entities through ports |
| `postgres/` | PostgreSQL adapter, transaction-bound entry points; the workspace tables and their deferred cross-module constraints are in the baseline schema (`internal/storage/migrations/001_schema.sql`) |
| `git/` | Hardened Git adapter: `Reader` (read credential), `Publisher` (write credential), bundle verification, bounds |
| `http/` | The five human routes, error mapping, DTOs generated from `api/workspace.yaml` |
| `runtime/` | Translation of the frozen `runtimev1` source proposal; registers no route |
| `workspacetest/` | Shared test fixtures (real PostgreSQL + Git, explicit peer-port fakes, fault injection) |
| `../../api/workspace.yaml` | OpenAPI 3.0 fragment referencing `api/common.yaml` |
| `../../test/support/git/` | HTTPS `git-http-backend` fixture: distinct read/write tokens, hold/drop/delay faults |

`internal/workspace` itself imports only the standard library and
`internal/domain`: no pgx, HTTP or process dependency. Dependencies point
inward; adapters import the domain package, never the reverse.

## Model

States are exactly `provisioning`, `available`, `archived`. Errors are safe
reason codes on the resource (`last_error`), not states. A workspace, a writer
reservation, a publication and a retained reference have separate lifecycles.

* **Workspace** (`workspaces`): unique per work item and per repository branch;
  immutable ownership/work-item/repository identity; distinct checkout and
  publisher secret *references*; portable branch `refs/heads/kodama/work/<work-item-id>`;
  `initial_commit` (recorded before the branch is created), `accepted_commit`,
  integer `version` (the ETag), the single `writer_*` reservation, the
  `publish_*` pending intent, `archive_requested_at`, `last_error` and the
  restore-recovery fence (`fence_epoch`, `publication_fenced`).
* **Publication receipt** (`workspace_publications`): one immutable receipt per
  proposing attempt (commit, expected tip, bundle artifact + digest) with
  `pending → published | conflict`, the observed tip of a lost race, and the
  writer outcome (`unsettled | succeeded | failed | cancelled`). It outlives
  release of the writer slot. Publication of source and success of the attempt
  are independent: only a *published commit of a successful writer* can be
  selected downstream.
* **Retained reference** (`workspace_retained_refs`): `intent → confirmed →
  released → removing → removed`. The remote reference name is persisted, in
  the proposed ordinary-branch namespace `refs/heads/kodama/retained/<workspace>/<run>-<pin>`.

Database invariants (not only Go): all-or-nothing writer and pending fields, a
pending publication requires the same attempt to still hold the writer slot
(settlement before release), composite ownership foreign keys, forced RLS, no
`DELETE`, and column-scoped `UPDATE` grants that keep identity immutable.

## Publication protocol

1. `ReserveWriter` refreshes the remote tip (human synchronization), checks the
   attempt's *current* authority through the injected `Attempts` port and
   atomically reserves the single slot against the current version.
2. `ProposeSource` validates attempt authority, writer reservation, admission,
   artifact provenance (scope, producing attempt, role, completeness), then in
   bounded private scratch: transfer size, digest, bundle header (one ref at the
   proposed commit; prerequisites only the pinned input/expected tip), Git object
   integrity, descent from the input commit and the injected-secret scan of the
   *newly introduced* objects. Only then is the pending intent persisted.
3. The intent is driven by claiming it (durable driver claim), reading the
   remote, and — if the tip is unchanged — pushing with an explicit
   expected-old-ref lease (`--force-with-lease=<branch>:<expected>`), preceded by
   a separate fast-forward ancestry check. There is no unconditional force push.
4. Success is acknowledged only after the remote branch is read back as
   containing the commit **and** accepted commit, receipt, audit and
   `workspace.checkpoint_created` are durably recorded in one transaction.
5. A lost expected-tip race records a conflict receipt with the observed tip and
   preserves the proposal; the human branch is never overwritten.
6. Unknown outcomes (dropped acknowledgment, database failure after the push,
   client timeout) leave the intent pending with ownership retained; the
   reconciler resolves it from remote evidence. An unchanged tip is not proof of
   failure; the same CAS push may be repeated only after `RedriveAfter`. A moved
   ref settles it definitively (a delayed duplicate fails its CAS).
   Publication already contained in later history (`BranchContains`) is
   recorded as published.
7. The writer slot is released only by `SettleWriter` after the publication is
   settled. `RecoveryPoint` returns the last published commit and
   `UncommittedWorkMayBeLost`.

Only `git.Publisher` resolves publisher credentials; `git.Reader` and every
use case see only secret references.

## Archive, restore, fence, retention

* `Archive`: closes admission durably (version-checked), then waits up to
  `ArchiveWait` for settlement; completes only with no unsettled publication,
  otherwise returns `ErrArchiveUncertain` (HTTP 409 `archive_pending`, new ETag)
  and the reconciler finishes it. `Restore` reopens the *same* workspace and
  branch and never provisions a replacement.
* `FencePublishers` / `ResumePublishing`: installation-recovery hooks. A
  publisher whose claim predates the fence cannot finalize or start writes;
  resumption needs `PublisherRotation` evidence and the in-flight publication is
  then settled from remote evidence.
* `PinCommit`, `PinWriterOutput`, `ReleasePin`, `RequestRemoval`: a pin is
  usable only after its remote reference is confirmed. Removal is audited
  (including denied/deferred requests), needs an explicit `DeletionPolicy`
  grant, never deletes a live reference or while a publication is unsettled, and
  serializes with pinning of the same commit under the workspace row lock.

## Failure and recovery

`Reconcile` / `RunReconciler` repair, independent of NATS: stopped provisioning,
unknown publications, unsettled archives, unconfirmed or half-deleted retained
references. Every Git call happens outside a database transaction; every
database transaction is short and holds only the workspace row (and its
receipts/pins).

## Bounds (adapter-local, required, no environment variables)

`git.Options`: `CommandTimeout`, `MaxBundleBytes` (transfer, enforced while
copying, also against an understated declared size) and `MaxScratchBytes`
(enforced while Git runs by a size watcher, not only afterwards), `ScratchRoot`.
`workspace.Options`: `GitDeadline`, `ArchiveWait`, `ArchivePoll`,
`DriverStaleAfter`, `RedriveAfter`, `ReconcileBatch`. Zero values are refused.

## Integration owner (T-027, T-020, T-022, T-011, T-012, T-015, T-019)

Compose with:

```go
manifest, _ := storage.NewManifest(storage.BaseContribution())  // the workspace tables are in the baseline
store, _ := workspacepg.NewStore(pool, eventsStore, workspacepg.Options{})
reader, _ := gitad.NewReader(gitOptions, checkoutCredentials)     // resolves checkout references only
publisher, _ := gitad.NewPublisher(gitOptions, publisherCredentials) // resolves publisher references only
svc, _ := workspace.NewService(workspace.Dependencies{ /* store, authorizer, peers, reader, publisher */ }, options)
httpHandler, _ := wshttp.NewHandler(svc); httpHandler.Register(router)
source, _ := wsruntime.NewSource(svc) // call source.Propose(ctx, scope, attemptID, req) from the runtime /source route
go svc.RunReconciler(ctx, interval)
```

* Peer ports to implement: `Authorizer`, `WorkItems` (T-011), `Repositories`
  (project settings, T-019/T-012 references), `Attempts` (T-020), `Bundles`
  (T-015), `SecretNeedles` (T-012), `PublisherRotation` and `DeletionPolicy`
  (O-11 owners). A missing port fails closed at construction.
* T-020 dispatch calls `(*postgres.Store).UpdateInTx` with
  `workspace.ReserveWriterMutation` inside its attempt-creation transaction, and
  result finalization calls it with `workspace.SettleWriterMutation`. Lock
  order: eligible run/step, attempt insert, then the workspace row last.
  T-020's `writer_attempt_id` / `publish_artifact_id` / `work_item_id` foreign
  keys are part of the baseline schema, added after the work, execution and
  artifact tables they reference.
* Migration order: handled by the baseline; this module contributes no migration.
* No new event catalog entries, CLI commands, environment variables or runtime
  context fields.

## Tests

`go test -race -count=1 ./internal/workspace/... ./test/support/git/...` with
`KODAMA_TEST_POSTGRES_ADMIN_URL` set (and `KODAMA_REQUIRE_POSTGRES=1` to fail
rather than skip). Git, `git-http-backend` and TLS fixtures start locally per
test; names carry the `t013` prefix.

| Test | Requirements |
| --- | --- |
| `Test_human_push_race` | WS-04, WS-09, WS-10; WS-A07 |
| `Test_revoked_writer` | WS-03–WS-05, WS-11; WS-A02, WS-A03 |
| `Test_push_ack_loss` | WS-06, WS-08, WS-11; WS-A05, WS-A06 |
| `Test_retained_commit` | WS-02, WS-05, WS-11, WS-12; WS-A04, WS-A08 |
| `Test_archive_publish_race` | WS-07, WS-12; WS-A08 |
| `Test_duplicate_provisioning` | WS-01, WS-02; WS-A01 |
| `Test_writer_reservation_atomicity` | WS-03; WS-A02 |
| `Test_workspace_transitions` | entity transitions, SD-01–SD-08 |
| `Test_bundle_validation_and_bounds` | WS-05, WS-06, WS-09, WS-10 |
| `Test_tenant_isolation_and_atomic_events` | IT-02–IT-05 |
| `Test_retention_pin_delete_race`, `Test_publisher_restore_fence` | WS-11, WS-12 |
| `Test_workspace_http_contract`, `Test_source_runtime_compatibility`, `Test_migration_reapply` | WS-09; contract compatibility |
