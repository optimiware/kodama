# Git service contract v1

The executable wire contract of the internal Git service ([ADR 0011](../../docs/adr/0011-internal-tenant-git-service.md)), delivered by T-126 for the [Git service contract](../../docs/contracts/git-service.md). The behavior and acceptance scenarios are in [Git repositories](../../docs/specs/git-repositories.md) (GR); this directory owns the exact shapes and the consumer-owned port signatures that T-127–T-157 implement in parallel. **Nothing here is served**: no route, hook, signer, store or worker exists because this package does; a fixture proves a shape, never behavior.

| Path | Contents |
| --- | --- |
| [`v1/schema.json`](v1/schema.json) | JSON Schema 2020-12: one definition per message in `$defs` |
| `v1/*.go` | `gitv1` package: DTOs, strict/tolerant decoding (`DecodeRequest`, `DecodeResponse`), validation, error and reason codes with `Status`, the route table `Operations`, ref and branch helpers, `Settle`, `NewSourceSet`/`Digest`, grant, assertion and identity verification helpers |
| [`v1/fixtures/<area>.json`](v1/fixtures/) | Generated payload cases per area (`repository`, `grants`, `seed`, `assertion`, `worker_rpc`, `push_authorization`, `commit_intent`, `ref_journal`, `source_set`, `trigger_config`), with the schema's verdict next to the expected Go verdict |
| [`v1/fixtures/settlement.json`](v1/fixtures/settlement.json) | Hand-written settlement decision table: entry, evidence, expected resolution and reason |
| [`../events/v1`](../events/v1/) | The additive `repository.created`, `repository.state_changed` and `git.refs_updated` event types |
| [`../runtime/v1`](../runtime/v1/) | Reused decoder, `Timestamp` and `ContractError` (no capability package is imported) |

## Rules consumers rely on

- **Strict requests, tolerant responses.** `DecodeRequest` rejects unknown, misspelled, differently cased, duplicate and omitted members (the runtime v1 decoder), so a nullable member is an explicit `null`. `DecodeResponse` tolerates additional members at any depth but still requires the contracted ones. Claim sets, grants, assertions, journal entries, source sets and trigger configuration are decoded strictly even when read from storage. Unknown enum values are not success.
- **Optional members are omitted, never null.** The only optional members are `defaultBranch` and `policy` (and `policy.directPushToDefault`) of a create body, `defaultBranch` and `policy` of a patch body, and `excludeOriginRuns` of a trigger. An omitted `directPushToDefault` is `false` (`EffectivePolicy`, O-33); an omitted `excludeOriginRuns` is `true` (`TriggerConfig.Excludes`); an omitted create `defaultBranch` is `main` (`EffectiveDefaultBranch`). A patch that names a policy states the flag.
- **Integers only,** within ±(2^53−1); journal sequences and versions start at 1; objects are lowercase hexadecimal Git object IDs (SHA-1 or SHA-256), the all-zero ID meaning "no ref" (create `old`, delete `new`, absent `observed`). Old and new of one update differ and have one length.
- **IDs are opaque** (`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`). Ownership comes from scope comparison (`GrantClaims.CheckScope`), never from an ID's spelling. A foreign or unknown repository, project or tenant is the same `not_found`; a storage path is never derived from anything but a server-assigned ID.
- **Ref syntax is a conservative subset of git-check-ref-format** (`ValidateRefName`): `refs/` and at least two components that start with a letter, digit or underscore and continue with letters, digits and `._-`; no `..`, no component ending in `.lock` or `.`; at most 255 bytes. Anything else is refused explicitly, never rewritten. A **ref pattern** (grants, triggers) is a full ref name under `refs/heads/` or `refs/tags/` with an optional trailing `/*` after at least one component; `refs/heads/*`, an inner wildcard and anything under `refs/kodama/` are not patterns (`ValidateRefPattern`, `MatchRefPattern`: the wildcard matches one or more further characters, `/` included). A **default branch** is a branch name, not a ref path, outside the reserved `kodama` namespace (`ValidateBranchName`).
- **Branch namespace** (`TaskBranch`, `AgentBranch`, `RunBranch`, `RetainedRef`, `ClassifyRef`): task `refs/heads/kodama/work/<workItemId>`; agent `refs/heads/kodama/agents/<workItemId>/<runId>/<label>`; run without a work item `refs/heads/kodama/runs/<runId>/<label>`; retained `refs/kodama/runs/<runId>/<n>` (`n` ≥ 1, no leading zero). A label is one component (`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`). `ClassifyRef` returns `default` (wins over every other class), `task`, `agent`, `run`, `other_branch` (including a malformed name under `refs/heads/kodama/`), `tag`, `server` (`refs/kodama/…`) or `other` (`refs/notes/…` and the like).
- **Audiences are disjoint:** grants `kodama-git`, forwarding assertions `kodama-git-worker:<tenantId>`, worker identities `kodama-git-control`. A claim set of one audience never validates as another (`CheckForwarding`, `CheckWorkerCall` and `GrantClaims.Validate` each require their own).
- **Lifetimes (engineering bounds where the specification says only "short-lived"):** forwarding assertion ≤ 5 minutes, worker identity ≤ 1 hour, seed grant ≤ 15 minutes, human grant ≤ 1 hour, other workload grants ≤ 24 hours; `ClockSkew` (30 s) tolerance on `issuedAt`.
- **Bounds:** ≤ 1024 ref updates per receive and per push record (`MaxPushUpdates`, the contract ceiling, GR-16). An installation may configure a lower bound; a receive above it is refused explicitly with `too_many_updates` (422, `CheckUpdateBound`), and a larger push is sent in batches of refs (several receives). `git.refs_updated` carries the applied updates inline (up to this bound); a very large push's payload exceeds the generic envelope's data bound and is carried outside an envelope (see Events). ≤ 64 refs per exact-ref read (a reconciler batches larger records), ≤ 16 push options of ≤ 1024 bytes, ≤ 32 sources, ≤ 64 repositories and ≤ 32 patterns per grant, pages ≤ 1000.

