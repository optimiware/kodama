# Project status

**Simplification branch:** [ADR 0013](adr/0013-two-layer-architecture-and-domain-simplification.md) replaces the legacy model. Phase 2 supplied the 15-table schema and task domain. Phase 3 adds `kodama server --assembly kernel`: projects, API-registered `.kodama` pipelines, envelope-encrypted secrets, sequential container runs with a shared workspace, retained logs, artifact metadata and the task/role-inbox loop, verified end to end on PostgreSQL and Podman. Native Git hosting is now wired into it (below). Long-lived services, agents/gateways and a full `kodactl` remain. The legacy `--assembly core` still refuses the new schema. See [Phase 3 handoff](implementation/simplification-phase-3.md).

Kodama is **early and not production-ready**. The core platform works end to end on a developer machine. Production deployment, key recovery and hosted use are still ahead. This page says plainly what is which.

## Works today

Verified by acceptance suites that run the real server against PostgreSQL, NATS JetStream, S3, Git and OIDC, with pipeline steps run as Podman containers.

| Area | State |
| --- | --- |
| Work items, projects, memberships, RBAC | Working |
| Git workspaces with controlled publication | Working |
| Container pipelines: runs, retries, cancellation, restart recovery, logs, artifacts | Working ([M1 report](acceptance/m1.md)) |
| Variables and encrypted secrets | Working |
| CLI and generated Go client for the HTTP API | Working |
| Agent steps, human gates, environments and deployments | Working with deterministic test agents ([M2 report](acceptance/m2.md)) |
| Native Git push trigger | Working |
| Kernel Git hosting (`--assembly kernel`) | Working in unit and in-process checks, not yet qualified against real PostgreSQL, Podman or the stage. A project's repositories are created and listed over the API, an engineer requests a seed credential and pushes with `git`, a push becomes one `git.refs_updated` event, `.kodama` pipelines are indexed from the pushed commit, matching triggers admit runs pinned to it, and a run checks out its pinned sources with a run-scoped credential. `development` hosts one in-process worker per tenant with no external Git credential; a worker container per tenant for the `test` environment is wired in the dev tooling. See [assembly status](implementation/assembly-status.md#kernel-assembly-git-hosting-t-166t-170) and the [bootstrap example](../dev/README.md#bootstrap-example-project-repository-seed-push-pipeline-run) |
| Dependencies between work items, flow metrics, ChatOps notifications | Working ([M3 report](acceptance/m3.md)); a human cannot yet move an item from review to done through the assembled server |
| Kubernetes Job runtime and Helm chart | The kernel chart and `test/e2e/kube-kernel` suite are implemented; a fresh-stage readiness receipt has not been recorded yet |

## Not yet

| Area | Where it stands |
| --- | --- |
| Production profile | `KODAMA_ENV=production` selects the cloud KMS adapter; the stage kernel profile is deployable with the O-37 embedded-Git test-stage deviation. A supported production profile is not claimed: TLS hardening (O-27), per-tenant Git workers, recovery and PITR, and hosted authority remain |
| Installed server on Kubernetes | The ADR 0013 kernel chart (`deploy/charts/kodama-kernel`) and `test/e2e/kube-kernel` qualification suite are implemented for a fresh Scaleway test stage. A passing live receipt is still required; the previous core-era T-104/T-090 reports are historical and are not relabelled |
| Source, data and key recovery drills | Planned |
| Live model and coding-agent qualification | Agents are tested with scripted responses only |
| Kernel Git: Kubernetes installation, backup/restore/offboarding, managed-agent and MCP Git tools, full `kodactl`, provider import, stage acceptance | Deferred (O-36). The kernel serves Git over the API and `/git/` today; installing the per-tenant workers on Kubernetes (T-156/T-157), recovery of tenant Git volumes, Git for managed agents and MCP tools, complete CLI coverage, importing from external providers and stage acceptance (T-133) are not delivered. The `test` environment's container profile and the real-PostgreSQL smoke run on `main` after the merge |
| Optimi Platform (hosted) integration | Design target; standalone identity works today |
| Other agent harnesses, untrusted multi-tenant isolation, UI, Supabase blueprint, browser QA | Deferred |
| Shared run workspace, support services and ko/Buildah builds (`run_shared`, ADR 0010 / O-30) | Implemented and composed for Kubernetes (off by default); the builder node profiles are installed and qualified on the stage cluster ([profile report](acceptance/run-resource-profile.md)). The assembled-product stage acceptance and cutover (T-117) is **parked** until stage access is provisioned on the operator machine; see [resume checklist](tasks/T-117-run-resource-acceptance.md#parked-2026-10-30-resume-checklist). BuildKit is deferred (T-123) |
| Native release/version catalogue and tool-neutral delivery | [ADR 0012](adr/0012-container-sdlc-and-artifact-releases.md) accepted as direction; [T-140–T-146](implementation/container-sdlc-refinement.md) prepared for planning. Existing verified file/OCI artifacts are implemented; release discovery/fetch/publication and generic deployment/rollback are not yet delivered |

The full list of remaining work is the [task backlog](tasks/README.md). The technical record of how the server is composed, with every route, worker and unwired capability, is [assembly status](implementation/assembly-status.md).

## Try it

```sh
make test-m1     # the whole M1 slice in memory: no containers, no services, about 30 seconds
make build       # builds dev/.state/bin/kodama
dev/.state/bin/kodama --help
```

`kodama server` starts a health-only scaffold unless started with `--assembly core`, which needs PostgreSQL, JetStream, S3, Git and an OIDC issuer. The [development runbook](../dev/README.md) brings those up locally with Podman.
