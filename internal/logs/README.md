# Scoped log service and retention contract (T-023)

Makes attempt logs available **without Loki**: authorize one specific attempt,
build the Kubernetes query server-side, bound and redact the output, serve
retained log artifacts, and keep explicit completeness evidence. Specification:
[observability and audit](../../docs/operations/observability-and-audit.md#logs),
[step entrypoint](../../docs/implementation/step-entrypoint.md),
[HTTP contract](../../docs/contracts/http-api.md) (`GET P/runs/{id}/logs`).
Not in this task: entrypoint upload call sites (T-063), the assembled
wrapper/controller join and `missing_node_tail` (T-064), root wiring (T-071).
Mandatory Loki, guaranteed completeness after node loss and arbitrary
label-selector access are out of scope.

## Packages

| Package | Role | May import |
| --- | --- | --- |
| `logs` | `Record` capture entity, redaction, bounded streaming, `Service` (reads), `Controller` (fallback, observation, reconciliation), retention policy, consumer-owned ports | `domain`, `logs/contract` only |
| `logs/contract` | Frozen, versioned completeness sidecar and its codec | `domain` |
| `logs/adapters` | Identity authority, execution attempts/placement, artifact reader and controller writer, retention bridge | peers (`identity`, `execution`, `artifacts`) |
| `logs/postgres` | `Store`; `log_records` is in the baseline schema (`internal/storage/migrations/001_schema.sql`) | `pgx`, `storage` |
| `logs/kubernetes` | `Reader` over the Kubernetes API (injected authenticated `net/http` client) | `net/http` |
| `logs/http` | Protected route, NDJSON framing, generated DTOs from `api/logs.yaml` | `httpapi`, `identity` (permission names) |
| `logs/logstest`, `logstest/kubefake` | Test fixtures only | tests |

`TestModuleBoundaries` enforces these directions.

## Wiring (T-071 owns the composition)

```go
store, _ := logspg.NewStore(logspg.Config{DB: runtimePool})
reader, _ := logskube.NewReader(logskube.Config{Endpoint: apiURL, Client: authenticatedClient})
authority, _ := logsadapters.NewAuthority(identityAuthority, accessReader)
attempts, _ := logsadapters.NewAttempts(executionStore)
workloads, _ := logsadapters.NewWorkloads(logsadapters.StaticPlacement{Namespace: runNS, Container: "step"})
artReader, _ := logsadapters.NewArtifacts(artifactRepo, objectStore)
retained, _ := logsadapters.NewRetained(artReader, logsadapters.WriterConfig{
    Repository: artifactRepo, Objects: objectStore, Limits: artifacts.Limits{DiagnosticBytes: b, DiagnosticFiles: n}})

svc, _ := logs.NewService(logs.Config{Authority: authority, Attempts: attempts, Workloads: workloads,
    Live: reader, Retained: retained, Store: store, Redactions: injectedValues,
    Limits: logs.Limits{MaxLines: 5000, MaxBytes: 1 << 20, MaxDuration: 5 * time.Minute, MaxStreams: 32, WriteTimeout: 10 * time.Second}})
handler, _ := logshttp.NewHandler(svc, 10*time.Second)
_ = handler.RegisterRoutes(protectedRouter) // router prefix /api/v1

ctrl, _ := logs.NewController(logs.ControllerConfig{ /* same ports */ CaptureBytes: 8 << 20, CaptureTimeout: time.Minute, Lease: time.Minute})
// Called by T-064's reconciler, never started automatically:
//   ctrl.Collect(ctx, scope, attempt)   server-side fallback capture
//   ctrl.Observe(ctx, scope, attempt)   record the wrapper's verified upload
//   ctrl.Reconcile(ctx, scope, attempt) mark content that retention removed

retention, _ := logsadapters.NewRetention(0, protection) // artifacts.Retention; fails closed without protection
```

Numbers above are placeholders: the constructor requires positive ceilings for
lines, bytes, duration, concurrent streams and write inactivity (the default tail
is a conservative 100 lines); T-071 selects operational values. No environment
variable or project setting changes them.

**Migration order:** the log tables are part of the baseline schema
(`internal/storage/migrations/001_schema.sql`), created after the artifact,
execution and attempt tables they reference; `002_grants.sql` holds the
runtime grants. There is no log contribution to order and the central manifest is
not edited here. `TestMigrationReapply` proves re-applying the baseline is a no-op.

**Route and action mapping:** `GET /api/v1/tenants/{tenantId}/projects/{projectId}/runs/{id}/logs`,
action `logshttp.ActionLogs` (`run.logs`), resource kind `run`. Add
`logshttp.RoutePermissions()` to the identity authorizer rules (coarse gate:
`run:read`, project level). The module's authority adapter then requires **both**
`run:read` and `evidence:read` on every call (`adapters.DiagnosticPermissions`);
no role or permission is added.

## Behavior

**Reads.** `Service.Open` authorizes (current membership, every call, including
reconnects), resolves the attempt through execution's scoped lookups (the run
must own it; a run with several attempts needs an explicit `attemptId`), takes a
stream slot and opens the source *before* any output, so the transport can still
answer with the standard envelope. `Session.Run` streams `meta`, `chunk`s and a
`summary`; a missing summary means interruption. Live reads use the persisted Job
name and **observed Job UID** plus injected namespace/container; an unobserved Job
has no workload. The Kubernetes reader verifies the Job UID and the Pods'
controlling owner references and the container before reading, builds any Pod
selector itself from the Job's own labels, and rejects caller-supplied
namespace/Pod/container/selector/URL values (the HTTP layer rejects the
parameters before they get that far). Bounds: tail (default 100, rejected not
clamped above the ceiling), lines, bytes, line length (cut, flagged), scan bytes,
duration, concurrent streams, slow-client write timeout; the upstream is closed
on every exit path. A long follow re-asks the authority after sending a line once `ReauthInterval`
has elapsed, so one more line can be sent after access ended and an idle follow is
not rechecked until a line arrives; it is otherwise bounded by the duration
ceiling.

