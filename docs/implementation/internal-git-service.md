# Internal Git service: implementation plan

**Status:** scheduled by the owner on 2026-10-30 (O-31). Normative design: [ADR 0011](../adr/0011-internal-tenant-git-service.md), [Git repositories](../specs/git-repositories.md), [Git service contract](../contracts/git-service.md). Nothing here is delivered until its task merges; nothing is qualified until T-133 passes.

## Goal

```text
Create project → provision repositories → submit tasks
    → agents implement on branches → review/test/integrate
    → default-branch update → build/deploy

Developer git push → the same CI/CD machinery
```

Tenant isolation holds from the first usable checkpoint.

## Phases and tasks

```text
T-125 docs + ADR                         → T-126 contracts
T-126                                    → T-127 lifecycle, T-128 worker, T-129 hosting
T-127, T-128                             → T-130 push journal + triggers
T-113 (run_shared authority), T-130      → T-131 task/agent/source integration
T-116 (run_shared assembly), T-127…T-131 → T-132 API/CLI/MCP assembly
T-117 (run_shared acceptance), T-132     → T-133 Kubernetes acceptance + cutover
```

| Task | Phase | Owns (summary) | Evidence |
| --- | --- | --- | --- |
| [T-125](../tasks/T-125-git-service-architecture.md) | Documentation and ADR | Specs, contracts, ADRs, glossary, book, examples, task graph | `make docs-check`, review |
| [T-126](../tasks/T-126-git-service-contracts.md) | Contract checkpoint | `contracts/git/`, `contracts/events/` additions | Schema/fixture tests |
| [T-127](../tasks/T-127-repository-lifecycle.md) | Repository lifecycle and persistence | `internal/repository/`, one appended migration | Unit + real PostgreSQL/RLS |
| [T-128](../tasks/T-128-git-worker.md) | Native Git worker | `internal/gitservice/` | Unit + real Git |
| [T-129](../tasks/T-129-tenant-git-hosting.md) | Tenant hosting on Kubernetes | `internal/kube/gitworker/` | Unit + kind |
| [T-130](../tasks/T-130-git-push-events.md) | Push journal, events, triggers | `internal/gitpush/`, `internal/integrations/gitwebhook/` | Unit + real PostgreSQL/Git |
| [T-131](../tasks/T-131-git-source-integration.md) | Task, agent and source integration | Execution, workspace, runtime authority, entrypoint, results, gates | Unit + local checkpoint |
| [T-132](../tasks/T-132-git-service-assembly.md) | API, CLI, MCP, chart assembly | `internal/app/`, CLI, API/client, chart, operations docs | `make api-check`, composition tests |
| [T-133](../tasks/T-133-git-service-acceptance.md) | Kubernetes acceptance and cutover | Acceptance suites, status, traceability | Two-tenant stage acceptance |

T-127, T-128 and T-129 work on disjoint new packages and can proceed in parallel after T-126. T-131 and T-132 edit files that T-113 and T-116 own for the shared run workspace, so they are ordered after those tasks rather than racing them.

## Seams

| Seam | Owner | Transaction / failure rule | Frozen by |
| --- | --- | --- | --- |
| S1 `repository.Store` and lifecycle use cases | T-127 | Repository row, audit and outbox commit together; Git initialization happens outside SQL and is confirmed by the worker | T-126 DTOs |
| S2 `repository.Hosts` port (initialize, read refs, read ref journal and receive liveness, retained refs, maintain) | T-127 consumer, T-128/T-129 adapters | Every call idempotent on repository ID; unknown outcome retried, never assumed | T-126 |
| S3 Forwarding assertion and worker identity | T-126 schema, T-128 verifier, T-132 issuer | Short-lived, audience per tenant worker; the worker resolves paths itself | T-126 |
| S4 Push authorization and journal (`pre-receive`, `reference-transaction`, `post-receive`) | T-130 (control plane), T-128 (hooks) | Authorize and create the record before refs change; commit intent with refs locked, aborting on refusal; confirm with audit and outbox; settle unconfirmed entries from the ref journal and receive liveness | T-126 |
| S5 Git grants for humans and attempts | T-131 (attempt issuance), T-132 (human exchange) | Checked against current authority at every push; revoked with the attempt | T-126 |
| S6 Source set in run admission and runtime context | T-131 | Pinned at admission; versioned context addition; old contexts unchanged | T-126 |
| S7 Trigger admission from `git.refs_updated` | T-130 | Inbox deduplication by `(trigger, push, ref)`; exact pushed commit | Existing gitwebhook admission ledger |

## Reuse

- `dev/gitserver/main.go`: the smart-HTTP wrapper around `git http-backend`, its default-deny endpoint list and its handling of chunked pushes become the starting point of `internal/gitservice/`; fixture tokens are replaced by forwarding assertions and request bodies are streamed with a size limit instead of buffered.
- `test/support/git/`: the HTTPS `git-http-backend` fixture stays the test double for external hosting.
- `internal/integrations/gitwebhook/`: its delivery claim, admission lease, refusal ledger and exact-commit admission are reused for internal push events; the HMAC endpoint stays for external hosts.
- `internal/workspace/`: CAS publication, retained refs and reconciliation stay for external repositories and admitted runs; T-131 adds the internal-hosting path beside them.
- `internal/kube/`: client, preflight and ownership-verification patterns are reused by the new `gitworker` subpackage.

## Migration and cutover

1. T-127 adds repository records and maps each project's current configured repository to a `hosting: external` record.
2. T-132 enables internal hosting for newly created repositories.
3. Import (GR-17, scope pending O-32) copies an external repository, verifies refs and commits, settles active writers and switches `hosting`.
4. T-131's direct agent pushes apply only to internal repositories and to runs admitted after the switch.
5. T-133 accepts the assembled product on two tenants and makes internal hosting the default for new repositories.
6. The bundle/publisher path stays as long as any repository is `hosting: external`, because those repositories keep admitting publishing runs. Retiring it requires a separate owner decision removing external hosting (or disabling publishing runs on external repositories), followed by draining every remaining consumer.

## Out of scope

Provider webhooks and mirroring to external hosts (O-32), a web UI, a direct network path from execution namespaces to workers, sharding a tenant across workers, and untrusted-code isolation (T-055).
