---
id: T-164
milestone: production
status: done
depends_on: [T-150, T-162]
decision_blockers: []
owns: [internal/entrypoint/]
check: make check-go PKGS='./internal/entrypoint/...'
resources: [go]
acceptance: [unit_entrypoint_integration_already_integrated, unit_entrypoint_integration_base_moved_fails]
---
# T-164 — Entrypoint handles an already-integrated approval

## Goal
An integration step whose approved candidate is already on the default branch completes without a push (T-150's coordinator decision, [runtime v3](../contracts/runtime-v3.md)).

## Contract
[Runtime v3](../contracts/runtime-v3.md) (integration route: `409 publication_conflict` with `reason: already_integrated` and `observedTip` equal to the candidate), `internal/entrypoint/README.md` (`step-git integration`).

## Deliverables
- `kodama step-git integration` and the entrypoint treat that answer as success with no grant and no push: the step completes with no commit output for that repository (this attempt has no push record to cite). A `409` with any other `observedTip`, or `reason: base_moved`, remains a classified failure (`publication_conflict`) so the pipeline recomputes the candidate. Nothing about the response is written to the credential file.
- Real-Git tests with the existing fixtures: already integrated → step succeeds, no receive request; base moved → failure, no receive.

## Acceptance
Declared check with the named tests.

## Out of scope
Server side (T-150, done), runtime adapters (T-152).

## Completion evidence
Implemented by a subagent; independent review requested one simplification (the candidate comparison used an untrusted value, the command's `--candidate` or the workspace HEAD, so it could only cause false failures; the control plane's answer is the authority) and one sticky-refusal minor; both were applied exactly as prescribed (`24bd7a35`) and the revision was merged without a second review round. Coordinator checks: `make check-task TASK=T-164` PASS (2 acceptance names); `go test -race ./internal/entrypoint/...` PASS.

Rule implemented: a 409 `publication_conflict` with `reason == already_integrated` and a well-formed `observedTip` is success with no grant, no push and no commit output for that repository; any other 409 (including `base_moved`, any tip) fails the step as `publication_conflict`; a refusal stays set until a later grant or an already-integrated answer (a transient error does not clear it). No `--candidate` flag.
