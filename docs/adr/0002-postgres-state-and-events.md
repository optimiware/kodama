---
status: accepted
---

# PostgreSQL owns state; NATS delivers events

Work management, approvals, and execution decisions need transactional consistency and direct API access. Use PostgreSQL as the authoritative write model and commit event-publication intent in the same transaction through an outbox. NATS JetStream provides at-least-once asynchronous delivery; periodic reconciliation repairs missed wakeups.

## Considered options

An event-sourced primary model would make event history the basis of reconstruction and require projections for many product operations. A broker-authoritative workflow would make execution progress depend more directly on broker retention and delivery state. The proposed relational model keeps work-item transitions, WIP admission, and approvals locally transactional while retaining explicit audit and event history.

## Consequences

Consumers must deduplicate stable event identities and tolerate reordering. Publication can be duplicated after a crash. Events carry resource versions and references; consumers reload authoritative state for decisions. Database restore requires reconciliation with Kubernetes and deployment targets before dispatch resumes.