## Decisions frozen here

- **Grants.** `GrantClaims` adds `kind` (`human`, `seed`, `preparation`, `attempt`, `integration`), `issuedAt` and `actor.attemptId` to the proposal. **Every** grant names its tenant, project and repositories explicitly (GR-06), with `read: true`, and `CheckScope` enforces project and repository for every kind. A **human** grant names exactly the one project and repository the credential was requested for (the credential helper knows them from the URL path); its recorded `update` is the *maximum class set* `HumanUpdateCeiling()` (`["refs/heads/*", "refs/tags/*"]`, the only place a bare wildcard is accepted) or `[]` for a read-only person, and it is never a frozen permission: the person's current project roles and the repository's ref policy restrict every update at every use (`PermitsUpdate` matches the ceiling to any ordinary branch or tag and never `refs/kodama`). A **seed** grant (user or a provisioning run's attempt) names exactly one repository with `seed: true` and `update` exactly `["refs/heads/<defaultBranch>"]` (`CheckSeed` compares with the repository's branch). A **preparation** grant belongs to a workload without attempt and updates nothing. An **attempt** grant belongs to one attempt (`actor.attemptId`). An **integration** grant names exactly one repository whose `integration` constrains one update (`ref` from `from` to `to`, `gateId`) and whose `update` is exactly `[integration.ref]`; `PermitsUpdate` is exact for it. `PermitsUpdate` decides scope only; ref policy and current authority are the push authorizer's.
- **Seed admission.** `CheckSeedEligible` refuses, in order, `hosting_mismatch` (422), `repository_archived`, `repository_not_ready`, `repository_not_empty` (the worker-reported `empty` is false) and `seed_pending` with 409. `seed_pending` means the repository has **any** unsettled push record, of any kind (git-repositories.md, "Bootstrap by push"): `UnsettledPush(records)` is true when a record's derived status is `authorized` (a receive in flight, even one with refused entries beside an open one) or `unknown`; applied and refused records do not count. The engineer role is a separate `403`. A consumed seed grant is refused in push authorization as `seed_consumed`.
- **Credential exchange.** `POST /tenants/{tenantId}/git-credentials` carries `{"projectId": …, "repositoryId": …, "seed": false|true}`: a credential never spans repositories. The `kodama git credential` helper derives both IDs from the clone URL path (`/git/<tenant-key>/<project-key>/<repository-name>.git`), which is why `git config credential.useHttpPath=true` is required (the credential helper otherwise receives only the host). `seed: false` yields a human grant, `true` the single-use seed grant of an empty repository. The answer is `{username, password, grantId, expiresAt}`. `password` is the opaque secret, shown once and never stored in clear (only a hash); `username` is conventional and any username is accepted at use.
- **Forwarding assertion** (`ForwardingAssertion`): audience, tenant, repository, grant, `receivePack`, `issuedAt`, `expiresAt`, `requestId`. It never carries the caller's credential; the worker derives the path from `repositoryId`. `CheckService` permits `git-upload-pack` always and `git-receive-pack` only with `receivePack`.
- **Push authorization.** The worker sends `PushAuthorizeRequest` with one `gitFacts` entry per update, in order, so the control plane never calls Git while holding SQL locks: `fastForward` is `null` exactly for creates and deletes, and `repositoryEmpty` (same in every fact) requires every update to be a create. The answer holds one verdict per update, in order (`CheckAuthorizeResponse`); a `deny` carries one of the eight push refusal reasons (`ref_protected`, `not_fast_forward`, `grant_scope`, `attempt_inactive`, `repository_archived`, `seed_only`, `seed_consumed`, `integration_mismatch`) and one deny fails the whole receive. A request for a ref under `refs/kodama/` or any ref outside the grant is a verdict, not a malformed request.
- **Commit intent.** `200 {"committing": true}` or `409` with code `push_settled`, `attempt_inactive` or `receive_expired`; the hook then exits non-zero and Git aborts the transaction. These are error codes; `attempt_inactive` is also a deny reason.
- **Record status** (`DeriveStatus`) is derived, never chosen: `unknown` while any entry is `unknown` (an unresolved entry blocks the record, so its one event is emitted exactly once, when the record settles); otherwise `authorized` while any entry is `authorized` or `committing`; otherwise `applied` if any entry applied, else `refused`. A refused entry carries a push refusal reason or a settlement refusal reason (`no_commit_intent`, `receive_aborted`, `ref_unchanged`), an unknown entry one of `journal_unreadable`, `journal_regressed`, `evidence_missing`, `conflicting_evidence`, `ref_diverged`, `operator_required`; other states carry none. `originRunId` is the actor's run for a workload and `null` for a person.
- **Entry kind.** Every push entry carries a persisted `kind` (`create`, `fast_forward`, `rewrite`, `delete`), set at authorization from the worker's git facts (`NewPushEntries`, `KindOf`): `create` exactly when `old` is all-zero, `delete` exactly when `new` is all-zero, and `fast_forward` or `rewrite` (the worker's `fastForward` fact) only when both are non-zero. It never changes afterwards, so a consumer that reads the record after the event keeps each ref's classification. The event's `updates` are derived from the applied entries and their persisted kinds (`eventsv1.NewGitRefsUpdated`), and the tests check that they agree with the record.
- **Ref journal entry.** Exactly one of `receiveId` and `operationId`; `observed` is required for `prepared` (the value under the lock; all-zero when absent) and `null` for `committed` and `aborted`.
- **Settlement** (`Settle`, tested against `fixtures/settlement.json`): `authorized` → refused; `committing` with the receive running → wait; with the receive ended or the worker verified stopped: own `committed` → applied, own `aborted` → refused, only own `prepared` → the next *prepared* entry's `observed` for that ref by sequence, else the current value read under the worker's lock with no receive in progress (`quiescent`): equal to `new` → applied, equal to the value observed before the transaction → refused, otherwise unknown. Outcome entries never order other transactions; attribution is by `receiveId`, so identical tuples of two receives are not confused. These guards run **before** any outcome entry or value is accepted: an unreadable journal, a missing, duplicated, contradictory or inconsistent entry (an outcome without its prepared entry or before it, an `observed` that contradicts `old`, one that equals `new`), and a journal whose head is below one the control plane already saw (`KnownHead`, a restored older volume, GR-A13) are `unknown`, so even an own `committed` or `aborted` entry of an older journal settles nothing; weak evidence is never refused. For a forced or server-side update with an all-zero `old`, the `observed` value is the before-value.
- **Control-plane → worker** (`WorkerBasePath`): initialize, list refs, read exact ref values (`quiescent`, `journalHead`), read journal (by `receiveId` or by `ref`, since a sequence, with `headSequence`), receive liveness (`running`, `workerInstance`), set/delete a retained ref, **create** and prune a managed branch (task, agent or run branch only, never the default branch, a tag or a server ref), run maintenance. Every call is idempotent on the repository ID (and `operationId` for mutations). Server-side ref changes (retained refs, branch creation and pruning) are journaled with the `operationId` in place of a `receiveId` (`prepared` with `observed`, then `committed` or `aborted`).
- **Source set** (`SourceSet`): ordered strictly by path; paths relative to `/workspace`, clean, made of components that start with a letter, digit or underscore (so never `.git` or a hidden name), without `..`, unique, and none a directory prefix of another; `.` only as the sole entry; commits non-zero; `write` requires a branch (`refs/heads/…`), `read` has `branch: null`. `Digest()` is the SHA-256 of the canonical JSON, so evidence and gate subjects identify a multi-repository run. A push-originated run pins the exact commit `PinnedCommit(record, ref)` returns: the `new` of that entry of an **applied** push record (the record applied, the entry applied and not a delete), never a branch tip.
- **Trigger configuration** (`TriggerConfig`): one repository, 1–32 ref patterns, origin filter (default exclude), the definition, `enabled`.
- **File read (T-168).** `ReadFiles` (`POST /repositories/{id}/files`, `contracts/git/v1/objects.go`) reads pipeline definitions at one exact commit so the control plane indexes `.kodama/<name>.yaml` without mounting the volume. A path is a `.yaml` or `.yml` file under `.kodama/` (`DefinitionPath`) or the directory `.kodama/` for its direct files; at most 64 files, 256 KiB each, 1 MiB together, refused whole (`invalid_request`) beyond that; a missing file is absent and a missing commit is `commit_not_found`. The response carries each file's SHA-256, which the contract checks.
- **Object operations (T-160).** `IsAncestor`, `HasCommit`, `CommitTime`, `CreateBundle`, `ReplayBundle` and `SetRestoreFence` (`contracts/git/v1/objects.go`) let the control plane prune, back up and restore without mounting the volume. Codes `commit_not_found` 404, `repository_empty` 409, `bundle_too_large` 422 and `restore_fence_required` 409. Bundles are framed as one JSON line then the bundle bytes (`BundleMediaType`); the manifest carries refs, the journal head and `consistent` (concurrent mutation only). While the restore fence is raised the worker refuses receives, server ref operations and maintenance with 503 and admits only replay and initialization; the fence is durable across restarts. `ReplayBundle` creates only missing refs and never advances, rewinds or deletes one; conflicts are reported with `repository_ahead`, `bundle_ahead` or `diverged`. Classification: `409 restore_fence_required` is Refused; 503 and 429 are Unavailable; a transport failure, including an aborted replay, is Unknown and the identical request is repeated.
- **Events** (`contracts/events/v1`): `repository.created` and `repository.state_changed` use resource type `repository` with the repository ID; `git.refs_updated` uses resource type `git_push` with the `receiveId` as its resource ID (the push is its receive); project required. `repository.created` is committed when the repository becomes ready (after the worker confirmed initialization), not at reservation. `git.refs_updated` carries the applied updates **inline**: `{receiveId, repositoryId, actor, originRunId, updates: [{ref, old, new, kind}], journalPosition?}` (1 to 1024 updates, never a server ref), so a consumer needs no reload to match a ref, pin a commit or exclude a run's own push; the push record (a dormant store) adds what the event omits (refused and unknown entries, the grant). `originRunId` is an explicit `null` for a person, so a trigger can exclude a run's own push from the event alone; the envelope `resource.id` equals `receiveId`. `journalPosition` is the receive's position in the worker's journal (`OutcomeRequest.journalPosition`): the order in which pushes were applied, which the order of reports and event commits is not.

