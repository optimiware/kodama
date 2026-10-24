<p align="center">
  <img src="docs/assets/kodama-header.jpg" alt="Kodama: a glowing forest spirit holding a gear-shaped key" width="100%">
</p>

# Kodama

**An API-first, Kubernetes-native software factory: Optimi's agent layer and autonomous SDLC.**

Kodama turns business requests into working software. It connects a request to a specification, a working environment, an executable pipeline, review evidence and a deployment, with humans deciding at the points that matter.

It is built for organizations that write software to fill the gaps in their ERP, CRM and legacy systems, and want less dependence on disconnected SaaS products.

> **Status:** early development. The core packages and the command-line client are in place, and the server already runs a first end-to-end slice. It is not yet a complete, production-ready factory. See the [detailed status](docs/implementation/assembly-status.md).

## How it works

- A **work item** captures intent, acceptance criteria and business progress.
- A **workspace** pairs an actionable work item with a Git branch. Commits are durable checkpoints.
- A **pipeline run** executes a versioned workflow and records evidence.
- A **gate** asks one accountable human to decide, based on identified source and evidence.
- A **blueprint** provides stack-specific conventions, templates and agent guidance.

Engineers work from the CLI, HTTP API, MCP and webhooks, in the editors they already use. Business analysts give requirements and review results through the same interfaces and integrations.

## Reference lifecycle

```text
Story or task → Plan → Specification → Prototype → Implementation
  → QA gate → Build → Preview → Staging → E2E testing
  → AI exploratory QA → Human review → Completion
```

Each project picks the stages it needs. ChatOps reports meaningful events along the way, and review and rework form an explicit loop.

## Capabilities

- Built-in work management with Kanban policies, WIP limits and a fast API.
- Container pipelines with durable retries, cancellation, recovery and human gates.
- Agent steps using a native harness or a headless coding harness.
- Tenant-aware authentication, RBAC, quotas and audit.
- AI gateway and MCP proxy with scoped access and usage accounting.
- Project variables and KMS-encrypted secrets.
- Build, preview, staging, test evidence, human review and ChatOps.
- Blueprints for Go + PostgreSQL and Supabase + Hono + Vite.

## Built with

| Technology | Role |
| --- | --- |
| Go | API, domain logic, orchestration and gateways, in one server |
| Kubernetes | Pipeline steps run as native Jobs, with tenants separated by namespace |
| PostgreSQL | Product and workflow state, variables, encrypted secrets |
| NATS JetStream | Event delivery and notifications |
| Git and S3-compatible storage | Source and guidance in Git; artifacts and logs in S3 |

Kodama supports AWS, GCP and Scaleway, and requires Kubernetes, PostgreSQL and that cloud's KMS. Hosted and standalone installations share the same product contracts.

Optimi Platform is the company core and owns shared tenant authority. Kodama owns agent execution and the software-delivery lifecycle within that authority. See [platform integration](docs/specs/platform-integration.md).

## Documentation

Start at the [documentation index](docs/README.md).

- [Vision and scope](docs/book/01-vision-and-scope.md)
- [Domain and lifecycles](docs/book/03-domain-and-lifecycles.md)
- [System architecture](docs/book/05-system-architecture.md)
- [Reference workflow](docs/examples/workflows/erp-gap-to-preview.md)
- [Installation design](docs/operations/installation-and-configuration.md)
- [Implementation roadmap](docs/book/07-delivery-roadmap.md)
- [Decision register](docs/decisions.md)

## Contributing

Build, test and contribution instructions are in [AGENTS.md](AGENTS.md).

## License

Kodama is licensed under the [Apache License, Version 2.0](LICENSE).
