# Database schema

**Implementation of [ADR 0013](../adr/0013-two-layer-architecture-and-domain-simplification.md), including its 2026-10-30 pipeline-runtime amendment.** The current executable baseline has **15 product tables**, plus `git_credentials` from the appended Git hosting contribution: **16 product tables**. The migration runner's `schema_migrations` ledger is separate. The earlier fixed “14/15-table” promise is withdrawn in favor of this explicit inventory.

The executable schema is [001_schema.sql](../../internal/storage/migrations/001_schema.sql), with runtime privileges in [002_grants.sql](../../internal/storage/migrations/002_grants.sql). The [pipeline-runtime simplification plan](pipeline-runtime-simplification.md) owns the pending removal of `projects.task_workflow` and the task workflow fields. Existing server queries are ported as their replacement owners become executable.

**Appended contribution (O-36, T-167):** [003_git_hosting.sql](../../internal/storage/migrations/003_git_hosting.sql) is the only migration after the baseline. It adds `repositories.policy` (`directPushToDefault`, `integrationPipeline`), `repositories.empty` (the tenant worker's report) and `repositories.journal_ack` (the acknowledged receive-journal sequence), and one table, `git_credentials` (below, 16 product tables in total). No push-record, grant or workspace-association table exists; a receive is one `git.refs_updated` row in `audit_events` and the worker's local journal recovers a lost outcome. The baseline statements above describe `001`/`002`; the sections below name the Git additions where they apply.

## Tables

| Table | Ownership and principal data |
| --- | --- |
| `tenants` | Unique tenant key, display name, settings, version |
| `users` | Global external principal mapping, unique `(issuer, subject)`; tenant-filtered visibility |
| `memberships` | Tenant/project role binding; tenant-wide bindings have null project; null-safe uniqueness |
| `projects` | Tenant-owned key, settings, version. The pipeline-runtime amendment removes the workflow object; a project's process definitions are its pipelines. |
| `repositories` | Tenant/project owner, relative `name`, default branch, provisioning/ready/archived status; (`003`) `policy`, `empty` and `journal_ack`. Full Git path is rendered from owner keys, rather than stored redundantly |
| `git_credentials` | (`003`) Short-lived Git credentials: the SHA-256 digest of the opaque secret (never the secret), `kind` (`human`, `seed`, `run`, `integration`), claims, tenant/optional project/repository, optional user or run, `issued_at`, `expires_at`, `revoked_at`, seed `consumed_at`/`consumed_receive`/`settled_at`, version. Composite ownership keys for the run, and a scope trigger (`git_credentials_owner`) rather than a composite key for the repository, whose project may be null: a credential names the same project scope as its repository, except that a `run` credential may be issued for the tenant-level repository (the tenant's context, read-only, never naming an update) while carrying the run's own project, so that the run's liveness is read with the credential; null-safe, unique per tenant by digest; forced RLS on `kodama_tenant_id()`; the runtime role may only change the lifecycle columns |
| `pipelines` | Project, optional repository, process name, one complete `definition` JSON object, `source` (`git` or `api`), Git `source_commit`, version. Names are project-unique in the target model; a conflicting definition source is refused. |
| `tasks` | Project, kind, title/body, selected pipeline, current run, projected execution status/step, waiting assignment, parent, optional repository/branch, context, result, progress/version. The target model removes `state`, `parked_at`, `resume_state`, `blocked_by_id`, and workflow-specific resolution fields. |
| `runs` | Pipeline and optional task, immutable definition/input/source/binding snapshots, trigger/request deduplication keys, status, cancellation and cleanup progress. A task may have several runs; one is its current execution. |
| `run_steps` | Run, step key and ordinal, image, runtime identity, status/exit code, log URI, outputs, and durable wait state/result for control steps. One row per step per run; no agent-specific kind. |
| `services` | Tenant/project/run scope, image/configuration, endpoint/runtime identity, status; scope and nullable owners are checked together |
| `secrets` | Tenant/project name, ciphertext, self-contained encryption envelope and key reference, version, revocation timestamp |
| `artifacts` | Immutable external metadata, producing run/optional step/task, URI, kind and digest. Preview URLs may have no content digest |
| `agent_sessions` | Project/run/step, optional task, model, status, usage counters, messages or transcript reference, result |
| `mcp_servers` | Tenant/project registration, URL, allowed tools, optional same-project or tenant-shared secret |
| `audit_events` | Immutable actor/resource/payload/time plus mutable publication timestamps, retry count and next attempt time |

JSON is for documents and snapshots. Ownership, identities, queryable states, versions and references remain relational. No independently editable `steps` definition table, raw-YAML mirror or separate trigger document duplicates `pipelines.definition`. YAML presentation is reconstructed from the stored document; comments/formatting remain in Git.

## Ownership and constraints

- Every tenant-owned relation has explicit `tenant_id`; project records also carry `project_id`. Composite foreign keys reject cross-project references even within one tenant.
- A repository's relative name may contain nested slash-separated segments; tenant-level repositories use `context`. Routing resolves existing tenant/project keys and authorizes creation before associating the repository. A path is not permission to create a project.
- Null-safe unique constraints cover tenant-level memberships, repositories, services, secrets and MCP registrations, and API-managed pipelines with no repository.
- `services.scope = pipeline` means **one run's lifetime**: both project and run are required. Project services have no run; tenant services have neither. Long-lived service reconciliation is Phase 3.
- Artifact/session step references include the producing run ID, preventing attribution to a step in a different run.
- MCP secret references carry the secret's project scope separately. A project may select its own or a tenant-shared secret, never another project's. Tenant-wide MCP registrations cannot select a project's private secret.
- Runtime cannot rewrite run inputs, source/binding/definition snapshots, artifact metadata or event payloads. State updates have narrow column grants. Runtime cannot truncate tables or access the migration ledger.

## Pipeline ownership and task projection

A project's process definitions are its pipelines. Pipeline names are unique within a project; the repository is the definition source, not a second naming scope. A task selects one pipeline and may have several runs through explicit reruns or rework. Run and step records own execution truth.

The target task fields are execution projections: selected pipeline, current run, status, current step, waiting assignment, result, and monotonic progress/version. Projection updates compare the run version so an older event cannot overwrite a newer run. The projection is rebuildable from `runs` and `run_steps`; it is not an independent workflow authority.

`projects.task_workflow` and the workflow-specific task fields are removed in the [pipeline-runtime simplification](pipeline-runtime-simplification.md). A wait is persisted with its run step and records a unique identity, expected input, accepted result, deadline, and consumed signal. Resolution records an authorized input against the exact run, step, and wait; the pipeline decides the next branch. A role inbox is a query over current waits, not a blocking task that drives a second state machine.

The task persistence port has memory and PostgreSQL adapters with one shared contract. Its transaction joins task, run-projection and audit/outbox appends. SQL locks the task before reading its current run; memory uses one `memdb.Update`. Authorization, trigger dispatch, reference-peer wiring and API operations belong to the shell.

## Transaction boundaries

- Task mutation: task lock, expected task/progress version, current-run projection change and event append commit together. No network call runs in that transaction.
- Admission: persist the entire run snapshot and its step records together. `runs.trigger_key` identifies one logical trigger delivery per pipeline. User submission deduplicates `(project, principal, request_key)`; `request_hash` is compared, not part of the unique key.
- Outbox: commit the event with state, publish outside SQL using its stable ID, then update publication progress. A crash may redeliver. Consumers use resource uniqueness or transactional version checks; JetStream alone does not make database effects exactly once.
- Git and container effects remain outside SQL transactions. Durable reconciliation of external effects is a Phase 3 responsibility, not removed by the smaller schema.

## Secrets and run history

The single secret row contains ciphertext and envelope metadata sufficient to decrypt it with its referenced KMS/keyfile wrapper. Authenticated data continues to bind ownership, secret ID and revision. Ordinary configuration lives in the project or pipeline document.

Runs retain secret identity/revision references, not plaintext. If a current-only secret has rotated or been revoked before use, resolution fails rather than silently substituting its new value. A new run can bind the new revision. Historical secret-value recovery is not supplied by this schema.

## RLS and migration policy

All product tables, including `git_credentials`, enable and force RLS. `WithTenant` sets validated transaction-local context under a non-owner role. Global user visibility is filtered through memberships; principal discovery and worker tenant enumeration use narrow, PUBLIC-revoked definer functions. Tenant RLS is supplemented by project predicates and operation authorization.

This is an intentional **fresh-database prerelease reset**. Existing checksums are rejected before applying changes; the migration runner never drops or rewrites an existing database. Use a new isolated database for the reset. Later migrations append to this new baseline. The application manifest contains the two baseline migrations plus the one Git hosting contribution `003_git_hosting.sql`; legacy run-resource/grant/provenance contributions are not appended.

No deployed database was reset as part of Phase 2. The legacy server explicitly refuses the new schema until Phase 3 replaces its adapters.
