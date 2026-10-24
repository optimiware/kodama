---
id: T-096
milestone: M2
status: done
depends_on: [T-032, T-033, T-086]
decision_blockers: []
owns: [internal/runtimeauth/, internal/runtime/bootstrap/]
check: make check-go PKGS="./internal/runtimeauth/... ./internal/runtime/bootstrap/..."
resources: [go]
acceptance: [runtime_ceiling_agent_actions, runtime_ceiling_deployment_actions, runtime_ceiling_default_closed]
---
# T-096 — Runtime action-ceiling policy

## Goal
Let an operator-configured ceiling grant `model.invoke`, `tool.invoke` and `deployment.call` to the attempts that legitimately need them, and nothing else.

## Contract
T-038 found that every Podman/Process attempt is registered with `runtimeauth.DefaultActions` only: `bootstrap.Issuer` has no actions policy and `rpg.NewAttemptAuthority` receives a nil `ActionPolicy`, so every model-gateway and MCP call from an agent is answered 403 and no deployment callback is possible. Add the policy mechanism, not the wiring: an `ActionPolicy` implementation selected by step kind from operator configuration, where an agent step receives `model.invoke`/`tool.invoke` only when its pinned profile names a model or tool, a deployment-declaring step receives `deployment.call`, and every other step keeps `DefaultActions`. The ceiling is the upper bound; a narrower per-attempt grant (AR-A06's agent-specific revocation) must still be able to take actions away. An unknown step kind, a missing profile pin or an unreadable policy source grants the default set, never more.

## Deliverables
The policy type and its configuration struct in `internal/runtimeauth/`, the `Issuer` option in `internal/runtime/bootstrap/` that consumes it, documentation in the runtimeauth README (replacing the "operator policy" placeholder), and unit tests. Do not edit `internal/app`; T-101 binds it.

## Acceptance
`runtime_ceiling_agent_actions` proves an agent attempt whose profile names a model and a tool holds exactly `model.invoke` and `tool.invoke` beyond the defaults, and one that names neither does not. `runtime_ceiling_deployment_actions` proves a deployment-declaring step holds `deployment.call` and no other step does. `runtime_ceiling_default_closed` proves an unknown kind, a missing pin or a failing source yields only `DefaultActions`, and that a per-attempt revocation still removes a granted action. Covers AR-A03, AR-A06 and GM-A02.

## Out of scope
Binding in `internal/app`, the model gateway and MCP proxy themselves, and any widening of the default action set.
