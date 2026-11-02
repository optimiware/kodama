---
status: accepted
---

# ADR 0011: Internal tenant Git service

**Date:** 2026-10-30  
**Direction:** owner-requested (O-31, 2026-10-30). Kodama hosts its projects' Git repositories itself; Git is an internal coordination layer for agents and humans, with tenant isolation from the first release.  
**Disposition:** accepted as the normative design of **internal repository hosting**. This is not delivery or qualification: the current executable model (an external HTTPS Git host, private attempt clones and the control-plane publisher of [ADR 0005](0005-git-backed-workspaces.md)) stays authoritative for existing projects and admitted runs until the T-133 cutover. The one-worker-per-tenant topology is the proposed baseline (P-17) and is validated by T-129/T-133.  
**Register:** [O-31](../decisions.md#internal-git-service--2026-10-30-o-31).  
**Specification:** [Git repositories](../specs/git-repositories.md) (GR). **Contract:** [Git service](../contracts/git-service.md). **Implementation:** [plan](../implementation/internal-git-service.md).

## Context

Kodama's input is tasks, issues and change requests; its output is deployed code. Agents do most of the source work, and they coordinate through Git: branches per task or agent, merges, rebases and pushes. Humans mostly feed Kodama tasks, but an engineer can still clone a project to debug, review or fix it, and can bring an existing project in with an ordinary `git push`, in the style of Heroku.

The current design treats Git as an external, untrusted host. A workload receives only read access; its changes travel as a Git bundle through the artifact store to a control-plane publisher, which alone holds branch-write credentials and performs an expected-tip compare-and-swap. A project has one configured repository, a durable workspace pairs one work item with one branch, and a run needs an actionable work item. O-03 explicitly excluded a Kodama Git server.

Those choices made sense for a factory attached to someone else's Git host. They do not fit a product that creates its own repositories, lets several agents exchange commits, admits developer pushes, and must isolate tenants inside the same installation.

## Decision

### 1. Repositories are project resources hosted by Kodama

A project owns zero or more **repositories**, each with an immutable ID, a project-unique name and a configurable default branch. PostgreSQL stores repository metadata, ownership and status; Git stores history. Kodama does not copy the commit graph into SQL. Tasks and runs reference repositories and exact commits; a repository exists independently of any task.

Repositories are created through the API, CLI or an MCP tool, typically by an agent's provisioning step. Provisioning is idempotent: reserve metadata, initialize a bare repository at an ID-derived path, install server-owned configuration and hooks, mark it ready. A provisioning run can start before the repository exists.

### 2. Native Git does the Git work; Kodama wraps it

The protocol and storage are Git's own: `git http-backend` for smart HTTP (protocol v2), `git receive-pack`/`upload-pack`, server-side hooks and `git maintenance`. The Kodama binary gains a `kodama git serve` role that authenticates requests, resolves repositories from server-held paths, runs the backend with a sanitized environment, and implements the hooks in Go. No Git library replaces the CLI, and no new wire protocol is written.

### 3. One Git worker and one volume per tenant

Each tenant gets one Git worker workload and one durable volume holding all of that tenant's bare repositories, in a Kodama-managed tenant service namespace separate from the namespaces that run tenant commands. Workers mount only their tenant's volume and hold no database or broker credentials. The control plane and PostgreSQL stay shared ([ADR 0003](0003-shared-control-plane.md)). The volume uses single-Pod access so that at most one worker writes it, and storage outlives every worker replacement, run and pipeline.

All Git traffic, from humans and from workloads, enters through the Kodama server's `/git/` route, which authenticates the credential, authorizes the repository, and forwards to the tenant's worker with a short-lived forwarding assertion that the worker verifies independently. Step workloads already reach the control plane, so no new execution egress is opened.

### 4. Access is granted per repository and ref, and checked when refs move

Git credentials are short-lived, audience-bound grants: a human's comes from the Kodama login through a Git credential helper; a workload's is issued for its attempt. A grant names repositories, read access and the ref patterns it may update. Before accepting any ref update, the worker's `pre-receive` hook asks the control plane to authorize it against the grant and **current** authority (membership, attempt liveness, cancellation, ref policy). Branch rules distinguish run-scoped agent branches, which any attempt of their run may push; the task branch, which only the work item's reserved writer may advance; the protected default branch, which only an integration grant or permitted human may advance by fast-forward; and server-only `refs/kodama/*`.

Git's own expected-old-value check on each ref update is the compare-and-swap. The pre-receive authorization adds what Git cannot know: who may move which ref, now.

### 5. Every accepted push is journaled and becomes an event

A Git ref update and a PostgreSQL transaction cannot commit atomically. The authorization call therefore records a **push record** (intended updates) before Git updates refs. Git's `reference-transaction` hook, which runs with the refs locked, then acts as a two-phase fence: it writes a receive-attributed entry to a durable per-repository ref journal on the tenant volume and obtains commit intent from the control plane, or aborts. `post-receive` confirms the outcome. A reconciler settles unconfirmed entries from that journal and the receive's liveness, never from ref values, reflog or Pod deletion alone, and leaves anything it cannot prove `unknown`. A confirmed push commits a `git.refs_updated` event through the existing outbox; pipeline triggers admit exact-commit runs from it with event-identity deduplication.

Git reports ref updates, not workflow intent. Kodama supplies the meaning: a task-branch push triggers validation, an accepted integration records a merge, a default-branch update triggers CI/CD. A revert is a pipeline that creates a revert commit or redeploys an identified artifact; a branch reset or forced update is a recorded history rewrite, never a rollback.

### 6. Agents push directly; integration stays an ordinary pipeline step

For internally hosted repositories, an attempt pushes its commits with its own grant: to its run's agent branches always, and to the task branch only while it holds the writer reservation. The run models of ADR 0010 are unchanged, so a run's attempts still execute one at a time; agents work in parallel as separate runs with independent clones and agent-branch namespaces, and a later run's writer merges their branches. Worktrees remain a within-workload convenience. Integration is an ordinary step: produce the candidate, test and review that exact commit, then advance the default branch with an expected-tip push under an integration grant bound to the approved candidate. A changed target requires a new candidate.

This replaces the bundle-and-publisher path for internal repositories only. It keeps what that path protected: stale or revoked attempts cannot write, accepted work cannot be overwritten, unknown outcomes are reconciled before reassignment, and pinned commits stay reachable through retained refs.

### 7. Lifetimes stay separate

Repositories live as long as their project. A run's workspace disk, its checkouts and its support services (a database, Valkey) live as long as the run ([ADR 0010](0010-run-workspace-services-builders.md)). The Git service is a coordination layer like those services, but it is not a run resource and is never removed by run cleanup. Run scratch is still never a checkpoint; a commit is durable once Kodama confirms its push.

### 8. Multiple repositories per run

A run pins a **source set**: each selected repository with one exact commit and a checkout path. Evidence and gate subjects identify that set. A multi-repository delivery pins all commits together but does not claim atomic updates across repositories.

## Consequences

- **Supersedes** O-03's "do not implement a Git server" for internal repositories, and amends [ADR 0004](0004-logical-workspaces.md) (work item ↔ task branch association instead of a single-repository workspace) and [ADR 0005](0005-git-backed-workspaces.md) (publisher-only write authority) for internally hosted repositories. Their rationale and the current model remain in force for external repositories and for admitted runs.
- **New operational duties.** Kodama now runs Git: Git CVE patching through the image, request and pack size limits, scheduled maintenance, retention and pruning of merged branches, backups (volume snapshots plus per-repository bundles in object storage) and restore drills.
- **Cost and availability.** One Pod and volume per tenant is a baseline cost. A worker restart briefly interrupts that tenant's Git access; Git clients retry. Throughput per tenant is one worker; scaling is per tenant first, sharding later.
- **Confidentiality granularity is the repository.** Branch rules restrict writes, not reads; separate confidential work into separate repositories.
- **Forwarding through the server** keeps network policy simple but places Git traffic on the control plane. A direct authenticated path from execution namespaces to workers is a later optimization that must preserve the same checks.
- **Migration (amended by O-32/O-33, 2026-10-30).** There is no provider import or mirroring in this release. An engineer bootstraps an existing codebase by pushing its local history into a new, empty internal repository under a single-use seed grant; commit IDs are preserved. Existing externally hosted repositories keep their configuration, publisher and admitted runs and are not migrated in place. The default branch is protected by default (`directPushToDefault: false`). Bundles and the publisher remain while any repository stays externally hosted; retiring them needs a separate decision removing external hosting, then draining its consumers.

## Alternatives considered

- **External Git host (GitHub, GitLab) as the coordination layer.** Rejected for the core product: it puts repository creation, credentials, branch policy and push events behind third-party APIs and rate limits. Mirroring and import are excluded from this release (O-32); a developer push bootstraps instead.
- **Bundled Forgejo or Gitea.** A capable, small server, but it duplicates Kodama's identity, permissions, tenancy and event handling and adds a second product to operate. Native Git plus thin wrappers is smaller.
- **`git daemon` with network-only access control.** Simplest transport, but it has no authentication or TLS and cannot provide tenant isolation or per-attempt permissions.
- **go-git or libgit2 in process.** go-git lacks a complete server and merge implementation; libgit2 needs cgo. Both replace a well-tested CLI with a less-tested layer.
- **Shared repository volume across tenants.** Fewer Pods, but one process and filesystem for every tenant; rejected for first-release isolation.
- **Bare repositories mounted into step Pods.** Avoids a network hop, but lets tenant code write repository internals, hooks and refs directly; rejected.
