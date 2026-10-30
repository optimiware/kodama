# Git service contract

**Status:** contract for internal repository hosting under [ADR 0013](../adr/0013-two-layer-architecture-and-domain-simplification.md); behavior and acceptance are in [Git repositories](../specs/git-repositories.md). The executable forms are [`contracts/git/v1`](../../contracts/git/) (repository, credential, forwarding-assertion and push-authorization shapes) and [`contracts/events/v1`](../../contracts/events/README.md) (`git.refs_updated`). Where this page and the executable contract differ, the executable contract wins and this page is corrected. IDs, hosts and digests in examples are placeholders.

## Repository resource

| Route | Purpose |
| --- | --- |
| `GET, POST /api/v1/tenants/{tenantKey}/repositories` | List or create tenant-level repositories (`<tenant>/context`) |
| `GET, POST /api/v1/tenants/{tenantKey}/projects/{projectKey}/repositories` | List or create project repositories (`<tenant>/<project>/<name>`) |
| `GET, PATCH /api/v1/…/repositories/{id}` | Read; change `defaultBranch` or policy under `If-Match` |
| `POST /api/v1/…/repositories/{id}/archive`, `…/restore` | Change push and run admission |
| `GET /api/v1/…/repositories/{id}/refs` | Current branch and tag tips (bounded, cursor-paginated) |
| `POST /api/v1/tenants/{tenantKey}/git-credentials` | Exchange the caller's access token for a short-lived Git credential for one repository; with `seed: true`, a single-use seed grant |

```json
{
  "id": "repo_01",
  "tenantId": "t_acme",
  "projectId": "p_erp",
  "path": "acme/erp/backend",
  "defaultBranch": "main",
  "status": "ready",
  "reason": null,
  "cloneUrl": "https://kodama.example.invalid/git/acme/erp/backend.git",
  "policy": {"directPushToDefault": false, "integrationPipeline": "integrate"},
  "empty": false,
  "version": 3,
  "createdAt": "2026-10-30T10:00:00Z"
}
```

`name` matches `^[a-zA-Z0-9_-]+(/[a-zA-Z0-9_-]+)*$` and is unique per project. `status` is `provisioning`, `ready` or `archived`; `reason` is a stable code, `null` for a ready repository and set while provisioning has not completed or after an archive. `empty` is reported by the tenant's worker and decides whether a seed can be issued. `policy` is optional on creation; an omitted policy or `directPushToDefault` is `false` (O-33). `integrationPipeline` optionally names the process name whose runs may advance the default branch. `cloneUrl` never carries credentials.

Create and patch bodies are strict: unknown fields are errors and an optional member is omitted, never `null`. Additional error codes: `repository_name_taken` (409), `repository_not_ready` (409), `repository_archived` (409), `repository_not_empty` (409) and `seed_pending` (409).

## Git endpoint

```text
https://<git-host>/git/<tenant-key>/context.git
https://<git-host>/git/<tenant-key>/<project-key>/context.git
https://<git-host>/git/<tenant-key>/<project-key>/<repository-name>.git
```

Only the smart HTTP endpoints are served: `GET …/info/refs?service=git-upload-pack|git-receive-pack`, `POST …/git-upload-pack` and `POST …/git-receive-pack`. Dumb HTTP, `upload-archive` and every other path return `404`. Protocol v2 is passed through the `Git-Protocol` header.

Credentials arrive as HTTP Basic with any username and the Git credential as password, or as a bearer token. Unknown, foreign and unauthorized repositories return the same `404` after authentication; a missing or invalid credential returns `401` with `WWW-Authenticate`.

The server resolves the path to a repository by tenant and project key and forwards the request to that tenant's worker with a forwarding assertion:

```json
{
  "audience": "kodama-git-worker:t_acme",
  "tenantId": "t_acme",
  "repositoryId": "repo_01",
  "grantId": "gg_01",
  "receivePack": true,
  "issuedAt": "2026-10-30T10:00:00Z",
  "expiresAt": "2026-10-30T10:05:00Z",
  "requestId": "req_01"
}
```

The audience names the tenant's worker and the lifetime is at most five minutes. `receivePack` is true only when the credential may update a ref. The worker verifies the assertion and derives the on-disk path from `repositoryId`; the original credential is never forwarded.

