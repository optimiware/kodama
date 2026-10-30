# Implementation baseline

These documents define Kodama's implementation architecture under [ADR 0013](../adr/0013-two-layer-architecture-and-domain-simplification.md):

- [Pipeline-runtime simplification plan](pipeline-runtime-simplification.md)
- [Database schema](database-schema.md)
- [Phase 2 implementation and verification (historical)](simplification-phase-2.md)
- [Phase 3 kernel slice and verification (transitional)](simplification-phase-3.md)
- [Repository layout and package ownership](repository-layout.md)
- [Software design (layer boundaries and ports/adapters)](../specs/software-design.md)
- [Platform core and agent-layer boundary](../specs/platform-integration.md)
- [Step entrypoint contract](step-entrypoint.md)
- [Environments and adapters](environments.md)
- [Local development and testing](development-and-testing.md)
- [Task graph and agent workflow](../tasks/README.md)
- [Autonomous task harness](harness.md)

The target binary roles are `server`, `git serve`, `step`, `agent`, and the `kodactl` CLI. PostgreSQL owns authoritative shell state, NATS JetStream is the central system bus, the native Git service hosts code and Markdown knowledge-base repositories (`<tenant>/context`, `<tenant>/<project>/*`), and external registries/services store published production artifacts while Kodama tracks their metadata. The [pipeline-runtime simplification plan](pipeline-runtime-simplification.md) owns the convergence from the transitional assemblies.
