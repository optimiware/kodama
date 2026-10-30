# Pipeline execution and services

**Scope:** Layer 1 Drone CI-inspired pipeline execution, shared `/workspace`, and container services at tenant, project, or pipeline scope ([ADR 0013](../adr/0013-two-layer-architecture-and-domain-simplification.md)).

## Model and states

1. **Pipeline (`pipelines`):**
   - Defined in `.kodama/<processname>.yaml` in Git or registered via API/`kodactl`.
   - The whole parsed YAML definition is persisted once in `pipelines.definition` (JSONB), including triggers. Each run stores an immutable `definition_snapshot`; later edits cannot change it.
2. **Run (`runs`):**
   - One execution of a pipeline against a repository commit and optional `task_id`.
   - States: `queued`, `running`, `succeeded`, `failed`, `cancelled`.
   - Allocates one shared `/workspace` filesystem mounted across all steps of the run.
3. **Step (`run_steps`):**
   - One row per step per run, created when the run is admitted.
   - States: `pending`, `running`, `succeeded`, `failed`, `skipped`, `cancelled`.
   - Runs the declared container `image` and `commands` with mounted project `secrets`, environment variables, and the shared `/workspace`. There is no agent-specific SQL kind. This baseline stores one execution per step; a rerun creates a new run rather than overwriting history.
4. **Scoped Container Services (`services`):**
   - Support containers (such as PostgreSQL, Valkey, mock APIs, or preview servers) that support pipeline runs.
   - Scopes:
     - **`pipeline` scope:** Started with a pipeline run (`services:` block in `.kodama/<processname>.yaml`) and stopped when the run finishes.
     - **`project` scope:** Long-lived support container shared across runs of a project (`project_id` set, `run_id IS NULL`).
     - **`tenant` scope:** Long-lived support container shared across projects of a tenant (`project_id IS NULL`, `run_id IS NULL`).
   - States: `starting`, `ready`, `failed`, `stopped`.

## `run_shared` model

Every pipeline run uses a shared `/workspace` directory across its steps:
- Before the first step command runs, Kodama checks out the target repository commit into `/workspace` (and optionally checks out `<tenant>/context` and `<tenant>/<project>/context` into `/workspace/.context/`).
- Steps execute in topological/declaration order (`depends_on` / `needs`), mounting the same `/workspace` so generated files, build caches, and commits flow naturally from step to step.
- Any step (including `ko`, `buildah`, `opencode`, `claude-code`, or `kodama agent`) runs as an ordinary container workload with authorized secrets injected as environment variables or files under `/kodama/secrets/`.

## NATS coordination and triggers

NATS JetStream triggers and coordinates pipeline runs:
- **Git triggers:** `git.push`, `git.tag`, `git.merge` events match pipeline `triggers` rules.
- **Task state triggers:** When a task enters a state (e.g., `plan`, `implement`, `review`, `test`, `deploy_preview`, `promote`), NATS emits `task.transitioned`, starting the pipeline configured for that state in `projects.task_workflow`.
- **Step & release signals:** Steps and agents emit coordination events (`run.started`, `run.completed`, `artifact.published`) onto NATS.

## Requirements

| ID | Requirement |
| --- | --- |
| PX-01 | The whole `.kodama/<processname>.yaml` definition MUST be validated and persisted on the `pipelines` table; each execution MUST create one `runs` row and one `run_steps` row per step. |
| PX-02 | All steps of a run MUST share the run's `/workspace` filesystem and receive only the secrets and services configured in their project/tenant space. |
| PX-03 | Container `services` MUST be supported at `tenant`, `project`, and `pipeline` scopes and reach `ready` before dependent steps run. |
| PX-04 | Execution MUST use the backend-neutral runtime port (`Kubernetes` in production, `Podman` in test, `Process` in development). |
| PX-05 | Run and step state transitions MUST publish signals to NATS JetStream and handle cancellation and controller restart cleanly. |

## Acceptance

| ID | Scenario | Requirements |
| --- | --- | --- |
| PX-A01 | A `.kodama/test.yaml` pipeline runs multiple steps sharing `/workspace` against a scoped PostgreSQL service | PX-01, PX-02, PX-03, PX-04 |
| PX-A02 | Project-scoped and tenant-scoped services remain available across multiple pipeline runs; pipeline-scoped services are cleaned up when the run ends | PX-03 |
| PX-A03 | Cancelling a run stops active step containers and marks the run `cancelled` | PX-04, PX-05 |
