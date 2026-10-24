---
id: T-041
milestone: M3
status: done
depends_on: [T-005, T-010]
decision_blockers: []
owns: [internal/integrations/chatops/, api/chatops.yaml, pkg/netpolicy/]
---
# T-041 — Durable outbound ChatOps links

## Goal
Notify a configured webhook destination of meaningful work/review/delivery events.

## Contract
Begin with a documented generic JSON webhook payload and authenticated review URLs. Use durable event consumption plus notification delivery records before external calls; destination setup is project-scoped.

Build against T-005's event envelope and the gate references in the [event catalog](../contracts/event-catalog.md). A notification needs a resource reference and authenticated link, not a gate implementation or authority to approve. Use fixtures for gate events and a consumer-owned current-record lookup where needed; T-046 verifies the real gate producer and link target. T-010 supplies authorization for destination setup.

Use [pkg/netpolicy](../../pkg/netpolicy/README.md) for direct public webhook dialing rather than a separate DNS precheck. The adapter owns approved HTTPS destinations, credential handling, redirect/proxy restrictions, deadlines and response bounds. Private destinations require an explicitly scoped installation policy; the helper has no blanket bypass.

## Deliverables
Payload/config schema, event filtering, retry/dedup/provider receipt, visible failed delivery/manual replay, current-record review links.

## Acceptance
`notification_outage`, `duplicate_delivery`, `forwarded_review_link`, and `changed_subject_link` cover AC-08/AC-09, DQ-04, and IT-03.

`webhook_destination_policy` exercises the destination/redirect restrictions described above, including a redirect to a refused address. Module link tests prove the emitted link grants no authority; real authorization and changed-subject behavior remain required at T-046/T-044.

## Out of scope
Interactive Slack/Teams approval semantics or claiming exactly-once delivery to a receiver lacking idempotency.
