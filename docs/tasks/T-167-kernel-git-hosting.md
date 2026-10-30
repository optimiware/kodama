---
id: T-167
milestone: production
status: done
depends_on: [T-166]
decision_blockers: []
owns: [internal/gitservice/, internal/kernel/store/, internal/kernel/api/, internal/kernel/serve/, internal/storage/, contracts/git/, internal/app/kernel.go, internal/app/cli.go, internal/app/app.go, internal/app/git_commands.go, internal/app/git_commands_test.go, internal/app/migrate_test.go, internal/app/sdlc_test.go, internal/events/postgres/migrations_test.go, internal/integrations/chatops/postgres/migrations_test.go, internal/artifacts/postgres/migrations_test.go, internal/identity/postgres/isolation_test.go, internal/runtimeauth/postgres/resource_test.go, docs/implementation/database-schema.md, docs/implementation/simplification-phase-2.md, docs/implementation/assembly-status.md, internal/storage/README.md]
check: go run ./dev/cmd/repoctl fmt ./internal/gitservice/... ./internal/kernel/... ./internal/storage/... ./contracts/git/... ./internal/app && go vet -tags unit ./internal/gitservice/... ./internal/kernel/... ./internal/storage/... ./contracts/git/... ./internal/app && { go test -json -race -count=1 -tags unit ./internal/gitservice/... ./internal/kernel/... ./internal/storage/... ./contracts/git/... > "$FORGE_RESULTS"; a=$?; go test -json -race -count=1 -tags unit -run 'Test_unit_kernel_git_serve_command' ./internal/app >> "$FORGE_RESULTS"; b=$?; [ $a -eq 0 ] && [ $b -eq 0 ]; }
resources: [go]
acceptance: [unit_kernel_repository_create_assigns_project, unit_kernel_git_route_resolves_path, unit_kernel_git_forwarding_assertion, unit_kernel_git_credential_current_authority, unit_kernel_git_seed_single_use, unit_kernel_git_default_branch_protected, unit_kernel_git_receive_event_once, unit_kernel_git_journal_ack_reconcile, unit_kernel_git_serve_command]
---
# T-167 — Kernel Git hosting, authority and receive events

## Goal
Serve native Git for kernel repositories: repository records and routes, `/git/` forwarding, the tenant worker command, short-lived credentials, ref policy, and one durable push event per receive.

## Contract
[Git repositories](../specs/git-repositories.md), [Git service contract](../contracts/git-service.md), [plan](../implementation/internal-git-service.md), O-36. Reuse `internal/gitservice` as the worker; do not rebuild Git transport, hooks, fencing or the local journal.

## Deliverables
- `003_git_hosting.sql` appended after `002_grants.sql`: `repositories.policy`, `repositories.empty`, `repositories.journal_ack`, the `git_credentials` hash table, runtime-role grants and RLS.
- Kernel repository store and routes: create/list/get/patch/archive/restore/refs for tenant and project scope; project association from the requested scope; idempotent provisioning confirmed by the worker.
- `/git/<tenant>/<project>/<name>.git` route: credential verification, path-to-repository resolution, forwarding assertion issuance, streaming to the tenant worker, and the same 404 for unknown, foreign and unauthorized paths.
- Credential exchange (`POST /tenants/{tenantKey}/git-credentials`) for people and seeds; run credentials derived from the run's pinned sources and current liveness; revocation when the run ends.
- `pre-receive` authorization adapter over current membership, run state, seed rules, repository policy and the integration constraint.
- `post-receive` outcome: one deterministic `git.refs_updated` audit row with inline updates plus the acknowledged journal sequence; reconcile unacknowledged receives from the worker journal.
- `kodama git serve` and `kodama git-hook` commands; in-process per-tenant hosting for `development`; production refuses the temporary-directory adapter.

## Acceptance
Declared check. It is the scoped Go gate (`gofmt`, `go vet -tags unit`, `go test -json -race -tags unit`) over the listed packages, written out as raw `go test -json` so that a second invocation can append the `unit_kernel_git_serve_command` test of `internal/app`: that package also holds unrelated, pre-existing stale-OpenAPI failures, so only the named test is run there. Focused unit coverage of the named scenarios; the assembled path is smoked by T-170.

## Out of scope
Pipeline indexing and trigger admission (T-168), checkout and run credentials in the workspace (T-169), Kubernetes installation, backup/restore/offboarding, CLI commands beyond the server roles.

## Completion evidence
Implemented by a subagent; independent review requested changes on integration approval, recovery evidence, paging, assignment starvation, project-key authorization, default-branch HEAD and the declared check; all were fixed and re-verified. Commits `3d01f401`, `601ae474`, `b537a182`, `a95dbe8c`, `7aa03f45`. Coordinator checks on `7aa03f45`: `make check-task TASK=T-167` PASS (9 acceptance names); `go build ./cmd/... ./internal/...` PASS; `make validate-tasks` PASS. Real SQL was exercised against a throwaway PostgreSQL 17.11 instance. Merged into `integration/native-git` as `3c3eca18`; nothing pushed.

Open: the dormant `gitv1` types and the kernel DTOs remain separate; the contract documents the kernel shapes. The worker identity token for a standalone container worker is read from a file and nothing mints it yet (deferred to installation).
