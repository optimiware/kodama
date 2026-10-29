# Implementation baseline

These documents make the simplified design concrete before parallel implementation starts.

- [Repository layout and ownership](repository-layout.md)
- [Preferred hexagonal design and DDD entities](../specs/software-design.md)
- [Platform core and agent-layer boundary](../specs/platform-integration.md)
- [Design conformance and drift control](design-conformance.md)
- [Upstream reuse review and parallel task adjustments](reuse-review.md)
- [Database schema sketch](database-schema.md)
- [Step entrypoint contract](step-entrypoint.md)
- [Environment pinning contract and ownership](environment-pinning.md)
- [Environments and adapters](environments.md)
- [External MCP server](mcp-server.md)
- [Proposed run workspace, services, and builders](run-workspace-services-builders.md)
- [Local development and testing](development-and-testing.md)
- [Task graph and agent workflow](../tasks/README.md)
- [Autonomous task harness](harness.md)

One Go binary provides `server`, `step`, `mcp`, and CLI commands. The server hosts API, orchestration, event delivery, and enabled gateways in one process. `mcp serve` is a separate client process over the public API and the selected OIDC issuer. Split deployments only after measurements require it. PostgreSQL is authoritative, NATS is durable event transport, Git holds source checkpoints, and S3-compatible storage holds artifacts/logs.

Package boundaries follow real ownership and failure behavior. Prefer hexagonal ports/adapters with module-owned DDD entities and inward dependencies. Small consumer-owned ports, including entity-specific persistence ports, keep use cases independent of infrastructure; concrete adapters are wired at composition. Runtime wire contracts must be frozen before the server and entrypoint diverge; other interfaces evolve through the owning task and its dependents. Avoid a universal repository/service/plugin abstraction.