## Kernel additions (T-167, O-36)

The kernel hosting ([Git repositories](../../docs/specs/git-repositories.md), [Git service contract](../../docs/contracts/git-service.md)) uses these shapes with the additions below. Every addition is optional on the wire and omitted, never null, so earlier consumers keep working; the dormant packages that predate ADR 0013 are unchanged.

- **`run_inactive`** is a push refusal reason and a 409 error code (commit intent): the credential's run is no longer queued or running, or its credential was revoked. `attempt_inactive` remains for the earlier design.
- **Receive IDs carry their grant**: `NewReceiveID(grantId)` returns `rcv_<32 hex>_<grantId>` and `GrantOfReceive` reads it back. The worker's journal keeps the receive ID of every entry, so a receive whose outcome report was lost is attributed to its credential, repository, actor and run from the journal alone. The grant ID is a claim of the worker that the control plane verifies against the credential store; the push ID of an authorization is the receive ID.
- **`OutcomeRequest.journalPosition`** (optional, at least 1; T-168): the journal sequence of the receive's own first entry. A receive appends its prepared entries while it holds the locks of every ref it updates, so two receives that update a common ref are ordered by this position exactly as their updates were applied. It rides on `git.refs_updated` and orders default-branch pushes for pipeline indexing: neither the time a report arrives (a lost report is recovered after newer pushes) nor the acknowledgement below says which push is newer. A re-report of a receive carries the same value; the worker omits it only when it cannot read its journal.
- **`OutcomeRequest.journalSequence`** (optional, at least 1): the highest journal sequence at or below which every receive of the repository has been reported. The control plane stores it as `repositories.journal_ack` and never lowers it. A worker sends it only when no other receive in the window after the acknowledged sequence is running or unreported, so an acknowledgement can never skip a receive that still needs a report.
- **`Assignment.journalAck`** (optional, at least 0): on a `reconcile` assignment the acknowledged sequence; the worker re-reports every receive of its journal after it that was not reported, in sequence order, and acknowledges up to the journal head only when none is still running or failed. The control plane deduplicates by receive ID (one `git.refs_updated` row per `(repository, receive)`).
- The kernel's public repository resource, credential exchange, credential claims and event payload are defined in the [Git service contract](../../docs/contracts/git-service.md) and implemented by `internal/kernel/api`; `GrantClaims`, `Repository` and the push-record types in this package keep their earlier shapes for the dormant packages and are not used by the kernel.

