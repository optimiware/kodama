---
id: T-151
milestone: production
status: done
depends_on: [T-131, T-136]
decision_blockers: []
owns: [internal/entrypoint/, internal/runtime/bootstrap/]
check: make check-go PKGS='./internal/entrypoint/... ./internal/runtime/bootstrap/...'
resources: [go]
acceptance: [unit_prepare_checks_out_source_set_once, unit_entrypoint_verifies_all_sources, unit_entrypoint_git_credential_helper_scoped, unit_entrypoint_pushes_agent_branch, unit_entrypoint_writer_pushes_task_branch, unit_entrypoint_no_publisher_for_internal, unit_entrypoint_old_contexts_unchanged]
---
# T-151 — Workload programs for source sets and pushes

## Goal
`kodama prepare` and `kodama step` handle source sets and push explicitly with the attempt grant.

## Contract
[ADR 0011](../adr/0011-internal-tenant-git-service.md), [Git repositories](../specs/git-repositories.md), [Git service contract](../contracts/git-service.md), the [implementation plan](../implementation/internal-git-service.md), T-131 contracts. Hardening of T-113 (sanitized Git config, no hooks/filters/helpers from the repository) applies to every source.

## Deliverables
- One preparation for all sources at their paths; pre-command verification of every source.
- A scoped Git credential helper fed by the attempt credential; explicit pushes of agent branches and, for the reserved writer, the task branch; commit outputs with push references.
- Old v1/v2 behavior unchanged.

## Acceptance
Declared check with real `git` against a local smart-HTTP fixture.

## Out of scope
Backend delivery (T-152).

## Completion evidence
Implemented by a subagent; independent review requested changes three times (runtime-issued credentials registered for redaction too late; destination ref names not scanned; broker shutdown unbounded and skipped on authority loss) and approved `714a0da7`. Coordinator checks: `make check-task TASK=T-151` PASS (7 acceptance names); `go test -race ./internal/entrypoint/... ./internal/runtime/bootstrap/...` PASS. Requires T-128's `kodama-push-id` line (merged). For T-150/T-152/T-132: set `KODAMA_RUNTIME_API=v3`, `KODAMA_GIT_DIR`, deliver files with `WriteGit`/`WriteGitPreparation` into the `/kodama/git` mount, register `GitCredentialHelperCommand()` and `StepGitCommand()`.
