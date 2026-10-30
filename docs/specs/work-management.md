# Task tracker and role inbox

**Scope:** project-defined task workflows and asynchronous inbox requests under [ADR 0013](../adr/0013-two-layer-architecture-and-domain-simplification.md). Phase 2 implements the model and transactional persistence; API dispatch and NATS consumers follow in Phase 3.

## Model

`tasks` holds a unit of work or a decision/promotion/incident request. A task has tenant/project ownership, title, Markdown body, state, optional parent, role/user assignment, context and one optimistic version. Repository and branch references are optional. A task's state comes from its project's workflow, not a product-wide enum.

Example project workflow (illustrative process names; configured pipelines are resolved by Phase 3):

```json
{
  "initialState": "plan",
  "states": ["plan", "implement", "review", "test", "preview", "promote", "done", "question"],
  "terminalStates": ["done"],
  "inbox": {"openState": "question", "resolvedState": "done"},
  "transitions": [
    {"from": "plan", "to": "implement"},
    {"from": "implement", "to": "review"},
    {"from": "review", "to": "implement"},
    {"from": "review", "to": "test"},
    {"from": "test", "to": "preview"},
    {"from": "preview", "to": "promote"},
    {"from": "promote", "to": "done"}
  ],
  "triggers": {"plan": "plan", "implement": "implement", "review": "review", "test": "test", "preview": "preview", "promote": "promote"}
}
```

State triggers have one source: the workflow's `triggers` map. They apply on initial creation, entry into a state and resumption in that state. A request's inbox state has no automatic coding trigger. Resolved inbox requests use the dedicated resolution operation rather than a general task transition.

## Parking and resolution

Parking preserves the business state and sets `parked_at`, `blocked_by_id` and `resume_state`. These are fields on the same task, not a second workspace/gate lifecycle. A role inbox is a query for unresolved request tasks assigned to the caller's authorized role.

1. Create a decision/promotion/incident task in the project's inbox open state, assigned to a role. In the same transaction, park the work task behind that request and record an event.
2. End the current run normally; a human wait does not retain scratch or a running step.
3. Resolution records the verified actor, assigned role, an `approved`, `rejected` or `answered` outcome, answer and timestamp. Human-only production approval is enforced by the use case's actor policy, not merely by a role string supplied by a workload.
4. An approval may resume the blocked task. An `answered` decision may resume clarification work. A rejected request or an `answered` promotion cannot resume production promotion. Explicit rework creates a fresh request or follows another project transition.
5. Resume rechecks the current workflow edge and task version, clears parking fields and emits the next state trigger. Resuming the same state can start its pipeline again with the recorded answer.

The request remains linked by history/events after resumption. Resolved decisions are immutable through the store. Candidate context/repository/branch cannot change while a task is parked; changed inputs need a new applicable request. Duplicate delivery cannot repeat a transition against a newer task version.

## Requirements

| ID | Requirement |
| --- | --- |
| WM-01 | Each project MUST persist its states, terminal states, edges, state triggers and inbox states in `projects.task_workflow`. |
| WM-02 | Transitions MUST validate the workflow and expected version; a parked task MUST NOT advance through an ordinary transition. |
| WM-03 | Parking MUST link a same-project unresolved request assigned to a role. Parent, request and event changes MUST commit atomically. |
| WM-04 | Resolution MUST verify caller authority and record an explicit immutable outcome. Only an applicable affirmative result MAY resume the blocked task; rejection MUST NOT promote it. |

## Acceptance

| ID | Scenario | Requirements |
| --- | --- | --- |
| WM-A01 | A project with entirely custom state names validates its transitions; duplicate/unknown states, stale versions and invalid edges fail | WM-01, WM-02 |
| WM-A02 | Park, resolve and resume persist with their events; rollback loses neither half; concurrent updates have one winner | WM-02, WM-03, WM-04 |
| WM-A03 | Rejection, wrong-role resolution, changed parked context and cross-project blockers cannot promote a task; answering a clarification can re-trigger the same state's process | WM-03, WM-04 |

The shared memory/PostgreSQL contract under `internal/tasks/taskstest` verifies persistence and scope. Transport identity checks, human-only actor policy and real NATS-triggered execution remain Phase 3 acceptance.
