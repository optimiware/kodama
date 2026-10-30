# Git repositories and knowledge base

**Scope:** Layer 1 native Git service ([ADR 0013](../adr/0013-two-layer-architecture-and-domain-simplification.md), O-31): Gerrit-style repository hierarchy, automatic project association, bootstrap by push, ref policy, push events, integration, and the Git-native knowledge base. Implementation is T-166–T-170 ([plan](../implementation/internal-git-service.md)); hosting installation and qualification follow on `main`.

## Purpose

Kodama turns tasks into deployed code. Git is where agents and engineers exchange that code. Kodama hosts each project's repositories so it can create them on demand, grant exactly the access each person and run needs, observe every accepted push, and keep tenants apart, while everyone uses ordinary Git commands.

## Model

Kodama embeds a native Git HTTP service (`kodama git serve`, wrapping `git http-backend`, `git receive-pack`, `git upload-pack` and server-owned hooks) isolated per tenant. PostgreSQL stores lightweight metadata in `repositories` (`id`, `tenant_id`, `project_id`, `name`, `default_branch`, `status`, `reason`, `policy`, `empty`, `journal_ack`, `version`); Git stores the commit graph, branches and tags on the tenant's Git volume.

The full path is rendered from owner keys and the relative name, never stored redundantly:

| Path pattern | Scope and association | Typical usage |
| --- | --- | --- |
| `<tenant>/context` | Tenant scope (`project_id` null) | Tenant engineering guidelines, security policy, global skills |
| `<tenant>/<project>/context` | Associated with `<project>` | Project architecture, glossary, conventions |
| `<tenant>/<project>/<name>` | Associated with `<project>` | Source code, `.kodama/<processname>.yaml`, deployment manifests |

A repository is created through the API/CLI/MCP for a tenant or project; the service derives `project_id` from the requested scope. **A Git path is not permission to create a project or a repository**: a clone or push to an unknown path is `404`, and creating a repository under `<tenant>/<project>/…` requires that `<project>` already exists.

### Provisioning

1. An authorized caller creates the repository; the control plane reserves the row (`status: provisioning`).
2. The tenant's worker initializes a bare repository with the default branch, `core.logAllRefUpdates=always`, `receive.fsckObjects=true`, size limits, `uploadpack.hideRefs=refs/kodama` and the server-owned hooks path.
3. The worker confirms initialization; the control plane marks the repository `ready` and commits `repository.created`.

Initialization is idempotent on the repository ID, and a reconciler finishes interrupted provisioning. An empty repository accepts its first default-branch commit only from a seed grant.

### Bootstrap by push

Kodama does not import or mirror repositories from other hosts (O-32). An existing codebase enters Kodama the way any Git history moves between remotes: an engineer pushes it.

1. The engineer creates an empty repository (`provisioning → ready`, no refs).
2. The engineer requests a **seed grant**: issued only to a current `engineer` on the project, only while the repository has no refs, and expiring after a short bounded lifetime. It permits exactly one receive whose only update creates the configured default branch. Any other update refuses the whole receive (`seed_only`).
3. The engineer pushes with that credential. History, authors and commit IDs are preserved byte for byte. Other branches and tags follow as ordinary engineer pushes.
4. The seed is consumed when its receive is authorized: a second receive with the same grant is refused (`seed_consumed`), and concurrent seeds authorize at most one.
5. A refused seed leaves the repository empty, so a new grant may be requested. A seed whose outcome is not recorded blocks a new grant until the worker's journal is reconciled.

After the seed the default branch is protected: it advances by integration, or by engineers only where the repository opted in with `policy.directPushToDefault: true`.

### Access and ref policy

Every clone, fetch and push is authorized against tenant, project and repository before the backend runs, and ref updates are authorized again in `pre-receive` against current authority:

| Ref class | Allowed updates |
| --- | --- |
| Default branch | Integration (below); an engineer's fast-forward where `policy.directPushToDefault` is true; the seed create of an empty repository |
| Task branch (`refs/heads/kodama/work/<taskId>`) | The run working that task; engineers |
| Run and agent branches (`refs/heads/kodama/runs/<runId>/…`, `refs/heads/kodama/agents/<runId>/…`) | Any step of that run; engineers |
| Other branches | Engineers; workloads only when their run's source grants the branch |
| Tags | Engineers |
| `refs/kodama/*` | Server only; hidden from fetch |

Authority is current, not recorded: a removed role or a cancelled run stops working on the next request even while its credential is unexpired. Git rejects any update whose old value no longer matches; the losing pusher fetches and retries.

Human credentials are exchanged from a verified login for a short-lived Git credential scoped to one project and repository. A run receives its credential through its runtime context; it never enters `/workspace` and is revoked when the run ends.

### Push events

