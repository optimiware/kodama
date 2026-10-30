# 1. Vision and scope

Kodama covers the software development lifecycle: native repositories and task tracking, engineer/agent work, CI/CD pipelines, retained builds and reviewed delivery. Focused software around ERP, CRM and legacy gaps is one application of that model. It retains on-premises ownership and a hosted path.

Optimi Platform is the company core; Kodama is its agent layer and autonomous SDLC for platform tenants. The [platform integration contract](../specs/platform-integration.md) separates core authority from SDLC ownership and keeps standalone adapters explicit. Autonomous implementation stays within granted permissions and produces evidence for human acceptance.

## Principles

- **API-first:** every product operation is accessible to the CLI and authorized integrations.
- **Small core:** one server process, one step entrypoint, a few durable records, ordinary Git and container commands.
- **One execution primitive:** tools for build, test, agents, publication, deployment and rollback run in containers with explicit secrets and a shared run workspace; managed profiles are optional convenience.
- **Build once:** Git retains source, artifacts retain outputs, immutable releases identify deployable versions, and ordinary publication steps copy them where production can reach them ([ADR 0012](../adr/0012-container-sdlc-and-artifact-releases.md); implementation pending).
- **Explicit ownership:** tenant/project scope travels through data, execution, events, and artifacts.
- **Recoverable progress:** published source and committed results survive container/server failure.
- **Human acceptance:** agents produce evidence; people accept business outcomes.
- **Useful conventions:** blueprints begin with guidance/templates; shared libraries follow repeated real needs.

## User value

Analysts provide specifications and acceptance criteria. Engineers refine intent, supervise agents, and edit through ordinary Git. Reviewers inspect identified source and deployments. Operators can install a supported profile without maintaining a new custom orchestration platform alongside Kodama.

The first users work through API/CLI. A graphical workbench and analyst interface are later tasks. The initial Kubernetes profile assumes mutually trusting engineers; untrusted hosted execution has a separate qualification gate.

## Measure outcomes

Observe lead/cycle/review/blocked time, rework, failure recovery, and usage per work item. Start with SQL summaries and explicit definitions. Advertise latency/scale only after measuring a named reference workload.

The [roadmap](07-delivery-roadmap.md) defines useful increments. [Decisions](../decisions.md) distinguish confirmed direction, implementation defaults, and release blockers.