## Consumer-owned ports

Each consumer owns its small interface in its own package and takes the DTOs above. These are signatures and failure semantics, not code of this package; the implementing task creates the package. `ctx` cancellation is a transport failure, never an outcome. `*gitv1.ContractError` carries the stable code and maps through `gitv1.Status`.

### `repository.Hosts` (S2; consumer T-127, adapters T-128/T-129)

```go
package repository

// Hosts is the control plane's view of one tenant's Git worker. The tenant
// selects the worker (the adapter resolves it with the discovery port below);
// no method can address another tenant.
//
// Every method is idempotent on the repository ID (and OperationID where the
// request has one) and safe to retry. A call that fails after the request may
// have been sent has Outcome Unknown: the caller retries the same request, and
// never assumes it was applied or not.
type Hosts interface {
	// Initialize creates the bare repository at the worker's ID-derived path with
	// the server-owned configuration. Created is false when it already existed.
	Initialize(ctx context.Context, tenant domain.TenantID, req gitv1.InitializeRepositoryRequest) (gitv1.InitializeRepositoryResponse, error)
	// ReadRefs lists one namespace page; Empty is true when the repository has no refs.
	ReadRefs(ctx context.Context, tenant domain.TenantID, req gitv1.ReadRefsRequest) (gitv1.RefsResponse, error)
	// ReadRefValues reads exact refs under the worker's mutation lock. Quiescent
	// is true only if no receive or server operation was in progress.
	ReadRefValues(ctx context.Context, tenant domain.TenantID, req gitv1.ReadRefValuesRequest) (gitv1.ReadRefValuesResponse, error)
	// ReadJournal returns entries after SinceSequence for one receive or one ref.
	// HeadSequence below a head the caller already saw means an older volume.
	ReadJournal(ctx context.Context, tenant domain.TenantID, req gitv1.ReadJournalRequest) (gitv1.ReadJournalResponse, error)
	// ReceiveLiveness reports whether the worker still runs the receive and which
	// instance answered.
	ReceiveLiveness(ctx context.Context, tenant domain.TenantID, repositoryID, receiveID string) (gitv1.ReceiveLivenessResponse, error)
	// SetRetainedRef pins a commit at refs/kodama/runs/<runId>/<n>; a different
	// existing value is retained_ref_exists.
	SetRetainedRef(ctx context.Context, tenant domain.TenantID, req gitv1.SetRetainedRefRequest) (gitv1.SetRetainedRefResponse, error)
	// DeleteRetainedRef releases a pin whose value is ExpectedCommit; another value is ref_changed.
	DeleteRetainedRef(ctx context.Context, tenant domain.TenantID, req gitv1.DeleteRetainedRefRequest) (gitv1.DeleteRetainedRefResponse, error)
	// CreateBranch creates a Kodama-managed branch (refs/heads/kodama/work/<workItemId>,
	// an agent branch or a run branch; never the default branch, a tag or a
	// server ref) at Commit, for the workspace's task branch (WS-17, T-147). The
	// worker journals it under OperationID. Idempotent on the ref: already at
	// Commit is success with Created false; at another commit it is
	// branch_exists (409) and nothing changes. After an Unknown outcome the
	// caller repeats the identical request: the worker first observes the ref
	// (and the journal for OperationID) and converges: the ref at Commit is
	// success whether or not the earlier attempt finished its journal entry.
	CreateBranch(ctx context.Context, tenant domain.TenantID, req gitv1.CreateBranchRequest) (gitv1.CreateBranchResponse, error)
	// DeleteBranch prunes a task, agent or run branch whose value is ExpectedCommit.
	DeleteBranch(ctx context.Context, tenant domain.TenantID, req gitv1.DeleteBranchRequest) (gitv1.DeleteBranchResponse, error)
	// Maintain runs git maintenance, serialized with pushes.
	Maintain(ctx context.Context, tenant domain.TenantID, req gitv1.MaintainRequest) (gitv1.MaintainResponse, error)
}

// Outcome classifies a failed Hosts call.
type Outcome string

const (
	// Refused: the worker answered with a contract error (Err); nothing changed.
	// Do not retry the same request unchanged.
	Refused Outcome = "refused"
	// Unavailable: the request provably was not delivered (no worker, connection
	// refused, not ready). Retry with backoff.
	Unavailable Outcome = "unavailable"
	// Unknown: the request may have been applied (timeout or reset after sending,
	// an unreadable response). Retry the identical request; the repository ID and
	// OperationID make the repeat safe.
	Unknown Outcome = "unknown"
)

// CallError is the only error a Hosts adapter returns for a failed call.
type CallError struct {
	Outcome Outcome
	Err     *gitv1.ContractError // set for Refused
}
```

