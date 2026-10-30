---
id: T-159
milestone: production
status: done
depends_on: [T-128]
decision_blockers: []
owns: [internal/gitservice/, contracts/git/v1/]
check: make check-go PKGS='./internal/gitservice/... ./contracts/git/...'
resources: [go]
acceptance: [unit_gitservice_committed_only_for_applied_refs, unit_gitservice_partial_receive_journal, unit_gitservice_journal_unreadable_is_definite]
---
# T-159 — Git worker: journal only applied updates; definite unreadable journal

## Goal
Fix two worker defects found by T-130's real-Git integration tests (GR-09).

## Contract
[Git repositories](../specs/git-repositories.md#push-events), `contracts/git/README.md` Hosts failure semantics.

## Deliverables
- For a non-atomic receive where Git rejects some refs (e.g. a directory/file ref conflict), Git still runs the `committed` reference-transaction hook for the rejected refs. Journal `committed` only for updates whose ref now holds the new value (verify under the lock), and `aborted` otherwise. Real-Git test with a partially rejected receive.
- An affirmatively unreadable journal (corrupt or missing file, not a transport problem) is answered with a definite contract error (additive code `journal_unreadable` in `contracts/git/v1`), distinct from the plain `503 unavailable` used when nothing could be read for transient reasons, so the control plane's `ErrJournalUnreadable` mapping is exact.

## Acceptance
Declared check with the named tests.

## Out of scope
Control-plane settlement (T-130).

## Completion evidence
Implemented by a subagent; independent review requested changes once (the committed-hook check raced with concurrent receives) and approved `c1345eca` (decision under the journal's exclusive append lock: no outcome when a later prepared entry exists for the ref). Coordinator checks: `make check-task TASK=T-159` PASS (3 acceptance names); `go test -race -count=2 ./internal/gitservice/...` PASS. After merging, T-130's real-Git test `partial_receive_lost_confirmation` encoded the old worker bug (expected `unknown`); the coordinator updated it to the now-correct outcome (rejected ref `refused`, record `applied`, one event) and `go test -race ./internal/gitpush/...` passes.