An accepted receive is recorded as one `git.refs_updated` event with a stable identity derived from the repository and the receive, committed in `audit_events` with its state change. The event carries the applied updates inline (bounded, reference-only) so a consumer never needs a separate push record.

A push made by a run carries that run as its origin, so a trigger can exclude it and avoid recursion.

### Triggers

When the default branch changes, each `.kodama/<processname>.yaml` at that exact commit is parsed and validated and upserted into `pipelines` (`name = <processname>`, whole `definition`, `source: git`, `source_commit`). A definition that fails validation is not indexed and does not displace the previous one; the refusal is recorded. Removing a file disables that pipeline rather than leaving a stale definition.

Triggers select a repository, ref patterns and the event kind, and admit a run pinned to the pushed commit — never a later tip. Admission deduplicates by `(pipeline, push event, ref)` through `runs.trigger_key`, so a redelivered or replayed event starts nothing new.

| Update | Typical pipeline |
| --- | --- |
| Task or agent branch | Validation for that task |
| Default branch | Build and deploy for that exact commit |
| Tag | Release |

### Integration

Integration is an ordinary pipeline step, not a server-side merge:

1. A run combines its branch with the default branch's current tip in its workspace and pushes the result to its own branch, producing candidate `C` over base `B`.
2. Test and review steps run against `C`; a human approval is a resolved role-inbox task.
3. An integration step pushes `C` to the default branch with expected old value `B`. The run's integration authority permits exactly that update, once.

A run may push the default branch only when its pipeline is the repository's configured integration pipeline and its task is in an approved state. If the default branch moved, the push is rejected and the pipeline recomputes the candidate. Code on the task branch cannot widen the integration authority.

### Knowledge base

The knowledge base is Markdown in the Git service, with no dedicated tables: `<tenant>/context` and `<tenant>/<project>/context` hold engineering guidelines, skills and blueprints. A run checks out the context repositories it needs under `/workspace/.context/`.

### Retention, maintenance and recovery

Runs pin commits; the server keeps each pinned commit reachable with a hidden `refs/kodama/runs/<runId>/<n>` ref until retention releases it. Each worker runs `git maintenance` on a schedule, serialized with pushes per repository.

Each worker keeps a local receive journal with monotonic sequence numbers. The control plane stores the acknowledged sequence per repository (`repositories.journal_ack`), so a worker that lost an outcome report re-reports every unacknowledged receive on its next reconcile; no push-state table exists.

Volume snapshots and per-repository bundles remain the backup mechanism. Restore and offboarding are deferred operational work (T-138); their unimplemented paths stay closed rather than partially wired.

## Requirements

| ID | Requirement |
| --- | --- |
| GR-01 | Repositories MUST be tenant/project-owned resources with immutable IDs and project-unique names; metadata MUST be authorized and RLS-protected like other project data. |
| GR-02 | Provisioning MUST be idempotent and recoverable; retried or concurrent creation MUST yield one repository, and storage paths MUST derive only from server-assigned IDs. |
| GR-03 | Each tenant's repositories MUST be stored and served by a worker that mounts only that tenant's volume, holds no platform database or broker credentials, and accepts traffic only through the authenticated forwarding path. |
| GR-04 | At most one worker MUST be able to write a tenant volume at a time; storage MUST survive worker replacement, run cleanup and pipeline completion. |
| GR-05 | Every Git request MUST be authenticated and authorized for tenant, project and repository before the backend runs; cross-tenant or unknown repositories MUST be indistinguishable. |
| GR-06 | Git credentials MUST be short-lived, audience-bound and scoped to one tenant, project and repository; run credentials MUST stop working when the run is cancelled or ends. |
| GR-07 | Every ref update MUST be authorized per ref against current authority before Git applies it; the default branch MUST only advance by integration, a seed grant, or the repository's explicit direct-push policy. |
| GR-08 | Server-owned configuration and hooks MUST NOT be replaceable or bypassable by pushed content, client options or repository-local configuration. |
| GR-09 | Each receive MUST be authorized before any ref changes and recorded as one `git.refs_updated` event after it applies; a lost confirmation MUST be recoverable from the worker's journal and the repository's acknowledged sequence. |
| GR-10 | An applied push MUST produce exactly one `git.refs_updated` event; triggers MUST deduplicate by event identity and pin the pushed commit. |
| GR-11 | Pushes made by runs MUST carry their origin so triggers can exclude them. |
| GR-12 | Integration MUST advance the default branch only to an approved candidate from the expected base; a moved target MUST require a new candidate. |
| GR-13 | Commits pinned by runs and evidence MUST stay reachable until retention releases them; pruning and maintenance MUST NOT remove them. |
| GR-14 | Archive MUST refuse new pushes and runs without deleting history; deletion MUST follow retention and record audit. |
| GR-15 | Request size, pack size, ref count, duration and per-tenant concurrency MUST be bounded and refusals MUST be explicit. |
| GR-16 | An empty repository MUST accept its first default-branch update only from an unexpired seed grant issued to a current engineer; each seed grant MUST authorize at most one receive, concurrent seeds MUST authorize at most one, and a repository with any ref MUST NOT be seedable. |
| GR-17 | `policy.directPushToDefault` MUST default to `false`; after seeding the default branch MUST advance only by integration or, where explicitly enabled, by a current engineer's fast-forward. |
| GR-18 | `.kodama/<processname>.yaml` at an accepted default-branch commit MUST be indexed into `pipelines` atomically with the trigger event, and an invalid definition MUST NOT displace a valid one. |
| GR-19 | Bootstrapping MUST preserve pushed objects and commit IDs exactly and MUST NOT change any externally hosted repository, its credentials, its workspaces or its admitted runs. |

