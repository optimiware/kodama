# Kodama v2

**Status:** Final self-contained target specification for the clean v2 implementation.

## 1. Product and architecture

### Purpose

Kodama is an Apache-2.0, API-first, multi-tenant software factory, deployable standalone or with Optimi Platform. Engineers configure projects, pipelines, credentials, and guidance; users submit tasks. Container pipelines, agents, and ordinary tools produce source changes, tested artifacts, and deployed software. Humans answer questions and approve candidates through a role inbox.

**The central rule: a pipeline is an ordered list of actions; an action runs a container or requests an answer; an execution runs a pipeline once; a task is a conversation that starts executions.**

### Three functional layers

| Layer | Responsibilities |
|---|---|
| **1. Kernel** | Executions, container and request action lifecycles, shared workspaces, declared secret delivery, cancellation, action deadlines, recovery, logs, and outputs. |
| **2. Shell** | Control plane: tenants, projects, identity, authorization and verification, resource bindings, pipelines, task conversations, request decisions, inbox, secrets, service accounts, persistence, the event table as stream, event publication, push triggers, and one operation router with REST and MCP edges. Optional in-process capabilities: model gateway, external MCP proxy. |
| **3. Userland** | Actual work and integrations: pipeline definitions, containers, scripts, images, engineering conventions, delivery recipes, adapters, optional Git hosting, independently operated agentic services, external harnesses, and the native agent. |

**Kernel executes. Shell coordinates and authorizes. Userland performs the work.** From inside out: **Kernel → Shell → Userland**; dependency direction: **Userland → Shell → Kernel**. These are code-ownership and dependency boundaries; deployment topology is an installation choice. `Shell` means the control plane; container command shells are workload tools.

The kernel consumes small interfaces for durable state, workloads, resources, secrets, workspace preparation, logs, and time, implemented and composed by the Shell. It depends on no tasks, Git hosting, identity, HTTP, SQL drivers, NATS, or agents. Request settlement enters through these interfaces; the Shell owns who may answer and records what was approved.

Optional in-process capabilities belong to the Shell and invoke its authorizer directly. Independent services belong to Userland and use public APIs and scoped NATS events, never Shell internals. Only the Shell accesses the factory database. Projects can operate without Git or gateways. Optionality is configuration within these three layers; there is no separate Modules layer.

**The kernel advances executions; PostgreSQL records facts; NATS distributes activity; the Shell authorizes operations and connects them.** NATS is an independent integration boundary: execution progression, runtime observation, and deadline recovery do not depend on broker delivery.

