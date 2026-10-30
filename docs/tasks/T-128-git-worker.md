---
id: T-128
milestone: production
status: done
depends_on: [T-126]
decision_blockers: []
owns: [internal/gitservice/]
check: make check-go PKGS='./internal/gitservice/...'
resources: [go]
acceptance: [unit_gitservice_smart_http_only, unit_gitservice_protocol_v2, unit_gitservice_forwarding_assertion_required, unit_gitservice_path_from_id_only, unit_gitservice_bounded_chunked_push, unit_gitservice_hooks_not_overridable, unit_gitservice_ref_journal_attributes_receive, unit_gitservice_prepared_hook_aborts_without_intent, unit_gitservice_receive_deadline_enforced, unit_gitservice_initialize_idempotent, unit_gitservice_refs_and_journal_reads, unit_gitservice_hidden_refs_not_fetchable, unit_gitservice_maintenance_serialized]
---
# T-128 — Native Git worker

## Goal
The tenant Git worker around native Git (GR-03, GR-05, GR-08, GR-09 worker side, GR-16): the package behind `kodama git serve`.

## Contract
[ADR 0011](../adr/0011-internal-tenant-git-service.md), [Git repositories](../specs/git-repositories.md), [Git service contract](../contracts/git-service.md), the [implementation plan](../implementation/internal-git-service.md), seams S2–S4, T-126 DTOs. Start from `dev/gitserver/main.go`; replace fixture tokens with forwarding assertions and its in-memory buffering of chunked pushes with a bounded on-disk spool.

## Deliverables
- Smart-HTTP handler around `git http-backend`: default-deny endpoints, protocol v2, assertion verification per tenant audience, repository paths from IDs only, sanitized environment (no inherited `GIT_*`), bounded body size, duration and concurrency.
- Server-owned hooks run by the Kodama binary (`pre-receive`, `reference-transaction` with the fsynced receive-attributed ref journal and `observed` value under lock, `post-receive`) behind a small control-plane client port; system configuration that ignores repository hooks, filters, credential helpers and `uploadpack.allowAnySHA1InWant`.
- Process-tree termination at the receive deadline; per-repository mutation lock shared with maintenance and reconciliation reads.
- Worker RPC server: initialize, read refs, read journal and receive liveness, manage retained refs, run maintenance; an exported command constructor for `kodama git serve`.

## Acceptance
Declared check against real `git` in temporary directories (GR-A05 server side, GR-A06 worker side, GR-A11).

## Out of scope
Push records and policy (T-130), Kubernetes objects (T-129), command registration (T-132).

## Completion evidence
Implemented by a subagent; independent review requested changes four times (ambiguous mutations reported as refusals; stale locks after crashes; journal outcome look-back cutoff; CreateBranch conflict semantics; then a coordinator-directed redesign to an inherited generation lock and transport-abort Unknown outcomes; queued requests and initialization outside the fence; a forgeable push-ID marker) and approved `eaee00d2`. Added for T-151: `post-receive` prints one sanitized `kodama-push-id: <pushId>` line per applied receive. Coordinator checks: `make check-task TASK=T-128` PASS (13 acceptance names); `go test -race -count=2 ./internal/gitservice/...` PASS repeatedly (one earlier failure under heavy concurrent load did not reproduce in eleven further runs). For T-132: register `kodama git serve`/`kodama git-hook`, the worker reads the `KODAMA_GIT_*` environment defined by T-129, and the generation descriptor (fd 3) must be preserved by every descendant.