No SQL lock or transaction is held across a `Hosts` call; reconciliation reads evidence first and applies the decision in its own short transaction.

### Worker discovery (S2/S3; consumer T-127/T-130/T-138, provider T-129)

```go
package repository

// Workers resolves a tenant's Git worker.
type Workers interface {
	// Endpoint returns the Service endpoint of the tenant's worker. A tenant
	// without a worker (not yet provisioned, being replaced) is an unavailable
	// error, retryable; it never returns another tenant's endpoint. The result is
	// a cache hint: callers must tolerate a replaced worker (new WorkerInstance).
	Endpoint(ctx context.Context, tenant domain.TenantID) (Endpoint, error)
}

type Endpoint struct {
	BaseURL string // https://<service>.<namespace>.svc, for WorkerBasePath
}
```

### `gitauth` issuance and verification (S3/S5/S8; provider T-136)

```go
package gitauth

// Credentials issues what a person asks for through the public API.
type Credentials interface {
	// IssueHuman exchanges a verified login for a human credential (GrantHuman)
	// for exactly one project and repository: not_found for a foreign or unknown
	// one, forbidden when the person's roles do not allow reading it,
	// repository_not_ready, repository_archived or hosting_mismatch for a
	// repository that cannot be served. The recorded update is HumanUpdateCeiling
	// for a person who may push and [] for a read-only person; current project
	// roles and ref policy are checked again at every use.
	IssueHuman(ctx context.Context, tenant domain.TenantID, user identity.UserID, projectID, repositoryID string) (gitv1.CredentialResponse, error)
	// IssueSeed issues a single-use seed grant for an empty repository. It
	// checks, atomically with the grant row and under the repository's seed
	// lock (so concurrent requests issue at most one), the engineer role
	// (forbidden), the repository's existence in the caller's project
	// (not_found for a foreign or unknown one) and gitv1.CheckSeedEligible with
	// gitv1.UnsettledPush over the repository's push records, so any unsettled
	// push of any kind refuses (hosting_mismatch, repository_archived,
	// repository_not_ready, repository_not_empty, seed_pending). The credential
	// is never returned twice.
	IssueSeed(ctx context.Context, tenant domain.TenantID, user identity.UserID, projectID, repositoryID string) (gitv1.CredentialResponse, error)
}

// Verifier resolves a presented Git credential for the Git endpoint and the push
// authorizer.
type Verifier interface {
	// Verify returns the claims of the credential's grant when the credential is
	// known, unexpired, unrevoked, of the grant audience and issued for tenant.
	// It re-checks current authority that can change after issuance: the grant's
	// attempt is live (not cancelled, finished, revoked or superseded). A human
	// grant's current project roles are checked by the caller per repository with
	// the identity authorization. Every refusal is invalid_credential with one
	// message (no reason is disclosed); a grant of another tenant is the same.
	Verify(ctx context.Context, tenant domain.TenantID, credential string) (gitv1.GrantClaims, error)
	// ConsumeSeed marks the seed grant consumed when its receive is authorized
	// (not when it settles). It is idempotent for the same receive and returns
	// seed_consumed (a deny reason) for any other receive, so concurrent seed
	// receives authorize at most one.
	ConsumeSeed(ctx context.Context, tenant domain.TenantID, grantID, receiveID string) error
	// Revoke is idempotent.
	Revoke(ctx context.Context, tenant domain.TenantID, grantID string) error
}

// Assertions signs and verifies forwarding assertions and worker identities with
// key rotation; both are pure claim sets from this contract.
type Assertions interface {
	SignForwarding(ctx context.Context, a gitv1.ForwardingAssertion) (token string, err error)
	VerifyForwarding(ctx context.Context, token string, workerTenant domain.TenantID, now time.Time) (gitv1.ForwardingAssertion, error) // gitv1.CheckForwarding
	SignWorkerIdentity(ctx context.Context, w gitv1.WorkerIdentity) (token string, err error)
	VerifyWorkerIdentity(ctx context.Context, token string, pathTenant domain.TenantID, now time.Time) (gitv1.WorkerIdentity, error) // gitv1.CheckWorkerCall
}
```