## Git credentials and grants

```json
{
  "grantId": "gg_01",
  "audience": "kodama-git",
  "tenantId": "t_acme",
  "projectId": "p_erp",
  "actor": {"type": "workload", "id": "run_01", "runId": "run_01"},
  "kind": "run",
  "repositories": [
    {
      "id": "repo_01",
      "read": true,
      "update": ["refs/heads/kodama/runs/run_01/*", "refs/heads/kodama/agents/run_01/*"],
      "seed": false,
      "integration": null
    }
  ],
  "issuedAt": "2026-10-30T10:00:00Z",
  "expiresAt": "2026-10-30T10:15:00Z"
}
```

`kind` is `human`, `seed`, `run` or `integration`. `update` lists full ref names under `refs/heads/` or `refs/tags/` with an optional trailing `/*`; `refs/heads/*` and anything under `refs/kodama/` cannot be named. For a run, `actor.id` and `runId` are the run's ID.

Every grant names its tenant, project and repositories explicitly with `read: true`, and all three are checked at use. A human grant's recorded `update` is a ceiling, not a permission: current roles and the repository's ref policy restrict every update at every push. Run grants are issued at run admission from the run's pinned source set and revoked when the run ends.

The exchange request names the repository and says whether it is a seed:

```json
{"projectId": "p_erp", "repositoryId": "repo_01", "seed": false}
```

```json
{"username": "kodama", "password": "<opaque credential>", "grantId": "gg_02", "expiresAt": "2026-10-30T10:15:00Z"}
```

`password` is the credential, shown once and stored only as a hash; any username is accepted. `seed: true` requests a seed grant: `kind: "seed"`, `update` exactly `["refs/heads/<defaultBranch>"]` and a lifetime of at most 15 minutes. It is refused with `409 repository_not_empty` when the repository has any ref, `409 seed_pending` while a previous seed receive is unacknowledged, `409 repository_not_ready` or `repository_archived`, `404` for a foreign or unknown repository and `403` without `engineer` on the project. A push with a consumed seed grant is refused with reason `seed_consumed`.

An integration grant names exactly one repository whose `update` is exactly the default branch, and constrains that one update. It is issued, and honored at every `pre-receive`, only while the run's pipeline is the repository's `policy.integrationPipeline` **and** the run's task is approved: the task is a promotion request resolved `approved`, or a work task whose latest promotion request (a request whose parent it is) was resolved `approved`. A newer pending or rejected request withdraws an earlier approval; a run with no task, an unknown task or a task whose approval cannot be read is refused (`integration_mismatch`, or an unavailable error that fails the receive):

```json
{"ref": "refs/heads/main", "from": "2222222222222222222222222222222222222222", "to": "3333333333333333333333333333333333333333"}
```

## Branch namespace

| Ref | Class | Meaning |
| --- | --- | --- |
| `refs/heads/<defaultBranch>` | default | Protected |
| `refs/heads/kodama/work/<taskId>` | task | The task's branch |
| `refs/heads/kodama/runs/<runId>/<label>` | run | A run's own branches (no task) |
| `refs/heads/kodama/agents/<runId>/<label>` | agent | A run-scoped agent branch |
| any other `refs/heads/…` | other | Engineers; runs only when their source grants the pattern |
| `refs/tags/…` | tag | Engineers |
| `refs/kodama/runs/<runId>/<n>` | server | Retained pin; hidden from fetch, never in an event or a grant |

A label is one component of letters, digits and `._-`. Ref names use a conservative subset of Git's rules: components start with a letter, digit or underscore, no `..`, no `.lock` or trailing `.` component; anything else is refused explicitly.

## Worker interface

Workers call the control plane at `/internal/git/v1/tenants/{tenantId}` with a tenant-bound worker identity (audience `kodama-git-control`; claims `tenantId`, `workerInstance`, `issuedAt`, `expiresAt`, at most one hour). These routes are not part of the public API or client, and a worker identity is refused on another tenant's path.

