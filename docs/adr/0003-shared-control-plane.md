---
status: accepted
---

# Shared control plane with tenant-scoped execution

The hosted product needs multi-tenant operation while the same distribution must be installable on premises. Use a shared API/control plane and PostgreSQL deployment with tenant-owned resources and tenant execution namespaces. The owner selected this model over a separate factory per tenant or an initial remote-execution topology.

## Consequences

Authorization and ownership must be enforced across database access, events, artifacts, logs, provider credentials, and execution identities. Namespace separation is one layer; supported runtime profiles must state and enforce their process, network, storage, and node isolation assumptions for tenant code.

A dedicated installation remains a deployment of the same product. Cross-tenant administrative operations require a distinct operator authority and audit. Tenant-specific quotas and fair scheduling are required for shared capacity.

## Execution-profile clarification — 2026-10-20

O-07 selects trusted-engineer staging with regular Kubernetes containers and the existing restricted execution controls. gVisor, Substrate and other isolation/security options are deferred extensions. Hosted OAuth/tenant integration is independent of a future untrusted-code profile; stronger backend qualification does not block the current staging scope. This clarification does not claim that regular containers provide a boundary for mutually untrusted code.