```go
package execution // consumer T-148, provider gitauth (T-136); revocation delivered by T-150

// GitGrants is what execution asks of gitauth for a run's workloads.
type GitGrants interface {
	// IssueAttempt issues a preparation or attempt grant (or the seed grant of a
	// provisioning run's attempt) for the actor and repositories of the admitted
	// source set. Idempotent on (actor.attemptId or workload ID, kind): a retry
	// returns the same grant with a fresh credential only through the same
	// delivery path as the attempt token. The credential is never logged or stored.
	IssueAttempt(ctx context.Context, scope domain.Scope, req AttemptGrantRequest) (GrantHandle, error)
	// RevokeAttempt revokes every grant of the attempt. Idempotent. It is called
	// in the same transaction as the cancellation, completion, revocation or
	// supersession of the attempt, so a cancelled attempt can never push after
	// the commit; a transaction that already obtained commit intent completes.
	RevokeAttempt(ctx context.Context, scope domain.Scope, attemptID domain.AttemptID) error
}

type AttemptGrantRequest struct {
	Kind         gitv1.GrantKind // preparation, attempt or seed
	Actor        gitv1.Actor
	Repositories []gitv1.GrantRepository
	ExpiresAt    time.Time // bounded by the attempt's own deadline and the grant lifetime bounds
}

type GrantHandle struct {
	GrantID    string
	Credential string // opaque secret, for delivery only
	ExpiresAt  time.Time
}
```

```go
package gates // consumer T-034 successor, provider gitauth

// IntegrationGrants issues the single-update grant of an approved candidate.
type IntegrationGrants interface {
	// IssueIntegration binds a grant to one approved gate decision: the gate
	// subject names IntegrationConstraint.To, the decision is current, and the
	// repository's default branch is IntegrationConstraint.Ref. Idempotent on
	// (gateId, attemptId). forbidden when the gate is not approved or its subject
	// differs; a moved target is a new constraint, never an edit.
	IssueIntegration(ctx context.Context, scope domain.Scope, attemptID domain.AttemptID, repositoryID string, c gitv1.IntegrationConstraint) (execution.GrantHandle, error)
}
```

### `gitpush` push authorization, journal and reconciliation (S4/S8; provider T-130, caller: control-plane routes registered by T-132)

```go
package gitpush

// Service answers the worker's calls under its tenant-bound identity: the route
// handler has already applied gitv1.CheckWorkerCall, so tenant is the identity's
// tenant, and every record, grant and repository is read under that tenant.
//
// No method calls Git or a worker while holding a SQL lock: Git facts come from
// the request, and evidence for settlement is read before the deciding transaction.
type Service interface {
	// Authorize checks each update against the grant (gitv1.GrantClaims.PermitsUpdate),
	// the ref policy matrix, the repository's policy and archive state, and current
	// authority (roles, attempt liveness, writer reservation, seed consumption via
	// gitauth.Verifier.ConsumeSeed, integration constraint), creates the push
	// record with one authorized entry per ref (kind persisted from the git
	// facts, gitv1.NewPushEntries) and returns the verdicts. The
	// record is durable before the response: no ref changes without it. A repeated
	// receiveId with the identical request returns the same response; a different
	// request for that receiveId is idempotency_conflict. A receive of more
	// updates than the installation's bound is too_many_updates (422,
	// gitv1.CheckUpdateBound; the contract ceiling is 1024). A foreign repository
	// or grant is not_found. The worker treats any error as a refusal.
	Authorize(ctx context.Context, tenant domain.TenantID, req gitv1.PushAuthorizeRequest) (gitv1.PushAuthorizeResponse, error)
	// CommitIntent moves the transaction's entries from authorized to committing
	// when every listed entry is authorized, the grant's attempt is live and the
	// receive deadline has not passed; otherwise push_settled, attempt_inactive or
	// receive_expired (409) and the hook aborts. Repeating it for entries already
	// committing succeeds while those conditions hold.
	CommitIntent(ctx context.Context, tenant domain.TenantID, pushID string, req gitv1.CommitIntentRequest) (gitv1.CommitIntentResponse, error)
	// Outcome marks the reported updates applied and every entry that never
	// reached committing refused, then derives the record status
	// (gitv1.DeriveStatus). Audit and one git.refs_updated outbox event
	// (eventsv1.NewGitRefsUpdated: receive ID, the applied updates inline with
	// the entries' persisted kinds, actor, origin run) commit in the same
	// transaction. Idempotent: the event is emitted once per record.
	Outcome(ctx context.Context, tenant domain.TenantID, pushID string, req gitv1.OutcomeRequest) (gitv1.OutcomeResponse, error)
	// Reconcile settles every unsettled entry of the repository: authorized entries
	// refused, committing entries by gitv1.Settle over evidence from
	// repository.Hosts (liveness, journal, current value under the lock, the
	// highest journal head seen before). Wait leaves the entry for the next pass;
	// unknown records a visible reason and blocks writer reassignment. A
	// Hosts error with Outcome Unknown or Unavailable leaves the entry unchanged.
	Reconcile(ctx context.Context, tenant domain.TenantID, repositoryID string) error
}

// WriterGate is the port the workspace and execution consumers use to honor
// WS-11 for internally hosted repositories.
type WriterGate interface {
	// UnknownBlocksWriter reports whether an entry for the work item's task branch
	// (or the run's branch) is unknown, in which case the writer attempt must not
	// be reassigned until an operator resolves it.
	UnknownBlocksWriter(ctx context.Context, scope domain.Scope, repositoryID, branchRef string) (bool, error)
}
```

