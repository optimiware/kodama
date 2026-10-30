---
id: T-170
milestone: kernel
status: done
depends_on: [T-169]
decision_blockers: []
owns: [api/openapi.yaml, api/client/, api/kernel-git.yaml, api/delivery.yaml, internal/app/apispec/, docs/implementation/assembly-status.md, docs/implementation/database-schema.md, docs/status.md, dev/, internal/kernel/serve/, internal/app/kernel.go, internal/app/kernel_git_api_test.go, internal/app/m1_test.go, internal/app/m2_test.go, internal/app/m3_test.go, internal/kernel/api/git.go, test/integration/assembly-core/, Makefile]
check: make check-go PKGS='./internal/kernel/... ./internal/app/apispec/... ./api/client/...' && make api-check && make test-m1 && make docs-check
resources: [go, integration]
acceptance: [unit_kernel_git_development_hosting, unit_kernel_git_bootstrap_roundtrip, unit_kernel_git_api_document_current]
---
# T-170 — Kernel Git runnability and closeout

## Goal
Make the kernel Git service usable in development and test, prove the bootstrap-to-run path, and publish an accurate API document and status.

## Contract
[Git repositories](../specs/git-repositories.md), [Git service contract](../contracts/git-service.md), [plan](../implementation/internal-git-service.md), O-36. Full container, Kubernetes and end-to-end acceptance runs on `main` after the merge.

## Deliverables
- `development`: in-process per-tenant workers with a temporary data root, seeded from configuration; no external Git credential required.
- `test`: one `kodama git serve` container per tenant, selected explicitly; the external `git-http-backend` fixture stays available as a compatibility profile.
- A short documented bootstrap example: create project, create repository, request a seed, push, see the pipeline indexed and the run start.
- API document and generated client regenerated for the kernel repository and credential routes, including the required `source` regression fix.
- Assembly status and project status updated to state exactly what works and what remains deferred.

## Acceptance
Declared check. A real-Git and real-PostgreSQL smoke of create → seed → push → index → run is recorded in the task evidence.

## Out of scope
Kubernetes installation (T-156/T-157), stage acceptance (T-133), backup/restore, managed-agent and MCP Git tools, full `kodactl` coverage.

## Completion evidence

**Real-PostgreSQL smoke (O-36), run in the task worktree, 2026-10-30.** A throwaway `docker.io/library/postgres:17.11@sha256:b8faeff7712663c2773a190181e1246e37e364b887611c04658c28b44e2408fe` container (`podman run -d --name t170-postgres -e POSTGRES_PASSWORD=… -p 127.0.0.1::5432 …`, PostgreSQL 17.11) and:

```sh
KODAMA_REQUIRE_POSTGRES=1 KODAMA_TEST_POSTGRES_ADMIN_URL='postgres://postgres:<password>@127.0.0.1:<port>/postgres?sslmode=disable' \
  go test ./internal/kernel/serve/ -run 'Test_unit_kernel_git_bootstrap_roundtrip' -count=1 -race -v
```

Result: `Test_unit_kernel_git_bootstrap_roundtrip_postgres` and the in-memory `Test_unit_kernel_git_bootstrap_roundtrip` both PASS. The PostgreSQL leg runs the migrated base schema, `serve.Seed` with the `admin` and `engineer` roles, the kernel's own HTTP routes (create project, create repository, seed credential), the real `git` client, an in-process tenant worker, the outbox publisher and the dispatcher, and reads the indexed pipeline (source `git`, `sourceCommit` = the pushed commit), the admitted run (pinned to the pushed commit) and the refs back through the API. The same run with `go test ./internal/kernel/... ./internal/storage/...` and the same variables passed, including the PostgreSQL-backed store, credential and dispatch tests. The container was removed afterwards. Without the variable the PostgreSQL leg skips.

Not run here, left to `main` per O-36: the Podman/Docker run of the `DEV_GIT=native` worker containers (covered by unit tests against a fake engine only), Kubernetes, stage, and the full end-to-end acceptance.
