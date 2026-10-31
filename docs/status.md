# Project status

**Architecture:** [ADR 0013](adr/0013-two-layer-architecture-and-domain-simplification.md) defines a pipeline-runtime kernel and four functional layers ([O-38](decisions.md#pipeline-runtime-and-four-layer-refinement--2026-10-30-o-38)). The kernel owns pipeline execution; the shell owns native Git, tasks, identity, persistence, API/MCP/CLI, and NATS. Each task selects a project-unique pipeline. The current `--assembly core` and `--assembly kernel` paths are transitional; their convergence and legacy deletion are in the [pipeline-runtime simplification plan](implementation/pipeline-runtime-simplification.md).

**Pipeline-runtime position (after P2, [O-39](decisions.md#pipeline-runtime-delivery-decisions--2026-10-31-o-39)).** The kernel and shell capability packages are implemented and tested on the integration branch: the runtime contract, durable engine, memory and PostgreSQL run state, Process, Podman and Kubernetes lifecycle adapters, the project pipeline registry, run-derived Git integration authority, the outbox and NATS bus, and source and secret custody adapters. The new engine, run-state adapters, pipeline registry and custody adapters are not composed into `kodama server`: the executable is still the transitional `core` and `kernel` assemblies, the kernel assembly still runs the old blocking runner, and its task routes still use the project workflow. Only the outbox/NATS publisher, run-derived Git integration authority, the dispatcher's durable name and the source checkout of the old runner are in use there. Task tracking over pipelines (T-184) and the P3 cutover are pending. See the [post-P2 handoff](implementation/pipeline-runtime-handoff.md).

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
| Single composition over the new kernel and shell (P3) | Not started. Needs production `Keys`, `Tips` and vault `Source` implementations, an engine host, task tracking (T-184) and one `internal/app` composition; deletion of the superseded implementation follows cutover (P4), and agent/gateway/MCP-proxy adaptation and the recipe set are a later workstream (P5). Entry point: [handoff](implementation/pipeline-runtime-handoff.md) |
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
