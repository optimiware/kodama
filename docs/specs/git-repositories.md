# Git repositories

**Scope:** internal repository hosting ([ADR 0011](../adr/0011-internal-tenant-git-service.md), O-31): project-owned repositories, the tenant Git service, access grants, ref policy, push events, integration and retention. **Normative design; not delivered or qualified.** Implementation is T-126–T-133 ([plan](../implementation/internal-git-service.md)). Until a project's repository is internally hosted, the current external-host model in [workspaces](workspaces.md) applies to it unchanged. Wire shapes are in the [Git service contract](../contracts/git-service.md).

## Purpose

Kodama turns tasks into deployed code. Git is where agents and engineers exchange that code. Kodama hosts each project's repositories so that it can create them on demand, grant exactly the access each person and attempt needs, observe every accepted push, and keep tenants apart, while everyone uses ordinary Git commands.

## Model

| Concept | Rule |
| --- | --- |
| Repository | Tenant/project-owned; immutable `id`, project-unique `name`, `defaultBranch`, `hosting` (`internal` or `external`), status `provisioning`, `ready`, `archived`, plus `reason` and `version`. PostgreSQL owns these fields; Git owns refs and objects |
| Tenant Git service | One Git worker and one durable volume per tenant (P-17), in a Kodama-managed service namespace. Bare repositories live at a path derived from the repository ID, never from a name or URL |
| Git endpoint | `https://<git-host>/git/<tenant-key>/<project-key>/<repository-name>.git`, served by the Kodama server, which authenticates, authorizes and forwards to the tenant's worker |
| Git grant | A short-lived credential naming actor, tenant, project, repositories, read access and the ref patterns it may update; for workloads, bound to one attempt |
| Ref classes | Default branch (protected), task branches `refs/heads/kodama/work/<work-item-id>`, run-scoped agent branches `refs/heads/kodama/agents/<work-item-id>/<run-id>/<label>`, other branches, tags, server-only `refs/kodama/*` |
| Push record | The durable intent and outcome of one receive operation, keyed by its `receiveId`: actor, grant, repository and one entry per `(ref, old, new)` with its own state (`authorized`, `committing`, `applied`, `refused`, `unknown`). The record's status is `authorized`, `applied`, `refused` or `unknown` |
| Ref journal | An append-only, durable per-repository log on the tenant volume of `prepared`, `committed` and `aborted` ref transactions, each attributed to a `receiveId` or server operation |
| Source set | A run's pinned list of `(repository, commit, checkout path)` |

Repository status changes are explicit transitions: `provisioning → ready`, `ready ↔ archived`. A provisioning failure is a `reason` on a `provisioning` repository, retried by reconciliation; it is not another status. Archive refuses new pushes and new runs on the repository; it never deletes history.

## Behavior

### Provisioning

