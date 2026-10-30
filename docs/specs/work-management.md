# Task tracker and role inbox

**Scope:** native shell task tracking and wait-based inbox under [ADR 0013](../adr/0013-two-layer-architecture-and-domain-simplification.md) and [O-38](../decisions.md#pipeline-runtime-and-four-layer-refinement--2026-10-30-o-38). The pipeline owns process progression; a task records which pipeline it runs and projects that pipeline's execution state.

## Model

`tasks` holds a unit of work or a request. A task has tenant/project ownership, title, Markdown body, optional parent, role/user assignment, context, and one optimistic content version. Repository and branch references are optional.

Every task selects one **project-unique pipeline**. Persistence resolves the name to an immutable pipeline ID at creation. The pipeline's YAML defines the task's process: planning, implementation, review, test, deployment, rework, and human waits. There is no `projects.task_workflow`, transition graph, or state-to-pipeline trigger map.

A task may accumulate several runs through explicit reruns or rework. Exactly one run is its **current execution**. The task exposes a queryable projection of that run:

| Field | Meaning |
| --- | --- |
| `pipeline_id` | The selected project pipeline. |
| `current_run_id` | The selected execution; history remains in `runs.task_id`. |
| `status` | `pending`, `queued`, `running`, `waiting`, `succeeded`, `failed`, or `cancelled`. |
| `current_step_key` | The current step, when one is active or waiting. |
| `waiting_for` | The role or external correlation the current wait expects. |
| `result` | A bounded result reference or summary. |
| `progress_version` | Monotonic projection version used to reject stale updates. |

Run and step records own execution truth. The projection is updated idempotently from versioned runtime events and is rebuildable from those records. Task content edits use their own optimistic version and do not advance execution.

## Example

An illustrative `sdlc` pipeline selects its own branch on a wait result:

```yaml
name: sdlc
steps:
  - name: plan
    image: ghcr.io/sst/opencode:latest
    commands:
      - opencode run "Plan ${KODAMA_TASK_ID}"
  - name: implement
    depends_on: [plan]
    image: ghcr.io/sst/opencode:latest
    commands:
      - opencode run "Implement ${KODAMA_TASK_ID} following /workspace/.context"
  - name: review
    depends_on: [implement]
    wait:
      for: reviewer
      input: approval
      timeout: 72h
  - name: test
    depends_on: [review]
    when: review.outcome == approved
    image: golang:1.27-alpine
    commands:
      - go test -v ./...
  - name: rework
    depends_on: [review]
    when: review.outcome == rejected
    image: ghcr.io/sst/opencode:latest
    commands:
      - opencode run "Rework ${KODAMA_TASK_ID}"
```

The wait syntax above is illustrative; the executable contract freezes exact field names, condition grammar, and timeout behavior before implementation.

## Waiting and resolution

A wait pauses the same run without holding a worker or running a command. It records a unique wait identity, expected input, deadline, accepted result, and continuation position. The kernel persists it; the shell authorizes the actor.

A **role inbox** is a query of tasks whose current wait is assigned to the caller's authorized role. Resolution is an operation against the exact run, step, and wait:

1. Verify the caller's current membership and the wait's assigned role or correlation.
2. Compare the wait version and expected identity.
3. Record an immutable input: actor, outcome, answer, and time.
4. Commit the consumed wait and its outbox intent together.
5. Publish the signal. A redelivery or a signal arriving after restart converges on the same consumed result.

A successful signal delivery is not approval by itself. The recorded outcome determines the pipeline's next branch. A rejection cannot authorize a protected integration or promotion unless the pipeline explicitly expresses that path. A changed candidate requires a new wait and a new applicable decision.

Workspace and run-scoped services have explicit bounded lifetimes during a wait. Loss of required state fails the run rather than replaying earlier commands. A task's current execution cannot advance through a second, independently stored state machine.

## Requirements

| ID | Requirement |
| --- | --- |
| WM-13 | A task MUST select one same-project pipeline by project-unique name, resolved to an immutable pipeline ID; missing, ambiguous, or cross-project references MUST fail. |
| WM-14 | A task MAY accumulate runs, MUST select at most one current execution, and MUST expose a projection of that run's status, step, waiting assignment, and result that is rebuildable from run and step records. |
| WM-15 | A wait MUST pause the same run without a running workload, persist its identity, expected input, deadline, accepted result, and continuation, and MUST NOT hold a worker. |
| WM-16 | Inbox resolution MUST verify current authority and the exact run/step/wait, record an immutable input, and consume the wait transactionally; duplicate or late delivery MUST NOT advance it twice. |
| WM-17 | Task projection updates MUST reject stale run versions, and task content edits MUST use an independent optimistic version. |
| WM-18 | Task, wait, and projection state MUST commit with audit/outbox intent; external effects MUST run outside the transaction and be reconciled after failure or restart. |

WM-01–WM-04 are retired by this amendment. They described the removed project workflow, parking fields, and blocking-task inbox. Historical acceptance reports that cite them remain historical evidence for the superseded model.

## Acceptance

| ID | Scenario | Requirements |
| --- | --- | --- |
| WM-A04 | A project with two pipelines of the same name is refused; a task selects one pipeline and a cross-project or unknown name fails without creating a task | WM-13 |
| WM-A05 | A task's runs accumulate while exactly one is current; stale run events do not regress its status, and rebuilding the projection from run/step records yields the same view | WM-14, WM-17 |
| WM-A06 | A run waits across controller restart without a worker; one authorized result resumes it once, duplicate and late signals change nothing, and a rejection follows the pipeline's rejected branch | WM-15, WM-16 |
| WM-A07 | A lost response after a task or wait commit is reconciled without a duplicate run, duplicate resolution, or missing outbox event | WM-16, WM-18 |

Transport identity checks, human-only actor policy, and real NATS-triggered execution belong to the shell integration checkpoint in the [simplification plan](../implementation/pipeline-runtime-simplification.md).
