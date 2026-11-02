---
id: T-190
milestone: kernel
status: done
depends_on: [T-188]
decision_blockers: []
owns: [internal/shell/dispatch/, internal/shell/serve/events.go, internal/shell/serve/events_internal_test.go]
check: make check-go PKGS='./internal/shell/dispatch/... ./internal/shell/serve/...'
resources: [go]
acceptance: [unit_dispatch_durable_consumer_name]
---
# T-190 — Migrate the dispatcher to the durable consumer-name scheme

## Goal
O-39 selects the `kodama_<service>_<purpose>` durable-name scheme for every shell consumer. The dispatcher still uses the grandfathered `kernel-git-dispatch` name, which is outside the scheme and bypasses its validation.

## Contract
[Event catalog](../contracts/event-catalog.md) (EC-01 and consumer deduplication), O-39 in the [decision register](../decisions.md), and the consumer-name rules in `internal/natsbus`.

## Deliverables
- Replace the dispatcher's literal durable name with a name produced by the shared scheme, and pass it through the same validation `Subscribe` applies, so an out-of-scheme name cannot be constructed.
- Accept the one-time cursor reset that the rename causes for in-flight deliveries; record it in the task handoff and in the operational note where the durable names are documented.
- Keep the dispatcher's existing delivery, deduplication and replay behaviour unchanged.
- Add a test that the constructed name round-trips through the scheme, is accepted by `Subscribe`, and differs from the retired literal.

## Acceptance
`make check-go` with `Test_unit_dispatch_durable_consumer_name` passing.

## Out of scope
Other consumers, the outbox publisher, and any change to `internal/natsbus`.