**Completeness is explicit** and separate from command outcome and from
truncation by query bounds (see [`contract`](contract/metadata.go)):

| Capture | Completeness |
| --- | --- |
| Wrapper log with a valid sidecar bound to its artifact ID/digest/size | as stated (`complete` only from the wrapper) |
| Wrapper log without a sidecar, or with an unbound/invalid one | `unknown` (`unverified`) |
| Controller capture | never `complete`; `unknown`, or `partial` when truncated/interrupted |
| Live read | `unknown` |

A retention removal changes availability (`RemovedAt`), never the recorded
completeness. A missing workload is explicit unavailable evidence
(`workload_missing`), not an empty successful log.

**Capture records** (`log_records`) hold ownership, capture generation, fenced
lease, artifact references and safe completeness metadata, never bytes, secret
values or URLs. Transitions (enforced by the entity and by the database trigger
`t023_log_records_guard`): `pending → collecting → retained | unavailable`;
`unavailable → collecting` only for retryable failures within 5 claims;
`pending|unavailable → retained` for wrapper observation; `retained` is immutable
except `removed_at`. A claim takeover bumps the fence and the old collector's
freeze/retain fail `ErrClaimLost`. A new generation (max 3) exists only after the
frozen bytes were lost.

**Replay and recovery.** Exact duplicate linkage succeeds without change; a
changed one is `ErrConflict`. The controller records a frozen snapshot (names,
digest, size, statement) before any upload; retries reserve/stage/seal the same
declaration, recover reserved/staged/committed artifacts by name, and treat a
lost response as unknown (re-read, never assumed). If the bytes existed only in
a lost spool, that generation is fenced (`frozen_bytes_lost`, earlier failure and
snapshot kept) and a bounded new generation recaptures; abandoned reservations
are reclaimed by the artifact reconciler. Wrapper and controller captures coexist
as separate records (`controller-g<N>.log` never collides with `command.log`);
automatic selection prefers a verified complete wrapper capture and never merges
sources.

**Lock order.** `log_records` row lock (the claim/linkage serializer) → artifact
rows read by the guard trigger (`FOR`-less reads; the log module never mutates
artifacts through SQL). The controller never holds a SQL transaction across the
Kubernetes read or an object transfer. The log module takes no execution locks.

**Controller storage is an internal system operation.** It uses the artifact
owner's exported `NewFile`, repository reservation/completion claims, recorded
object destinations and `CommitUsable` with a system audit actor
(`logs-controller`), restricted to `controller-g<N>.log` + sidecar names,
diagnostic role and fixed media types of an attempt the execution module
resolved. It is not runtime authority and does not call the runtime routes.

**Retention.** `adapters.Retention` implements `artifacts.Retention`: diagnostic
files (logs and sidecars) are eligible 30 days after verification (O-11), unless
`logs.Protection` reports an extending reference; any other role is not this
policy's to delete. Artifact pins still win (the reconciler serializes deletion
against them). A missing protection policy fails closed at construction and as a
zero value; an outage is reported and deletes nothing.

