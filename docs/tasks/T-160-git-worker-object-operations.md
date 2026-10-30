---
id: T-160
milestone: production
status: done
depends_on: [T-159]
decision_blockers: []
owns: [internal/gitservice/, contracts/git/v1/]
check: make check-go PKGS='./internal/gitservice/... ./contracts/git/...'
resources: [go]
acceptance: [unit_gitservice_is_ancestor, unit_gitservice_has_commit_and_time, unit_gitservice_bundle_create_under_shared_gate, unit_gitservice_bundle_replay_only_while_fenced, unit_gitservice_replay_never_rewinds]
---
# T-160 — Git worker object operations for maintenance and recovery

## Goal
Give the control plane the object-level worker operations T-138's `internal/gitops` needs without mounting the tenant volume (GR-13, GR-15).

## Contract
`contracts/git/README.md` (S2/S9), `internal/gitops/README.md` (its object port and `localgit` semantics).

## Deliverables
- Worker RPCs with DTOs, schema and fixtures in `contracts/git/v1`: `IsAncestor(repository, ancestor, descendant)`, `HasCommit`, `CommitTime`, `CreateBundle` (streamed, under the shared gate) and `ReplayBundle` (only while the repository is fenced for restore; never rewinds or deletes a ref).
- Server handlers in `internal/gitservice` matching `localgit`'s semantics, with real-Git tests.

## Acceptance
Declared check with the named tests.

## Out of scope
Scheduling and recovery policy (T-138), wiring (T-132).

## Completion evidence
Implemented by a subagent; independent review requested changes once (the restore fence failed open when its marker directory was unreadable; capture held the gate mutex across blocking I/O; deterministic replay refusals after the first ref aborted as unknown; silently dropped refs; test gaps; a discovery that `git bundle unbundle` does not honour `transfer.fsckObjects`, replaced by an explicit prerequisite check and `index-pack --strict`) and approved `da906a82`. Coordinator checks: `make check-task TASK=T-160` PASS (5 acceptance names); `go test -race -count=2 ./internal/gitservice/... ./contracts/...` PASS. The coordinator added the six RPCs and codes to `contracts/git/README.md` and `docs/contracts/git-service.md`.

Open minors (not blocking; for a hardening pass or T-133): the bundle header read in replay is unbounded per line (bound at 64 KiB per line, about 4 MiB total, also before `list-heads`); `objects/pack/tmp_*` left by a killed `index-pack` is not removed at startup; non-fsck `index-pack` failures (disk full) are classified `invalid_request` instead of retryable `unavailable`; re-entering an already set fence does not re-persist a missing marker. For T-132: expose `Limits.MaxBundleBytes`, `BundleTimeout`, `MaxConcurrentBundles`; markers live on the volume, so after a snapshot restore the control plane must raise the fence again from its own durable closure; leave the fence explicitly after a failed leave; the volume needs headroom for the largest bundle in `<DataRoot>/tmp`; replay has no rollback (restore the snapshot first).
