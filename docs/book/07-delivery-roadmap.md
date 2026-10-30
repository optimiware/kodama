# 7. Delivery roadmap

Following [ADR 0013](../adr/0013-two-layer-architecture-and-domain-simplification.md) and its 2026-10-30 pipeline-runtime amendment ([O-38](../decisions.md#pipeline-runtime-and-four-layer-refinement--2026-10-30-o-38)), Kodama is converging on one pipeline authoring model, one execution implementation, and one server composition.

## Simplification roadmap

### Phase 1: Specification and contract reset — in progress

- Establish the four-layer architecture and the pipeline as the sole task process.
- Rewrite the glossary ([CONTEXT.md](../../CONTEXT.md)), book chapters, specifications, contracts, and the [database schema](../implementation/database-schema.md).
- Retire the project workflow, legacy multi-version pipeline/runtime contracts, S3 Git-bundle workspace publishing, Kanban WIP locks, canonical-JSON gates, and the event quarantine/replay subsystem.
- Freeze pipeline identity, task projection, durable waits, protected Git integration, and NATS coordination contracts.

The ordered work is in the [pipeline-runtime simplification plan](../implementation/pipeline-runtime-simplification.md).

### Phase 2: Persistence and native shell — planned

- Remove `projects.task_workflow` and workflow-specific task fields.
- Enforce project-unique pipeline names.
- Add task pipeline/current-run/projection fields and durable wait state on run steps.
- Wire SQL, secret custody, source preparation, Git worker, NATS outbox, triggers, and task projection outside the kernel.
- Replace task-status Git approval with run/step/wait authority.

### Phase 3: Kernel isolation and composition — planned

- Refactor the kernel behind state, runtime, resource, secret, workspace-preparation, log, and clock ports.
- Implement durable progression, waits, cancellation, restart adoption, services, and confirmed cleanup.
- Converge Process, Podman, and Kubernetes behind one contract.
- Wire one normal server composition and publish only served interfaces.

### Phase 4: Deletion and externalization — planned

- Delete the legacy execution stack, workflow engine, obsolete gates/delivery/metrics/workspace engines, and their contract versions.
- Port retained lifecycle, security, and authority tests to their new owners.
- Move delivery, QA, notification, and hook behavior into copyable recipes and examples.
- Keep native Git, secrets, logs, and monitoring helpers that remain part of the shell.

## Environments

Every phase preserves the three execution environments ([ADR 0009](../adr/0009-environments.md)):

- `development`: in-memory adapters and Process runtime for fast unit tests.
- `test`: real PostgreSQL, NATS JetStream, native Git, and Podman containers on a developer machine.
- `production`: Kubernetes, PostgreSQL, NATS JetStream, and cloud KMS (AWS, GCP, or Scaleway).

Deployed `test` and `production` profiles require NATS. `production` refuses development and test adapters.

## Milestones

Package completion is not milestone acceptance. A milestone is accepted only with its declared real-service, runtime, and recovery evidence. Existing M1–M3 acceptance remains evidence for the transitional assemblies; it does not automatically accept the four-layer composition. See [project status](../status.md) and [assembly status](../implementation/assembly-status.md).
