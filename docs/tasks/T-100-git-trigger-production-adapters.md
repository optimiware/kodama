---
id: T-100
milestone: M2
status: done
depends_on: [T-012, T-036, T-088]
decision_blockers: []
owns: [internal/integrations/gitwebhook/postgres/, internal/integrations/gitwebhook/adapters/]
check: make check-go PKGS="./internal/integrations/gitwebhook/..."
resources: [go]
acceptance: [git_trigger_store_atomic_claim, git_trigger_refusal_ledger_durable, git_trigger_hook_secret_from_secrets]
---
# T-100 — Native Git trigger production adapters

## Goal
Give the native Git push trigger (T-036) durable PostgreSQL adapters, so it can be mounted by the core instead of running on memory doubles.

## Contract
T-038's `native_hook_roundtrip` joins the module's own parts itself because no trigger store, hook-secret source, definition lookup or admission ledger has a production adapter; the delivery and refusal ledgers it used are in memory, so an admission-owned atomic refusal ledger has no real-persistence evidence. Implement, against the ports in `internal/integrations/gitwebhook/ports.go` and the module README ("StartRun/FindRun/DecideRefusal over one admission store"): a PostgreSQL trigger store and delivery ledger (a durable claim keyed by trigger and delivery, replay collapsing to the first outcome), an admission adapter mapping the `Runs` port onto the core's idempotency-keyed run admission with a durable, atomic refusal ledger so a stale lease holder cannot create a run after a terminal refusal, and a secrets-backed hook-secret source that resolves the installation's secret by reference without logging it. Tenant isolation follows the existing modules: composite keys, tenant predicates and transaction-local RLS context. Migrations follow the repository's ordering.

## Deliverables
The adapters and migrations in the owned packages, README notes, and unit/contract tests over the module's existing port doubles plus PostgreSQL integration tests behind the existing integration-test conventions. Do not edit the module's service or ports and do not edit `internal/app`; T-101 mounts it.

## Acceptance
`git_trigger_store_atomic_claim` proves two concurrent identical deliveries produce one claim and one outcome, a replay returns the stored outcome, and another tenant's trigger or delivery is never visible. `git_trigger_refusal_ledger_durable` proves a refusal recorded by one holder is honoured after a restart and by a stale holder (no run is created after a terminal refusal), and that success and refusal are mutually exclusive durable outcomes. `git_trigger_hook_secret_from_secrets` proves the hook secret resolves from the secrets module by reference, a missing or foreign reference is refused, and the value never appears in an error or log. Covers AC-07, PX-02 and IT-A05.

## Out of scope
Mounting in `internal/app` (T-101), third-party provider adapters, and changes to the hook payload or signature protocol.