| Route | Caller | Purpose |
| --- | --- | --- |
| `POST …/pushes` | `pre-receive` | Authorize intended updates; answer verdicts and the receive deadline |
| `POST …/pushes/{receiveId}/outcome` | `post-receive` | Report applied updates; commit the `git.refs_updated` event and the acknowledged journal sequence |
| `POST …/repositories/{id}/initialized` | Worker | Confirm provisioning |
| `GET …/assignments` | Worker | Repositories to initialize, archive, maintain or reconcile, with the acknowledged journal sequence |

The control plane calls the worker through the tenant's Service at `/internal/worker/v1`; every call is idempotent on the repository ID and `operationId`:

| Route | Purpose |
| --- | --- |
| `POST /repositories/{id}/initialize` | Initialize the bare repository (`created` false on a repeat; `empty` reports whether it has refs) |
| `POST /repositories/{id}/refs/list` | One page of refs under `refs/heads/`, `refs/tags/` or `refs/kodama/` |
| `POST /repositories/{id}/refs/values` | Exact ref values under the mutation lock with `quiescent` and `journalHead` |
| `POST /repositories/{id}/journal` | Journal entries after a sequence, for one `receiveId` or one `ref`, with `headSequence` |
| `GET /repositories/{id}/receives/{receiveId}` | `running` and the answering `workerInstance` |
| `POST /repositories/{id}/branches/create`, `…/delete` | Create or prune a task, run or agent branch at an expected commit, idempotently |
| `POST /repositories/{id}/retained-refs/set`, `…/delete` | Create or release `refs/kodama/runs/<runId>/<n>` |
| `POST /repositories/{id}/maintenance` | Run `git maintenance`, serialized with pushes |
| `POST /repositories/{id}/files` | Read `.kodama/*.yaml` and `.kodama/*.yml` at one exact commit (see below) |

Authorization request and response:

```json
{
  "repositoryId": "repo_01",
  "grantId": "gg_01",
  "receiveId": "rcv_8f2c",
  "updates": [
    {"ref": "refs/heads/kodama/agents/run_01/impl", "old": "0000000000000000000000000000000000000000", "new": "4444444444444444444444444444444444444444"}
  ],
  "pushOptions": [],
  "gitFacts": [
    {"ref": "refs/heads/kodama/agents/run_01/impl", "fastForward": null, "repositoryEmpty": false}
  ]
}
```

```json
{
  "deadline": "2026-10-30T10:10:00Z",
  "updates": [
    {"ref": "refs/heads/kodama/agents/run_01/impl", "verdict": "allow", "reason": null}
  ]
}
```

`receiveId` is generated by the worker once per receive and carries the grant the receive was made under (`rcv_<random>_<grantId>`), which the control plane verifies against the credential store; the answer's `pushId` is the `receiveId`. Authorization is decided afresh from current authority on every call: the control plane keeps no record of it, so a repeated request is answered by what is true now and there is no authorization replay or `idempotency_conflict`. The only idempotent step is the outcome, keyed by `(repository, receiveId)`. `gitFacts` carry what only Git can say, read by the worker so the control plane never calls Git while holding SQL locks: one fact per update, in order; `fastForward` is `null` exactly for a create or a delete, and `repositoryEmpty` is true only when the repository had no refs. A receive has at most 1024 updates and 16 push options; a larger push is sent in batches of refs. A request for a ref outside the grant, or under `refs/kodama/`, is a `deny` verdict, not a malformed request.

If any update is denied, `pre-receive` fails and Git applies none of them; the denied refs and their stable reasons (`ref_protected`, `not_fast_forward`, `grant_scope`, `run_inactive`, `repository_archived`, `seed_only`, `seed_consumed`, `integration_mismatch`) are returned to the client on stderr. Push options are recorded but never interpreted as authority.

`post-receive` sends the applied updates, possibly none: `{"receiveId": "rcv_8f2c", "applied": [{"ref": "…", "old": "…", "new": "…"}]}`, answered with the push ID (the `receiveId`) and the status `applied` or `refused` (nothing applied). The outcome carries `journalPosition`, the journal sequence of the receive's own first entry (omitted only when the worker cannot read its journal; a re-report carries the same value), which orders pushes as they were applied: a receive appends its prepared entries while it holds the locks of every ref it updates, so receives that update a common ref are ordered by it. The outcome may also carry `journalSequence`, the highest worker journal sequence at or below which every receive is reported; the control plane stores it as `repositories.journal_ack` and never lowers it. The deterministic event ID is not returned. A `reconcile` assignment carries `journalAck`, and the worker re-reports every receive after it. The initialization confirmation is `{"repositoryId": "repo_01", "defaultBranch": "main", "empty": true}`.

