# Implementation baseline

These documents define Kodama's implementation architecture under [ADR 0013](../adr/0013-two-layer-architecture-and-domain-simplification.md):

- [Database schema (15-table model)](database-schema.md)
- [Phase 2 implementation and verification](simplification-phase-2.md)
- [Phase 3 kernel slice and verification](simplification-phase-3.md)
- [Repository layout and package ownership](repository-layout.md)
- [Software design (hexagonal ports and adapters)](../specs/software-design.md)
- [Platform core and agent-layer boundary](../specs/platform-integration.md)
- [Step entrypoint contract](step-entrypoint.md)
- [Environments and adapters](environments.md)
- [Local development and testing](development-and-testing.md)
- [Task graph and agent workflow](../tasks/README.md)
- [Autonomous task harness](harness.md)

The target binary roles are `server`, `git serve`, `step`, `agent`, and the `kodactl` CLI. PostgreSQL owns the 15-table relational state, NATS JetStream is the internal message bus, the native Git service hosts code and Markdown knowledge-base repositories (`<tenant>/context`, `<tenant>/<project>/*`), and external registries/services store published production artifacts while Kodama tracks their metadata. Phase 2 supplies persistence; Phase 3 wires these target interfaces.
