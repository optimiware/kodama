# Database schema sketch

This is the shared schema baseline. `internal/storage/migrations/001_schema.sql` is the squashed schema of every capability in dependency order and `002_grants.sql` the runtime role's grants over it; capability packages own queries, and later schema changes append one contribution each. All tenant-owned relations use explicit `tenant_id`, and project-owned relations additionally use `project_id`. Composite foreign keys enforce matching ownership. Times are UTC; IDs are opaque; integer versions drive optimistic concurrency.

Local `tenants`/`users`/`memberships` and encrypted secret values describe standalone authority. In Optimi-hosted operation, selected O-18 uses the verified Optimi OAuth UUID `tenant_id` as the tenant ownership/RLS identity, with qualified role claims and current access checks. T-061 defines any compatibility constraints or migrations; existing opaque-ID fixtures are not hosted UUID acceptance. Selected O-19 reuses the KMS-encrypted PostgreSQL secrets module with immutable pins, scoped delivery and an agreed hosted read-for-use interface. Local projections do not independently grant access, and Platform tables are not queried as a shortcut. Project/workflow state remains Kodama-owned in either profile.

| Tables | Key data and constraints | Owner |
| --- | --- | --- |
| `tenants`, `projects` | Unique tenant/project keys; project settings and active WIP limit | T-002 |
| `users`, `memberships` | Unique issuer/subject; tenant/project role binding uniqueness | T-002 schema, T-010 behavior |
| `audit` | Actor/action/resource/version/reason; append-oriented safe metadata | T-002 |
| `outbox`, `inbox` | Stable event ID; durable publication claim/status; unique consumer/event result | T-005 |
| `idempotency` | Unique principal/scope/method/route/key, separate request hash, response and expiry | T-004 |
| `work_items`, `work_item_history` | State, spec/criteria, content revision, blocked reason, parent, version; history is immutable | T-011 |
| `variables`, `variable_versions`, `secrets`, `secret_versions` | Project/name uniqueness; secret envelopes and `revoked_at`; immutable logical values | T-012 |
| `workspaces` | Unique work item, repo/ref, accepted commit, writer attempt, pending publication refs, state/error | T-013 |
| `pipeline_definitions` | Project/name/version and content digest uniqueness; canonical body | T-014 |
| `artifacts` | Attempt/producer, kind, digest, size, storage key, upload completion, source provenance | T-015 |
| `runs`, `step_runs`, `step_attempts` | Pinned inputs, purpose, status/reason, cancellation, definition step identity, attempt ordinal, deterministic Job name/UID | T-020 |
| `agent_profiles` | Immutable project/profile version, image/wrapper, model/tool limits | T-030 |
| `model_requests`, `model_usage` | Run/attempt, one-in-flight claim, request count, observed/estimated tokens | T-032 |
| `mcp_servers`, `mcp_sessions` | Scoped registration, endpoint/tool list, caller-bound remote-session mapping/expiry | T-033 |
| `gates` | Run/step, unique subject digest/context, pending/approved/rejected, decision actor/reason, expiry/invalidation | T-034 |
| `environments`, `deployments` | Target namespace, secret ref, config; unique step action key, artifact/source/config, expiry/deletion | T-035 |
| `webhook_deliveries` | Provider/project/delivery ID uniqueness, verified event and trigger result | T-036 |
| `notification_deliveries` | Consumer/event/destination uniqueness, attempts, terminal error, provider receipt | T-041 |
| `work_item_dependencies` | Same-project predecessor/successor; no self/cycles | T-045 |
| `repositories` (designed) | Project-unique name, immutable ID, default branch, hosting, status/reason, version | T-127 |
| `git_push_records` (designed) | Unique repository/receive ID; actor, grant, origin run, per-ref old/new/outcome, status | T-130 |
| Tenant lifecycle fields / backend deletion receipts | Admission disabled/deletion timestamps and attributable per-backend retention/deletion results | T-056 |

Foreign keys involving execution are added after the run tables exist; T-020 adds writer-attempt references through its own migration contribution. Artifact upload rows created before execution exists in development cannot admit runtime writes until integrated authentication is wired. IDs and ownership still validate through the final relational constraints.

## Transaction boundaries

- Work mutation: version check + work/history + audit + outbox.
- WIP transition: lock project + count active + transition/history/audit/outbox.
- Dispatch: lock eligible run/step + check limits/cancellation + insert unique attempt + reserve writer + outbox. No Kubernetes call inside the transaction.
- Result: validate scoped immutable output references, then lock attempt/run/workspace and commit finalization/audit/outbox. Git publication is reconciled before success is acknowledged; no distributed transaction is assumed.
- Gate: lock current subject/context, decide once, record audit/outbox. Progression rechecks applicability.
- Push authorization (designed): check grant/current authority + insert push record and entries. Commit intent: lock the record, move entries `authorized → committing` only if unsettled and the attempt is live; Git commits the refs afterwards. Confirmation marks entries applied with audit/outbox; reconciliation settles the rest from worker evidence.
- Consumer: inbox claim and local effect commit together. External effects are represented by their existing deployment/notification records before calls.

## Queue and indexes

Index nonterminal runs, pending steps/next-attempt time, unpublished outbox age, active writer, tenant/project list sort keys, and expiring previews. Use short row-lock transactions and durable claim timestamps for external dispatch/publication work; never hold SQL locks across network requests. Polling repairs lost wakeups while NATS delivers durable domain events.

## RLS and migration policy

Use a non-owner application role and transaction-local validated tenant context. Global user mapping and membership discovery have narrow principal-filtered queries; worker tenant enumeration exposes tenant IDs only. Production tasks test grants and RLS with real pooled connections.

Schema changes are reviewed before dependent agents start. The baseline and every later migration are immutable once merged: the runner refuses an edited checksum, an applied ID missing from the manifest, and a pending migration ordered before an applied one. Component tests that exercise one module without the rows another module owns drop the cross-module foreign key explicitly; composition tests cover those links against real rows. Production recovery verifies database/Git/blob/KMS consistency together.
