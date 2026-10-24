---
id: T-092
milestone: M2
status: done
depends_on: [T-065, T-066, T-077]
decision_blockers: []
owns: [internal/deliverycleanup/, internal/execution/cleanup-run.go, internal/execution/admission.go]
check: make check-go PKGS="./internal/deliverycleanup/... ./internal/execution/..."
resources: [go]
acceptance: [cleanup_deployment_input, cleanup_many_due_one_definition]
---
# T-092 — Cleanup-run deployment input handoff

## Goal
Let one project cleanup definition clean every due deployment, by handing the recorded deployment to the cleanup step through the run, not through a per-deployment definition.

## Contract
T-066's scheduler admits a cleanup run with `CleanupCommand.Inputs` empty, and the runtime context carries no deployment identity, yet T-065's `delete.sh` needs the deployment it deletes (`DEPLOY_DEPLOYMENT_ID`, or `KODAMA_INPUT_DEPLOYMENT_ID` from a declared run input). The handoff must come from the recorded deployment the run was admitted for, never from caller text: the scheduler (or admission) sets the run input `deployment_id` to `CleanupCommand.DeploymentID`, which authorization has already bound to the scope. A definition that declares the input receives it; a definition that does not keeps working. The value is persisted with the run, so retries, restart and rounds see the same deployment. A caller cannot supply or override it.

## Deliverables
The input handoff in the owned scheduler/admission paths, the declared-input convention for the project cleanup definition documented in the module README and in T-065's tool contract, and unit tests with record/execution port doubles. Do not change the delivery record, the tools image scripts or the process assembly (T-072).

## Acceptance
`cleanup_deployment_input` proves the admitted run carries exactly its own deployment as the `deployment_id` input, a foreign or caller-supplied value is refused or replaced, and the value survives a retry round. `cleanup_many_due_one_definition` proves several deployments of one project becoming due together are each admitted against the single unchanged project definition, each with its own input, with duplicate claims still collapsing to one run per round. Keep the T-066 acceptance (`cleanup_after_work_done`, `cleanup_scope`, `cleanup_duplicate_claim`) passing. Covers DQ-09/DQ-10 and PX-06/PX-07.

## Out of scope
Real SQL/kind qualification (T-067), process registration (T-072), changes to deployment records or tool scripts.
