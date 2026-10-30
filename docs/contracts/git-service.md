# Git service contract

**Status:** proposed contract for internal repository hosting ([ADR 0011](../adr/0011-internal-tenant-git-service.md)); behavior and acceptance are in [Git repositories](../specs/git-repositories.md). T-126 freezes the executable schemas, DTOs and fixtures under `contracts/git/`; until then the shapes below are proposals and no route is served. IDs, hosts and digests in examples are placeholders.

## Repository resource

`P` means `/tenants/{tenantId}/projects/{projectId}` as in the [HTTP contract](http-api.md).

| Route | Purpose |
| --- | --- |
| `GET, POST P/repositories` | List; create (idempotent on `name`, `Idempotency-Key` required) |
| `GET, PATCH P/repositories/{id}` | Read; change `defaultBranch` or policy under `If-Match` |
| `POST P/repositories/{id}/archive`, `POST P/repositories/{id}/restore` | Change push/run admission |
| `GET P/repositories/{id}/refs` | Current branch and tag tips (bounded, cursor-paginated) |
| `GET P/repositories/{id}/pushes`, `GET P/repositories/{id}/pushes/{pushId}` | Push records and outcomes |
| `POST P/repositories/{id}/imports` | Import an external repository (scope pending O-32) |
| `POST /tenants/{tenantId}/git-credentials` | Exchange the caller's access token for a short-lived Git credential |

```json
{
  "id": "repo_01",
  "name": "backend",
  "defaultBranch": "main",
  "hosting": "internal",
  "status": "ready",
  "reason": null,
  "cloneUrl": "https://git.example.invalid/git/acme/inventory/backend.git",
  "policy": {"directPushToDefault": false},
  "version": 3,
  "createdAt": "2026-10-30T10:00:00Z"
}
```

`name` matches `^[a-z][a-z0-9-]{0,62}$`. `policy.directPushToDefault` is required on creation and has no server default until O-33 is decided. `defaultBranch` is a valid branch name, not a ref path. A `PATCH` of `defaultBranch` changes which branch is protected and applies to the next push; it renames nothing in Git. Errors follow the HTTP contract; additional codes are `repository_name_taken` (409), `repository_not_ready` (409), `repository_archived` (409) and `hosting_mismatch` (422).

## Git endpoint

```text
https://<git-host>/git/<tenant-key>/<project-key>/<repository-name>.git
```

Only the smart HTTP endpoints are served: `GET …/info/refs?service=git-upload-pack|git-receive-pack`, `POST …/git-upload-pack`, `POST …/git-receive-pack`. Dumb HTTP, `upload-archive` and every other path return 404. Protocol v2 is passed through the `Git-Protocol` header. Credentials arrive as HTTP Basic with any username and the Git credential as password, or as a bearer token. Unknown, foreign and unauthorized repositories return the same 404 after authentication; a missing or invalid credential returns 401 with `WWW-Authenticate`.

The server forwards an authorized request to the tenant's worker with a forwarding assertion: a signed, short-lived token with audience `kodama-git-worker:<tenant>`, the repository ID, the grant ID and whether receive-pack is permitted. The worker verifies it and resolves the repository path itself; the original credential is never forwarded.

## Git credentials and grants

```json
{
  "grantId": "gg_01",
  "audience": "kodama-git",
  "tenantId": "t_acme",
  "projectId": "p_inventory",
  "actor": {"type": "workload", "id": "att_01", "runId": "run_01"},
  "repositories": [
    {
      "id": "repo_01",
      "read": true,
      "update": ["refs/heads/kodama/agents/wi_1042/run_01/*", "refs/heads/kodama/work/wi_1042"],
      "seed": false,
      "integration": null
    }
  ],
  "expiresAt": "2026-10-30T10:15:00Z"
}
```

These are the grant's claims as the server records them; the credential itself is opaque. Human credentials come from `POST /tenants/{tenantId}/git-credentials` (bounded lifetime, renewed by the `kodama git credential` helper) and carry the person's current project roles at use time, not a frozen ref list. Workload grants are issued at attempt admission from the run's source set and its declared access, delivered like the attempt token, and revoked with it. Every executable attempt may update its run's agent-branch namespace; the task branch appears only in the grant of the attempt holding the writer reservation, as in the example above.

An integration grant constrains exactly one update:

```json
{"ref": "refs/heads/main", "from": "2222222222222222222222222222222222222222", "to": "3333333333333333333333333333333333333333", "gateId": "gate_07"}
```

## Worker interface

Workers call the control plane at `/internal/git/v1/tenants/{tenantId}` with a tenant-bound worker identity (audience `kodama-git-control`). These routes are not part of the public API or client.