1. An authorized caller (API, CLI, MCP tool or a provisioning step's grant) creates the repository. The control plane reserves the record (`provisioning`) and returns it.
2. The tenant's worker initializes a bare repository with the default branch name, `core.logAllRefUpdates=always`, `receive.fsckObjects=true`, size limits, `uploadpack.hideRefs=refs/kodama` and the server-owned hooks path.
3. The worker confirms initialization; the control plane marks the repository `ready` and emits `repository.created`.

Retries and concurrent requests converge on one repository: names are unique per project and initialization is idempotent on the ID-derived path. A reconciler finishes interrupted provisioning. An empty repository accepts its first default-branch commit only from a grant with explicit seed permission.

### Access and ref policy

Every clone, fetch and push is authorized against tenant, project and repository before the backend runs. Ref updates are authorized again in `pre-receive`, per ref, against the grant and current authority:

| Ref class | Allowed updates |
| --- | --- |
| Default branch | Fast-forward by an integration grant bound to the approved candidate (see [integration](#integration)); by an engineer only where the repository's `policy.directPushToDefault` permits it (explicit on creation; default pending O-33); seed of an empty repository |
| Task branch | Fast-forward by an engineer, or by the attempt holding the work item's writer reservation ([WS-03](workspaces.md#requirements)); non-fast-forward only by an engineer, recorded as a history rewrite |
| Agent branches | Create/update/delete by any executable attempt of the run named in the branch, whatever its declared access, because they publish no checkpoint; engineers may update or delete |
| Other branches | Engineers; workloads only when their grant names the pattern |
| Tags | Engineers or a release grant; never moved or deleted by workloads |
| `refs/kodama/*` | Server only; hidden from fetch |

A cancelled, finished, revoked or superseded attempt can no longer push. Removing a person's role takes effect on the next push even while their credential is unexpired. Git rejects any update whose old value no longer matches; the losing pusher fetches and retries.

### Push journal and events

Git's `reference-transaction` hook runs inside every ref transaction with the refs already locked (`prepared`) and again after the transaction commits or aborts. Kodama uses it as a two-phase commit fence, so that every ref change is attributable to one receive operation and no authorized receive can commit after its record was settled. Each receive operation carries a worker-generated `receiveId` through all hooks.

1. **Authorize.** `pre-receive` sends the intended updates. The control plane checks each against the grant, ref policy and current authority, creates one push record (`authorized`) with one entry per ref, and returns the verdict. Any denial fails the whole receive; nothing changes.
2. **Commit intent.** For each ref transaction, the `prepared` hook (refs locked) first appends `prepared` for `(receiveId, ref, old, new)` to a durable per-repository ref journal on the tenant volume, then asks the control plane to move those entries from `authorized` to `committing`. The control plane refuses if the record was already settled, the grant's attempt is no longer live, or the receive deadline passed. If the journal write or the commit intent fails, the hook aborts the transaction and Git changes nothing.
3. **Outcome.** The `committed` or `aborted` hook appends the outcome to the same journal. `post-receive` reports the applied refs; the control plane marks those entries `applied`, appends audit and commits one `git.refs_updated` event for the record in the same transaction. Entries that never reached `committing` become `refused`.

**Settlement.** Each ref entry settles separately, because a non-atomic push can apply some refs and reject others. The record's status is `applied` if any entry applied, `refused` if none did, and `unknown` while any entry is unresolved.

| Entry state found by reconciliation | Rule |
| --- | --- |
| `authorized` (no commit intent) | May be settled `refused` at any time: a late `prepared` hook is refused by the control plane and aborts |
| `committing`, worker reports the receive still running | Wait; the worker enforces the receive deadline by terminating overrunning receives |
| `committing`, receive ended or worker instance verified stopped | The transaction's own `committed` entry → `applied`; its own `aborted` entry → `refused`. With only `prepared`, the ref's next value decides: the `observed` value of the next **`prepared`** entry for that ref, or, if there is none, the ref's current value read under the worker's per-repository mutation lock with no receive in progress. Equal to `new` → `applied`; equal to `old` → `refused` |
| Anything else (journal unreadable, volume restored from an older snapshot, conflicting evidence) | `unknown` with a visible reason until an operator resolves it |

Each `prepared` entry records the ref's `observed` value read while the hook holds the lock. `prepared` entries for one ref are therefore totally ordered and each observes the result of every earlier transaction on that ref. Git releases ref locks before the `committed` or `aborted` hook runs, so outcome entries can be appended late and out of order; they are used only as positive evidence for their own transaction, never to order or settle another one. Matching `(ref, old, new)` tuples from two receives are never confused, because attribution comes from the `receiveId` in the journal, not from ref values. A refs-only reading, the reflog and a deleted Pod are never sufficient evidence on their own. An `unknown` entry blocks reassignment of the affected writer attempt, as an unknown publication does today ([WS-11](workspaces.md#requirements)). Refused updates are recorded with reasons and emit no event. Server-side ref changes (provisioning, retained refs, maintenance) use the same journal with a server operation ID instead of a `receiveId`.

### Triggers

Pipeline triggers select repositories, ref patterns and the originating actor kinds they react to. Admission consumes `git.refs_updated`, deduplicates by `(trigger, push record, ref)`, and pins the pushed commit, never a later tip. Typical mappings:

| Update | Workflow |
| --- | --- |
| Task or agent branch | Validation pipeline for that work item; a run's own pushes are excluded by origin |
| Default branch | CI/CD pipeline for that exact commit |
| Tag | Release pipeline |
| History rewrite | Audit and project policy; never an automatic rollback |

A push made by a run carries that run as its origin, so a trigger can exclude it and avoid accidental recursion. A developer's push to an existing project uses the same path; a pipeline started this way has no work item.

### Integration

Integration is an ordinary pipeline step, not a server-side merge:

1. The run's writer step (holding the writer reservation) combines the task branch with the default branch's current tip in the run workspace and pushes the result to the task branch, producing candidate commit `C` over base `B`.
2. Test and review steps run against `C`; a gate approves the subject that names `C`.
3. An integration step pushes `C` to the default branch with expected old value `B`. Its grant permits exactly that update: the protected ref, from `B`, to the approved `C`, once.

If the default branch moved, the push is rejected and the pipeline recomputes the candidate and revalidates it. Integrations of one repository should run through a per-repository queue to reduce such retries; Git's expected-old check remains the protection. Code on the task branch cannot widen the integration grant.

### Retention, maintenance and recovery

Runs pin commits; the server keeps each pinned commit reachable with a hidden `refs/kodama/runs/<run-id>/<n>` ref until retention releases it. Merged task and agent branches are pruned on a schedule only after their commits are reachable from a retained ref. Each worker runs `git maintenance` on a schedule, serialized with pushes per repository.

Backups combine volume snapshots with per-repository bundles in object storage. Restore recovers the volume or replays bundles, then reconciles push records, refs and retained pins before Git access reopens. Offboarding a tenant revokes grants, settles push records, and deletes the volume only after retention.

## Requirements

| ID | Requirement |
| --- | --- |
| GR-01 | Repositories MUST be tenant/project-owned resources with immutable IDs and project-unique names; repository metadata MUST be authorized and RLS-protected like other project data. |
| GR-02 | Provisioning MUST be idempotent and recoverable; concurrent or retried creation MUST yield one repository, and storage paths MUST derive only from server-assigned IDs. |
| GR-03 | Each tenant's repositories MUST be stored and served by a worker that mounts only that tenant's volume, holds no platform database or broker credentials, and accepts traffic only through the authenticated forwarding path. |
| GR-04 | At most one worker MUST be able to write a tenant volume at a time; storage MUST survive worker replacement, run cleanup and pipeline completion. |
| GR-05 | Every Git request MUST be authenticated and authorized for tenant, project and repository before the backend runs; cross-tenant or unknown repositories MUST be indistinguishable. |
| GR-06 | Git grants MUST be short-lived, audience-bound and scoped to repositories and ref patterns; workload grants MUST be bound to an attempt and stop working when it is cancelled, finished, revoked or superseded. |
| GR-07 | Every ref update MUST be authorized per ref against the grant, the ref policy and current authority before Git applies it; the default branch MUST only fast-forward under an integration grant, a seed grant or the repository's explicit direct-push policy. |
| GR-08 | Server-owned configuration and hooks MUST NOT be replaceable or bypassable by pushed content, client options or repository-local configuration. |
| GR-09 | Each receive operation MUST be authorized and recorded before any ref changes, and each ref transaction MUST obtain commit intent from the control plane while its refs are locked and record itself in a durable, receive-attributed ref journal; a transaction refused commit intent MUST abort. Settlement MUST follow the evidence rules of the push journal, MUST NOT rely on refs, reflog or Pod deletion alone, and MUST keep `unknown` outcomes blocking writer reassignment. |
| GR-10 | An applied push MUST commit exactly one `git.refs_updated` event with its record; triggers MUST deduplicate by event identity and pin the pushed commit. |
| GR-11 | Pushes made by runs MUST carry their origin so triggers can exclude them. |
| GR-12 | Integration MUST advance the default branch only to an approved candidate from the expected base; a moved target MUST require a new candidate. |
| GR-13 | Commits pinned by runs, evidence or deployments MUST stay reachable until retention releases them; pruning and maintenance MUST NOT remove them. |
| GR-14 | Archive MUST refuse new pushes and runs without deleting history; deletion MUST follow retention and record audit. |
| GR-15 | Backup and restore MUST recover repositories consistently with push records and retained pins before Git access reopens. |
| GR-16 | Request size, pack size, ref count, duration and per-tenant concurrency MUST be bounded and refusals MUST be explicit. |
| GR-17 | Importing an existing repository MUST verify its refs and commit identities, settle active writers, and switch hosting explicitly; admitted runs MUST keep their original source semantics. |

## Interfaces and authorization

Repository management and Git credentials are public HTTP/CLI/MCP operations in the [Git service contract](../contracts/git-service.md) and the [HTTP contract](../contracts/http-api.md). Git transport is plain smart HTTP at the Git endpoint. Worker-to-control-plane calls use a separate internal audience and a tenant-bound worker identity. Roles follow [identity and RBAC](identity-tenancy-and-rbac.md): `engineer` manages repositories and pushes; `viewer`, `analyst` and `reviewer` may read where their project role allows; `admin` manages policy but gains no implicit push or review authority.

## Failure and recovery

| Failure | Behavior |
| --- | --- |
| Worker crash during a push | Git's ref locking leaves each ref old or new; the ref journal attributes it; the push record settles under the evidence rules |
| Lost `post-receive` confirmation | Reconciliation settles the entries from the ref journal; the event is emitted once |
| Control plane unavailable in `pre-receive` | The push is refused; nothing changes |
| Worker unavailable | Clones and pushes fail with a retryable error; runs needing Git fail or retry under their own policy |
| Volume lost | Restore from snapshot and bundles, then reconcile; access stays closed until consistent |
| Revocation during a push | Commit intent re-checks attempt liveness with the refs locked; a revoked attempt's transaction aborts. A transaction that already obtained commit intent completes and is recorded |

## Acceptance

| ID | Scenario | Requirements |
| --- | --- | --- |
| GR-A01 | Two tenants create identically named projects and repositories; each sees only its own, and foreign or guessed URLs, IDs and paths return not found | GR-01, GR-03, GR-05 |
| GR-A02 | Concurrent and retried repository creation, and a restart at each provisioning step, converge on one ready repository | GR-02 |
| GR-A03 | A replacement worker serves the same repositories; a second writer cannot mount the volume; run cleanup leaves repositories intact | GR-03, GR-04 |
| GR-A04 | Expired, foreign-audience, revoked and cancelled-attempt credentials cannot fetch or push; a removed role cannot push with an unexpired credential | GR-05, GR-06, GR-07 |
| GR-A05 | An agent pushes its agent and task branches but not the default branch, tags or `refs/kodama/*`; a pushed hook, config or option changes no server behavior | GR-07, GR-08 |
| GR-A06 | A crash or lost confirmation at each step (after authorization, after commit intent, after `prepared`, after commit, before `post-receive`), and a partial multi-ref push, produce exactly the applied updates and one event per record | GR-09, GR-10 |
| GR-A07 | A developer `git push` to an existing project starts one CI/CD run for the pushed commit; replay and later branch movement start nothing else; a run's own push does not retrigger it | GR-10, GR-11 |
| GR-A08 | Two integrations race for the default branch; one succeeds, the other is rejected and recomputes; an integration grant cannot push an unapproved commit | GR-07, GR-12 |
| GR-A09 | Pruning and maintenance keep every pinned commit; archive blocks pushes and runs and keeps history | GR-13, GR-14 |
| GR-A10 | A restored volume plus bundles reopens with consistent refs, push records and pins | GR-15 |
| GR-A11 | Oversized pushes, excessive refs and slow clients are refused or cut off with explicit errors without affecting another tenant | GR-16 |
| GR-A12 | An external repository is imported with identical commits; in-flight runs finish under the external model; new runs use the internal repository | GR-17 |
| GR-A13 | Two receives with identical `(ref, old, new)` race and only one applies; a receive delayed past settlement of its `authorized` record is refused commit intent and changes nothing; a receive overrunning its deadline is terminated before its entries settle; a predecessor's `committed` entry appended after a successor's `prepared` entry (A→B committed late, then B→A prepared and crashed) settles the successor `refused`, not `applied`; a volume restored from an older snapshot yields `unknown`, never `refused` | GR-09 |

## Unresolved decisions

- **O-32:** whether the first release imports or mirrors external repositories (GitHub, GitLab), and in which direction.
- **O-33:** whether repositories allow direct human pushes to the default branch by default. Until decided, the policy is explicit on creation.
- P-17's one worker per tenant is the proposed baseline; sharding and a direct execution-to-worker path are later work.