### `gitservice` control-plane client (S4; consumer T-128 worker hooks and loops)

```go
package gitservice

// ControlPlane is the worker's only port to the control plane, implemented over
// HTTPS to gitv1.ControlBasePath with the worker identity as bearer credential.
// A worker cannot call any other control-plane route.
type ControlPlane interface {
	// Authorize is called by pre-receive. Any error (unavailable, timeout,
	// refusal) fails the receive; nothing changes.
	Authorize(ctx context.Context, req gitv1.PushAuthorizeRequest) (gitv1.PushAuthorizeResponse, error)
	// CommitIntent is called by the prepared reference-transaction hook after the
	// prepared journal entry is durable (fsynced). A non-200 answer, a timeout or
	// any error makes the hook exit non-zero so Git aborts the transaction; the
	// aborted outcome is appended best-effort.
	CommitIntent(ctx context.Context, pushID string, req gitv1.CommitIntentRequest) (gitv1.CommitIntentResponse, error)
	// Outcome is called by post-receive. A failure is retried in the background
	// with the same body; if it never arrives, reconciliation settles the entries
	// from the journal.
	Outcome(ctx context.Context, pushID string, req gitv1.OutcomeRequest) (gitv1.OutcomeResponse, error)
	// Initialized confirms an initialization; repeated until acknowledged.
	Initialized(ctx context.Context, req gitv1.InitializedRequest) (gitv1.InitializedResponse, error)
	// Assignments lists repositories to initialize, archive, maintain or
	// reconcile; an assignment repeats until its completion is confirmed.
	Assignments(ctx context.Context) (gitv1.AssignmentsResponse, error)
}
```

The worker serializes ref mutations per repository and appends to the journal under that lock, so a `ReadRefValues` taken with no receive in progress is a consistent boundary.

### Source-set admission (S6; consumer T-148, format T-131)

```go
package execution

// SourceSets resolves a run's declared sources into the pinned set stored in its
// input snapshot and runtime context.
type SourceSets interface {
	// Pin resolves each declared source to a repository of the run's project,
	// requires it internal, ready and not archived (hosting_mismatch,
	// repository_not_ready, repository_archived; a foreign or unknown repository
	// is not_found, indistinguishable) and fixes one exact commit per source:
	//
	//   - PushOrigin set: the commit is gitv1.PinnedCommit of the applied push
	//     record entry (the entry's new value). The record is loaded by ID under
	//     the run's scope; an unapplied record, a refused or deleted entry or a
	//     record of another repository or project is refused.
	//   - Commit set: exactly that commit.
	//   - neither: the tip of Ref (default: the default branch) read once, now.
	//
	// The branch a write source updates is Branch, a separate input: it never
	// selects the commit of a push-originated or commit-pinned source. The
	// commit must exist and be reachable; it is pinned with a retained ref
	// (Hosts.SetRetainedRef, idempotent on the run), and the result is
	// gitv1.NewSourceSet. A set with one source at path "." is the
	// single-repository case. Nothing is pinned for a refused set.
	//
	// Replay: admission is keyed by the run (for a push origin, by the trigger,
	// push record and ref). Repeating it after a crash or a redelivery returns
	// the stored set with the same pinned commits, even when the branch has
	// since moved; the branch's later movement is irrelevant and is never read
	// again. A different declaration under the same key is idempotency_conflict.
	Pin(ctx context.Context, scope domain.Scope, runID domain.RunID, declared []DeclaredSource) (gitv1.SourceSet, error)
}

// DeclaredSource is one declared source of a run. Ref, Commit and PushOrigin are
// alternatives (at most one); Branch is only for a write source.
type DeclaredSource struct {
	Repository string // repository name within the project
	Path       string
	Access     gitv1.SourceAccess
	Ref        *string     // read: the ref whose tip is pinned now; nil with the others nil is the default branch
	Commit     *string     // an exact, immutable commit to pin (40 or 64 hex digits)
	PushOrigin *PushOrigin // a push-originated run: pin that applied push entry's new commit
	Branch     *string     // write: the managed branch the writer updates (refs/heads/kodama/...); never selects the commit
}

// PushOrigin names the applied push record entry that started a run (PX-24).
type PushOrigin struct {
	PushID string
	Ref    string
}
```

### Trigger admission (S7; consumer T-137, event from T-130)

