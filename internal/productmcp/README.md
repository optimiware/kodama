# internal/productmcp

Product MCP tools of the internal Git service (task T-155, seam S10): an authenticated MCP server that lets a person's agent, or a provisioning run's attempt, create and inspect **projects, repositories and work items**, ask how to bootstrap an existing codebase by `git push`, and start a bounded provisioning run. It is the MCP face of the same authorized use cases the HTTP routes and CLI call (AC-15, AC-A12) and is distinct from the MCP proxy (`internal/mcpproxy`), which relays tools of *external* servers to a run.

**Nothing here is wired.** No route is registered and no adapter exists: T-132 mounts the handler, supplies the `Authenticator` and adapts the consumer-owned ports to the project, repository, work, gitauth and execution use cases. Package existence is not product behavior.

## Baseline

| | |
| --- | --- |
| Requirements | AC-15, AC-02 (idempotent creates), AC-04 (bound cursors, by the adapters), GR-18 and PX-25 (seed authority), IT-17 to IT-19 (caller authority); AC-A12. |
| Decisions | O-31 to O-33 (internal hosting, bootstrap by push, `directPushToDefault` off by default); [ADR 0011](../../docs/adr/0011-internal-tenant-git-service.md). |
| Not claimed | Route registration, the adapters, a durable `Results` store, a real-SQL pass (the package owns no table), live-agent qualification. |

## Tools

| Tool | Offered to | Use case behind it |
| --- | --- | --- |
| `kodama_project_create` / `_get` / `_list` | a person | identity project use cases (`project:create` is tenant-level) |
| `kodama_repository_create` / `_get` / `_list` | a person; an attempt in its own project | `repository.Service.Create/Get/List` |
| `kodama_repository_request_seed` | a person | `gitauth.Credentials.IssueSeed` (engineer, empty ready repository, one receive creating the default branch) |
| `kodama_work_item_create` / `_get` / `_list` | a person; an attempt in its own project | `work.Service.Create/Get/List` (a task unless another kind is named) |
| `kodama_provisioning_run_start` | a person | execution admission of origin `provision` |

Creating a repository as an attempt makes the port record the **creating run** (`Actor.RunID` and `AttemptID`) with the repository in the reservation's transaction: T-150 allows a run's seed grant only for "a repository this run created" and reads that record.

Content is bounded before any port is called, with the stable `invalid_argument` code: title 200 characters, specification 64 KiB, at most 100 acceptance criteria (id 64 characters, text 2000 characters), project key 63 and display name 200 characters, valid UTF-8 without control characters (newline and tab allowed); the work and identity use cases validate again. A list port that returns more than was asked (the requested limit for work items, `MaxPage` for projects and repositories) is not trusted and fails as `unavailable`.

Every tool returns structured JSON (`outputSchema`) and a stable `code: text` on failure. Creating tools require an `idempotencyKey`.

## Authority

