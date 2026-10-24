# Working on Kodama

Kodama has merged foundation and capability packages alongside its specifications. The repository's [README](README.md) distinguishes package implementation from executable wiring; the [roadmap](docs/book/07-delivery-roadmap.md) defines assembly and acceptance milestones.

## Workflow

Start each task in its own `.work/` worktree and follow the review/test-before-merge rules in [AGENTS.md](AGENTS.md#git-workflow).

1. Read [CONTEXT.md](CONTEXT.md) before changing domain terminology or behavior.
   Read [platform integration](docs/specs/platform-integration.md) before adding hosted identity, credentials, tools, jobs, or channel behavior: Platform is the company core; Kodama is its tenant agent/SDLC layer.
2. Locate the authoritative specification through [docs/README.md](docs/README.md). Read its acceptance scenarios and linked contracts before proposing implementation changes.
   For implementation, select a ready task through [docs/tasks/README.md](docs/tasks/README.md); check its dependencies, decision blockers, exclusive paths, and contract checkpoint before claiming it.
3. Check [docs/decisions.md](docs/decisions.md) for confirmed, proposed, and open choices. Preserve their distinction in code and documentation.
4. Change the behavior, its contract, and its affected examples together. Give new requirements and acceptance scenarios stable identifiers following the existing document prefix.
   Record the relevant immutable design baseline and conformance disposition using [design conformance](docs/implementation/design-conformance.md); resolve material conflicts with the owning contract before dependent implementation proceeds.
5. Verify the changed interfaces and relevant failure paths. During the documentation stage, check relative links, fenced examples, terminology, state transitions, and requirement-to-acceptance coverage.
6. Summarize completed changes, checks performed, and unresolved decisions that affect the result.

## Task-specific reading

- **Work management or workspace changes:** read [work management](docs/specs/work-management.md), [workspaces](docs/specs/workspaces.md), and the [lifecycle chapter](docs/book/03-domain-and-lifecycles.md). Source publication uses private clones and expected-tip Git CAS.
- **Execution or recovery changes:** read [pipeline execution](docs/specs/pipeline-execution.md), [pipeline format](docs/contracts/pipeline-format.md), and the [event catalog](docs/contracts/event-catalog.md).
- **Agent or tool integration:** read [agent runtime](docs/specs/agent-runtime.md) and [AI gateway and MCP proxy](docs/specs/ai-gateway-and-mcp-proxy.md).
- **Tenant data, identity, or credentials:** read [identity and RBAC](docs/specs/identity-tenancy-and-rbac.md) and [variables and secrets](docs/specs/variables-and-secrets.md).
- **Delivery or approval changes:** read [delivery and quality gates](docs/specs/delivery-and-quality-gates.md).
- **HTTP or CLI changes:** read the [interface specification](docs/specs/api-cli-and-integrations.md) and [HTTP contract](docs/contracts/http-api.md).
- **Installation or operations:** use the [operations reading path](docs/README.md#operators).
- **Parallel implementation:** read [ownership](docs/implementation/repository-layout.md), [testing](docs/implementation/development-and-testing.md), and the assigned task. Shared-file or interface changes go through the integration owner before dependents proceed.
- **Automated or parallel runs:** the [OpenForge harness](docs/implementation/harness.md) drives repository contributions; its merge gates define review approval for those runs. Bootstrap each worktree with `make openforge-install`; native role definitions live in `.opencode/agents/`. This repository tooling is separate from Kodama's T-031 product wrapper.
- **Module design or persistence boundaries:** read [software design](docs/specs/software-design.md) for the preferred hexagonal ports/adapters and DDD entity design.

## Design discipline

Keep business state transitions explicit. A work item, workspace, run, and gate have independent lifecycles. Carry tenant ownership through records, authorization, events, artifacts, and runtime identities. Describe recovery and idempotency alongside the successful path.

Keep one Go module with vertical capability packages. Entities own business invariants and transitions; application use cases coordinate them through small consumer-owned ports. Keep transport and infrastructure details in adapters explicitly wired by `internal/app`; add layers/interfaces only at concrete boundaries. Preserve transaction, failure/replay and authority semantics in the [package seam table](docs/implementation/repository-layout.md#package-seam-contracts). Follow the [software design specification](docs/specs/software-design.md) and [ADR index](docs/adr/README.md) when revisiting architectural choices.

Update the glossary when a domain term changes; keep implementation decisions in specifications and ADRs. Examples must identify placeholders and planned interfaces. Report implemented capabilities only when the corresponding implementation exists.
