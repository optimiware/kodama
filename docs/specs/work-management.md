# Work management

**Scope:** M1 work items and human completion; M3 dependency links and flow reporting. This is the simplified implementation baseline.

## Model

A work item has tenant/project ownership, kind (`epic`, `story`, `task`, `defect`), title, `spec` Markdown, structured `acceptance_criteria`, assignee, state, `blocked_reason`, `version`, and `content_revision`. The plan is a section of `spec`. Each content edit appends the previous/current content to history and increments `content_revision`; every mutation increments `version`.

A project has `wip_limit_active`. Roles grant transitions; there is no configurable flow-policy engine. A defect is an ordinary work item with evidence links. An epic groups children and has no workspace. M3 adds same-project dependency edges and cycle detection.

## Transitions

| From | To | Condition |
| --- | --- | --- |
| `backlog` | `ready` | Specification and acceptance supplied |
| `ready` | `backlog` | Reason supplied |
| `ready` | `active` | Active WIP capacity; no `blocked_reason`; M3 dependencies satisfied |
| `active` | `ready` | No unsettled writer attempt; reason supplied |
| `active` | `review` | Candidate and evidence available, or child summary for an epic |
| `review` | `active` | Rework reason; active WIP capacity |
| `review` | `done` | Human with `work_item:complete` records acceptance and any acknowledged findings |
| `done` | `active` | Reopen reason; active WIP capacity |
| Any unfinished state | `cancelled` | Authorized cancellation and reason |

Blocked items still count toward active WIP. Admission locks the project row and checks/counts in the same transaction as the transition. A request to leave active while a writer remains returns `409 workspace_busy`; the caller cancels the run and retries after settlement. Business cancellation may commit immediately while runtime cancellation proceeds separately.

## Requirements

| ID | Requirement |
| --- | --- |
| WM-01 | Ownership and relationships MUST be tenant/project consistent. |
| WM-02 | Content changes MUST append history and use `If-Match`; conflicting edits MUST NOT overwrite each other. |
| WM-03 | Transitions MUST check allowed edges, role, expected version, and current prerequisites transactionally. |
| WM-04 | All entries into active MUST enforce `wip_limit_active` under contention. |
| WM-05 | M3 dependency edges MUST be same-project, acyclic, and evaluated at admission. |
| WM-06 | Setting/clearing `blocked_reason` MUST retain actor and timing in history/audit. |
| WM-07 | Completion MUST be an explicit human decision bound to the observed content revision and candidate; a passing run cannot complete work. |
| WM-08 | Mutations MUST atomically record audit and outbox events. |
| WM-09 | Cancellation MUST prevent new work dispatch and request cancellation of current work runs. |
| WM-10 | Reads/lists MUST be authorized, filtered, and cursor-paginated. |
| WM-11 | Each actionable item MUST have at most one logical workspace; epics have none. |
| WM-12 | M3 SQL reports MUST derive active/review/blocked intervals and reopens from recorded history. |

The human is the business completion policy. Required pipeline gates still guard the corresponding pipeline actions. Completion does not create a generic gate override; waived business findings are named in the completion reason.

## Interface and recovery

Use work-item CRUD, `GET history`, and `POST transitions` in the [HTTP contract](../contracts/http-api.md). State changes emit `work_item.transitioned`; content/blocking changes emit `work_item.updated`. No separate revisions, blockers, findings, or flow-policy resources are needed.

Analysts edit intent, engineers initiate technical work, and reviewers accept outcomes. Database commit is the mutation boundary. On uncertain responses, reread the resource/version before retrying an edit. Run creation has a separate idempotency contract.

## Acceptance

| ID | Scenario | Requirements |
| --- | --- | --- |
| WM-A01 | Cross-tenant relations and copied cursors reveal no data | WM-01, WM-10 |
| WM-A02 | Concurrent content edits preserve one accepted edit and one conflict | WM-02, WM-08 |
| WM-A03 | Two admissions compete for one WIP slot; rework/reopen use the same guard | WM-03, WM-04 |
| WM-A04 | M3 rejects dependency cycles and blocks dependent admission | WM-05 |
| WM-A05 | A passing build cannot complete work; a current authorized human can | WM-07 |
| WM-A06 | Cancellation persists through a broker outage and stops new dispatch | WM-08, WM-09 |
| WM-A07 | Concurrent provisioning yields one workspace; epic provisioning fails | WM-11 |
| WM-A08 | Blocking/unblocking and reopen history produce correct SQL intervals | WM-06, WM-12 |