- **The caller is the only identity.** Every port method receives the `Caller` the authenticator produced and must authorize it against current state, as the HTTP handler's use case does, failing closed. The package has no service identity to fall back on, and no tool argument can name a tenant, an actor, a role or an origin (the schemas reject extra properties).
- **Tenant from the principal, project from the argument, scope from both.** A person's project is whatever they name and the use case lets them see; a foreign or unbound project is `not_found`. A run's attempt is bound to its run's project: omitting the project selects it, naming another is `not_found`.
- **Only a provisioning attempt is a workload caller.** The `Authenticator` mints a workload `Caller` only for a live attempt of a run whose origin is `provision`, in that run's admitted project, and sets `RunOrigin` to `provision`. `Caller.Validate` runs the contract's `Actor.Validate` (a well-formed type, identity, run and attempt: an empty or odd run id would collapse every workload onto one idempotency principal) and refuses any other origin (a work item's, push's or manual run's attempt, or an unset one) as `invalid_credential` before any port is reached, and the handler answers 401. Liveness changes after authentication, so every port re-checks that the attempt is still live and the run still provisioning, like any other current-authority check.
- **A run's attempt is limited.** It sees only the repository and work-item tools, cannot administer projects, request seeds or start runs (a refused call never reaches a port), so a provisioning run cannot fan out or seed what it did not create. Its own seed authority is issued by the runtime route (T-150) only for repositories its run created (PX-25).
- **Listing is not authorization.** `tools/list` is filtered by actor kind, and every call is authorized again.
- **No session.** The endpoint serves one MCP request per HTTP request (SDK stateless mode): every request authenticates its bearer and builds a server bound to that caller, so a session identifier cannot be replayed by another credential.

## Idempotency

A create takes `idempotencyKey` (1 to 64 characters of `A-Za-z0-9._~:-`). The key's identity is (tenant, principal, project, tool, key); the request hash is a value, never part of the key:

1. A recorded key replays: another request hash is `idempotency_conflict`; the same one **rereads the resource through the caller's current authority**, so a revoked caller gets neither the resource nor a stored copy, and the response has `replayed: true`. The hash covers the normalized request (the defaulted kind and branch are part of it).
2. A new key creates through the use case, handing it the namespaced key `mcp.<tool>.<key>`, so the use case can make the create exactly-once in its own transaction (as `repository/postgres.Keyed` does), and then records the resource ID (`Results`, put-if-absent, so concurrent creators converge).
3. A failed record does not fail the create that happened; a retry converges through the use case's key. Concurrent creators that race to record converge on the first record (the loser rereads the winner's resource; a different request is `idempotency_conflict`). An unavailable `Results` fails the call before creating.

The hash covers the **effective** request: an omitted default branch or policy is the same request as spelling the default (`main`, `false`) out, and the defaulted work item kind is `task`. The idempotency principal (`IdempotencyPrincipal`) is the person's identity, but for a workload it is the **run**, not the attempt: a replacement attempt of the same run that retries a create converges on the original resource (at the tool layer through `Results`, and in the use case because adapters key their record by the same principal) instead of colliding with it (`repository_name_taken`). Another run is another principal.

`Results` stores identifiers and hashes only. A seed request is deliberately not idempotent and not recorded: each call asks the grant issuer, which alone decides eligibility (`repository_not_empty`, `seed_pending`, ...) and issues a one-time credential.

## Seed guidance

`kodama_repository_request_seed` is for a person who wants to bring an existing codebase in. It checks, under the caller's authority, that the repository is internal, ready and empty (the grant issuer re-checks authoritatively and adds `seed_pending`), then returns the credential-free clone URL, the default branch, the grant's one-time `username`/`password`/`grantId`/`expiresAt` (at most 15 minutes, one receive, the default branch only) and push instructions (`git remote add`, `git push <branch>:refs/heads/<default>`, the `seed_only` rule, the protected default branch afterwards). The password is returned once to the caller; it is never stored, logged or repeated in the instructions. There is no GitHub or GitLab import.

## Provisioning delegation

A person with `run:manage` may start a provisioning run in a project they name. The tool is bounded: the scope is that project; inputs are at most 16 strings (key 64 bytes, value 4 KiB, 32 KiB total); the run identity is derived from the idempotency identity; and the **project's** active provisioning runs, whoever started them, are capped by `Limits.MaxActiveProvisioning` (default 2): engineers share the cap, they do not each get one. The request has no credential, grant or origin member: the port always admits origin `provision`, and the tool returns no credential. The tool performs no pre-count; **the port enforces the budget**, and its contract (see `StartProvisioning` in `ports.go`) is, in order:

1. A run with the RunID already exists in the scope. It is a replay only if this caller (the same principal) created it with origin `provision`: compare the **whole declaration** (definition, repository, inputs; the tool-layer hash is lost with the `Results` record). Identical is a replay, returned **before and regardless of the budget**; different is `ErrIdempotencyConflict`, the same `idempotency_conflict` code the tool layer gives its own mismatch, whether or not `Results` survived. A run of another creator or origin under that RunID is `ErrNotFound` (its existence is not disclosed) and is never returned, replayed or modified.
2. Otherwise authorize, then **count the project's active runs and admit as one atomic step** (a locked admission row or one conditional insert): `ErrBudget` when the count is already `MaxActive`. "Active" means a run of origin `provision` in this project, whoever created it, whose status is not terminal.
3. `Get` returns `ErrNotFound` for an unknown id or a foreign tenant's, project's or origin's run.