### Reading definitions at a commit

`POST /repositories/{id}/files` takes `{"repositoryId": "repo_01", "commit": "<non-zero object ID>", "paths": [".kodama/"]}` and answers `{"files": [{"path": ".kodama/build.yaml", "content": "…", "sha256": "<hex of the content>"}]}`, sorted by path. A path is a file under `.kodama/` ending in `.yaml` or `.yml` (path components of letters, digits, `.`, `_` and `-`), or the directory `.kodama/` itself, which stands for every such file directly in it (not recursive). A requested file that does not exist at the commit, is a directory or a submodule is absent from the answer, not an error; a commit the repository lacks (or an object that is not a commit) is `commit_not_found` (404). A request for any other path, a ref name instead of an object ID, an empty or oversized path list is `invalid_request`. At most 64 files are read at once, each at most 256 KiB and together at most 1 MiB: a request that would exceed a bound is refused whole (`invalid_request`), nothing is truncated. A symbolic link is returned as the link text and never followed. The worker runs Git with its sanitized environment, hooks disabled and replacement objects ignored, so a pushed `refs/replace/*` cannot change what is read.

## Push events

`git.refs_updated` is committed with the receive outcome in `audit_events`, under a deterministic ID derived from the repository and `receiveId`, so a retried or re-reported outcome emits exactly one event. The project is required. It carries the applied updates inline so consumers need no separate push record:

```json
{
  "receiveId": "rcv_8f2c",
  "repositoryId": "repo_01",
  "actor": {"type": "workload", "id": "run_01"},
  "originRunId": "run_01",
  "updates": [
    {"ref": "refs/heads/main", "old": "1111111111111111111111111111111111111111", "new": "2222222222222222222222222222222222222222", "kind": "fast_forward"}
  ],
  "journalPosition": 41
}
```

`journalPosition` (optional) is the receive's position in the worker's journal, carried from the outcome; `actor.type` is `user` or `workload`; `originRunId` is the run for a workload and an explicit `null` for a person, so a trigger can exclude a run's own push from the event alone. `kind` is `create`, `fast_forward`, `rewrite` or `delete`, decided from the update's old and new value and the worker's fast-forward fact. The event carries references only: no file contents, commit messages or credentials, and never a server ref. The `updates` of one event are the applied updates of one receive, 1 to 1024 of them; a server ref (`refs/kodama/…`) applied by a receive is not carried, and a receive that applied only server refs emits no event. Each update's kind agrees with its values (a create has an all-zero `old`, a delete an all-zero `new`, a fast-forward or rewrite neither), a ref appears once and `old` differs from `new`; the control plane validates the payload against the event contract before it commits the row. The envelope's `resource.id` equals `receiveId`. The envelope's data bound (32 KiB) holds for pushes of up to roughly 150 updates; a larger push's payload stays valid and is carried by the outbox row and the bus message, which are not envelopes.

### Publication

Every `audit_events` row is published from the outbox: rows with `published_at` null are read in `(next_attempt_at, id)` order and published to NATS JetStream under `kodama.v1.<tenant-key>.<project-key>.<event-type>` (`_` as the project key of a tenant-wide event), with the row ID as the broker message ID, then marked published. A failed publication increments `publish_attempts` and defers `next_attempt_at` (1 s doubling to 5 min); the row is never dropped. Delivery is at least once, so consumers deduplicate on a resource they own: a crash between the publication and the mark repeats the publication under the same event ID. `KODAMA_NATS_URL` (`nats://[token@]host:port`, the token otherwise `KODAMA_NATS_TOKEN`) selects JetStream, and a URL that cannot be reached or a server without JetStream fails startup rather than falling back; without it the events are delivered in process through the same port, which the server logs once at startup. `KODAMA_EVENT_POLL_INTERVAL` sets the outbox poll period (default 2 s). The consumer name is `kernel-git-dispatch`, on `kodama.v1.*.*.git.refs_updated`.

