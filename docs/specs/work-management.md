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
| `waiting_for` | The current wait's assignment, when it has one. Correlation values belong to the wait and are not projected. |
| `result` | A bounded result reference or summary. |
| `progress_version` | Monotonic projection version used to reject stale updates. |

Run and step records own execution truth. The projection is updated idempotently from versioned runtime events and is rebuildable from those records. Task content edits use their own optimistic version and do not advance execution.

## Example

The [pipeline format](../contracts/pipeline-format.md#example-kodamasdlcyaml) contains an illustrative `sdlc` pipeline with plan, implement, optional clarification, review wait, test, protected integration, and rework steps. The pipeline, not the task, selects each branch from the recorded wait outcome.

## Inbox and resolution

Wait lifecycle, decisions, deadlines and the separation of assignment from correlation are owned by [pipeline execution](pipeline-execution.md#wait-decisions); the task tracker neither stores nor derives them. The shell owns who may see and decide a wait.

A **role inbox** is a query of tasks whose current wait is *assigned* to a role the caller currently holds. Only assignment grants visibility: correlation values identify what is decided and never confer access, and a wait with no assignment has no inbox entry. An entry is bound to one wait identity, its version, its request text and its display of the correlation (for example the candidate commit).

Resolution addresses the exact run, step and wait:

1. Authenticate the actor and verify current membership of the assigned role.
2. Check the wait identity, the caller's expected wait version, an outcome from the wait's declared set (never a reserved one), and an answer of at most 4 KiB.
3. Submit one decision carrying a caller key. With the tenant, project and actor it forms the signal identity ([signal identity and routing](pipeline-execution.md#signal-identity-and-routing)), and the actor, outcome, answer and time are recorded immutably.
4. Return the result. A retry of a recorded key returns its recorded result even after the wait closed; a new request that lost a race to another resolution, expiry or cancellation returns that recorded decision unapplied.

Reassignment changes only the assignment: an authorized, version-checked, audited operation that removes the previous assignee's authority at once and leaves identity, correlation and outcomes unchanged. A changed candidate appears as a new wait and a new inbox entry; a decision on an earlier wait is never presented as applying to it, and a new attempt to resolve a closed or earlier wait is refused as stale without touching the current one, while a replay of a recorded key still returns its recorded result.

A successful resolution is not approval by itself. The recorded outcome selects the pipeline's next branch, and rejection has no special effect beyond that outcome ([rejection and joins](pipeline-execution.md#eligibility-skipping-and-joins)). A task's current execution cannot advance through a second, independently stored state machine.

## Requirements

| ID | Requirement |
| --- | --- |
| WM-13 | A task MUST select one same-project pipeline by project-unique name, resolved to an immutable pipeline ID; missing, ambiguous, or cross-project references MUST fail. |
| WM-14 | A task MAY accumulate runs, MUST select at most one current execution, and MUST expose a projection of that run's status, step, waiting assignment, and result that is rebuildable from run and step records. |
| WM-15 | *Superseded by [PX-31](pipeline-execution.md#requirements) and PX-41, which own wait persistence and decisions.* |
| WM-16 | *Superseded by WM-19, WM-20 and [PX-41](pipeline-execution.md#requirements).* |
| WM-17 | Task projection updates MUST reject stale run versions, and task content edits MUST use an independent optimistic version. |
| WM-18 | Task, wait, and projection state MUST commit with audit/outbox intent; external effects MUST run outside the transaction and be reconciled after failure or restart. |
| WM-19 | The inbox MUST list a wait only to callers who currently hold its assigned role, MUST bind each entry to the wait identity and version, and MUST NOT grant access through correlation; a wait without assignment MUST have no inbox entry. |
| WM-20 | Resolution MUST verify current authority, the exact run/step/wait, the expected version, a declared outcome and a bounded answer, MUST record an immutable actor input, and MUST be submitted as the single PX-41 decision under a caller key forming the PX-43 signal identity; a replay of a recorded key MUST return its recorded result, including after the wait closed. |
| WM-21 | Reassignment MUST be authorized, version-checked and audited, MUST revoke the previous assignee's authority at once, and MUST NOT change wait identity, correlation or declared outcomes. |
| WM-22 | A changed candidate MUST appear as a new wait and inbox entry; a decision on one wait MUST NOT be presented as applying to another, and a new request to resolve a closed or earlier wait MUST be refused as stale; a replay of a recorded key is not a new request. |

WM-15 and WM-16 are retained by identifier as superseded and are not renumbered. WM-01–WM-04 are retired by this amendment. They described the removed project workflow, parking fields, and blocking-task inbox. Historical acceptance reports that cite them remain historical evidence for the superseded model.

## Acceptance

| ID | Scenario | Requirements |
| --- | --- | --- |
| WM-A04 | A project with two pipelines of the same name is refused; a task selects one pipeline and a cross-project or unknown name fails without creating a task | WM-13 |
| WM-A05 | A task's runs accumulate while exactly one is current; stale run events do not regress its status, and rebuilding the projection from run/step records yields the same view | WM-14, WM-17 |
| WM-A06 | A run waits across controller restart without a worker; one authorized result resumes it once, duplicate and late signals change nothing, and a rejection follows the pipeline's rejected branch | WM-20 |
| WM-A07 | A lost response after a task or wait commit is reconciled without a duplicate run, duplicate resolution, or missing outbox event | WM-20, WM-18 |
| WM-A08 | A user without the assigned role, a user holding a different role, and a caller who knows only the correlation values cannot see or resolve the wait; a user who loses the role stops seeing it at once | WM-19, WM-20 |
| WM-A09 | Two reviewers submit different outcomes concurrently; the first commit decides, a retry with the winner's idempotency key returns its result, and the other reviewer receives the recorded decision unapplied; an undeclared or reserved outcome is refused | WM-20 |
| WM-A10 | Reassigning a wait removes the old assignee's authority and leaves identity and correlation unchanged; a new commit produces a new wait and inbox entry, and the earlier review cannot be applied to it | WM-21, WM-22 |
| WM-A11 | After a reviewer's approval closes a wait, a retry with the same key returns the recorded approval, including after a later wait with an identical correlation has opened; a fresh key against the closed wait is refused as stale; the same reviewer resending a changed outcome under that key is refused as key reuse without altering the recorded decision | WM-20, WM-22 |
| WM-A12 | A second reviewer submits the first reviewer's key after the wait closed: the identity includes the caller, so it is an independent signal and receives the ordinary closed-wait refusal, and the first reviewer's recorded result is unchanged | WM-20, WM-22 |

Transport identity checks, human-only actor policy, and real NATS-triggered execution belong to the shell integration checkpoint in the [simplification plan](../implementation/pipeline-runtime-simplification.md).
