# Kodama domain language

Kodama connects business intent, human and agent work, and reviewed software delivery.

## Ownership

**Platform core**: Optimi's company-wide authority for tenant identity/membership, entitlements, core credentials, and core domain operations.

**Agent layer**: Kodama's bounded agent execution and software-delivery capabilities operating within granted tenant authority.

**Factory**: An installation coordinating software work for one or more organizations.

**Tenant**: An organization owning projects, memberships, and work within a factory.
_Avoid_: Account, customer workspace.

In Optimi-hosted operation, the verified Optimi OAuth principal carries the authoritative UUID `tenant_id` and role claims. That tenant UUID owns tenant-scoped PostgreSQL resources and their RLS context; local project bindings do not create core membership. Standalone installations use an explicitly selected local authority. See [platform integration](docs/specs/platform-integration.md).

**Project**: A tenant-owned grouping of related work, repositories, and delivery environments.

**Membership**: A person's association with a tenant or project and their granted roles.

**Application tenant**: A customer or organization inside an application produced by the factory, separate from factory tenancy.

**Workload identity**: The scoped identity under which an automated activity accesses factory capabilities.

## Intent and work

**Work item**: A tracked unit of intent with a specification, acceptance criteria, owner, and business state.
_Avoid_: Ticket as a separate entity.

**Card**: A board representation of a work item.

**Actionable work item**: A story, task, or defect that can acquire a workspace and produce deliverables.

**Epic**: A grouping of work items toward a larger outcome.

**Specification**: The current description of required behavior, including the plan as a section where useful.

**Acceptance criterion**: An observable condition used by a reviewer to judge the business outcome.

**WIP limit**: A limit on work items admitted to active work.

**Workspace**: The durable source-working context paired with an actionable work item.

**Checkpoint**: A committed source revision preserved for subsequent work or recovery.

**Candidate**: The source commit proposed for review and delivery.

**Business analyst**: A person expressing business needs and reviewing delivered outcomes.

**Engineer**: A person shaping software and supervising its implementation.

**Reviewer**: A person authorized to approve or reject identified work and evidence.

**Workbench**: The optional, deferred Platform-hosted interface for work, execution, and delivery review; CLI/API/MCP/webhooks remain the primary Kodama surfaces.

## Execution and delivery

**Pipeline definition**: An immutable version of a dependency graph of executable steps and human gates.

**Pipeline run**: One execution of a definition with pinned inputs and recorded outcomes.

**Step run**: The run-local progression of a step.

**Step attempt**: One attempt to execute a step, including its result and failure reason.

**Step entrypoint**: The wrapper coordinating source preparation, command execution, and result collection for an attempt.

**Runtime**: The backend that runs attempt workloads (Kubernetes, Podman, or Process in tests) behind one execution port.
_Avoid_: Executor.

**Workload**: The runtime's instance running one attempt, identified by the attempt's deterministic name and the instance UID bound at dispatch.
_Avoid_: Job, container or process in backend-neutral contexts.

**Environment**: The adapter set a process composes, chosen by the required `KODAMA_ENV`: `development` (in memory, ephemeral), `test` (real services and Podman) or `production` (Kubernetes). `production` refuses development and test adapters.
_Avoid_: Profile, mode.

**Memory adapter**: An in-memory implementation of a port over the `memdb` transaction kernel, qualified by the same contract suite as its PostgreSQL adapter. It proves policy, never SQL semantics.

**Runtime guarantee**: A property a runtime enforces for every workload, such as image, isolation or network policy. Admission pins the guarantees a run requires.

**Harness**: The environment managing an agent's model interaction and tools.

**Agent profile**: Versioned defaults selecting a harness image, instructions, model access, tools, and limits.

**Artifact**: An identified retained output, such as a report, log, image, or screenshot.

**Evidence**: Artifacts and observations used to review a candidate or deployment.

**Gate**: A recorded request for one authorized human decision about an identified subject.

**Gate decision**: An immutable approval or rejection with its reviewer and reason.

**Environment**: A named application delivery target with scoped credentials and configuration.

**Deployment**: A record connecting an environment, source, image, configuration, and execution outcome.

**Preview**: A temporary deployment for reviewing a candidate.

**AI exploratory QA**: Agent-led application exploration producing evidence and defects.

**Blueprint**: Versioned application guidance, templates, and skills for a particular stack.

**Application chassis**: Markdown-only engineering guidance for a tenant or project, covering architecture/microservices, approved stacks/libraries and cloud deployment methods; not a shared application runtime library.

**Guidance repository**: A Git repository containing tenant/project-scoped chassis Markdown. Agent context identifies its immutable commit and selected paths; a moving branch is not an admitted run's instruction baseline.
