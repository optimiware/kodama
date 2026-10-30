# Pipeline execution and services

**Scope:** Layer 1 pipeline-runtime kernel under [ADR 0013](../adr/0013-two-layer-architecture-and-domain-simplification.md) and [O-38](../decisions.md#pipeline-runtime-and-four-layer-refinement--2026-10-30-o-38). This specification owns pipeline documents, runs, steps, durable waits, workspaces, and container services. The [pipeline-runtime simplification plan](../implementation/pipeline-runtime-simplification.md) owns the transition from the current executable compositions.

## Model

1. **Pipeline (`pipelines`):**
   - Defined in `.kodama/<processname>.yaml` in a repository or registered through the API.
   - The whole parsed definition is persisted once in `pipelines.definition` (JSONB).
   - Names are unique within a project. The repository is the definition source, not a second naming scope; a conflicting definition source is refused.
   - Each run stores an immutable definition snapshot; later edits cannot change it.
2. **Run (`runs`):**
   - One execution of a pipeline against pinned inputs and optional source commits.
   - States: `queued`, `running`, `succeeded`, `failed`, `cancelled`.
   - Allocates one shared `/workspace` filesystem for its steps. A task may accumulate several runs; one is its current execution.
3. **Step (`run_steps`):**
   - One row per step per run, created when the run is admitted.
   - Container states: `pending`, `running`, `succeeded`, `failed`, `skipped`, `cancelled`.
   - Wait states: `pending`, `waiting`, `succeeded`, `failed`, `skipped`, `cancelled`. A wait is `succeeded` when it is decided with any recorded outcome, including a rejection.
   - Runs the declared container `image` and `commands` with declared secret references, environment variables, and the shared `/workspace`. There is no agent-specific SQL kind.
4. **Wait:**
   - A non-container control step that pauses the same run until it is decided by an authorized signal, its deadline, or cancellation.
   - Persists a unique identity, assignment, correlation, optional request, expected input kind, deadline, version, and at most one decision. Syntax and bounds are owned by the [pipeline format](../contracts/pipeline-format.md#waits).
   - Holds no worker and runs no command. A signal is not approval by itself.
5. **Condition:**
   - A step's `when` predicate, a bounded expression over declared inputs, prerequisite step results, and wait outcomes. Its grammar, missing-value rule, visibility and bounds are owned by the [pipeline format](../contracts/pipeline-format.md#conditions).
   - There is no condition step kind: a requirement that `when` cannot express must amend the contract first. The engine executes no arbitrary code.
6. **Scoped container services (`services`):**
   - Support containers such as PostgreSQL, Valkey, mock APIs, or preview servers.
   - **`pipeline` scope:** started with a run and stopped when it finishes.
   - **`project` scope:** long-lived support shared across a project's runs (`project_id` set, `run_id IS NULL`).
   - **`tenant` scope:** long-lived support shared across a tenant's projects (`project_id` and `run_id` null).
   - States: `starting`, `ready`, `failed`, `stopped`.

## Kernel boundary

The kernel executes a prepared plan. It receives the immutable pipeline snapshot, pinned inputs and sources, declared secret references, and resource requirements through kernel-owned ports. It does not read Git, call the Kodama API, query tasks, publish NATS subjects, or open SQL transactions.

The shell owns:

- resolving a project pipeline and preparing its run admission;
- indexing definitions from Git and matching triggers;
- source checkout and knowledge-base preparation;
- secret custody and authorization;
- task projection and wait-inbox authorization;
- durable state and outbox persistence;
- NATS publication and signal delivery;
- API, MCP, and CLI transport.

Concrete runtime, state, secret, workspace-preparation, log, and clock adapters are wired by the composition root. The kernel's unit and contract tests use memory implementations without a database, broker, or container.

## Execution

1. Admission snapshots the definition, inputs, sources, secret references, and resource requirements, and records one step row per step.
2. A worker claims the run and progresses one executable step at a time. A prior container must settle and release the workspace before the next executable step starts.
3. The backend launches a workload with a stable identity. Observation and recovery use that identity; an uncertain launch or stop is reconciled and never blindly replayed.
4. A wait commits its durable state before its signal is announced. An authorized result consumes the wait and advances the same run.
5. Cancellation blocks new claims, stops active workloads, and preserves evidence needed to settle uncertain effects.
6. A run is terminal only after every owned resource is verified stopped and removed, or after an explicit bounded failure records why cleanup remains pending.
7. A rerun creates a new run, a new workspace, and new history. It does not reset completed step rows; see [rework and continuation](#rework-and-continuation).

## Rework and continuation

A plan is acyclic and each step runs at most once per run. Rework takes one of two forms:

1. **A declared forward step** in the same run, for example a step selected by a `when` on a wait outcome. It never reopens or repeats an earlier step.
2. **An explicit new run** that continues the work. Its admission names the continued run, the pinned source commits it starts from (typically the candidate commit the earlier run pushed to a run branch), and any recorded outputs it receives as declared inputs. Nothing else carries over.

The new run prepares a fresh workspace from those pinned commits. The workspace of a terminal run, or one discarded after lost or uncertain state, is never reused, adopted, reset, or handed to another run. A run admitted for a task is admitted only when the task's current run is terminal; admission makes it the current execution. Waits, decisions and authority of the earlier run do not carry over.

## Agent-requested waits

A workload never suspends mid-command and cannot open, resolve or answer a wait; run authority excludes those operations. A step that needs input records a bounded request as a declared output and exits. When the step settles as `succeeded`, its outputs are recorded with its result in the same transition. A declared downstream wait step consumes them through `request` and `when`; if the output is absent the wait is skipped by its `when` or opens without a request, as declared. Outputs of a `failed` or `cancelled` step are discarded and open nothing. A step that cannot continue exits non-zero and fails normally.

## Resume

Resume means the same run continues through the next eligible declared step after a wait is decided or the controller restarts. The next step starts from persisted references only: the run snapshot, settled step results, recorded wait decisions, and the run's workspace and resource identities. The previous command is not replayed and a settled step is not repeated. A step whose launch or stop outcome is uncertain is reconciled, not restarted ([PX-32](#requirements)).

A run with an open wait stays `running`; the task projection reports `waiting`. The run workspace and run-scoped services are retained until the wait is decided, bounded by the wait's deadline. If required workspace or service state is lost during a wait or before the next step starts, the run fails with reason `workspace_lost` or `service_lost`; a replacement is never prepared in its place.

## Eligibility, skipping and joins

A step is evaluated once, when every prerequisite in `depends_on` is terminal, in declaration order among simultaneously eligible steps. A prerequisite is visible as `succeeded` or `skipped`: a failed prerequisite ends the run before any dependent is evaluated. The first matching rule applies:

| Order | Check | Result when not satisfied |
| --- | --- | --- |
| 1 | Join: with `join: all` (default) every prerequisite succeeded; with `join: any` at least one succeeded | `skipped`, reason `prerequisite_skipped` |
| 2 | `when` evaluates to true | `skipped`, reason `condition_false` |
| 3 | Protected steps only: every `requires` entry names a wait that is `succeeded` with its `approving` outcome | `skipped`, reason `requires_unmet` |

Otherwise the step runs, or opens its wait. Evaluation reads only persisted values. The `when` result and a digest of the values read are recorded with the step in the same transition; recovery reuses the record and never re-evaluates.

A rejection is a recorded outcome, not a failure. A wait decided with any declared outcome settles `succeeded` with that outcome; it is never `failed`, never retried, and never reported as an infrastructure or workload failure, and it does not fail the run. Only the following produce a failed step, each recorded with a bounded reason code: a failed workload, `wait_expired` (deadline passed with `on_expiry: fail`), `correlation_missing` (a declared correlation value was missing when the wait would open, so no wait opens), `workspace_lost`, `service_lost`.

A failed step fails the run. No further step starts, steps that never started become `skipped` with reason `run_failed`, and every open wait is decided `cancelled`. Cancelling a run instead marks steps that never started `cancelled`. A run whose steps all settle without failure is `succeeded`, even when branches were skipped.

A step with `authority` is a **protected step**. It is eligible only through check 3, so only a wait decided with its single `approving` outcome satisfies it: a rejection, any other declared outcome, `expired`, and a skipped, failed or cancelled wait never do, and the step cannot run unconditionally. The format refuses a definition whose `requires` names anything else ([protected steps](../contracts/pipeline-format.md#protected-steps)), and the engine checks the recorded outcome again regardless. Timeout branching on unprotected steps uses `when` only. The shell repeats the check when it issues authority ([integration](git-repositories.md#integration)).

## Wait decisions

A wait has a version and exactly one decision, taken from `waiting`:

- **resolved** with a declared outcome and the identity of the deciding signal;
- **expired** when the store's clock reaches the deadline; the step then follows the wait's `on_expiry` (`fail` or `continue` with the reserved outcome `expired`);
- **cancelled** by cancellation of the run (decided in the same transaction that records the cancellation) or by failure of the run.

Each decision is one version-checked compare-and-set, committed with the step transition, task-projection intent and outbox events in one transaction. Resolution, expiry and cancellation race on that single check, and the deadline is compared with the store's one clock inside it, so a resolution committing at or after the deadline loses to expiry. The first commit wins. A later request performs no transition, creates no second continuation, and returns the recorded decision:

- a replay of a recorded signal returns its recorded result ([signal identity and routing](#signal-identity-and-routing));
- a new signal addressed to a wait that is already decided is refused as `decided`, returns the recorded decision marked not applied, and is recorded once as `signal.refused`;
- a request carrying an outdated expected version against a still-waiting wait is refused as a version conflict and applies nothing.

The continuation derives from the recorded decision and is re-driven by reconciliation after a crash. The guarantee is one committed decision and an idempotent continuation; commands that run afterwards are not exactly-once.

## Signal identity and routing

Every signal, inbox resolution or external, carries a caller-supplied key of 1 to 128 characters. Its **signal identity** is the tuple (tenant, project, authenticated caller principal, key). The shell derives an opaque `signalId` from the tuple; the raw key never appears in events. Two callers, projects or tenants that use the same key have unrelated identities.

A durable signal record binds the identity to its original target, a digest of the request (address, outcome, answer) and the result, applied or refused. A winning signal's record commits in the same transaction as its decision; a refusal's record commits when the refusal is returned. Every record, including a refusal that resolved no target, is a minimal fixed-size tombstone (identity, request digest, result code, time). It never expires: it outlives the wait and run it refers to and is removed only with the project, so a retry can never be rerouted to a wait that opened later. The shell bounds growth by a per-caller rate limit applied before routing; a rate-limited request creates no record and does not use its key. Routing runs in this order:

1. **Identity lookup.** If the identity has a record, return its recorded result unchanged, whatever the wait's state now is and however many other waits have opened. The signal stays bound to its original target and is never re-routed. A repeat with a different request digest is refused to its caller as `key_reuse`; that response creates no record, emits no event and never modifies the recorded result.
2. **Address a wait.** For a new identity addressed by wait identity, resolve it inside the caller's tenant and project and the caller's authority. A missing, foreign or unauthorized wait is refused as `unknown_wait`, indistinguishably, and discloses nothing about other tenants. A wait that is already decided is refused as `decided` with its recorded decision.
3. **Address by correlation.** For a new identity addressed by correlation, match only open waits of the caller's project that carry exactly those values. Zero matches is `unknown_wait` and several is `ambiguous_correlation`. Closed waits are never matched, so a late new signal cannot reach one.
4. **Decide.** One match proceeds to the single decision in [wait decisions](#wait-decisions).

Each refusal is recorded under the caller's own signal identity, so retrying it returns the same refusal.

## Assignment and correlation

A wait carries separate attributes:

| Attribute | Question | Mutability |
| --- | --- | --- |
| Identity | Which wait is this | Immutable and never reused |
| Assignment | Who may decide it through the inbox | Changeable only by an authorized, version-checked reassignment that never alters identity, correlation or a decision |
| Correlation | What is being decided | Immutable key/value pairs read when the wait opens |

An inbox resolution addresses the wait identity; an external signal addresses the complete correlation. When both are given they must agree. [Routing](#signal-identity-and-routing) defines how an address selects a wait. A decision belongs to one wait and is never inherited by another wait, step or run, even when assignment, input kind and correlation are identical. A changed candidate (any changed correlation value) can only arise in a different wait, a later declared wait step or a new run, and needs its own identity and decision.

## Shared workspace and services

Every run uses one disposable shared `/workspace`:
- The shell prepares the pinned source and context through an injected preparation step or adapter before user commands run.
- Steps execute in deterministic topological order, sharing generated files, caches, and commits. A step records outputs only through its per-step output directory, which is outside `/workspace`.
- Secret material is mounted outside `/workspace`; a step explicitly granted a secret may copy it into shared files, and publication filters must account for that disclosure.
- When the run finishes, the workspace is removed. Durable outputs are Git commits and metadata-only artifacts.
- Run services start with the run and are ready before dependent commands run. Loss of an essential service fails the run; it is not silently recreated with empty state.

## Signals and coordination

NATS is the shell's central bus. The shell publishes committed events and routes signals, triggers, and monitoring subscriptions. The kernel emits execution events through its ports; it does not depend on NATS types or subjects.

- **Git triggers:** `git.refs_updated` matches pipeline triggers and admits a run pinned to the pushed commit.
- **Task and wait signals:** an authorized resolution emits its decision event for the exact run, step, and wait ([wait decisions](#wait-decisions)).
- **Run and service observations:** committed run/step transitions feed the task projection and monitoring consumers.
- **External signals:** a pipeline may wait for an integration callback addressed by its correlation, with a deadline ([assignment and correlation](#assignment-and-correlation)).

Delivery is at least once. Consumers deduplicate against run, step, wait, event, or trigger identity. A missed wakeup is repaired by reconciliation.

## Requirements

| ID | Requirement |
| --- | --- |
| PX-28 | The kernel MUST execute a pipeline snapshot through kernel-owned state, runtime, resource, secret, workspace-preparation, log, and clock ports; it MUST NOT import shell, task, Git-hosting, SQL/NATS/HTTP client, identity, or agent packages. |
| PX-29 | A project MUST have at most one pipeline per name; conflicting definition sources MUST be refused rather than silently replacing one another. |
| PX-30 | A run MUST snapshot its definition and inputs at admission and MUST create one step row per step; a later pipeline edit MUST NOT change an admitted run. |
| PX-31 | A wait MUST pause the same run without a running workload and persist its identity, expected input, deadline, accepted result, and continuation; a duplicate or late signal MUST NOT advance it twice. |
| PX-32 | A run MUST progress through durable intent and reconciliation: workload identity is stable, uncertain launch/stop is not replayed blindly, and a run is terminal only after owned resources are confirmed stopped and removed or cleanup remains visibly pending. |
| PX-33 | The shell MUST publish committed execution events to NATS and consumers MUST deduplicate and reconcile; the kernel MUST remain runnable without a broker. |
| PX-34 | Run-scoped services MUST reach `ready` before dependent steps and MUST NOT be silently recreated after loss; tenant- and project-scoped services MUST outlive individual runs. |
| PX-35 | Execution MUST be acyclic with each step run at most once per run; rework MUST be a declared forward step or an explicit new run that names its continued run, pinned source commits and received outputs, and the workspace of a terminal or discarded run MUST NOT be reused. |
| PX-36 | A workload MUST NOT suspend mid-command or open, resolve or answer a wait; it MUST record a bounded request as a declared output that is persisted only when its step succeeds, and a declared downstream wait MUST consume it. |
| PX-37 | Resume MUST continue the same run through the next declared step from persisted references without replaying a settled or uncertain command; loss of required workspace or service state during a wait MUST fail the run, and a run with an open wait MUST stay `running`. |
| PX-38 | A `when` predicate MUST be evaluated once, when all prerequisites are terminal, over persisted values; the result MUST be recorded and reused on recovery; the engine MUST NOT add a condition step kind or execute arbitrary code. |
| PX-39 | A wait decided with any declared outcome, including a rejection, MUST settle `succeeded` with that outcome and MUST NOT fail the run or be treated as an infrastructure failure; skipped prerequisites and joins MUST follow the ordered eligibility rules, and run failure MUST skip unstarted steps and decide open waits `cancelled`. |
| PX-40 | A protected step MUST be eligible only when every `requires` wait is `succeeded` with its single `approving` outcome, MUST be skipped otherwise, and MUST NOT be reachable unconditionally by `when`, `join`, a rejection, any other outcome, `expired`, or a skipped, failed or cancelled wait; timeout branching on unprotected steps MUST use `when` only. |
| PX-41 | A wait MUST have exactly one decision (resolved, expired or cancelled) taken by one version-checked compare-and-set against the store clock; losing requests MUST change nothing, MUST NOT create a second continuation, and MUST return the recorded decision. |
| PX-42 | Wait identity, assignment and correlation MUST be separate; identity and correlation MUST be immutable, assignment MUST change only by a version-checked authorized operation, a decision MUST NOT be inherited by another wait, and a changed candidate MUST have a fresh wait identity and decision. |
| PX-43 | A signal identity MUST be the tuple (tenant, project, caller principal, caller key); routing MUST look up that identity before any wait matching, a recorded identity, including a targetless refusal, MUST be a non-expiring tombstone that returns its recorded result permanently and stays bound to its original target, a changed request under a recorded key MUST be refused without changing the record or emitting an event, and a new request MUST match only open waits of the caller's scope, with foreign and unknown waits indistinguishable. |

PX-01–PX-05 are retired by this amendment. They described the previous run model, project workflow triggers, and kernel composition. Historical acceptance reports that cite them remain historical evidence for the superseded model.

## Acceptance

| ID | Scenario | Requirements |
| --- | --- | --- |
| PX-A24 | A plain multi-step pipeline runs with memory/Process adapters and no SQL, NATS, Git, HTTP, task, or agent service; two steps share workspace files and declared secrets | PX-28, PX-30 |
| PX-A25 | A project refuses a duplicate pipeline name from another repository; an admitted run keeps its snapshot after the definition changes | PX-29, PX-30 |
| PX-A26 | A run waits across controller restart without a worker; one authorized signal resumes it once, a duplicate and a late signal change nothing, and a run-scoped service is not silently recreated after loss | PX-31, PX-32, PX-34 |
| PX-A27 | With the broker unavailable, state commits and the outbox retains the event; after recovery, consumers process it once, a redelivery changes nothing, and reconciliation repairs a missed wakeup | PX-32, PX-33 |
| PX-A28 | A rework branch runs as a declared forward step without repeating an earlier step; a continuing new run starts from the pinned candidate commit in a fresh workspace and inherits no wait or decision; a second run for a task with a non-terminal current run is refused | PX-35 |
| PX-A29 | An agent step that records a question and succeeds causes its downstream wait to open with that request; one that records none skips the wait; a failed step's outputs open nothing; a workload cannot resolve a wait with its run token | PX-36 |
| PX-A30 | After a controller restart between wait decision and the next step, the same run starts the next declared step once without re-running any settled step; loss of the workspace during a wait fails the run with `workspace_lost` | PX-37 |
| PX-A31 | A `when` over a missing output is false and skips the step; a skipped prerequisite still reads `status == "skipped"` while its outputs and outcome are missing; the recorded result is reused after restart even if the referenced value changed; every grammar bound is enforced | PX-38 |
| PX-A32 | A reviewer rejection takes the declared rejected branch and the run succeeds; `join: all` skips on a skipped prerequisite and `join: any` runs; a failed step skips unstarted steps and cancels open waits; a protected step is skipped after rejection, any non-approving outcome, expiry or cancellation | PX-39, PX-40 |
| PX-A33 | Concurrent resolution, expiry and run cancellation of one wait commit exactly one decision; a replay of the winning key returns its result, a new signal for the decided wait is refused as `decided` with the recorded decision, and one continuation runs | PX-41, PX-43 |
| PX-A34 | A candidate change in a new run yields a new wait identity and an open inbox entry that the earlier decision does not satisfy; reassignment changes who may decide but not identity or correlation; an external signal matching zero or two open waits is refused | PX-42 |
| PX-A35 | With `on_expiry: continue`, an unprotected step whose `when` tests `outcome == "expired"` runs after expiry; a protected step requiring that wait is skipped with `requires_unmet`, as it is after a rejection | PX-39, PX-40 |
| PX-A36 | After a wait is resolved, a retry of the winning key returns the recorded decision, including after the wait closed and after another wait with an identical correlation opened; a new signal by correlation matches only the open wait, and a new signal for the closed one is refused | PX-41, PX-43 |
| PX-A37 | An identical key from another tenant, project or caller is an independent identity; a changed request under a recorded key is refused as `key_reuse`, repeatedly, leaving the record unchanged; zero and multiple correlation matches are refused as `unknown_wait` and `ambiguous_correlation`, and replaying each returns the same refusal even after a matching wait has opened, which it never decides; a foreign wait ID is refused exactly as an unknown one | PX-43 |
