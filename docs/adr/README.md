# Architecture decision records

Use an ADR when a choice is costly to reverse, surprising without context, and the result of a real trade-off. Keep the record focused on the decision and its consequences. The [decision register](../decisions.md) also tracks less consequential and unresolved choices.

| ADR | Status | Decision |
| --- | --- | --- |
| [0001](0001-native-kubernetes-jobs.md) | Accepted | Native Kubernetes Jobs for step execution |
| [0002](0002-postgres-state-and-events.md) | Accepted | PostgreSQL state with an outbox and NATS event delivery |
| [0003](0003-shared-control-plane.md) | Accepted | Shared control plane with tenant-scoped execution |
| [0004](0004-logical-workspaces.md) | Accepted | Durable logical workspaces paired with actionable work items |
| [0005](0005-git-backed-workspaces.md) | Accepted | Git checkpoints, private clones, and controlled source publication |
| [0006](0006-hexagonal-domain-design.md) | Accepted | Prefer lightweight hexagonal ports/adapters and DDD entities |
| [0007](0007-platform-core-and-agent-layer.md) | Accepted | Platform is the company core; Kodama owns the agent/SDLC layer |
| [0008](0008-runtime-seam.md) | Accepted | One runtime seam: Process for tests, Podman locally, Kubernetes for staging |
| [0009](0009-environments.md) | Accepted | Three environments: development in memory, test on Podman, production on Kubernetes |
| [0010](0010-run-workspace-services-builders.md) | Proposed | One disposable run workspace, run-scoped support services, and a rootless builder baseline |

Accepted records capture owner-confirmed choices. Proposed records capture the baseline recommended for implementation. Detailed behavior remains in the relevant specifications. [ADR 0010](0010-run-workspace-services-builders.md) does not replace [ADR 0005](0005-git-backed-workspaces.md) or [ADR 0008](0008-runtime-seam.md) until its contract checkpoint is accepted.
