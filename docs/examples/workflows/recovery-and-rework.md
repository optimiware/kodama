# Recovery and rework fixtures

| Scenario | Expected behavior | Acceptance |
| --- | --- | --- |
| Controller dies after Job create | Observe the deterministic matching Job/spec/UID; preserve attempt identity | PX-A02 |
| Partitioned writer outlives Pod deletion | Private clone cannot modify accepted source; revoked publication and CAS reject stale result (`run_shared`: also no successor mounts the scratch until the holder is verified stopped, WS-A10) | WS-A03 |
| Human pushes while agent works | Agent expected-tip publication conflicts; preserve proposal and ask for explicit rework | WS-A07 |
| Git push succeeds but response is lost | Observe remote proposed/expected commit before repeating or releasing writer | WS-A05 |
| Broker redelivers an outbox event | Inbox/local-effect transaction and external delivery identity deduplicate | PX-A03 |
| Specification changes during review | Current content revision no longer matches subject; old decision cannot progress | DQ-A03 |
| Deployment apply times out | Observe row/action-annotated target objects; unresolved outcome blocks automatic repeat | DQ-A05 |
| Agent lacks a business decision | Fail with `needs_input` and question; human revises intent and creates new run | AR-A05 |
| Human rejects usable but incorrect application | Preserve reason/evidence, move to active under WIP, create new commit/test/review | DQ-A07 |
| Secret rotates while run is pinned | Keep pinned version if permitted; revocation fails new resolution explicitly | VS-A05, VS-A06 |
| Restore predates active external work | Fence old controllers, rotate attempt signing keys and retained external credentials, inventory effects before resuming | IT-A05 |

These become module integration tests and kind/production drills in the [backlog](../../tasks/README.md). Fakes can exercise local decisions; real Git/Kubernetes/NATS tests establish their actual race semantics.

Uncommitted scratch loss is reported rather than hidden behind a checkpoint claim. A passing run is not human business acceptance, and a cancelled token does not retroactively undo an already executed remote effect.

## `run_shared` additions (design fixtures)

| Scenario | Expected behavior | Acceptance |
| --- | --- | --- |
| Controller dies while a service is starting or after commands began | Verify scope, fingerprint and UID and adopt intact resources; an unbound intent may be re-ensured only before any command began; otherwise the run fails with `service_lost`/`workspace_lost`, never an empty replacement | PX-A14, WS-A11 |
| Pre-command infrastructure failure | No recorded command-begin and the workload verified stopped: retry on the same scratch and services | PX-A18 |
| Attempt began, then the node is lost | Not replayed automatically; the run fails once resources are verified stopped; rerun is a new run | PX-A18, WS-A11 |
| Cancellation races a resource create | Credentials revoked, late object reaped by scope/fingerprint, foreign objects untouched, removal retried until confirmed | PX-A15 |
| Run deadline expires during a gate wait | Resources stopped, pending gate invalidated with `run_deadline_exceeded`, nothing progresses | PX-A16 |
| Human pushes between two writers of one run | The later proposal conflicts on expected tip; the proposal stays an artifact for explicit rework | WS-A12 |
