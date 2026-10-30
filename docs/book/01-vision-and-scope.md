# 1. Vision and scope

Kodama helps organizations build focused software around ERP, CRM, and legacy gaps. It connects business intent, engineer/agent work, and reviewed delivery while retaining on-premises ownership and a hosted path.

Optimi Platform is the company core; Kodama is its agent layer and autonomous SDLC for platform tenants. The [platform integration contract](../specs/platform-integration.md) separates core authority from SDLC ownership and keeps standalone adapters explicit. Autonomous implementation stays within granted permissions and produces evidence for human acceptance.

## Product model

Kodama is a full-cycle SDLC in a dedicated Kubernetes installation, aiming for the simplicity of a small CI server. Tasks, issues and change requests go in; deployed code comes out. It embeds its own task tracker and its own Git service ([ADR 0011](../adr/0011-internal-tenant-git-service.md), designed and not yet delivered). A project owns one or more repositories and its pipelines. A pipeline is a set of container steps sharing a workspace disk: each step is an image with environment, secrets and a command, and deployment is one more such step (Terraform, Wrangler, kubectl or Helm with mounted credentials).

Agents do most of the work and coordinate through Git, so Git is mostly automated rather than exposed. People feed tasks, review evidence and decide. An engineer can still clone a project or branch to debug, review or fix it, or bring an existing project in with an ordinary `git push`.

## Principles

- **API-first:** every product operation is accessible to the CLI and authorized integrations.
- **Small core:** one server process, one step entrypoint, a few durable records, ordinary Git and container commands.
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
