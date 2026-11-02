# `internal/workspace` — Git workspaces and controlled publication

Task T-013. Commits are the durable checkpoints; there is no shared writable
filesystem. Implements the [workspace protocol](../../docs/specs/workspaces.md)
(WS-01–WS-12, WS-A01–WS-A08) with supporting WM-11, SD-01–SD-08 and IT-02–IT-05.

Status: implemented and tested against real PostgreSQL and real Git over HTTPS.
Nothing registers these routes or adapters yet; that is T-027's job
(see *Integration owner* below). T-147 adds the [internally hosted model](#internally-hosted-repositories-task-t-147)
beside it; external repositories keep everything described up to that section.

## Layout

| Path | Purpose |
| --- | --- |
| `workspace.go`, `publication.go`, `retention.go` | Entities: `Workspace`, `Publication` receipt, `RetainedRef`; explicit transitions, no I/O |
| `ports.go`, `errors.go` | Consumer-owned ports (store, authorization, peer modules, Git) and sentinel errors |
| `service.go`, `provision.go`, `sync.go`, `writer.go`, `publish.go`, `reconcile.go`, `archive.go`, `fence.go`, `retained_refs.go` | Use cases coordinating entities through ports |
| `repositories.go`, `internal_*.go` | The internally hosted model (T-147): `RepositoryAssociation`, `InternalCheckpoint`, `ProvisionInternal`, `ReserveInternalWriter`, `HoldsWriter`, `AcceptCheckpoint`, internal synchronization and reconciliation, and their consumer-owned ports (`Hosts`, `InternalRepositories`, `PushRecords`, `WriterGate`, `PushInspector`) |
| `postgres/` | PostgreSQL adapter, transaction-bound entry points; the workspace tables and their deferred cross-module constraints are in the baseline schema (`internal/storage/migrations/001_schema.sql`); `migrations/010_workspace_repositories.sql` (exported by `Migrations()`) adds `workspaces.hosting`, `workspace_repositories` and `workspace_checkpoints` |
| `git/` | Hardened Git adapter: `Reader` (read credential; also the `PushInspector` of internal pushes), `Publisher` (write credential), bundle verification, bounds |
| `http/` | The five human routes, error mapping, DTOs generated from `api/workspace.yaml` |
| `runtime/` | Translation of the frozen `runtimev1` source proposal; registers no route |
| `workspacetest/` | Shared test fixtures (real PostgreSQL + Git, explicit peer-port fakes, fault injection) |
| `../../api/workspace.yaml` | OpenAPI 3.0 fragment referencing `api/common.yaml` |
| `../../test/support/git/` | HTTPS `git-http-backend` fixture: distinct read/write tokens, hold/drop/delay faults |

`internal/workspace` itself imports only the standard library, `internal/domain` and the
Git contract (`contracts/git/v1`, for the shapes of the ports below): no pgx, HTTP or process dependency. Dependencies point
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

### Cumulative writers and prior disclosures (task T-113)

In a `run_shared` run the writers of one work item publish one after another: each writer is
reserved on the workspace's accepted commit (the previous writer's published head), its
bundle's prerequisites are that head, and the publisher's expected-tip compare-and-swap, the
ancestry check and the human-push conflict behave as for the first writer
(`cumulative_test.go`). `WithPriorDisclosures` makes the proposal scan include the values the
run disclosed to its other steps and services (`RunDisclosures`; a failure refuses the proposal).
The publisher never reads a workspace's `.git`; the entrypoint's sealed Git protects the
step side (see the entrypoint README).

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
* Migration order: the baseline holds the original tables; `010_workspace_repositories.sql`
  (`workspace/postgres.Migrations()`) is registered by T-132 after `009` (see below).
* No new event catalog entries, CLI commands, environment variables or runtime
  context fields.

## Internally hosted repositories (task T-147)

[ADR 0011](../../docs/adr/0011-internal-tenant-git-service.md), WS-17 to WS-19, WS-A13 to WS-A15. A
workspace is either **external** (one repository, the publisher model above, unchanged) or
**internal** (`Workspace.Hosting`; immutable; the column `workspaces.hosting`, default `external`, so
every existing row stays external). One workspace per work item and one writer reservation per work
item hold for both.

