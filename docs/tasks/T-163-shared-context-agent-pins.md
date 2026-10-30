---
id: T-163
milestone: production
status: done
depends_on: [T-148, T-153]
decision_blockers: []
owns: [internal/execution/, internal/agents/]
check: make check-go PKGS='./internal/execution/... ./internal/agents/...'
resources: [go]
acceptance: [unit_shared_context_carries_agent_pins, unit_shared_context_without_agent_payload_unchanged, unit_v3_agent_attempt_policy_end_to_end]
---
# T-163 — Agent pins in shared (v2/v3) contexts

## Goal
Shared and source-set attempts of agent steps carry the agent profile and design baseline so the agent policy and wrapper accept them (found by T-153's review; `BuildContextV3` and the shared v2 context core never set `Profile` or `DesignBaseline`, and the agents `Planner` decorates only the v1 planning path, so a real v2/v3 agent attempt fails closed with "lacks the agent pins").

## Contract
[Agent runtime](../specs/agent-runtime.md), [runtime v3](../contracts/runtime-v3.md), `internal/agents/README.md` (T-153), `internal/execution/README.md` (T-148).

## Deliverables
- A consumer-owned `execution` port that pins the agent profile and design baseline into the frozen shared context from the step's agent payload (`agents.DecodeArgs`, `Pins`), implemented in `internal/agents/adapters`; a step without an agent payload is unchanged.
- The pins are part of the frozen context and its digest, set at admission before dispatch; old v1/v2 non-agent goldens stay byte-identical.
- An end-to-end unit test: admit a source-set run with an agent step, build the v3 context, and read it with `PolicyReader`/`MetadataReader` and the wrapper's context check.

## Acceptance
Declared check with the named tests.

## Out of scope
Wiring (T-132), the runtime adapters (T-152).

## Completion evidence
Implemented by a subagent; independent review approved `b5ef9017` with one nit (an import kept alive by a blank assignment), which the coordinator removed. Coordinator checks on the rebased branch: `make check-task TASK=T-163` PASS (3 acceptance names); `go test -race ./internal/execution/... ./internal/agents/...` PASS. No schema change, so no real SQL.

For T-132: set `execution.Dependencies.AgentPins = chain.Pins` together with `Planner`, `Store` and `Hooks` (`internal/app/core.go` wires `chain.Planner` and `chain.Hooks` today). Without it, shared agent steps fail with `step_failed`: an agent step in a shared run with a nil port is refused at claim and never runs unpinned. An invalid frozen agent payload keeps the claim failing until the run deadline (as the v1 `Planner` does).