**Kodama owns identity, roles, and recorded decisions. Services own resources, their access rules, and enforcement.** [Authorization and verification](#authorization-and-verification) are Shell capabilities, not another layer or an execution engine.

### Core model

```text
Tenants
  Projects
    Resource bindings → service-owned repositories and other resources
    Pipelines
      Executions
        Actions → container or request
    Tasks → conversation + selected pipeline + execution history
    Secrets, service accounts
```

The primary entities are **Pipelines, Projects, Tenants, Tasks, Executions, and Actions**, named consistently in authoring, operations, persistence, and public contracts. Requests are actions in `actions`, not a task kind or separate entity. Artifacts are output values whose bytes live in external registries, not entities.

A tenant owns an isolated organization. A project groups its software, work, configuration, and resources.

A resource binding records a stable resource ID, ownership scope, enforcing service, registered endpoint origins, verifiable operations, and reportable events. Repository bindings add remote metadata such as path and default branch; the Git service owns objects, refs, and [access rules](#git-access-rules). Admins manage bindings through the API; each change increments the binding version, is audited, and publishes an event. Services and workloads cannot claim another resource. Registration stores metadata, not bytes, access rules, or provisioning jobs.

A pipeline has a stable ID, project-unique name, and YAML, created or updated only through the API. Files kept in Git can be posted with `kodactl pipeline apply .kodama/`; the Shell never discovers or synchronizes them on push. Git push imports code, not pipeline definitions, and is not the standard work-submission mechanism.

Admission stores the exact YAML bytes and their SHA-256 hash, resolves images to immutable digests, and snapshots inputs, declared secret names, runtime requirements, and any task conversation and metadata. Pipeline edits cannot change this snapshot. Commits, refs, and artifact digests are ordinary input and output values; Git is one optional snapshot system, not a Kodama concept. Executions need neither a task nor a source repository; task-free runs may use inline YAML. There is no YAML canonicalization, separate definition-blob store, mutable digest ref, or pipeline version table. Secret values resolve at container activation.

A task selects a pipeline and retains its execution history. Planned requests wait within an execution; unplanned conversation questions hand work to a respondent between fresh executions, as defined in [task handoffs](#conversation-handoffs-and-fresh-executions).

Ownership, identities, references, queryable states, and concurrency versions are relational. Snapshot data, outputs, and metadata may use JSON; exact YAML stays with executions. Table count follows the implementation, not a design target.

## 2. Pipeline execution

### Ordered flow

The kernel visits container or request actions in declaration order, with at most one active action per execution. Activation persists the action's absolute deadline, then runs its container or makes its request answerable. Only success permits advancement; failure or cancellation stops progression.

There are no `when` conditions, general expressions, parallel actions, dependency graphs, joins, loops, backward jumps, or `skipped` state. Conditional behavior belongs in scripts or a separately selected pipeline. Each action settles once; rework creates a new execution starting at the first action, never resetting history or resuming an interrupted command.

Request subjects allow literals and direct lookups only, with no expressions or transformations: `{from: inputs.<key>}` or `{from: <action>.outputs.<key>}`. Admission rejects unknown, invalid, self, or forward references, unsupported or mixed action kinds, conditions, and dependency/join declarations. An unavailable subject value fails the request before it becomes answerable.

The kernel records settlement and advances; it never interprets human roles or access rules. Services owning protected resources check approvals through [verification](#authorization-and-verification).

### Container actions

A container action declares its image, commands, environment, secret references, timeout, and optional outputs.

**Status is standard; outputs and custom events are optional.** Runtime exit zero succeeds, nonzero fails; cancellation and timeout follow the lifecycle below. No custom outcome or output is required.

Following Drone's model, the adapter replaces the entrypoint with a fail-fast script using the image's shell and tools, without automatic command tracing or an injected Kodama runner. The runtime supplies exit status and stdout/stderr logs; the controller owns scheduling, cancellation, deadlines, and recovery.

Commands use HTTP, Git, or MCP directly. The optional `kodactl` client may be included in the image or downloaded as a pinned, checksum-verified release.

Container actions share one disposable `/workspace`, used sequentially and retained across request waits and controller restarts. A request runs no container; its successor receives the same files. Workspace loss fails the execution visibly rather than recreating scratch state or replaying commands.

The Shell publishes lifecycle events and log chunks. Containers may publish custom observations through the authenticated API using their execution credential; these never settle actions. Standard success events follow settlement and include available outputs or references.

### Request actions

A request asks its assignee for a result about a fixed subject: the candidate, artifact, environment, failure, or operation concerned. It declares `request`, a subject, timeout, and optional response fields through `outputs`. `request` names the assignee: `role/<role>` for members currently holding that human role in scope, such as `role/reviewer` or `role/operator`, or `service/<id>` for one service account. Task questions use the same assignee grammar. It has no image, commands, environment, secrets, or workload credential and cannot be added dynamically to an admitted execution.

After confirmed stop of preceding workloads, activation resolves subject lookups and persists the immutable subject, assignment, and deadline before publishing `action.requested`. The execution stays `running`; the request appears in the inbox, and its execution credential is revoked while waiting.

Resolution names the exact execution/action and expected action version, supplies `decision: pass` or `decision: fail` plus declared response values, and atomically validates active execution/action state, current assignment authority, deadline, and response schema before recording settlement, outputs, actor, and time. Human assignments require the role currently held in scope; services may answer only explicit service-ID assignments, regardless of their roles. Correlation or result strings grant no response authority.

Resolution, timeout, and cancellation compete for one terminal transition. Identical retries return the recorded result; replacements are refused. Passing succeeds the action and advances the same execution. Rejection fails action and execution while retaining the answer, actor, and time; timeout fails them with `timeout` and no answer. Cancellation yields no answer. Timeout and cancellation never imply approval. Further decisions need distinct declared requests; changing this action's answer requires a new execution.

A request used as approval evidence includes `operation` and `resource` in its immutable subject alongside the effect parameters. [Verification](#authorization-and-verification) matches `operation`, `resource`, and the subject fields the enforcing service supplies. Other subject fields, such as the image being promoted, are immutable display context for the assignee: workload-supplied values, not verified facts, and ignored by matching. Whether an effect needs approval, and from whom, is the owning service's rule, not the assignment's; changing a candidate or target requires a fresh decision. Passing neither starts another execution nor changes the task's pipeline or permissions.

### Results

Container outputs and request responses share nullable `actions.outputs`: one JSON object with up to eight declared named UTF-8 string values, each at most 4 KiB. Several related answers fit one response; larger content uses authorized durable references. Request decision, actor, time, version, and subject are separate metadata that workloads cannot supply as proof.

`PUT /api/v1/executions/{execution}/actions/{action}/outputs` accepts container outputs using the execution credential only while that container action is current and running. Submission and settlement serialize on the action row. Identical retries converge; different replacements are refused. Values remain staged until runtime-confirmed success; failure or cancellation clears them.

Only succeeded actions supply downstream output files: runtime-confirmed container outputs or authorized passed-request responses. Rejected responses remain inspectable in the action's decision record without becoming downstream outputs.

### Environment and materialized files

The Shell supplies four standard environment variables through the runtime adapter:

| Variable | Value |
|---|---|
| `KODAMA_URL` | HTTPS base URL of Kodama's API, reachable from the workload; normally an internal address |
| `KODAMA_EXECUTION` | Execution ID |
| `KODAMA_ACTION` | Current action slug |
| `KODAMA_TASK` | Associated task ID; unset for a task-free execution |

Pipeline/action `env` contains literals only; action values override defaults. The four standard variables are unshadowable context, not identity proof.

At container action activation, one `Prepare(action)` call materializes database state as read-only files, readable by the workload user:

| Path | Source |
|---|---|
| `/kodama/inputs/<key>` | Declared inputs from the execution's admission snapshot |
| `/kodama/outputs/<action>/<key>` | Settled outputs of earlier succeeded actions |
| `/kodama/task.json` | Task conversation at admission: a JSON array of messages, each with author, time, body, and optional references; task metadata is recorded in the execution snapshot, not in this file; absent for a task-free execution |
| `/kodama/secrets/<NAME>` | Declared secrets resolved after the resolver's policy check |

`Prepare(action)` also supplies writable `/workspace`; read-only files stay outside it and remain fixed for that workload. Subject lookups and files share one source: `<action>.outputs.<key>` maps to `/kodama/outputs/<action>/<key>`. Preparation failure prevents workload start. Reads use activation-time files; output writes use HTTP for identity and serialization with settlement. The runtime never inspects container filesystems.

The credential mounts read-only at `/kodama/secrets/KODAMA_TOKEN`, a name stored secrets cannot shadow. Among materialized read-only files, only this file changes during a workload, on controller-managed renewal. Clients read its current value when authenticating; `kodactl` does so by default. Tools needing secret environment variables use explicit commands such as `export DB_PASSWORD="$(cat /kodama/secrets/TEST_DB_PASSWORD)"`.

### Time

A timestamp records occurrence; a deadline records expiry. Every action's finite timeout is resolved at admission from its declaration or an installation default, then converted to a persisted absolute UTC deadline at activation. Action timeouts range from one minute to thirty days. Recovery uses that deadline and an injected clock; sender timestamps cannot make late answers timely. There is no execution-level deadline. Action timeouts bound active work, not queue delay or uncertain stopping; queueing and cleanup remain separately observable and cancellable.

### Example

The API-posted pipeline `change` declares its admission inputs below. Images and commands are illustrative project-supplied tools.

```yaml
inputs: [repository, target_ref, expected_base]
env:
  LOG_LEVEL: info
actions:
  - name: implement
    image: registry.example.com/coding-agent:1
    outputs: [candidate]
    commands:
      - implement-and-push
      - |
        candidate="$(git rev-parse HEAD)"
        curl --fail --silent --show-error \
          --header "Authorization: Bearer $(cat /kodama/secrets/KODAMA_TOKEN)" \
          --header "Content-Type: application/json" \
          --request PUT --data "{\"candidate\":\"$candidate\"}" \
          "$KODAMA_URL/api/v1/executions/$KODAMA_EXECUTION/actions/$KODAMA_ACTION/outputs"

  - name: test
    image: registry.example.com/test-tools:1
    secrets: [TEST_DB_PASSWORD]
    env:
      APP_ENV: test
    commands:
      - export DB_PASSWORD="$(cat /kodama/secrets/TEST_DB_PASSWORD)"
      - test-candidate "$(cat /kodama/outputs/implement/candidate)"

  - name: review
    request: role/reviewer
    timeout: 72h
    subject:
      operation: git.ref.update
      resource: {from: inputs.repository}
      ref: {from: inputs.target_ref}
      candidate: {from: implement.outputs.candidate}
      expected_base: {from: inputs.expected_base}

  - name: integrate
    image: registry.example.com/git-tools:1
    commands:
      - |
        integrate-candidate \
          "$(cat /kodama/inputs/target_ref)" \
          "$(cat /kodama/outputs/implement/candidate)" \
          "$(cat /kodama/inputs/expected_base)"
```

The image supplies Git and curl; `implement-and-push` publishes the candidate before submitting its output. The test reads that settled candidate and exports the materialized placeholder secret. After confirmed test stop, review waits without a container. Passing advances to integration in the same execution and workspace, under the [Git access rules](#git-access-rules); rejection or timeout fails the execution, and cancellation cancels it. The happy path needs one pipeline and one execution.

### Lifecycle and recovery

Execution states are `queued`, `running`, `succeeded`, `failed`, and `cancelled`; both action kinds use `pending`, `running`, `succeeded`, `failed`, and `cancelled`. A running request keeps its execution running. An action reaching its deadline fails with `timeout`: the controller stops its container, or its request stops accepting answers. All actions succeeding succeeds the execution; a failed action fails it and cancels unstarted actions with `upstream_failed`. Execution cancellation cancels active and unstarted actions. Terminal outcomes never change. Task completion is governed separately by [task state](#tasks-and-role-inbox).

Before a controller-managed effect, persist intent and stable resource identity. After a crash, observe and reconcile that workload. Automatic launch retry requires affirmative evidence the command never executed; unknown outcomes are never blindly replayed.

Cancellation prevents new work, revokes authority, and stops workloads. Workspace handoff, request activation, and replacement execution dispatch require confirmed stop; an expired database lease or deleted Kubernetes object is insufficient. Task handoffs terminate the execution and clean up its disposable resources; declared requests retain its workspace.

Cleanup is separate from execution outcome: confirmed or visibly pending with resource identities, bounded retries, and an actionable reason.

Every fresh attempt starts at the first action with a new identity, workspace, action deadlines, and credential. Inputs and the task snapshot may name durable references such as commits, digests, and answers, but never inherit scratch state, process memory, prior action success, or approvals. It obtains its own required decisions and reconciles unknown external effects before repeating them.

## 3. Shell capabilities

### Tasks and role inbox

A task holds a title, Markdown description and messages, metadata, selected pipeline and inputs, initiating principal, execution history, and waiting assignee. `metadata` is one bounded JSON object for userland state such as labels, priority, or workflow stage; Kodama stores, versions, snapshots, and publishes it without interpreting it. Authorized pipeline selection records the principal whose current start authority is checked on replies. Messages retain author, time, and durable references. There is no task `kind`, prerequisite `parent_id`, or continuation definition.

| State | Meaning |
|---|---|
| `open` | Work can be started, is executing, or needs operator follow-up after failure. |
| `waiting` | A conversation question is assigned to a human role or service account; the prior execution has been cancelled. |
| `done` | The current execution succeeded while the task was open. |
| `cancelled` | The task was explicitly cancelled. |

A task's status has three layers: its own state above, the current execution's state, and that execution's current or last action state. The latter two come from the derived, rebuildable execution view, so queued, running, and failed are execution states, never separately maintained task progress. Metadata customizes further. A declared request leaves its task open; `waiting` means a conversational handoff. Only the current execution succeeding while the task is open can mark it done. Older events cannot regress progress or overwrite waiting, cancelled, or newer work.

Creating a task alone starts nothing. Start and create-and-start atomically admit a queued execution with the task update and operation receipt, checking pipeline, inputs, and current authority. The task must be open, with at most one current execution. A replacement may be admitted and queued while its predecessor is still nonterminal; it becomes the task's current execution and dispatches only after all prior workloads are confirmed stopped. A start granted only by the operator role requires a failed current execution. The queued execution itself is the durable dispatch record, with no separate start-intent lifecycle.

Task content, including metadata, has its own optimistic version and one editing API for authorized humans, applications, and workloads. State changes and pipeline selection use explicit versioned operations; content, metadata, and comment edits cannot start work, answer the outstanding question, replace the current execution, or alter a decision.

The role inbox lists active requests and waiting tasks assigned to the caller's current roles, plus failed tasks for authorized operators. Services use the same API for their explicitly assigned requests/questions. Decisions and handoffs remain inspectable in execution and conversation history.

### Conversation handoffs and fresh executions

On an open task, an authorized human or the current execution may ask, naming the expected task version, question, assignee (`role/<role>` or `service/<id>`), and durable references. One transaction appends the question, marks the task waiting, cancels its nonterminal execution with the reason `requested`, the same as an explicit cancel, and revokes its credential; the task's `waiting` state records the handoff. Besides the caller-supplied task version, ask and cancel are bound to the current execution's version, and a reply to the version of the execution it replaces: a concurrent execution commit refuses them with `version_conflict`, nothing is committed, and the caller re-reads the task and retries with the same idempotency key. Stopping and cleanup reconcile from that state. Conversation questions create no request action and carry no approval evidence.

Save context durably before asking and reference it from the question: pushed commits, artifact digests, or an external restoration service, never just a disposable worktree path or process-local session ID. References are context, not proof of action success; cancelled-container staged outputs are discarded. One message can ask several related questions; subsequent questions repeat this handoff without adding loops to a pipeline.

An assigned reply names the outstanding question and expected task version. It atomically appends the answer, reopens the task, and queues one execution of its selected pipeline with the updated conversation snapshot. The reply's receipt identifies that execution for deduplication. Admission rechecks the initiating principal's current start authority; answering grants no execution permissions or pipeline choice. Unauthorized admission is refused. Reply and stop confirmation may arrive in either order; dispatch still waits for confirmed stop, without broker dependency.

The fresh attempt follows [recovery rules](#lifecycle-and-recovery), reads `/kodama/task.json`, and restores required context; missing referenced context fails the restoring action. A waiting task permits no ordinary start: an assigned reply or cancellation must settle the handoff. Cancelling a task cancels its nonterminal execution under the same execution-version binding as ask, and stale replies cannot reopen it.

Failure leaves an open task open in the operator inbox, with no automatic recovery request. An authorized operator can start a fresh attempt or cancel. A retry preserves failed history and must reconcile uncertain effects; an operator decision does not prove an effect never ran.

A merge service can answer an assigned request with the observed merge/rebase result while owning durable repository/ref serialization. A resource-available broadcast is no exclusive grant, and service responses cannot satisfy human review. Responders inspect current requests or retained events as well as future delivery, so earlier events cannot strand requests.

### Authorization and verification

Shell operations and optional in-process capabilities call one authorizer:

```text
Authorize(principal, operation, resource) → allow | deny
```

The principal is verified identity, never an asserted ID or role. Operations have known schemas validating names, types, and required fields. Resources derive scope from relational ownership, and creation from the owning tenant/project. Each check validates current credentials and principal state (service enablement, or the execution's running container action and deadline), tenant/project scope, and the principal's roles or execution permissions. Unknown operations or resources, invalid input, and unavailable authorization are refused. Authorization is explicit code, not an expression language or user-supplied policy program.

Enforcing services own their resources' access rules and decide every effect themselves. Immediately before an effect, they ask the Shell for facts:

```text
POST /api/v1/verify
  actor      presented credential: kodex., kodsvc., or human access token
  resource   registered resource ID
  operation  attempted effect and its subject, e.g. git.ref.update {ref, expected_base, candidate}
  approval   optional role/<role> required by the service's rules
→ principal, roles in the resource's scope, approved (when asked), verification ID
```

Verification authenticates the calling service and checks that its binding covers the resource and operation, then separately verifies the actor's credential and current state; service permissions never substitute for the actor's. Approval evidence is an earlier passed request in the actor's own execution, assigned to exactly that `role/<role>`, whose subject matches the operation, resource, and every supplied subject field; resolution already verified that the answerer held the role. Humans and services never carry approval evidence, and copied result strings never count.

Verification returns facts, not decisions. Which roles grant access, and which effects need which approver, are the enforcing service's rules, such as the [Git access rules](#git-access-rules); pipeline authors, request assignments, and workloads cannot change them. The Shell records each verification with service, actor, execution/action where applicable, resource, operation, subject, requested approval, and result. Its ID proves a check, not a bearer grant or completed effect.

Forwarded actor credentials are secret, used only for verification, and never persisted in journals, receipts, logs, or events. Services receive no tenant derivation keys or factory database access. Every call evaluates current state; prior results and mutation receipts cannot bypass revocation, membership changes, or cancellation.

Trusted enforcing services verify immediately before the exact effect, enforce local domain invariants and atomic preconditions, and prevent bypass write paths. Delayed or retried attempts verify again. There is no cross-store transaction: cancellation prevents subsequent verification, while started or uncertain effects still require service-owned observation and reconciliation. Further enforcing services, such as deployment targets, reuse verification with their own rules.

### Human identity

Humans authenticate through external OAuth/OIDC with RBAC. Tenant and project memberships compose five roles:

- **Admin:** configuration and membership administration.
- **Engineer:** repositories, pipelines, tasks, and execution.
- **Reviewer:** assigned review decisions.
- **Operator:** runtime operations.
- **Viewer:** authorized read access.

The Admin role grants no review authority, and access to service-owned resources follows each owning service's rules.

Humans present OAuth access tokens issued for Kodama's audience, to Kodama APIs and to enforcing services that forward them to verification. The Shell validates issuer, signature, audience, and expiry, then resolves current memberships: role changes apply on the next call, and identity-provider disablement within the short access-token lifetime. `kodactl login` holds the session and acts as the Git credential helper, supplying a fresh access token as the password for username `kodama`. Automation uses service accounts, never human sessions.

Tenant isolation applies to database records, execution, networking, credentials, logs, and events. Registered enforcing services, including Git, enforce the same scope on their own storage and protocols. PostgreSQL uses forced row-level security under a non-owner runtime role, with composite ownership constraints and project-level authorization.

### Service accounts

**Userland services authenticate as service accounts:** tenant/project-scoped software principals outside executions, unrelated to Kubernetes ServiceAccounts. `services` records scope, enabled state, roles, and the credential fields below. Admins create/disable accounts and issue/rotate/revoke credentials through the API; ordinary management reads expose metadata only.

Services have scoped human-style roles excluding Admin. Requests and conversation replies require explicit service-ID assignment, never human-role membership. Broker access derives from scope/roles: subscribe to authorized events/logs and publish custom observations under the authenticated source. Admin-managed resource bindings permit verification calls and specified event reports, not arbitrary impersonation or approval.

Services have no factory database access, plaintext secret-management reads, or execution-token issuance. Forwarded actor credentials follow the verification contract and cannot be persisted or used for unrelated operations.

### Bearer-token security model

The Shell issues HMAC-derived Bearer tokens: adapters deliver execution tokens, service owners deliver service tokens. Cluster location grants no authority. The execution is the workload trust boundary: all its containers share the current credential, and a retained valid token exercises current execution authority regardless of the receiving action. A service token identifies only its service account.

```text
execution_mac = HMAC-SHA256(
  tenant_key,
  encode("kodama-execution-v1", tenant_id, execution_id, nonce, expiry)
)
execution_token = "kodex." + execution_id + "." + base64url(execution_mac)

service_mac = HMAC-SHA256(
  tenant_key,
  encode("kodama-service-v1", tenant_id, service_id, nonce, expiry)
)
service_token = "kodsvc." + service_id + "." + base64url(service_mac)
```

`encode` is unambiguous canonical encoding; `base64url` has no padding. IDs are lookup hints until constant-time MAC verification against the current tenant key and stored nonce/expiry. Distinct prefixes/domains prevent principal-kind substitution. Authentication also checks expiry, principal state, and current authority; missing nonce or expiry is refused.

- **Key and storage:** each tenant has one Shell-only derivation key, at least 256 bits of entropy, stored in PostgreSQL encrypted under the configured key provider and outside declared workload secrets. Each principal has one current nonce of at least 256 random bits and absolute expiry in nullable `executions.token_nonce`/`executions.token_expires_at` or `services.token_nonce`/`services.token_expires_at`. The Shell reconstructs tokens for delivery/recovery; plaintext exists only for delivery/use, and PostgreSQL stores neither tokens nor token hashes.
- **Tenant rotation:** tenant Admins rotate through the API, immediately invalidating that tenant's execution and service tokens. The controller replaces tokens for eligible running containers without restart; service owners obtain replacements through authorized issuance. Other tenants are unaffected.
- **Service lifecycle:** expiry is required and explicitly configured. Issuance/rotation atomically replaces nonce/expiry and securely returns the token; replacement immediately invalidates the old token, and disabling/revoking clears both fields. There is no controller-managed service renewal. HTTP, MCP, and NATS use the same credential and enforce expiry, replacement, revocation, and disablement, including established broker connections/subscriptions.
- **Execution authority:** check tenant/project scope, execution state, running container action, and its deadline together. Permissions derive from the admitted definition/current action; URL IDs and environment cannot widen them. No workload access is granted during a request wait. Service authority instead derives from its enabled record, scope, and current roles.
- **Execution lifetime:** first issue occurs at container activation; issuance and renewal use TTL at most eight hours, bounded by the active action's deadline. Consecutive containers reuse an unexpired token while checks use the new action's deadline. Renew before expiry during long actions by atomically replacing nonce/expiry in PostgreSQL, then updating the mounted file through the adapter. Replace expired tokens before another container activation.
- **Revocation:** request entry clears credential fields; the next container after a pass gets a new nonce/expiry in the same execution. Cancellation, including the one an ask commits, and every terminal execution transition clear both fields atomically with the state change. Fresh attempts get new credentials. Refresh never replays work or revives terminal executions; started effects still require stopping/reconciliation.
- **Execution permissions:** assigned-task content and metadata, project task creation, questions on its task, declared outputs, custom events, and configured models/tools. Excluded: membership/pipeline/resource-binding administration, plaintext secret-management reads, request resolution, and assigned conversation replies. Service-owned resources, such as Git repositories, apply their own rules to `@execution`; questions and references cannot widen authority.
- **Transport:** HTTPS Bearer for REST, MCP, and `kodactl`. The supplied Git service accepts Bearer or credential-helper password with username `kodama`, forwarding only to verification. The `kodactl` credential helper reads the mounted token inside containers and the human session elsewhere, and supplies either only to registered trusted Git-service origins/paths. Tokens never enter URLs, repository-local configuration, logs, events, or metadata, follow redirects to unregistered origins, or reach upstream Git providers; those use provider credentials behind a trusted integration.

### Secret custody

The Shell's `secrets` table contains `id`, `tenant_id`, nullable `project_id`, `name`, `ciphertext`, `wrapped_key`, `key_ref`, `created_at`, and `updated_at`. Names are unique per scope, including tenant-wide rows. Rows hold only current values: writes replace, deletion removes. Authenticated envelope encryption binds ciphertext to installation and scoped identity; the wrapped data key shares the row.

At container activation, the resolver policy-checks declared names and resolves current values; project secrets override same-named tenant secrets. Missing, denied, or unreadable values fail preparation. Materialized secret files stay fixed; changes affect subsequent activations. Request actions resolve no secrets, and the kernel knows only the resolver/materialization contract.

Management reads expose metadata only. Model/MCP proxies resolve upstream secrets within the Shell on use. Bootstrap database and key-provider access stay outside this store.

### Events and operations

**NATS carries events; the REST and MCP edges invoke operations.** There is no command submission over NATS.

Events are owner-published named facts or observations; operations are authorized application calls shared by public interfaces and push automation.

**Publish activity by default:** scoped events cover task creation/questions/replies, request activation/resolution, configuration, verified Git updates, execution/action transitions, service accounts, cleanup, usage, progress, and logs—every domain change and observable runtime activity.

The versioned envelope holds stable ID, type, source, timestamp, ownership scope, applicable subject identity/version, optional correlation/causation, and bounded data or authorized references. Scope/source derive from authentication or verified ingress, never the body. Shell/userland use separate publish subjects. Credentials and plaintext secrets are excluded.

The Shell's PostgreSQL event table is the durable stream: domain mutations commit state, audit, and event rows (publication intent) together. Custom ingress authenticates and records before routing; publication retries preserve IDs. Events describe domain changes, not SQL row layouts. External services own their state and recovery boundaries, such as Git's receive journal.

Resource ingress verifies reporting service, binding, allowed event type, and schema. Accepted Git updates carry a stable source event ID and the `pre-receive` hook's verification ID. That verification must belong to the reporting service and match the reported resource, operation, and subject; its record supplies actor and execution causation. Deduplicate by service/source-event ID and refuse changed content. Reports authenticate the service, so previously accepted effects can be reported after actor-token expiry/revocation. They retain the service's source and grant no new authorization. Custom HTTP/NATS publication cannot impersonate this verified path.

NATS connects Shell, services, agents, and clients. JetStream provides durable at-least-once delivery/replay; Core NATS carries live logs/progress, with retained logs fetched separately. Consumers deduplicate and tolerate reordering. Outages expose publication backlog and delay userland integrations, while execution progression, request settlement, push triggers, queued dispatch, and deadline reconciliation continue. Browsers use read-only NATS WebSocket subscriptions.

### Pipeline push triggers and userland services

Optional `on.push` in API-posted YAML starts that pipeline for accepted external pushes to an explicitly selected repository/ref:

```yaml
repository: <repository-id>
service: <service-id>
on:
  push: [refs/heads/main]
inputs: [repository, ref, commit, previous_commit]
actions:
  - name: test
    image: registry.example.com/test-tools:1
    commands:
      - test-commit "$(cat /kodama/inputs/commit)"
```

`repository` names the registered resource, whose binding selects its trusted event source; `service` names the automatic-start principal, not necessarily the reporting Git service. Refs match exactly. Admission pins the pushed commit and supplies declared inputs from `repository`, `ref`, `commit`, `previous_commit`; no other inputs may be required. Ref deletions and execution-originated pushes start nothing, even across pipelines. There are no filter expressions, input mappings, selectable operations, separate trigger entities, or trigger-management API.

Configuration checks pipeline-management authority and Admin authority to bind/change the service principal. Invocation uses the shared start operation, checking enabled service, project scope, event-read permission, and current start authority for the definition. Referencing service IDs rather than tokens preserves bindings across rotation; disabling the start principal prevents invocation.

`(pipeline ID, event ID)` selects a common operation receipt independently of definition/binding edits. First handling selects the current YAML; the receipt retains event content identity, YAML snapshot, service, inputs, and result. Replay uses that selection, not edited configuration. The Shell reads accepted push events from its own event table, not NATS delivery, and advances past each only after a recorded result; permanent refusals are recorded and published, and transient failures retry. Suppressing execution-originated pushes removes automatic push chains.

Slack/email/webhook/timer services handle external protocols, publish observations, and call APIs with stable idempotency keys to create/start tasks or executions or answer explicitly assigned requests. There is no generic event-to-operation engine; request progression and task replies use persisted lifecycles, not triggers.

```text
external Git push → Git service → verified event ingress → event table → on.push → queued execution
email adapter → API → create and start diagnostic task
diagnostic pipeline → request: role/operator → pass → next action, same execution
agent question → waiting task → assigned reply → fresh execution
action.requested → assigned service → API → resolve exact request action
action/execution events + logs → userland service or web UI
```

A **userland service** is independently operated outside the Shell: Git, agents, Redis, managed databases, validators, or adapters. Even supplied companions such as `kodama git serve` are deployed/supervised by installation operators. Services use public API/NATS contracts as service accounts; enforcing services also use verification. Pipelines connect through registered endpoints or declared address/credential secrets; reachability is installation network configuration, not pipeline syntax.

### Public operations

One router dispatches every application operation to a shared handler behind two HTTP edges, REST and MCP JSON-RPC, so both expose the same operations and authorization. `kodactl` is a REST client:

- Manage projects, resource bindings and their metadata, API-posted pipelines, secrets, and service accounts.
- Create, inspect, and update tasks and their conversations; create and start work; query the role inbox.
- Ask and answer assigned task questions; cancel tasks; resolve active request actions.
- Start eligible executions with or without tasks; inspect, watch, and cancel them.
- Verify actors and approval evidence for a registered enforcing service.
- Publish custom events, report authorized resource events, and consume authorized event/log streams.

A mutation requiring idempotency atomically records its effects and a durable receipt scoped to tenant, project, principal, operation, and caller-supplied key. Both edges and push starts share this mechanism, with the additional pipeline/event identity above. Identical retries return the result; changed content is refused. Receipts remain until project deletion; optimistic versions protect transitions. Authorization and verification never replay cached results. External resource services own their mutation idempotency and reconciliation.

`POST /api/v1/tasks/{task}/ask` records a handoff; `POST /api/v1/tasks/{task}/reply` answers it and queues a fresh attempt. `POST /api/v1/executions/{execution}/actions/{action}/resolve` supplies a request's decision/output object. Cancelling an active request cancels its execution. Schemas and clients describe the served implementation.

Laptops, external applications/CI, and containers use the same API. Task edits require no execution.

## 4. Userland, agents, and software delivery

### Git service and knowledge

The optional `kodama git serve` companion owns native Git binaries, smart HTTP, provisioning, hooks, access rules, storage, receive journal, and recovery. It runs independently as a registered service account using only public API/NATS contracts, without factory database access, tenant derivation keys, or Shell internals. Existing hosts require an integration implementing the same verification, access-rule, event, and retention contracts.

The service owns Git protocol handling, access rules, and resource mutation. Kodama owns registered repository identities, scope, actor authentication, roles, and approval decisions.

Smart HTTP verifies the actor and applies read rules; the `pre-receive` hook verifies every proposed ref update of a push and applies write rules before any of them is accepted, both through [verification](#authorization-and-verification). Integrations must prevent bypass writes; after-the-fact webhooks are insufficient.

Provision repositories through the Git service and register their metadata with Kodama. Humans reach Git with their access tokens through the `kodactl` credential helper. The supplied service isolates tenants with separate Git workers and durable volumes that execution containers never mount; other hosts must provide equivalent isolation.

Container Git commands clone/fetch/push and verify commits named by inputs or outputs from registered repositories in `/workspace`; the Shell does not perform checkout. Naming conventions are:

```text
<tenant>/admin
<tenant>/context
<tenant>/<project>/context
<tenant>/<project>/<component>
<tenant>/<project>/deploy
```

Context repositories hold Markdown architecture, domain language, skills, blueprints, and conventions, read through the same Git service and credentials as other authorized sources.

### Git access rules

In the spirit of gitolite, access rules live in the tenant's `admin` repository. Tenant Admins edit `conf/git.conf` and push; the Git service applies the rules once it accepts that push. Rule changes are commits with history, review, and revert from Git itself; neither Kernel nor Shell stores Git rules.

```text
# <tenant>/admin → conf/git.conf

repo admin
    RW                              = @admin

repo shop/web
    R                               = @all
    RW+  refs/heads/work/           = @engineer @execution
    RW   refs/heads/main            = @execution
    option approve.refs/heads/main  = reviewer

repo shop/deploy
    R                               = @operator @execution
    RW+  refs/heads/release/        = @execution
    RW   refs/heads/main            = @execution
    option approve.refs/heads/main  = operator
```

Repository names are relative to the tenant. `R` reads the repository, `RW` creates or fast-forwards a ref, `RW+` also rewinds or deletes it, and `-` denies. Rules apply in order: the first rule whose ref and principal match and whose permission covers the attempt allows, a matching `-` rule denies, and no match refuses. A rule without a ref covers every ref; otherwise refs match exactly, or by prefix when ending in `/`, with no regular expressions. Principals are Kodama roles held in the repository's scope (`@admin`, `@engineer`, `@reviewer`, `@operator`, `@viewer`), `@execution` for running executions in scope, `@all` for every principal in scope, or service-account IDs.

`option approve.<ref> = <role>` makes every update to that ref also require approval evidence: the `pre-receive` hook asks verification for a passed `request: role/<role>` in the pushing execution whose subject holds `operation: git.ref.update`, the repository, and the exact `ref`, `expected_base`, and `candidate`. Service roles, result strings, and labels never count. Humans and services carry no such evidence, so they cannot update approval-guarded refs directly; integrating laptop work means pushing a work branch and starting an integration pipeline.

Provisioning a tenant creates `admin` with `RW` for `@admin`, keeping rule history append-only. Provisioning a repository adds default rules: `R` for `@all`, `RW+` on `refs/heads/work/` for `@engineer` and `@execution`, and `RW` on the default branch for `@execution` with `reviewer` approval. Engineers import history into empty repositories under rules Admins grant. Admins have no bypass beyond a recorded rule change in `admin`, which may itself require approval.

Git applies each accepted update atomically: fast-forward ancestry unless the rule grants `RW+`, and only if the ref still equals the expected base. A moved target fails integration; a changed candidate needs fresh review. Before mutation, journal intent, stable source event ID, and verification ID. Confirm the outcome from Git and durably record acceptance before reporting `git.ref.updated` through [verified ingress](#events-and-operations); the Shell then records and distributes it on NATS. Reconcile uncertain entries against repository evidence before retrying; retry interrupted reporting, not the push. Verified causation keeps execution-originated pushes out of push triggers even when reported after the execution ends.

Commits named in retained executions' inputs or outputs must remain reachable. The service discovers them through authorized execution queries, using events as notifications rather than sole truth. Its operator owns storage, journal recovery, retention, and backups; Kodama exposes coordinating references.

### Two agent execution modes

**External harnesses** such as OpenCode, Claude Code, Pi, or other coding tools run as ordinary container commands. They use the workspace, Git, declared secrets, and optionally Kodama's gateways.

**The native `kodama agent` runtime** adds planning, decomposition, review, validation, Git operations, knowledge lookup, and reusable Markdown skills. It records session identity, model/tool usage, structured outcomes, and optionally retained transcripts.

Both run as userland images under the same pipeline lifecycle. Agents produce results, use declared requests for planned decisions, and follow [conversation handoffs](#conversation-handoffs-and-fresh-executions) for unplanned input and durable session restoration. Neither waiting mechanism preserves a live agent process.

### Agentic services

Independent agentic services plan, review, answer explicitly assigned requests, or provide tools as ordinary userland clients. Pipeline agents use `kodex`; independent services use `kodsvc`. Kodama APIs already authorize calls; services enforcing owned resources use verification, including separate actor proof. Agents receive no special permissions or decision policy.

### Model and tool access

Two optional in-process Shell capabilities keep upstream credentials within the Shell and call its authorizer:

- **Model gateway:** OpenAI-compatible API, model allowlists, request/token bounds, attributable usage.
- **External MCP proxy:** registered MCP servers and allowed tools under current caller authority.

The MCP edge exposes Kodama operations; the proxy federates outside tools. Neither adds an execution engine. Ordinary pipelines need neither capability nor agent runtime. Independently operated equivalents are userland services using the public contract for Kodama-governed effects.

### Delivery recipes and artifacts

Build, test, review, publish, deploy, rollback, cleanup, and notification compose container/request actions using standard tools and explicitly authorized credentials. Targets include Kubernetes, cloud services, static hosting, or any system those tools support.

An artifact is an output value, not a Kodama entity. A build action publishes bytes with registry credentials, then submits a digest-pinned reference such as `registry.example.com/acme/web@sha256:…` as a declared output; an OCI image index carries several platforms under one digest. The external registry owns bytes, retention, and attached SBOMs or attestations. There is no artifact table, registration API, or catalog; userland indexes registries or `action.succeeded` events when it needs one.

A recorded output proves which execution and action submitted it, not independently verified publication or target health. Images may carry the execution ID as a label; `GET /api/v1/executions/{execution}` confirms that pointer against recorded outputs. A lost output response is retried with the identical value, without rebuilding. Only results that actually test or build a candidate are candidate-bound.

Promotion passes the built digest forward, within an execution through `{from: build.outputs.image}` or across executions as an input, and records production intent as a protected update of the `deploy` repository: a release action commits the digest on a work branch and outputs `candidate` and `expected_base`, a request approves that exact update, and the next action fast-forwards the protected ref. A GitOps controller or later action rolls out and verifies. Separate build and promotion pipelines let promotion retry without rebuilding; rollback is a new promotion execution with an older digest, with no automatic data rollback.

Recipes own readiness checks and reconciliation: successful command exit alone does not prove target health, and uncertain deployment outcomes must be observed before repetition. Hooks are bounded commands in declared images. Engineering policy composes pipelines, skills, tools, and event-consuming services through public contracts, not controller plugins.

## 5. Implementation, operation, and acceptance

### Implementation discipline

Use one Go module organized around Kernel, Shell, and Userland, with one explicit Shell composition, one operation router, one pipeline authoring model, and one execution engine. Prefer standard-library facilities, ordinary SQL, and small consumer-owned interfaces; native Git stays in its companion. Colocate explicit business transitions and domain invariants with their owning capability/service. Add abstractions only for concrete boundaries or demonstrated reuse. Request answers enter through authorized Shell operations, not a second workflow engine.

Entrypoints are `kodama` (`server` for Shell plus Kernel; `git serve`, `git hook`, `git provision-tenant`, and `git provision-repository` for the independent Git companion; `agent`; `services`; `migrate`; and `bootstrap-admin`) and `kodactl`. Optional workload tools remain ordinary image dependencies.

### Execution environments

| Environment | State and messaging | Runtime | Keys |
|---|---|---|---|
| `development` | Ephemeral memory adapters | Process test adapter | Ephemeral |
| `test` | PostgreSQL and NATS | Podman or Kubernetes | Local keyfile |
| `production` | PostgreSQL and NATS (TLS `tls://`, DSNs `sslmode=verify-full`) | Kubernetes Jobs | Cloud KMS |

The single-host [Podman quickstart](docs/runbooks/podman-quickstart.md) installs the `test` environment on one machine.

Environment selection is explicit. Production refuses development and test adapters.

One runtime contract provides `Prepare(action)` as [specified above](#environment-and-materialized-files), start, observe, logs, stop, remove, credential refresh, and capabilities. Process runs host-process tests; Podman and Kubernetes run containers. Requests need no separate adapter. Admission rejects unsupported runtime requirements; Process tests establish neither container nor isolation guarantees.

Production uses Helm, an initially single-replica control plane, tenant execution namespaces, resource limits, and tenant-isolating network policies. Native Kubernetes Jobs run actions without automatic command replay. Workloads reach Kodama, configured Git, and other services permitted by installation network policy. Independent Git-service verification and event calls use authenticated HTTPS.

The initial execution trust model is trusted project engineers using regular containers. Stronger hostile-workload isolation requires separate qualification.

Supported installation clouds are AWS, GCP, and Scaleway, with one explicitly selected KMS provider. The repository ships reference deployments for each (OpenTofu roots, Helm charts and scripts) and a [production deployment guide](docs/runbooks/production-deployment.md); production-grade infrastructure and its operation belong to the operator.

### Platform and operations

In hosted operation, Optimi Platform owns tenant authority, entitlements, billing, and commercial policy; Kodama owns factory execution and operational usage evidence. Integration uses public contracts. Hosted authority failure never falls back to standalone grants. Any optional UI belongs in Platform.

Expose health/readiness, bounded logs, audit history, authorization and verification records, bindings, waiting tasks, active requests, queued/running execution progress, cleanup, publication backlog, consumer lag, refused push starts, rejected resource reports, and dependency failures. Userland operators expose resource health and reconciliation backlog.

Control-plane backup covers PostgreSQL, retained logs, encryption keys, and JetStream event/consumer state. Coordinate external-output retention and Git repository/journal/configuration backups with their operators. Owning services retain referenced commits and artifacts; disposable workspaces are excluded. Restore fences old controllers, enforcing services, and consumers, preserves resource bindings, verification records, pipeline IDs, conversations, request decisions, receipts, and queued executions, and reconciles external effects before dispatch. Unrecoverable workspaces fail executions; operators may start fresh attempts.

Recommended recovery objectives: RPO ≤15 minutes, RTO ≤4 hours, and 30-day PostgreSQL recovery. Kodama does not enforce them: the [production deployment guide](docs/runbooks/production-deployment.md) explains how to reach them, and qualification measures and reports the reference deployment's restore timeline against them. Default retention: logs 30 days, ordinary evidence 90 days, audit metadata one year, extended by active references. Raw model/tool content is off by default; authorized opt-in starts at seven days.

### Delivery order

1. **Standalone kernel:** ordered containers sharing files; workload-free request pass/reject/timeout; cancellation and recovery through injected adapters.
2. **First useful factory:** compose identity, authorization/verification and bindings, PostgreSQL, NATS/event stream, tasks/decisions, secrets, interfaces, and the Git companion. Prove task → external coding harness → tests → review → protected integration under `admin` rules in one execution/workspace, plus conversation handoffs and operator retries. Expose lifecycle events and live logs to subscribers.
3. **Composable services:** prove verified push starts, email-created diagnostic work, service-request resolution, serialized merges, and durable agent restoration. Complete optional gateways, userland runtimes, knowledge conventions, and delivery recipes; reuse verification as further enforcing services arrive.
4. **Production qualification:** exercise real Kubernetes, KMS, isolation, cleanup, backup, and restore.

### Acceptance criteria

These numbered checks exercise the complete contracts linked below; they are a verification index, not replacement definitions.

1. **Kernel independence:** reconstruct both action kinds through injected adapters/settlement, without Shell, agent, or broker; ordinary images expose logs/status without an injected runner. [Execution](#container-actions)
2. **Immutable admission:** API-only definitions, exact YAML/hash snapshots, inline task-free runs, and immunity to pipeline edits or Git imports. [Model](#core-model)
3. **Idempotency:** retry interfaces and replay events without duplicate work or regressed progress, including pipeline/binding edits. [Operations](#public-operations), [triggers](#pipeline-push-triggers-and-userland-services)
4. **Request settlement:** restart and race authorized answers against timeout/cancellation; validate assignment/version and retain rejected answers without downstream outputs. [Requests](#request-actions)
5. **Protected integration:** refuse rejected, stale, foreign, wrong-assignee, wrong-operation/candidate/base, or prior-execution evidence, forged approval strings, direct human pushes to approval-guarded refs, and rewinds or deletions without `RW+`; apply `admin` rule changes on the next push and enforce atomic fast-forward updates. [Git rules](#git-access-rules)
6. **Recovery:** uncertain launch/stop causes neither blind replay nor concurrent workspace ownership; requests/controller restarts preserve workspace, and loss fails visibly. [Recovery](#lifecycle-and-recovery)
7. **Identity:** qualify tenant isolation across interfaces, Git, secrets, and runtime; exercise both HMAC lifecycles, renewal, revocation, tenant-key rotation, tampered/wrong-kind credentials, human access-token issuer/audience/expiry checks through the Git credential helper, and live NATS invalidation. [Identity](#human-identity), [tokens](#bearer-token-security-model)
8. **Delivery evidence:** publish a digest-pinned image as an output, test that exact digest, and promote it through an approved deploy-repository update without rebuilding; confirm attribution through the execution record and converge output retries. [Artifacts](#delivery-recipes-and-artifacts)
9. **Cleanup:** failures remain attributable, visible, and recoverable. [Recovery](#lifecycle-and-recovery), [operations](#platform-and-operations)
10. **Optional capabilities:** external/native agents share semantics; non-Git pipelines run without Git, plain pipelines without gateways/agents, and independent agentic services use service authority. [Agents](#two-agent-execution-modes)
11. **Materialization:** verify reserved variables/token name, literal environment precedence, file snapshots, shared workspace, live token renewal, and secret-change isolation. [Files](#environment-and-materialized-files), [secrets](#secret-custody)
12. **Outputs:** race HTTP submission/retries with settlement; expose only successful results, discard failed/cancelled staging, and never inspect container filesystems. [Results](#results)
13. **Ordered flow:** reject unsupported declarations/references, enforce single-action progression and persisted action deadlines, and prevent activation after failure/cancellation or preparation errors. [Flow](#ordered-flow), [time](#time)
14. **Integrations:** verify scoped NATS delivery, authenticated Git reporting and delayed causation, push exclusions/deduplication, direct adapter APIs, and durable queued starts. [Events](#events-and-operations), [triggers](#pipeline-push-triggers-and-userland-services)
15. **Broker independence:** recover publication backlog without blocking persisted execution/request/push-start/dispatch/deadline work; preserve event IDs and refuse forged runtime/decision status. [Events](#events-and-operations)
16. **Planned waits:** no live workload, retained execution/workspace, assigned inbox, exactly-once advancement on pass, and no advancement after rejection/timeout/cancellation. [Requests](#request-actions)
17. **Conversation handoffs:** atomically ask/cancel and reply/queue; race replies with stop confirmation, reject stale/bypass starts, restore durable references, and fail the restoring action when they are missing. [Handoffs](#conversation-handoffs-and-fresh-executions)
18. **Task progress:** operator retry preserves failure history without automatic recovery requests; only current success completes open work, never overwriting waiting/cancelled/newer work. [Tasks](#tasks-and-role-inbox)
19. **Authorization and verification:** exercise in-process authorization, service-plus-actor verification, exact approval matching with ignored display fields, forwarded-credential secrecy, and fresh results after revocation, membership changes, or cancellation. [Verification](#authorization-and-verification)
20. **Userland Git:** public-contract-only operation, ordered gitolite-style rules from `admin` with exact or prefix refs and default refusal, owned storage/retention/backups, uncertain-effect reconciliation, and journal replay that reports rather than reapplies updates. [Git](#git-service-and-knowledge), [rules](#git-access-rules)

**KISS means one owner for each fact, one mechanism for each lifecycle, and composition through a small set of reliable primitives.**