**Redaction.** Known injected values (from the `Redactions` port, which reads
values the attempt already holds — no new secret-read authority) are removed
across chunk boundaries before emission *and* before storage; overlapping values
merge into one marker, the set (256 values × 8 KiB) and the held overlap buffer
are bounded, and an unavailable set fails the read or capture closed. Limits:
matching is byte-exact (encoded or line-split variants are not recognized), a
prefix of a value ending a truncated stream is released, and arbitrary tenant
output still requires access control and retention.

## Fixtures for T-063 and T-064

[`testdata/fixtures.json`](testdata/fixtures.json) freezes the sidecar for a
verified wrapper log, a legacy log without metadata, truncated collection, failed
upload, controller capture, missing workload, duplicate linkage and foreign
references; `contract.Encode/Decode/Bind` and `Unverified` are the helpers.
Ordinary runtime artifact uploads are unchanged: the plain-text `command.log` /
`entrypoint.log` stay as they are and the sidecar is one more diagnostic file
(`command.log.meta.json`, media type `application/vnd.kodama.log-diagnostic.v1+json`).
T-063 uploads it after the log artifact is usable (it needs the artifact ID);
T-064 calls `Controller.Observe`/`Collect`/`Reconcile` and owns the real
wrapper/controller join. `Observe` pins the wrapper link at first observation, so
the sidecar must already be usable when T-064 observes: a capture observed before
its sidecar exists stays `unknown` and a later `complete` sidecar is a conflict.
This never overclaims, but T-063/T-064 must order upload before observation.

## Checks

```sh
go test -race -count=1 ./internal/logs/...
go vet ./internal/logs/...
go generate ./internal/logs/http/... && git diff --exit-code -- internal/logs/http/logs.gen.go
make test-task TASK=T-023
make check-task TASK=T-023
```

Real PostgreSQL and S3 come from the harness environment
(`KODAMA_TEST_POSTGRES_ADMIN_URL`, the self-provisioned `t023-s3-localstack`
fixture); tests skip, and so do not count as acceptance, when a service is
absent. The Kubernetes API server in the HTTP and fallback tests is the
recording fake `logstest/kubefake` (controlled faults); the real reader's wire
and ownership behavior against a real API server is checked by
`TestKubernetesOwnership` (`kubernetes/reader_integration_test.go`), which runs
only when `KODAMA_KUBECONFIG` names a cluster (for example the development kind
scope: `make dev-up DEV_SCOPE=t023`) and skips otherwise.

Resource scope: tenants/projects/users carry the execution fixtures' `t020_`
identifiers inside a per-test database named after the task prefix; S3 objects
live under `<KODAMA_S3_PREFIX>t023-<random>/`.

## Unit qualification (task T-081)

Requirements: **PX-05**, **PX-07**, **IT-03**, **AC-06**; SD-13/SD-A11.
`logstest.Memory` is the `development` adapter set of every log port over one
`memdb` store: `Store` (tenant- and project-scoped records, fenced claims,
bounded generations, one safe audit entry per change in the same transaction),
`Attempts`/`Workloads`, `Retained` (in-memory diagnostic artifacts and a staged
object map), a fake read `Authority` and a fake Kubernetes `Live` reader that
verifies the resolved target and applies the query's tail and byte bounds.
`logstest.Contract(t, open)` is the shared suite for `logs.Store` and
`logs.Retained`; the memory side runs it unconditionally
(`Test_unit_results_logs_contract`) and `postgres.TestPostgresContract` runs the
same suite on PostgreSQL and the artifact-backed adapter in the `test`
environment (skipped in the unit layer). A subject names the scopes its fixtures
seeded (`Subject.Scopes`); the suite also asserts that a record identifier names
one capture and never overwrites another's retained evidence or audit history.

* `Test_unit_log_scope` - tenant, project, run and attempt selectors, the
  authority asked on every open, server-built queries, tail/byte/line/stream
  bounds, retained tails, and live/retained/controller selection with removed
  content and redaction, all without a service.
* `Test_unit_log_fallback` - the controller's capture replays after a failed
  store, lost frozen bytes and a missing workload, with explicit evidence.
* `Test_unit_log_retention` - retention eligibility fails closed.

Not modelled: SQL, RLS, lock order, the linkage guard trigger, real object-store
semantics and restart persistence (SD-14).
