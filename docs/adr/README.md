# Architecture decision records

Use an ADR when a choice is costly to reverse, surprising without context, and the result of a real trade-off. Keep the record focused on the decision and its consequences. The [decision register](../decisions.md) also tracks less consequential and unresolved choices.

| ADR | Status | Decision |
| --- | --- | --- |
| [0001](0001-native-kubernetes-jobs.md) | Accepted | Native Kubernetes Jobs for step execution |
| [0002](0002-postgres-state-and-events.md) | Simplified by [0013](0013-two-layer-architecture-and-domain-simplification.md) | PostgreSQL state with NATS JetStream event bus |
| [0003](0003-shared-control-plane.md) | Accepted | Shared control plane with tenant-scoped execution |
| [0004](0004-logical-workspaces.md) | Superseded by [0013](0013-two-layer-architecture-and-domain-simplification.md) | Durable logical workspaces paired with actionable work items |
| [0005](0005-git-backed-workspaces.md) | Superseded by [0013](0013-two-layer-architecture-and-domain-simplification.md) | Git checkpoints, private clones, and controlled source publication |
| [0006](0006-hexagonal-domain-design.md) | Accepted | Prefer lightweight hexagonal ports/adapters and DDD entities |
| [0007](0007-platform-core-and-agent-layer.md) | Accepted | Platform is the company core; Kodama owns the agent/SDLC layer |
| [0008](0008-runtime-seam.md) | Accepted | One runtime seam: Process for tests, Podman locally, Kubernetes for staging |
| [0009](0009-environments.md) | Accepted | Three environments: development in memory, test on Podman, production on Kubernetes |
| [0010](0010-run-workspace-services-builders.md) | Simplified by [0013](0013-two-layer-architecture-and-domain-simplification.md) | Shared run workspace, scoped container services, and rootless builders |
| [0011](0011-internal-tenant-git-service.md) | Simplified by [0013](0013-two-layer-architecture-and-domain-simplification.md) | Internal tenant Git service with Gerrit-style `<tenant>/<project>/*` hierarchy |
| [0012](0012-container-sdlc-and-artifact-releases.md) | Superseded by [0013](0013-two-layer-architecture-and-domain-simplification.md) | Container-first SDLC and metadata-only external artifacts |
| [0013](0013-two-layer-architecture-and-domain-simplification.md) | Accepted | Four functional layers: pipeline-runtime kernel, native shell, agentic services, and external behavior; one pipeline process per task |

[ADR 0013](0013-two-layer-architecture-and-domain-simplification.md) is the governing architecture baseline for Kodama. Its 2026-10-30 amendment replaces the original two-layer model: the kernel is the standalone pipeline runtime, the shell owns native Git, tasks, persistence, API/MCP/CLI, and NATS, and each task selects a project-unique pipeline. Earlier ADRs remain in this directory for historical context.