| Route | Caller | Purpose |
| --- | --- | --- |
| `POST …/pushes` | `pre-receive` | Authorize intended updates; create the push record |
| `POST …/pushes/{pushId}/commit-intent` | `reference-transaction` (`prepared`) | Move the transaction's entries from `authorized` to `committing`, or refuse so the transaction aborts |
| `POST …/pushes/{pushId}/outcome` | `post-receive` | Confirm applied updates |
| `GET …/receives/{receiveId}` | Control plane → worker | Whether the receive is still running, and its ref-journal entries |
| `POST …/repositories/{id}/initialized` | Worker | Confirm provisioning |
| `GET …/assignments` | Worker | Repositories to initialize, archive, maintain or reconcile |

The control plane calls workers through their Service for provisioning, ref reads, ref-journal and receive-liveness reads during reconciliation, retained-ref management and maintenance. Workers cannot call any other control-plane route.

Authorization request and response:

```json
{
  "repositoryId": "repo_01",
  "grantId": "gg_01",
  "receiveId": "rcv_8f2c",
  "updates": [
    {"ref": "refs/heads/kodama/agents/wi_1042/run_01/impl", "old": "0000000000000000000000000000000000000000", "new": "4444444444444444444444444444444444444444"}
  ],
  "pushOptions": []
}
```

```json
{
  "pushId": "push_01",
  "updates": [
    {"ref": "refs/heads/kodama/agents/wi_1042/run_01/impl", "verdict": "allow", "reason": null}
  ]
}
```

`receiveId` is generated by the worker once per receive operation, passed to every hook, and makes each call idempotent. The response sets the receive deadline that the worker enforces.

Commit intent is sent from the `prepared` state of each ref transaction, with the refs locked:

```json
{"receiveId": "rcv_8f2c", "updates": [{"ref": "refs/heads/kodama/agents/wi_1042/run_01/impl", "old": "0000000000000000000000000000000000000000", "new": "4444444444444444444444444444444444444444"}]}
```

The control plane answers `200` with `{"committing": true}` only while every listed entry is `authorized`, the grant's attempt is live and the deadline has not passed; otherwise `409` with reason `push_settled`, `attempt_inactive` or `receive_expired`, and the hook exits non-zero so Git aborts the transaction. The hook first appends `prepared` to the repository's ref journal, then requests commit intent, and exits zero only if both succeed; `committed` and `aborted` follow from the hook's later states. Journal entries are `{receiveId or operationId, ref, old, new, observed, state, sequence}`, fsynced before the hook returns. `observed` is the ref's value read during `prepared` while the hook holds the lock (Git may pass an all-zero `old` for a forced or server-side update); outcome entries carry no `observed` value. The worker serializes ref mutations per repository, so a reconciliation read taken under that lock with no receive in progress is a consistent boundary. If any update is denied, `pre-receive` fails and Git applies none of them; the denied refs and their stable reasons (`ref_protected`, `not_fast_forward`, `grant_scope`, `attempt_inactive`, `repository_archived`, `seed_only`, `integration_mismatch`) are returned to the client on stderr. Push options are recorded but never interpreted as authority.

## Push records and events

Push record status: `authorized`, `applied`, `refused`, `unknown`. A record stores the actor, grant, origin run (if any), repository, `receiveId` and one entry per update with its own state: `authorized`, `committing`, `applied`, `refused` or `unknown`. Settlement rules are in the [specification](../specs/git-repositories.md#push-journal-and-events).

`git.refs_updated` is a new event type (envelope as in the [event catalog](event-catalog.md); `resource.type` is `repository`):

```json
{
  "pushId": "push_01",
  "repositoryId": "repo_01",
  "actor": {"type": "workload", "id": "att_01"},
  "originRunId": "run_01",
  "updates": [
    {"ref": "refs/heads/kodama/agents/wi_1042/run_01/impl", "old": "0000000000000000000000000000000000000000", "new": "4444444444444444444444444444444444444444", "kind": "create"}
  ]
}
```

`kind` is `create`, `fast_forward`, `rewrite` or `delete`. The event carries references only: no file contents, commit messages or credentials. `repository.created` and `repository.state_changed` cover the repository lifecycle.

## Source set

Runs on internally hosted repositories pin a source set in their input snapshot and runtime context:

```json
{
  "sources": [
    {"repositoryId": "repo_01", "commit": "1111111111111111111111111111111111111111", "path": "backend", "access": "write", "branch": "refs/heads/kodama/work/wi_1042"},
    {"repositoryId": "repo_02", "commit": "5555555555555555555555555555555555555555", "path": "frontend", "access": "read", "branch": null}
  ]
}
```

`path` is relative to `/workspace` and unique within the run. A source set with one entry at path `.` is equivalent to today's single source. The pipeline-format and runtime-context fields that carry it are a versioned addition owned by T-131; existing `kodama/v1alpha1` and `kodama/v1alpha2` documents, contexts, digests and result hashes are unchanged.

## Compatibility

- Repositories with `hosting: external` keep the current workspace publisher, bundle transport, HMAC push hook ([`api/git-webhooks.yaml`](../../api/git-webhooks.yaml)) and single-repository configuration.
- Existing `RepositoryURL` provenance fields hold the internal clone URL for internal repositories; consumers that need identity also receive `repositoryId`.
- New response fields are optional; clients that ignore them keep working.
