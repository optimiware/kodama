---
id: T-093
milestone: M2
status: done
depends_on: [T-035, T-065, T-077, T-080]
decision_blockers: []
owns: [internal/delivery/adapters/]
check: make check-go PKGS="./internal/delivery/..."
resources: [go]
acceptance: [delivery_effect_replay_safe, delivery_effect_withheld_blocks, delivery_effect_scope]
---
# T-093 — Delivery effect reconciliation adapter

## Goal
Give execution a production `EffectReconciler` for deployment steps, so an apply whose answer was lost resolves from the deployment record instead of staying blocked forever.

## Contract
`execution.EffectReconciler` (`Reconcile(ctx, EffectQuery) (ReconciliationEvidence, error)`) is the trusted port that says what an uncertain attempt may have done. General commands have none and stay blocked; deployments do, because T-065's `apply.sh` registers the intent before the first change, honors the action key (a settled action replays without effects), settles an unknown outcome by observing the recorded objects and resumes an applying one idempotently by annotation. Implement the adapter over T-035's scoped deployment record: look up the logical action by `EffectQuery.ActionKey` within the query scope; no record means nothing was applied (replay safe); a registered-but-not-started record is replay safe; an `applying` record with no recorded object identities cannot be observed and stays still-unknown; a running (applying or unknown) or settled record is replay safe because the script observes before it acts. For a cleanup run the evidence comes from the deployment the run is bound to: replay safe once cleanup was requested, otherwise still unknown. An unreadable record or any outage yields still-unknown or an error, never an affirmative verdict. Evidence names its source and is derived only from the record, never from the attempt's own output.

## Deliverables
The adapter in `internal/delivery/adapters/`, its constructor, README note and unit tests over delivery/execution port doubles, including a withheld mode in the test double to prove blocking. Do not edit the delivery service, the execution package or the process assembly (T-072 binds it).

## Acceptance
`delivery_effect_replay_safe` covers the verdict table above (no record, pending, applying with and without identities, unknown, settled, cleanup requested and not requested). `delivery_effect_withheld_blocks` proves an unreadable record or outage never yields replay-safe or settled evidence and the attempt stays blocked. `delivery_effect_scope` proves another scope's or tenant's record is never consulted or revealed. Covers DQ-07–DQ-10 and PX-06/PX-07.

## Out of scope
Real PostgreSQL/kind qualification (T-067), process registration (T-072), launch-unknown evidence for non-deployment commands.
