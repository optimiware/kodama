---
status: accepted
---

# Prefer hexagonal ports/adapters and DDD entities

The owner prefers domain behavior that can be understood and tested independently of PostgreSQL, HTTP, Kubernetes, and other integrations. Adopt a lightweight hexagonal design within the existing domain modules, with DDD entities owning identity, invariants, and valid transitions.

Application use cases define narrow consumer-owned ports; infrastructure and transport adapters sit outside them and are injected by `internal/app`. Entity-specific repository ports are appropriate when they express a real use case. Domain entities remain module-owned; shared `internal/domain` types stay small.

This preference adds explicit dependency direction and domain behavior ownership to the KISS baseline. It does not require a generic repository framework, a uniform directory tree for every module, or separate deployments. Small modules may use a simpler arrangement with the rationale recorded in review. The trade-off is additional boundary mapping where it improves separation and testing.

PostgreSQL RLS, concurrency controls, atomic outbox writes, and external-effect reconciliation remain required. The [software design specification](../specs/software-design.md) owns the detailed guidance and acceptance checks.