## Ports (consumer-owned)

`Repositories` (S10 in [`contracts/git/README.md`](../../contracts/git/README.md)), `Projects`, `WorkItems`, `Provisioning`, `Results`, `Authenticator`. Adapters translate their module's failures into `ErrDenied`, `ErrNotFound`, `ErrInvalid`, `ErrConflict`, `ErrBudget`, `ErrUnavailable` or an `*Error` that carries a contract code (`repository_name_taken`, `repository_not_empty`, `seed_pending`) and client-safe text; anything else is `unavailable` with constant text. `MemoryResults` is the development store.

**Deviation from the S10 sketch.** `contracts/git/README.md` still says `Repositories.RequestSeed` also serves "the seed grant of a provisioning attempt". This package does not: a provisioning run's seed grant is issued by the runtime seed route (`/runtime/v3/attempts/{attemptId}/git/seed`, T-150) only for repositories the run created, and `kodama_repository_request_seed` refuses a workload caller before any port call. The coordinator corrects the contract README. The S10 `Create` also gains the duty to record the creating run, and the idempotency principal of a workload is its run.

`Caller` extends the S10 sketch (`Caller{Actor}`) with the authenticated `Tenant`, the attempt's `Project` and `RunOrigin`, and a `RequestID`, because a tool needs the tenant from the principal and never from an argument.

## Checks

```sh
make check-go PKGS='./internal/productmcp/...'
```

| Test | Scenario |
| --- | --- |
| `Test_unit_productmcp_tools_listed` | the SDK client lists the catalogue; schemas, annotations, `idempotencyKey` on creates only, no authority-selecting argument; an attempt sees fewer tools; 401/503 |
| `Test_unit_productmcp_create_project_repository_task` | project, repository, seed guidance and task through MCP; the same calls fail without the role; seed refusals; invalid input reaches no port |
| `Test_unit_productmcp_caller_authority_only` | roles, tenant isolation, attempt confinement, outage fails closed, replay rechecks authority, no detail leaks, the credential is never stored or logged |
| `Test_unit_productmcp_idempotent_create` | replay, changed request, per-principal keys, concurrency, lost record, store outage, key validation, seeds not recorded |
| `Test_unit_productmcp_provisioning_delegation_bounded` | project scope, active-run budget, input bounds, no credential, derived run identity |
| `Test_unit_productmcp_handler_binds_caller`, `Test_unit_productmcp_fails_closed_without_ports` | per-request binding and constructor fail-closed |
| `Test_unit_productmcp_workload_caller_only_provisioning` | a workload caller of any other origin, or with an empty or malformed identity, run or attempt, is refused before a port and with 401 |
| `Test_unit_productmcp_provisioning_replay_before_budget` | lost `Results` record: replay at budget exhaustion, whole-declaration conflict, terminal frees the slot, `Get` hides foreign runs |
| `Test_unit_productmcp_run_created_repositories_and_replacement_attempts` | the creating run is recorded; a replacement attempt converges |
| `Test_unit_productmcp_responses_are_not_cacheable` | `Cache-Control: no-store` and `nosniff` on every response, whatever the SDK sets |
| `Test_unit_productmcp_content_bounds_and_list_limits` | field bounds with `invalid_argument`; a list port returning more than asked fails |
| `Test_unit_productmcp_idempotency_hash_is_normalized` | defaults and spelled-out defaults are one request |

## Open for T-132 and later

Mount `NewHandler` (route and audience), implement `Authenticator` (person token and attempt token with its run's project), the adapters for each port with the caller's identity, the durable `Results` (the HTTP idempotency table or a new one; retention at least 24 hours) and the atomic `MaxActive` check in the provisioning admission. T-154 and T-158 consume the tools for the end-to-end provisioning scenarios (PX-A21, AC-A12).
