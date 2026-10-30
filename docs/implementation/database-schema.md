# Database schema

**Phase 2 implementation of [ADR 0013](../adr/0013-two-layer-architecture-and-domain-simplification.md).** There are **14 domain tables plus `audit_events`: 15 product tables** in the baseline (16 with the appended Git credentials table). The migration runner's `schema_migrations` ledger is separate. The earlier “14 total” was a counting error; audit/outbox persistence is required.

The executable schema is [001_schema.sql](../../internal/storage/migrations/001_schema.sql), with runtime privileges in [002_grants.sql](../../internal/storage/migrations/002_grants.sql). [Phase 2 handoff](simplification-phase-2.md) records implementation boundaries and verification. Existing server queries are ported in Phase 3.

**Appended contribution (O-36, T-167):** [003_git_hosting.sql](../../internal/storage/migrations/003_git_hosting.sql) is the only migration after the baseline. It adds `repositories.policy` (`directPushToDefault`, `integrationPipeline`), `repositories.empty` (the tenant worker's report) and `repositories.journal_ack` (the acknowledged receive-journal sequence), and one table, `git_credentials` (below, 16 product tables in total). No push-record, grant or workspace-association table exists; a receive is one `git.refs_updated` row in `audit_events` and the worker's local journal recovers a lost outcome. The baseline statements above describe `001`/`002`; the sections below name the Git additions where they apply.

## Tables

| Table | Ownership and principal data |
| --- | --- |
| `tenants` | Unique tenant key, display name, settings, version |
| `users` | Global external principal mapping, unique `(issuer, subject)`; tenant-filtered visibility |
| `memberships` | Tenant/project role binding; tenant-wide bindings have null project; null-safe uniqueness |
| `projects` | Tenant-owned key, settings, complete `task_workflow` JSON object, version |
| `repositories` | Tenant/project owner, relative `name`, default branch, provisioning/ready/archived status; (`003`) `policy`, `empty` and `journal_ack`. Full Git path is rendered from owner keys, rather than stored redundantly |
| `git_credentials` | (`003`) Short-lived Git credentials: the SHA-256 digest of the opaque secret (never the secret), `kind` (`human`, `seed`, `run`, `integration`), claims, tenant/optional project/repository, optional user or run, `issued_at`, `expires_at`, `revoked_at`, seed `consumed_at`/`consumed_receive`/`settled_at`, version. Composite ownership keys for the run, and a scope trigger (`git_credentials_owner`) rather than a composite key for the repository, whose project may be null: a credential names the same project scope as its repository, except that a `run` credential may be issued for the tenant-level repository (the tenant's context, read-only, never naming an update) while carrying the run's own project, so that the run's liveness is read with the credential; null-safe, unique per tenant by digest; forced RLS on `kodama_tenant_id()`; the runtime role may only change the lifecycle columns |
| `pipelines` | Project, optional repository, process name, one complete `definition` JSON object, `source` (`git` or `api`), Git `source_commit`, version |
| `tasks` | Project, kind, title/body, project-defined state, parent, optional blocker, `parked_at`, `resume_state`, role/user assignment, optional repository/branch, context, resolution, version |
| `runs` | Pipeline and optional task, immutable definition/input/source/binding snapshots, trigger/request deduplication keys, status, cancellation and cleanup progress |
| `run_steps` | Run, step key and ordinal, image, runtime identity, status/exit code, log URI, outputs. One row per step per run; no agent-specific kind |
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

## Project workflows and decisions

`projects.task_workflow` stores the [task workflow](../specs/work-management.md): initial state, states, terminal states, allowed edges, state-to-pipeline triggers and inbox open/resolved states. No SQL enum fixes a project's workflow. `internal/tasks` validates it and protects transitions.

Parking is represented by `parked_at`, `blocked_by_id` and `resume_state`; all three must be set or all absent. The business state is retained, so projects need not invent a universal `parked` state. A resolution stores `approved`, `rejected` or `answered`, actor, role, answer and time. Rejection never promotes a task. Resolved requests are immutable through the task store; changed decisions require a new request. Parked candidate context cannot be silently edited.

The task persistence port has memory and PostgreSQL adapters with one shared contract. Its transaction joins parent/request updates and audit/outbox appends. SQL locks the project before reading its workflow; memory uses one `memdb.Update`. The store checks versions; authorization, trigger dispatch, reference-peer wiring and API operations are Phase 3.

## Transaction boundaries

- Task mutation: project/workflow lock, expected task version, related task changes and event append commit together. No network call runs in that transaction.
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