```go
package gitwebhook

// PushTriggers admits runs from applied pushes.
type PushTriggers interface {
	// Admit consumes one git.refs_updated event (eventsv1 payload GitRefsUpdated:
	// receive ID, actor, origin run, the applied updates inline). It first skips
	// every trigger that excludes a run-originated push (Excludes() and
	// OriginRunID != nil), then reloads the push record of the receive under the
	// event's scope
	// (gitv1.PushRecord), and for every enabled trigger of the repository whose
	// patterns select an applied entry (gitv1.TriggerConfig.Matches) admits at
	// most one run per (trigger, receive, ref), pinning gitv1.PinnedCommit of
	// that entry (its new value, never a later tip), never for a delete or a
	// refused entry. A replay returns the original admissions with Created
	// false. Admission creates no work item; the run's origin is the push
	// record.
	Admit(ctx context.Context, scope domain.Scope, push eventsv1.Envelope) ([]Admission, error)
}

type Admission struct {
	TriggerID string
	Ref       string
	RunID     domain.RunID
	Created   bool
}

// Configurations stores and reads trigger configuration.
type Configurations interface {
	Put(ctx context.Context, scope domain.Scope, c gitv1.TriggerConfig) error // authorized by project role; validates the repository belongs to the project
	ForRepository(ctx context.Context, scope domain.Scope, repositoryID string) ([]gitv1.TriggerConfig, error)
}
```

### Retained refs, backup and restore (S9; consumer and provider T-138)

```go
package gitops

// Pins keeps commits reachable until retention releases them.
type Pins interface {
	// Pin creates the next retained ref of the run (Hosts.SetRetainedRef) and
	// records it; idempotent per (run, commit).
	Pin(ctx context.Context, scope domain.Scope, repositoryID string, runID domain.RunID, commit string) (ref string, err error)
	// Release deletes the run's retained refs once retention allows
	// (Hosts.DeleteRetainedRef); idempotent. A merged task, agent or run branch is
	// pruned (Hosts.DeleteBranch) only after its tip is reachable from a retained
	// ref or the default branch.
	Release(ctx context.Context, scope domain.Scope, repositoryID string, runID domain.RunID) error
}

// Recovery restores a tenant's repositories.
type Recovery interface {
	// Restore recovers the volume (snapshot) or replays per-repository bundles
	// and keeps Git access closed until Reconcile succeeds. Reconcile settles
	// push records with gitv1.Settle (a journal older than the head already seen
	// is unknown, never refused), checks refs against retained pins and reports
	// what remains for an operator. Access reopens only when no entry is
	// in flight; unknown entries stay visible and keep blocking their writers.
	Restore(ctx context.Context, tenant domain.TenantID) error
	Reconcile(ctx context.Context, tenant domain.TenantID) (Report, error)
}
```

### Product MCP delegation (S10; consumer T-155)

```go
package productmcp

// Repositories is the product MCP server's use of the same authorized use cases
// as the HTTP handlers; it adds no authority.
type Repositories interface {
	Create(ctx context.Context, caller Caller, scope domain.Scope, idempotencyKey string, req gitv1.CreateRepositoryRequest) (gitv1.Repository, error)
	Get(ctx context.Context, caller Caller, scope domain.Scope, repositoryID string) (gitv1.Repository, error)
	List(ctx context.Context, caller Caller, scope domain.Scope, cursor *string) (gitv1.RepositoryList, error)
	// RequestSeed is gitauth.Credentials.IssueSeed for a person only. A
	// provisioning run does not request seeds through the product MCP server:
	// it obtains its seed grant for repositories its run created through the
	// runtime route POST /runtime/v3/attempts/{attemptId}/git/seed (T-150), and
	// the creating run is recorded by Repositories.Create (T-155 as built).
	RequestSeed(ctx context.Context, caller Caller, scope domain.Scope, repositoryID string) (gitv1.CredentialResponse, error)
}

// Caller is the authenticated MCP principal (as built, internal/productmcp also
// carries Tenant, Project, RunOrigin and RequestID). A person acts with their own
// authority. A workload caller exists only for a live attempt of an origin
// `provision` run and acts only within the project of its run (scope mismatch is
// not_found); it can create and read repositories and work items there, and
// cannot administer projects, request seeds or start runs. Person IDs must satisfy
// the contract's ID pattern (an OIDC subject must be mapped to a domain ID).
type Caller struct {
	Actor gitv1.Actor
}
```

## Using the fixtures

Each `fixtures/<area>.json` entry states the message `type`, the payload (or a `raw` document the parser refuses before any schema applies), the expected Go verdict (`valid`, `code`) and the schema's verdict (`schemaValid`). A payload the Go contract accepts must pass the schema; a refusal the schema can express must be refused by it too, and the few the schema cannot (duplicate or overlapping source paths, a status that does not derive from its entries, one git fact per update, lifetimes and clock-dependent rules) are marked `goOnly`. `settlement.json` pins the decision table: the entry, the evidence (liveness, journal, head, known head, current value) and the expected resolution and reason; the tests also shuffle the journal and inject late outcome entries of other receives to prove the decision does not depend on either.

These are shape and decision fixtures. They do not prove authorization, persistence, Git behavior, hooks, the journal's durability or any route; T-127, T-128, T-130 and T-136 replay them against their implementations.

## Checks and regeneration

```sh
make check-go PKGS='./contracts/git/... ./contracts/events/...'
KODAMA_UPDATE_GOLDEN=1 go test ./contracts/git/v1   # after changing a base in fixtures_test.go, then review the diff
```

`Test_unit_git_contract_*` (repository, grants, seed, forwarding assertion, worker RPC, push authorization, commit intent, ref journal, source set, trigger config) and `Test_unit_events_git_types_schema` are the T-126 acceptance checks. The schema's string patterns are asserted equal to the Go patterns, every operation's DTO has a decoder, schema definition and fixtures, and every code and reason is in the schema. Changing a contract requires agreement from the dependent tasks (T-127, T-128, T-130, T-131, T-136, T-137, T-138, T-148, T-155).
