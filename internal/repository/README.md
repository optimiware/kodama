# internal/repository

Repository lifecycle and persistence of the internal Git service (task T-127): project-owned repositories with an idempotent, recoverable provisioning lifecycle. PostgreSQL owns the metadata; the tenant's Git worker owns refs and objects. It implements the control-plane half of [Git repositories](../../docs/specs/git-repositories.md) ("Model", "Provisioning") and seams S1 and S2 of the [implementation plan](../../docs/implementation/internal-git-service.md); the wire shapes, validation and codes are [`contracts/git/v1`](../../contracts/git/README.md)'s and are reused, never redefined.

**Nothing here is wired.** No route is registered, no migration is listed in `internal/app` (T-132 registers the `007` contribution in numeric order), no worker adapter exists (T-128/T-129), and no consumer reads the records yet. Package existence is not product behavior.

## Baseline

| | |
| --- | --- |
| Requirements | GR-01, GR-02, GR-14, GR-19; the persistence side of GR-A01, GR-A02; applies SD-01 to SD-07, SD-13 and SD-14 (memory adapter and shared contract, SQL proof in PostgreSQL). |
| Decisions | O-31 (internal hosting), O-32 (no import, bootstrap by push), O-33 (`directPushToDefault` is off by default); [ADR 0011](../../docs/adr/0011-internal-tenant-git-service.md). |
| Not claimed | A Git worker, Kubernetes, routes, CLI, seed grants (T-136), push records (T-130), the identity permission mapping (an installation task), the real-SQL pass until the coordinator records it. |

## Layout (ports and adapters)

| Package | Role |
| --- | --- |
| `repository` | The entity (`Repository`), the use cases (`Service`), the consumer-owned ports (`Store`, `Hosts`, `Authorizer`, `CloneURLs`, `ExternalSource`, `TenantSource`), the translation of effects into audit rows and event envelopes. Imports only shared values and the Git and event contracts. |
| `postgres` | `Store`, `Keyed` (the idempotent create transaction of the HTTP foundation), the `007_git_repositories.sql` contribution (`Migrations`). |
| `repositorytest` | The in-memory `MemStore` over `memdb` (development-only, see [MEMORY.md](repositorytest/MEMORY.md)), the fakes (`Hosts`, `Authz`, `External`, `Clock`), the exported `Contract` suite and its scenarios, and the PostgreSQL environment that runs the same suite. |
| `http` (`package repositoryhttp`) | Handlers and routes for the repository resource and the refs read; types generated from [`api/repositories.yaml`](../../api/repositories.yaml) and proven identical to the contract's. |

## Model

```text
 New (reserve)            Hosts.Initialize + confirm                archive          restore
 ------------> provisioning ------------------------> ready <---------------> archived
                  | reason: provisioning_pending,        reason: none          reason: archived
                  |  worker_unavailable, initialization_unknown,
                  |  initialization_refused, initialization_mismatch
                  v (a failure is a reason, never another status)
```

- **Provisioning** reserves the row first (audit `repository.provisioning_requested`), then calls `Hosts.Initialize` **outside any transaction**, then marks the repository ready and commits audit `repository.ready`, `repository.created` and `repository.state_changed` (provisioning to ready) in **one** transaction. A crash or a lost response at any step leaves a provisioning repository that `Reconcile` finishes; the worker's initialization is idempotent on the repository ID, so repeating it is safe. `ConfirmInitialized` is the worker's own confirmation (`POST .../repositories/{id}/initialized`, mounted by T-132) and completes the same transition.
- **Create is idempotent on `name`**: the same request returns the same repository (`created: false`); another default branch or policy under the name is `repository_name_taken`. Concurrent creators converge on one row by the unique `(tenant, project, name)` key.
- **Policy**: an omitted policy or `directPushToDefault` is `false`. A patch names the flag. Enabling it grants no one a push; authority is checked at every push (T-130).
- **Default branch** may change at any time while the repository is ready (before seeding it changes the ref the seed must create); it renames nothing in Git. Provisioning repositories finish first; archived ones are restored first.
- **Emptiness** (`empty`) is the worker's report, stored with the time it was observed. A report older than the stored one is ignored (a delayed report must never make a seeded repository seedable again); a newer confirmation of the same value moves only the observation and does not bump the version. Archive and restore never touch it.
- **External mapping**: `RegisterExternal` / `MapConfigured` record a project's existing configured repository as a read-only `hosting: external` record (ready, never empty, never provisioned, deterministic ID `repo_ext_…`, a name derived from the URL). Every later change (patch, archive, restore, refs, worker report) is `hosting_mismatch`. The module does not import `internal/workspace` (a test proves it); the app supplies the configuration through the `ExternalSource` port, with no secret reference. A changed configuration is not rewritten into an existing record (see open issues).