`repository.created` is committed when the repository becomes ready, after the worker confirmed initialization, and carries `name`, `defaultBranch` and `hosting`; `repository.state_changed` carries `from`, `to` and `reason`.

## Trigger configuration

Triggers are declared in the pipeline document itself, so a pushed definition and its triggers are indexed together:

```yaml
name: build
triggers:
  git:
    branches: ["main", "refs/heads/release/*"]
    tags: ["v*"]
    events: [push, tag]
steps:
  - name: test
    image: golang:1.27
    commands: ["go test ./..."]
```

A trigger selects one repository (the one the pipeline was indexed from), one to 32 ref patterns, and the event kinds it reacts to. `events` defaults to `[push]`. A pattern is a full ref (`refs/heads/release/*`) or a short name relative to `refs/heads/` (`branches`) or `refs/tags/` (`tags`); `*` matches any run of characters, including `/`. An update of a `refs/heads/…` ref is the event `push` and matches `branches`; an update of a `refs/tags/…` ref is the event `tag` and matches `tags`, so a pipeline that should start on tags lists `tag` in `events`. `merge` is reserved for integration and is not started by a push. A pipeline without `triggers.git` patterns starts nothing from a push.

Admission consumes `git.refs_updated`, skips a push whose `originRunId` names a run (a run's own push never retriggers), matches each applied update against every pipeline of the repository, starts at most one run per `(pipeline, push event, ref)` — the key `git.refs_updated:<event ID>:<ref>` in `runs.trigger_key` — and pins the entry's `new` commit as a read source at path `.`. A delete admits nothing.

### Indexing

When an applied update creates, fast-forwards or rewrites the default branch, `.kodama/<processname>.yaml` and `.yml` are read at that update's exact `new` commit (not the branch tip at processing time). Each file is parsed strictly and validated; a valid file is upserted into `pipelines` (`name` is the file name, `source: git`, `source_commit` the pushed commit, the canonical document) and its name must equal the document's `name`. A file that fails is refused with a stable code (the pipeline format's diagnostic codes, `name_mismatch`, `duplicate_name`, or `files_refused` when the commit's files cannot be read at all) recorded as a `pipeline.index_refused` audit row that names the file and code but never its content; the previous definition stays in force. A pipeline whose file is gone is disabled (`disabled_at`), not deleted, and a file that returns enables it again. A commit whose files are refused as a whole disables nothing. Indexing, the refusal rows and run admission of one event commit in one transaction under the repository's row lock, with a `git.pipelines_indexed` marker whose deterministic ID makes a redelivered event change nothing. A default-branch push whose `journalPosition` is lower than that of a default-branch push already indexed does not replace the newer definitions (a lost report is recovered after newer pushes, so neither the report time nor the event's commit time orders pushes, and the acknowledgement watermark does not either); an event without a position is indexed as it arrives. The repository's archived status is read again under the same row lock before indexing and admission: a repository archived while the files were read gets no index, no run and no marker, and the event can be dispatched after a restore.

## Source set

A run pins its sources in `runs.sources` at admission:

```json
[
  {"repositoryId": "repo_01", "commit": "1111111111111111111111111111111111111111", "path": "backend", "access": "write", "branch": "refs/heads/kodama/work/tsk_1042"},
  {"repositoryId": "repo_02", "commit": "5555555555555555555555555555555555555555", "path": "frontend", "access": "read", "branch": null}
]
```

`path` is relative to `/workspace`: clean, no `..`, made of components that start with a letter, digit or underscore (never `.git` or a hidden name), unique within the run, and no path is a directory prefix of another. A single entry at path `.` is the ordinary single-repository run. A `write` source names the branch its writer updates; a `read` source has `branch: null`.

Admission pins exactly one commit per source and never a later tip: a push-originated run pins the `new` value of its applied entry, a commit-pinned source exactly that commit, and a plain source the tip read once at admission. Repeating admission with the same trigger key returns the same pinned commits even after the branch moved.

## Compatibility

- Repositories with `hosting: external` keep the current workspace publisher, bundle transport and HMAC push hook ([`api/git-webhooks.yaml`](../../api/git-webhooks.yaml)).
- New response fields are optional; clients that ignore them keep working. Requests, claims and stored records are strict.