* **Associations.** `RepositoryAssociation` (table `workspace_repositories`): for each repository the
  work item touches, the task branch `refs/heads/kodama/work/<work-item-id>`, the `BaseCommit` the
  branch is created at (the repository's default-branch tip, recorded before creation, first write
  wins), the `AcceptedCommit` last accepted there and a state `intent → created`. The workspace's own
  repository fields describe its **primary** repository (the first one it was provisioned for), and
  `Workspace.AcceptedCommit` mirrors that repository's accepted commit, so the one writer reservation
  keeps its shape; the secret references of an internal workspace are a checkout reference the
  composition's read-credential resolver understands and the placeholder `NoPublisherRef`, never a
  publisher secret.
* **Provisioning.** `ProvisionInternal(item, repositories)` resolves each repository through
  `InternalRepositories` (foreign or unknown: not found; external: `ErrWrongHosting`; not ready or empty:
  `ErrRepositoryNotReady`), commits the workspace and every intent, then creates each task branch with
  `Hosts.CreateBranch` under `BranchOperationID` (a hash of tenant, project, workspace and repository:
  stable across retries, restarts and concurrent callers), outside SQL. Any failure is reconciled by
  observing the ref (`Hosts.ReadRefValues`): at the base commit is created; elsewhere, the ref's journal
  (`Hosts.ReadJournal`) shows whether a committed entry of this operation put it there (ours, moved on by
  later work) or not (an independent branch: reason `branch_exists`, never overwritten or adopted).
  Repeating the call, concurrently or after a crash, creates nothing more; the reconciler
  (`ReconcileWorkspace`) finishes intents the same way.
* **Writer.** `ReserveInternalWriter` is the same single slot (`ErrWriterBusy`), pinned at the primary's
  accepted commit. Before assigning, the gate asks, for the task branch of **every** created repository,
  `gitpush`'s `UnknownBlocksWriter` (an unknown outcome: `ErrWriterBlocked`) and `RefPushes.UnsettledOnRef`
  (a push still authorized, committing or unknown: `ErrWriterBlocked` and `ErrPushPending`); a missing
  gate or push-record reader fails closed (`ErrNotConfigured`) and an outage of either refuses as
  `ErrUnavailable` (retryable, 503; never reported as a block). Gitpush asks `HoldsWriter` when it
  authorizes a push, so a push the previous writer authorized can still land after it was released: the
  unsettled check is what keeps a new writer off that tip. Each task branch is then synchronized (see
  **Synchronization**). The gate answers and the synchronization are observed outside SQL *after* the
  workspace version is read and applied together with the reservation in **one** mutation that requires that
  version unchanged: another reservation, release or synchronization in between makes the observation stale
  and it is repeated (bounded by `admissionRetries`), so a reservation never commits against a gate answer
  that predates another writer's unknown push. `ReserveInternalWriterMutation(in, observed, now)` is the
  pure mutation a caller joins to its own transaction (`UpdateInTx`); it requires the version
  `CheckWriterGateAt` returned (zero is refused) and cannot itself run the gate or the synchronization.
  `HoldsWriter(scope, workItemID, attemptID)` is the `gitpush.Reservations` adapter: a plain committed read,
  called by gitpush outside its own transaction, so it needs no transaction (gitpush must also call it again
  after the `authorized` record is durably committed, see "What gitpush must provide"); it is true only for the holder
  of an internal workspace whose admission is open (available, no archive requested, not fenced).
* **Synchronization.** A task branch that moved beyond the accepted commit is adopted as human work (a
  `workspace.checkpoint_created` event marked human) only when durable push evidence explains it:
  `RefPushes.UnsettledOnRef` is false for the ref and the chain of `RefPushes.AppliedOnRef` entries from the
  observed tip back to the accepted commit consists only of pushes by an engineer (a user actor) or by a
  workload whose commit was accepted as a checkpoint. Anything else is never adopted: a workload push that
  was refused (a secret in its objects) or never reported, a human push stacked on top of one, a movement
  no applied record explains (`ErrCheckpointRequired`, reason `unaccepted_push` on the association; an
  engineer resets the branch to clear it) and a push still in flight (`ErrPushPending`, nothing recorded).
  A rewritten or missing branch is `ErrDiverged`. A refused or unaccepted push therefore never becomes
  "contained history" that later checkpoints skip scanning. `Sync` and admission observe outside SQL and
  apply every association in one mutation that checks `If-Match`, that the workspace is available and that
  each accepted commit still equals the one observed (`ErrStale` repeats the observation).