## Authorization

Every use case asks the injected `Authorizer` with current state and fails closed: an error other than denied or not-found is `unavailable`. Actions (the composition maps them to the identity permissions; `engineer` manages repositories, `viewer`, `analyst` and `reviewer` read, `admin` sets policy and gains no implicit push):

| Action | Operation |
| --- | --- |
| `repository.create`, `repository.list` | create (project resource); list |
| `repository.read` | get, refs |
| `repository.update` | change `defaultBranch` |
| `repository.set_policy` | change `policy` (a patch naming both needs both) |
| `repository.archive`, `repository.restore` | transitions |
| `repository.register_external` | the read-only mapping (an installer or service actor, never a person's route) |

The HTTP routes gate PATCH with the coarse `repository.patch` (engineer or admin) and the use case then applies the finer actions. A foreign or unknown repository is `not_found` whatever the actor may do; worker-facing paths (`ConfirmInitialized`, `ReportEmpty`) take the tenant of the verified worker identity and no user authorization.

## Persistence

`007_git_repositories.sql` creates `repositories` with a composite foreign key to `projects`, unique `(tenant, id)`, `(tenant, project, id)` and `(tenant, project, name)`, forced row level security on `kodama_tenant_id()`, status/reason/hosting checks, immutable identity columns and a guard trigger (status transitions, version advances by one, the observation moves only forward). The runtime role reads, inserts and updates only the lifecycle columns; it can never delete (deletion follows retention). The store locks the row for each mutation, runs the pure mutation on a copy, writes the row under its version only when it changed, and writes audit and outbox rows in the same transaction. Event IDs are deterministic per `(repository, version, position)`.

## Checks

```sh
make check-go PKGS='./internal/repository/...'     # unit layer: memory contract and acceptance, handler, migration text
# real SQL (services stack), run by the coordinator:
make check-go-integration DEV_SCOPE=t127 DEV_STACK=services PKGS='./internal/repository/...'
```

| Test | Scenario |
| --- | --- |
| `Test_unit_repository_create_idempotent` | retried and concurrent creation converge (GR-02, GR-A02) |
| `Test_unit_repository_name_unique_per_project` | names unique per project, free across projects and tenants (GR-01, GR-A01) |
| `Test_unit_repository_policy_defaults_deny` | omitted policy is false; admin sets policy only (GR-19) |
| `Test_unit_repository_provisioning_reconciles` | every failure and crash point converges on one ready repository and one `repository.created` (GR-02, GR-A02) |
| `Test_unit_repository_archive_restore` | explicit, versioned, audited transitions (GR-14) |
| `Test_unit_repository_cross_tenant_hidden` | foreign repositories are not found on every path (GR-05, GR-A01) |
| `Test_unit_repository_external_mapping` | the read-only external record and its idempotence |
| `Test_unit_repository_memory_contract` | the whole shared `Contract` on the memory adapter |
| `Test_postgres_repository_contract`, `Test_migration_reapply`, `Test_tenant_isolation_and_guards`, `Test_keyed_create` | the same contract, the migration, RLS, grants, guards and the keyed create on real PostgreSQL (skipped in the unit layer and without a service) |

## Obligations for later tasks

- **T-132** registers `repository/postgres.Migrations()` after `003` and `004`, builds `Service` with a `Hosts` adapter (T-128/T-129), the identity-backed `Authorizer`, a `CloneURLs` built from the Git host and the tenant and project keys, mounts the handler and the worker's `initialized` route (`Service.ConfirmInitialized`), runs `Service.ReconcileAll` periodically and calls `MapConfigured` for existing projects.
- **T-136 and T-130** read `Repository.Status`, `Hosting`, `DefaultBranch`, `Policy` and `Empty` through `Store.Get`/`GetInTenant` and evaluate `gitv1.CheckSeedEligible` and the ref policy themselves; this module issues no grant and records no push. A push that makes the repository non-empty reports it with `Service.ReportEmpty(false, observedAt)`.
- **T-147/T-148** address repositories by `ID`; an external record's ID is `repository.ExternalID(scope, configuredID)`.

## Open issues

- The identity permission names for the actions above, and the admin-only policy rule, are an installation mapping (identity roles do not name repositories yet).
- A configuration change of an already mapped external repository (another URL under the same configured ID) is not written into the record; reconciling it needs a decision on whether the record is a snapshot.
- A refs read refreshes the stored emptiness as a side effect of a `GET`; the version (and ETag) change when the flag does.