GR-17 and GR-A12 of the earlier numbering (provider import) remain deferred by O-32 and are not renumbered.

## Interfaces and authorization

Repository management and Git credentials are public HTTP/CLI/MCP operations in the [Git service contract](../contracts/git-service.md) and the [HTTP contract](../contracts/http-api.md). Git transport is plain smart HTTP at `/git/`. Worker-to-control-plane calls use a separate internal audience and a tenant-bound worker identity.

Roles follow [identity and RBAC](identity-tenancy-and-rbac.md): `engineer` manages repositories, requests seeds and pushes; `viewer`, `reviewer` and `operator` read; `admin` manages repository policy but gains no implicit push or review authority. A run's project-space token grants what the run's source set declares and nothing else.

## Failure and recovery

| Failure | Behavior |
| --- | --- |
| Worker crash during a push | Git's ref locking leaves each ref old or new; the worker's journal attributes it and the control plane acknowledges it on the next reconcile |
| Lost `post-receive` confirmation | The worker re-reports unacknowledged receives; the event is emitted once per receive identity |
| Control plane unavailable in `pre-receive` | The push is refused; nothing changes |
| Worker unavailable | Clones and pushes fail with a retryable error; runs needing Git fail under their own policy |
| Volume lost | Access stays closed until an operator restores and reconciles (deferred, T-138) |
| Revocation during a push | The transaction already authorized completes; the next request is refused |

## Acceptance

| ID | Scenario | Requirements |
| --- | --- | --- |
| GR-A01 | Two tenants create identically named projects and repositories; each sees only its own, and foreign or guessed URLs, IDs and paths return not found | GR-01, GR-03, GR-05 |
| GR-A02 | Concurrent and retried repository creation, and a restart at each provisioning step, converge on one ready repository | GR-02 |
| GR-A03 | A replacement worker serves the same repositories; a second writer cannot mount the volume; run cleanup leaves repositories intact | GR-03, GR-04 |
| GR-A04 | Expired, foreign-audience, revoked and cancelled-run credentials cannot fetch or push; a removed role cannot push with an unexpired credential | GR-05, GR-06, GR-07 |
| GR-A05 | A run pushes its own run and agent branches but not the default branch, tags or `refs/kodama/*`; a pushed hook, config or option changes no server behavior | GR-07, GR-08 |
| GR-A06 | A lost confirmation is recovered from the worker journal and produces exactly one event; a replayed event starts nothing | GR-09, GR-10 |
| GR-A07 | A developer `git push` to the default branch starts one run for the pushed commit; replay and later branch movement start nothing else; a run's own push does not retrigger it | GR-10, GR-11 |
| GR-A08 | Two integrations race for the default branch; one succeeds and the other is rejected; an integration step cannot push an unapproved commit | GR-07, GR-12 |
| GR-A09 | Pruning and maintenance keep every pinned commit; archive blocks pushes and runs and keeps history | GR-13, GR-14 |
| GR-A10 | Oversized pushes, excessive refs and slow clients are refused or cut off with explicit errors without affecting another tenant | GR-15 |
| GR-A11 | An engineer seeds an empty repository with an existing history, then pushes other branches and tags: commit IDs are identical; a seed receive with any extra ref is refused whole; the grant cannot be reused; a concurrent second seed is refused; a non-empty repository is refused | GR-16, GR-19 |
| GR-A12 | After seeding, a direct push to the default branch is refused unless the repository opted in; integration still advances it; pushing `.kodama/implement.yaml` indexes the pipeline and starts its run | GR-17, GR-18 |

## Unresolved decisions

- O-32 (resolved): no provider import or mirroring; bootstrap by push.
- O-33 (resolved): direct pushes to the default branch are off by default.
- One worker per tenant is the baseline (P-17); sharding and a direct execution-to-worker path are later work.
