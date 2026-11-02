# Internal Git service: implementation plan

**Status:** revised by the owner on 2026-10-30 (O-36) after the ADR 0013 simplification. Normative design: [ADR 0011](../adr/0011-internal-tenant-git-service.md) as simplified by [ADR 0013](../adr/0013-two-layer-architecture-and-domain-simplification.md), [Git repositories](../specs/git-repositories.md), [Git service contract](../contracts/git-service.md). Nothing here is delivered until its task merges into `integration/native-git`; publication to `main` is a separate owner decision.

## Goal

```text
Create project → create repository → engineer seeds by push
    → agents and engineers push branches → review/integrate
    → default-branch update → build/deploy pipeline
```

This slice targets the transitional ADR 0013 kernel (`kodama server --assembly kernel`) over the current product schema. The [pipeline-runtime simplification plan](pipeline-runtime-simplification.md) replaces that composition with the standalone kernel and native shell. Hosting installation on Kubernetes and full acceptance follow on `main` after the merge.

## What changed from the earlier plan

The earlier graph (T-126–T-164, migrations `007`–`016`) assumed the pre-ADR 0013 server: a push-record journal, Git grants as a separate subsystem, attempt/workspace associations, gate-bound integration and runtime-v3 source admission. ADR 0013 removed the tables and subsystems those tasks persist into. This plan therefore:

- **Keeps** the native worker (`internal/gitservice`), its Git transport, hooks, process fencing, generation lock, request bounds and local receive journal.
- **Keeps** the repository lifecycle concepts (provisioning, seed, archive, policy) but persists them in the existing `repositories` row.
- **Keeps** the tenant hosting renderer (`internal/kube/gitworker`) as implementation, without installation.
- **Replaces** the push-record journal with the worker's local journal plus one acknowledged sequence per repository.
- **Replaces** the grant subsystem with short-lived credentials derived from current membership and run state.
- **Replaces** gate-bound integration with the repository's configured integration pipeline plus the task's approved state.
- **Defers** backups, restore, offboarding, sharding, managed-agent integration and MCP wiring.

Nothing from the earlier work is deleted; the dormant packages stay in the tree and their retirement is part of the wider simplification cleanup.

## Phases and tasks

| Phase | Tasks | Output |
| --- | --- | --- |
| 1. Contracts | [T-166](../tasks/T-166-kernel-git-contracts.md) | Specification, contract, event payload, this plan and the revised backlog |
| 2. Hosting | [T-167](../tasks/T-167-kernel-git-hosting.md) | Repository store and routes, `/git/` forwarding, worker command, credentials, ref policy, receive outcome and event |
| 3. Dispatch | [T-168](../tasks/T-168-push-run-dispatch.md) | Outbox publisher, `.kodama` indexing, trigger matching, run admission pinned to the pushed commit |
| 4. Sources | [T-169](../tasks/T-169-run-git-sources.md) | Pinned checkout, context repositories, run Git credentials in the workspace |
| 5. Runnability | [T-170](../tasks/T-170-kernel-git-closeout.md) | Development and test hosting, bootstrap example, API generation, status |

## One Git service in every environment

There is one implementation, `internal/gitservice` (`kodama git serve`); environments differ only in how it is hosted ([ADR 0009](../adr/0009-environments.md)):

| Environment | Hosting | Notes |
| --- | --- | --- |
| `development` | In the server process, one instance per tenant with a temporary data root | Ephemeral; needs the `git` binary |
| `test` | One `kodama git serve` container per tenant on the Podman services network | Real HTTPS and forwarding assertions |
| `production` | One Deployment and volume per tenant on Kubernetes (T-129, retained) | Installation is deferred with the Kubernetes executor |

Steps reach Git only through the server's `/git/` endpoint with their run credential, whichever backend runs them. `production` refuses the in-process and temporary-directory adapters.

## Persistence

The baseline stays `internal/storage/migrations/001_schema.sql` and `002_grants.sql`. T-167 appends **one** contribution, `003_git_hosting.sql`, adding to `repositories`:

- `policy jsonb NOT NULL DEFAULT '{}'` — `directPushToDefault`, `integrationPipeline`;
- `empty boolean NOT NULL DEFAULT true` — the worker's report;
- `journal_ack bigint NOT NULL DEFAULT 0` — the acknowledged receive-journal sequence.

It also adds the runtime-role grants for the lifecycle columns and the `git_credentials` table holding only credential hashes and their claims. No push-record, grant, workspace-association, gate or attempt-Git table is created; those subsystems belong to the superseded design.

## Migration numbering

The baseline stays `001_schema.sql` and `002_grants.sql`. This slice appends exactly one contribution, `003_git_hosting.sql` (T-167), and nothing else. The earlier reservation of `007`–`016` for repositories, grants, pushes, workspace associations, run sources, triggers, provenance, attempt Git state, push operations and recovery belonged to the superseded design; those numbers are not reused, and the packages that own them keep their code without registering a contribution.

## Seams

| Seam | Owner | Transaction / failure rule |
| --- | --- | --- |
| Repository store and lifecycle | T-167 | The row, audit and outbox commit together; Git initialization happens outside SQL and is confirmed by the worker |
| Worker control-plane port | T-167 | Every call idempotent on repository ID; unknown outcomes retried, never assumed |
| Forwarding assertion and worker identity | T-167 | Short-lived, audience per tenant worker; the worker resolves the on-disk path itself |
| Push authorization (`pre-receive`) | T-167 | Authorize before refs change; any denial fails the whole receive |
| Receive outcome and event | T-167 | The outcome commits the `git.refs_updated` row and `journal_ack` together; the event ID is deterministic per `(repository, receiveId)` |
| Credentials | T-167 | Hashes only; current membership and run liveness are re-read at every use |
| Trigger admission | T-168 | Inbox deduplication by `(pipeline, push event, ref)` through `runs.trigger_key`; the pushed commit is pinned, never a later tip |
| Source pinning and checkout | T-169 | Pinned once in `runs.sources`; a run never follows a moved branch |

## Reuse

- `internal/gitservice/`: the smart-HTTP wrapper, hooks, journal, gate, fencing and object operations, with a new control-plane adapter and a kernel-facing configuration.
- `internal/kube/gitworker/`: the tenant renderer, retained for the deferred installation.
- `internal/integrations/gitwebhook/`: the delivery-claim and deduplication ideas, reimplemented against `audit_events` and `runs.trigger_key` rather than its own tables.
- `internal/repository/` and `internal/gitauth/`: reference implementations for lifecycle and credential shape; their persistence is not reused.
- `dev/gitserver`: the fixture for external-repository compatibility tests only.

## Migration and cutover

1. T-167 adds repository records and the `/git/` route for newly created repositories.
2. T-170 enables internal hosting in development and test; the external fixture stays as an explicit compatibility profile.
3. Existing codebases are bootstrapped by an engineer's push into a new, empty repository under a seed grant (O-32). There is no provider import or mirroring.
4. The bundle/publisher path stays as long as any repository is `hosting: external`.

## Out of scope

Provider import, mirroring and webhooks (O-32), a web UI, sharding a tenant across workers, Kubernetes installation (T-156/T-157), backup/restore/offboarding (T-138), managed-agent and MCP Git tools (T-153/T-155), and untrusted-code isolation (T-055).