* **Archive.** An internal archive completes only once no push of any created task branch is unsettled
  (`RefPushes.UnsettledOnRef`), checked after the workspace version and completed only at that version;
  otherwise the request stays pending (`ErrArchiveUncertain`) and the reconciler finishes it later. From the
  moment archive is requested `HoldsWriter` is false for every attempt.
* **Checkpoints.** `AcceptCheckpoint(attempt, repository, ref, commit, pushId)` accepts a commit output
  only from records: the attempt holds the reservation and declared write access; the repository is
  associated and created and the ref is its task branch; the push record reloaded through `PushRecords`
  is applied, its entry applied and neither delete nor rewrite, its new value is the commit
  (`gitv1.PinnedCommit`), it belongs to the repository and to this attempt (a record still authorized,
  committing or unknown is `ErrPushPending`, a refused one `ErrPushNotApplied`). Then `PushInspector`
  reads the commit server-side with a read-only credential (`git.Reader.Inspect`: the task branch is
  fetched into bounded scratch, the commit is related to the accepted one, and everything reachable from
  the commit and not from the accepted commit is scanned) for the attempt's injected values and, through
  `WithPriorDisclosures`, the values the run disclosed to its other steps: `ErrSecretInSource`, audit
  `workspace.checkpoint_refused`, nothing accepted. A fast-forward moves the association's accepted
  commit (and the workspace's, for the primary) and emits `workspace.checkpoint_created` in one
  transaction with the checkpoint record (`workspace_checkpoints`, keyed by repository, push and ref, so
  a replay returns the same checkpoint and repeats nothing); an earlier push of the same history arriving
  late is recorded without moving the accepted commit back.
  The execution module accepts a writer's checkpoints before it settles the writer
  (`SettleWriter` releases the reservation, after which `AcceptCheckpoint` refuses with `ErrNotWriter`),
  and settles an internal writer without a `SourceCommit` (each commit was verified when accepted).
* **Retained pins.** In that transaction the checkpoint records the retained ref
  `refs/kodama/runs/<run>/<n>` (numbered per run and repository); afterwards `Hosts.SetRetainedRef`
  creates it under a stable operation. `retained_ref_exists` for another commit (another module pinned the
  run) moves the checkpoint to the next number; an unknown outcome is settled by reading the ref; a pin
  that could not be confirmed leaves the checkpoint accepted, the call `ErrUnavailable` and the workspace
  reconcilable. The external `workspace_retained_refs` namespace is not used.
* **Unchanged for external hosting.** The publisher path, the writer reservation with its expected tip,
  bundle proposals, retained branch pins and the single recovery point refuse an internal workspace
  (`ErrWrongHosting`), and every internal operation refuses an external one; nothing in an external
  workspace's flow calls `Hosts`, the gate or the inspector (`Test_unit_workspace_external_publisher_unchanged`).
* **Ports.** All six are optional in `Dependencies`; a use case that needs a missing one fails closed
  with `ErrNotConfigured`. `repository.Hosts` satisfies `Hosts`, `*gitpush.Service` satisfies
  `PushRecords` (`Reload`), `WriterGate` and `gitpush.Reservations` is satisfied by `*workspace.Service`
  (compile-time proofs in the tests). `RefPushes` is satisfied by T-138's gitpush additions (below); the
  tests use the peer fake `workspacetest.FakePushes`, shaped like gitpush's records.
* **What gitpush must provide (`RefPushes`, T-138).** On `*gitpush.Service`, with the scope-checked,
  tenant-isolated semantics of `UnknownBlocksWriter` and a contract `not_found` for an unknown repository:
  `UnsettledOnRef(ctx context.Context, scope domain.Scope, repositoryID, ref string) (bool, error)` is
  true when any push record of the repository has an entry for `ref` that is `authorized`, `committing` or
  `unknown`; `AppliedOnRef(ctx context.Context, scope domain.Scope, repositoryID, ref string, limit int)
  ([]gitv1.PushRecord, error)` returns up to `limit` whole records with an `applied` entry for `ref`, newest
  first by the order their entries applied, each with `Actor` (`user` or `workload`), `PushID` and the entry's
  `Old`/`New`/`Kind`. Also required of gitpush (the workspace can only see records that already exist):
  after the `authorized` record is **durably committed** and before it answers the worker, gitpush must call
  `HoldsWriter` again and, when it is false, refuse the push and settle the record so nothing applies. A
  check before the transaction or inside it is not sufficient: the writer can be released or an archive
  requested after the question, and the uncommitted record is invisible to `UnsettledOnRef`. With the
  recheck either `UnsettledOnRef` sees the record or the recheck sees the closed or released admission. A
  recheck in `CommitIntent` is an extra guard, not the only one.
* **Fail-closed dead ends.** These are deliberate; each needs an engineer or an operator, never a workaround:
  a task branch more than 256 applied records (`maxMovementRecords`) ahead of the accepted commit is never
  adopted; only an exact reset of the branch to the accepted commit clears `unaccepted_push`, and a
  force-push over an unaccepted workload push is still walked through the old tip (its record's `Old`) and
  refused; a crashed or cancelled attempt's applied push that was never reported through `AcceptCheckpoint`
  blocks later admission (`ErrCheckpointRequired`) until an engineer resets the branch; an unknown actor
  type, a recreated branch (all-zero `Old`) and an `Old` no record explains are refused. While the attempt
  that made the push still holds the writer slot the same push is `ErrPushPending` (its output may still be
  reported), nothing is recorded, and `Sync` answers 409 `workspace_state_conflict`.
* **Archive and checkpoints.** While an archive is pending, `AcceptCheckpoint` still accepts the commits of
  the attempt that holds the reserved slot; every other attempt, and everyone once the archive completed, gets
  `ErrAdmissionClosed`. The grace is best effort: the archive waits only while a push is unsettled, so a push
  that applies and is not accepted before the next reconciler pass can still be archived without its
  checkpoint. If the writer's slot is still held, Restore reopens admission and the acceptance works again;
  once the writer was settled the push is `ErrCheckpointRequired` and needs an engineer reset.
* **Not here.** Execution admission and source sets (T-148, which calls `ProvisionInternal`,
  `CheckWriterGate`/`ReserveInternalWriter` and `AcceptCheckpoint` and writes the entrypoint's `commits`
  outputs), attempt grants and cancellation (T-150), the composition of the ports, the migration
  registration and the routes (T-132), operator resolution of unknown entries (T-138).

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
| `Test_unit_workspace_repository_associations` | WS-17; WS-A13 |
| `Test_unit_workspace_task_branch_provisioned_idempotent` | WS-17; WS-A13 |
| `Test_unit_workspace_single_writer_per_item` | WS-03, WS-18; WS-A02, WS-A14 |
| `Test_unit_workspace_internal_checkpoint_requires_applied_push` | WS-19; WS-A15 |
| `Test_unit_workspace_unknown_push_blocks_reassignment` | WS-11, WS-18, GR-09; WS-A15 |
| `Test_unit_workspace_retained_pins_internal` | WS-02, WS-19, GR-13 |
| `Test_unit_workspace_prior_disclosure_scan_on_push` | WS-16, WS-19 |
| `Test_unit_workspace_external_publisher_unchanged` | WS-19; WS-A15 |
| `TestInspectRelatesAndScansPushedCommits` (real Git over HTTPS), `Test_workspace_http_repositories`, `Test_migration_workspace_repositories` (PostgreSQL), the internal-hosting case of the shared `StoreContract` (memory; PostgreSQL) | the adapters of the above |

The internal tests run on the memory store with explicit peer fakes (`workspacetest.NewInternalEnv`: the
worker, repository module, push records, writer gate, inspector and run disclosures) and need no database.
Real SQL: `make check-go-integration DEV_SCOPE=t147 DEV_STACK=services PKGS='./internal/workspace/...'`.
