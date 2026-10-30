---
id: T-153
milestone: production
status: done
depends_on: [T-131, T-148]
decision_blockers: []
owns: [internal/agents/, internal/qa/, images/harness-opencode/, contracts/harness-opencode/]
check: make check-go PKGS='./internal/agents/... ./internal/qa/...' && images/harness-opencode/test.sh
resources: [go]
acceptance: [unit_agent_policy_new_context_version, unit_agent_multi_checkout_candidate, unit_qa_context_source_set]
---
# T-153 — Agent wrapper and policy compatibility

## Goal
The existing OpenCode wrapper and agent policy work with source sets and direct pushes (AR-15).

## Contract
[ADR 0011](../adr/0011-internal-tenant-git-service.md), [Git repositories](../specs/git-repositories.md), [Git service contract](../contracts/git-service.md), the [implementation plan](../implementation/internal-git-service.md). Bounded compatibility only: no new harness, browser or live-provider qualification (T-085).

## Deliverables
- Version-aware agent policy reader and QA context adapter.
- Wrapper: new runtime routes, multiple checkouts, candidate/conformance handling, leaving its commits where the entrypoint publishes them with the attempt credential (the wrapper never pushes and never holds a credential, AR-15, T-151); v1 behavior kept.
- `images/harness-opencode/test.sh`: runs the wrapper's Linux-only Go tests natively on Linux, otherwise in a digest-pinned Linux Go container through Podman (`DEV_PROVIDER`), exits 75 when neither is available, and fails if any test fails or if `Test_unit_wrapper_pushes_with_attempt_grant` or `Test_unit_wrapper_old_context_unchanged` did not pass.

## Acceptance
Declared check (both parts).

## Out of scope
Live agent qualification (T-085).

## Completion evidence
Implemented by a subagent; independent review requested changes three times (cross-task gaps recorded as T-162 and T-163; wrapper git hardening against hostile checkouts: symlinks, planted config, `commondir`, alternates, nested repositories with filters; branch-label collisions; QA lineage-published candidates; `test.sh` robustness; parser bypass for quoted config section headers) and approved `bb4a2bd4`. Coordinator checks on the rebased branch: `make check-task TASK=T-153` PASS (3 acceptance names); `go test -race ./internal/agents/... ./internal/qa/...` PASS; `images/harness-opencode/test.sh` PASS in the pinned container (the wrapper package does not build natively on darwin, `SYS_PRCTL`, a pre-existing limit). The coordinator amended the task text: the wrapper never pushes and never holds a credential; the entrypoint publishes (T-151).

Limits recorded: the v3 agent flow is not end-to-end until T-163 (agent pins in shared contexts); a reader's agent-branch commit can reach a later writer step's task-branch push until T-162 (entrypoint restores HEADs); conformance binds only the primary commit, so gates and QA must bind the whole set (T-149, T-150); the wrapper's hostile-config refusal is a deny-list; an ignored directory containing a nested `.git` fails the attempt (`source_invalid`, fail-closed); under the M1 trust model the harness shares the entrypoint's uid, so file isolation of the credential is not claimed. Minors: the nested-repository experiment test depends on the host git version (skip when not 2.47); use `--ignore-submodules=all` on both commit paths for consistency.
