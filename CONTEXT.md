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

**Repository**: A project-owned Git repository with an immutable ID, a project-unique name, a default branch and a hosting mode (`internal` or `external`). A project may own several; a repository exists independently of any work item. See [Git repositories](docs/specs/git-repositories.md).
_Avoid_: Using a clone URL as repository identity.

**Tenant Git service**: The Git worker and durable volume that store and serve one tenant's internally hosted repositories with native Git, behind Kodama's authenticated Git endpoint. It outlives every run.

**Git grant**: A short-lived credential scoped to repositories and the refs it may update, issued to a person through the Kodama login or to one attempt. The Git service checks it against current authority on every push.

**Push record**: The journaled intent and outcome of one Git receive operation; an applied record produces one `git.refs_updated` event.

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

**Workspace** (**Source workspace**): The durable source-working context paired with an actionable work item: its task branch in each repository the work touches. Existing `workspace` API/CLI names retain this meaning; in execution discussions, use **run workspace** for the shared `/workspace` filesystem ([ADR 0012](docs/adr/0012-container-sdlc-and-artifact-releases.md)).

**Task branch**: The branch `kodama/work/<work-item-id>` collecting a work item's changes in one repository; only the reserved writer attempt or an engineer updates it. **Agent branches** `kodama/agents/<work-item-id>/<run-id>/<label>` hold one run's work in progress; any executable attempt of that run may push them, so agents in separate runs can work in parallel and a later writer merges their branches.

**Checkpoint**: A committed source revision preserved for subsequent work or recovery.

**Candidate**: The source commit proposed for review and delivery; across several repositories, the set of commits pinned together.

**Integration**: Advancing a repository's protected default branch to an approved candidate from an expected base, performed by an ordinary pipeline step.

**Business analyst**: A person expressing business needs and reviewing delivered outcomes.

**Engineer**: A person shaping software and supervising its implementation.

**Reviewer**: A person authorized to approve or reject identified work and evidence.

**Workbench**: The optional, deferred Platform-hosted interface for work, execution, and delivery review; CLI/API/MCP/webhooks remain the primary Kodama surfaces.

## Execution and delivery

**Pipeline definition**: An immutable version of a dependency graph of executable steps and human gates.

**Pipeline run**: One execution of a definition with pinned inputs and recorded outcomes.

**Source set**: The repositories, exact commits and checkout paths a run pins at admission.

**Run origin**: What started a run: a work item, an accepted push, a manual request or repository provisioning.

**Step run**: The run-local progression of a step.

**Step attempt**: One attempt to execute a step, including its result and failure reason.

**Step entrypoint**: The wrapper coordinating workspace verification (or, in the `attempt_private` model, source checkout), command execution, and result collection for an attempt.

**Runtime**: The backend that runs attempt workloads (Kubernetes, Podman, or Process in tests) behind one execution port.
_Avoid_: Executor.

**Workload**: The runtime's instance running one attempt, identified by the attempt's deterministic name and the instance UID bound at dispatch.
_Avoid_: Job, container or process in backend-neutral contexts.

**Environment**: The adapter set a process composes, chosen by the required `KODAMA_ENV`: `development` (in memory, ephemeral), `test` (real services and Podman) or `production` (Kubernetes). `production` refuses development and test adapters.
_Avoid_: Profile, mode.

**Memory adapter**: An in-memory implementation of a port over the `memdb` transaction kernel, qualified by the same contract suite as its PostgreSQL adapter. It proves policy, never SQL semantics.

**Runtime guarantee**: A property a runtime enforces for every workload, such as image, isolation or network policy. Admission pins the guarantees a run requires.

**Run model**: The execution semantics a run pins at admission. `attempt_private` (format `kodama/v1alpha1`, runtime context v1) is the current executable model with a private checkout per attempt. `run_shared` (format `kodama/v1alpha2`, runtime context v2) is the versioned design with one run workspace, support services and a total deadline; it is not delivered or qualified until its cutover and acceptance. Old runs keep their model; a controller refuses a model it does not implement.

**Run workspace**: The disposable shared filesystem of one `run_shared` run, mounted at `/workspace` by one attempt at a time. It is prepared once from the run's pinned source (or empty for a source-free run); generated files persist between steps. Git retains source history and artifacts retain selected outputs. The run workspace is never a checkpoint or recovery authority and is distinct from the legacy **source workspace**.

**Preparation**: The controller-owned, one-shot checkout of a run workspace with read-only repository access and no user code; its control-plane receipt is the only evidence the checkout completed.

**Workspace holder**: The single executable attempt currently entitled to mount a run workspace. Handoff requires the previous holder's workload to be verified stopped and the volume released; an expired claim or deleted Pod is not a fence.

**Run resource**: Execution-owned state a `run_shared` run needs beyond its attempts: the run workspace, preparation, support services and the run's network boundary. It has deterministic identity, a spec fingerprint, a bound instance UID and ready/stopped/removed evidence, but no business status.

**Support service**: A declared, digest-pinned workload (for example a database) that starts with a run, must be ready before any user command, has a run-unique endpoint reachable as the pinned network model allows (the tenant's workloads under the tenant model, the run's under the run model), and is stopped when the run ends. It is neither a step nor a sidecar.

**Execution policy**: The versioned, operator-installed definition of workload security shapes, storage, limits, network model, egress profiles and builder modes that a `run_shared` run pins at admission. Pipeline documents cannot supply raw security, mount or network settings.

**Builder**: Tooling that builds and pushes OCI images, such as ko or Buildah, running as an ordinary step under the execution policy's `step` role. A run-exclusive builder service (BuildKit) is deferred (T-123). A mode is supported only with real qualification evidence.

**Command-begin fence**: The durable server record taken just before an attempt's command starts. Its absence, with the workload verified stopped, is the proof that no user code ran and an automatic retry is safe.

**Harness**: The environment managing an agent's model interaction and tools.

**Agent profile**: Versioned defaults selecting a harness image, instructions, model access, tools, and limits.

**Artifact**: An identified retained output, such as a report, log, image, or screenshot.

**Release**: A project-scoped name/version and immutable manifest identifying an exact set of retained artifacts and their source/producer provenance. A release version is distinct from a metadata concurrency revision. ADR 0012 design; implementation pending.

**Publication receipt**: An attributable verification that an artifact was copied to a particular destination with the expected digest. It records where a copy was observed, not perpetual availability. Source publication through Git is a separate operation.

**Evidence**: Artifacts and observations used to review a candidate or deployment.

**Gate**: A recorded request for one authorized human decision about an identified subject.

**Gate decision**: An immutable approval or rejection with its reviewer and reason.

**Environment**: A named application delivery target with scoped credentials and configuration.

**Deployment**: A record connecting a target environment, exact release/artifact inputs, source references, configuration and an ordinary step's reported execution outcome. ADR 0012 generalizes the legacy mandatory-image/Kubernetes shape; implementation pending.

**Rollback**: A new deployment run selecting a prior release and explicit configuration with currently authorized credentials. It does not rewrite history or automatically undo data/infrastructure changes.

**Preview**: A temporary deployment for reviewing a candidate.

**AI exploratory QA**: Agent-led application exploration producing evidence and defects.

**Blueprint**: Versioned application guidance, templates, and skills for a particular stack.

**Application chassis**: Markdown-only engineering guidance for a tenant or project, covering architecture/microservices, approved stacks/libraries and cloud deployment methods; not a shared application runtime library.

**Guidance repository**: A Git repository containing tenant/project-scoped chassis Markdown. Agent context identifies its immutable commit and selected paths; a moving branch is not an admitted run's instruction baseline.
