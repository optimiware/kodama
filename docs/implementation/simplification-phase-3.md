# Simplification Phase 3 handoff

> **Historical (2026-10-30):** this handoff describes the transitional `--assembly kernel` slice. [ADR 0013](../adr/0013-two-layer-architecture-and-domain-simplification.md) was subsequently amended by [O-38](../decisions.md#pipeline-runtime-and-four-layer-refinement--2026-10-30-o-38): the target kernel is the standalone pipeline runtime, the shell owns native Git/tasks/API/MCP/CLI/NATS, and each task selects a project-unique pipeline. Current work is in the [pipeline-runtime simplification plan](pipeline-runtime-simplification.md).

**Branch/worktree:** `task/simplification-v2`, `.work/simplification-v2`.

**Scope delivered:** the first working vertical slice of the ADR 0013 kernel —
identity/RBAC bootstrap, projects and workflows, API-registered `.kodama`
pipelines, secrets with envelope encryption, run admission, sequential step
execution on a shared workspace with two backends, retained logs, metadata-only
artifacts, and the task/role-inbox loop. It runs as
`kodama server --assembly kernel` against the current 16-table product schema.

## Implemented

| Area | Package | Notes |
| --- | --- | --- |
| Pipeline format | `internal/kernel/pipeline` | strict YAML parse, diagnostics, canonical JSON, deterministic order |
| Secret envelope | `internal/kernel/secret` | per-revision data key wrapped by `pkg/secretkeys`, ownership-bound AAD |
| Persistence | `internal/kernel/store` | tenant-scoped SQL over the ADR 0013 tables |
| Execution | `internal/kernel/runner` | sequential steps, one workspace per run, Podman and Process backends |
| HTTP API | `internal/kernel/api` | projects, pipelines, runs, logs, tasks/inbox, secrets, artifacts |
| Assembly | `internal/kernel/serve` | pool, keys, auth, scheduler loop, `kodama seed` |
| Schema completion | `001_schema.sql` / `002_grants.sql` | operational columns, `kodama_tenant_lookup`, narrow grants |

Authentication is OIDC bearer verification, or an explicit development token
that production refuses. Authorization requires a current membership; inbox
resolution additionally requires the request's assigned role (admin allowed).

## Verified end to end

Real PostgreSQL 17.11 (Podman) plus the built binary:

1. `kodama migrate` applied `001_schema.sql` and `002_grants.sql`.
2. `kodama seed --tenant acme --user-id u_dev` created tenant, principal and admin binding.
3. `GET /me` and `GET /tenants` returned the verified principal and tenant role.
4. A project was created with the default workflow.
5. A project secret was stored; list/set never returned plaintext.
6. A two-step pipeline ran: step one wrote a file and read the mounted secret,
   step two verified the same file from the shared workspace.
7. Logs were retained and served per step.
8. With `KODAMA_EXECUTOR=podman`, the same run executed in containers sharing a
   named volume, with the secret directory bind-mounted read-only.
9. Task flow: create → transition → park behind a `reviewer` request →
   inbox query → approve → resume to the target state.
10. Rejected promotion request resolves but does not resume the parked task.

## Not yet implemented

- Native Git service and `.kodama/*.yaml` indexing/triggers.
- NATS publishing and task-triggered run dispatch (the scheduler wakes on HTTP
  admission and cancellation only).
- Tenant/project long-lived `services` beyond the schema.
- Agent runtime, AI gateway and MCP gateway.
- `kodactl` command surface for the kernel routes (the HTTP API is usable with
  curl; the existing CLI still targets the legacy model).
- Removal of the legacy packages and the legacy `--assembly core` path.

The legacy composition still exists and is refused against the new schema by the
startup guard; `--assembly kernel` is the executable path. Unit tests are as
green as `main` (two pre-existing failures in `internal/runtime/podman` and
`internal/runtime/process` reproduce unchanged on `main`).

## Environment variables

| Variable | Purpose |
| --- | --- |
| `KODAMA_ASSEMBLY=kernel` or `--assembly kernel` | select the kernel server |
| `KODAMA_DATABASE_RUNTIME_URL` | non-owner runtime role |
| `KODAMA_DATABASE_MIGRATION_URL` | owner role, for `migrate` and `seed` |
| `KODAMA_INSTALLATION_ID` | secret-envelope AAD binding |
| `KODAMA_EXECUTOR` | `podman` (default) or `process` |
| `KODAMA_STATE_DIR`, `KODAMA_LOG_DIR` | workspaces, secret temp dir, retained logs |
| `KODAMA_SECRETS_KEYFILE` | keyfile KEK (required in production) |
| `KODAMA_DEV_TOKEN`, `KODAMA_DEV_USER_ID` | explicit development credential |
| `KODAMA_OIDC_ISSUER`, `KODAMA_OIDC_CLIENT_ID` | OIDC bearer verification |

On macOS, `KODAMA_STATE_DIR` must be a path shared with the Podman machine
(for example `$HOME/Library/Caches/...`); the system temp directory is not.

## Checks

```sh
make build
go test -tags unit -count=1 ./cmd/... ./internal/... ./api/... ./contracts/... ./pkg/... ./test/support/...
make docs-check
git diff --check
```

The two runtime-package failures above are pre-existing on `main` and are not
caused by this slice.
