# HTTP interface contract

**Status:** simplified proposed interface. Commands and endpoints become executable only as their tasks are completed.

## Conventions

Base URL is `https://factory.example.invalid/api/v1`. Let `P` mean `/tenants/{tenantId}/projects/{projectId}`. Use bearer access tokens, JSON, opaque IDs, UTC timestamps, and ETags equal to quoted integer resource versions. Lists return `{items, nextCursor}` with default limit 50 and maximum 200.

`If-Match` is required for editable content/settings, transitions, and gate decisions. Use idempotency keys for run/deployment creation and gate decisions. Unique key: `(principal, tenant, method, canonical route, key)`; store/compare the request hash separately. Replay authorization is checked before returning the prior response. Changed body gives `409 idempotency_conflict`; stale version gives `412 version_conflict`; missing required version gives `428 version_required`.

## Routes

| Route | Purpose |
| --- | --- |
| `GET /me`, `GET /tenants` | Verified identity and authorized tenant discovery |
| `GET, POST /tenants/{tenantId}/memberships` | Membership management |
| `PATCH, DELETE /tenants/{tenantId}/memberships/{id}` | Change/revoke roles |
| `GET, POST /tenants/{tenantId}/projects` | Project discovery/creation |
| `GET, PATCH P` | Project settings, active WIP limit, fixed role bindings |
| `GET, POST P/work-items` | Work list/create |
| `GET, PATCH P/work-items/{id}` | Work summary/spec/acceptance/blocking edits |
| `GET P/work-items/{id}/history` | Immutable content and transition history |
| `POST P/work-items/{id}/transitions` | Explicit business transition |
| `POST P/work-items/{id}/workspace` | Idempotent primary-workspace provision |
| `GET P/workspaces/{id}` | Branch, accepted commit, writer, state/error |
| `POST P/workspaces/{id}/archive`, `POST P/workspaces/{id}/restore` | Change admission after settling publication |
| `POST P/workspaces/{id}/sync` | Observe authorized human Git updates |
| `GET, POST P/pipeline-definitions` | List/register validated immutable versions |
| `GET P/pipeline-definitions/{id}` | Definition/digest |
| `GET, POST P/runs` | List/start work runs |
| `GET P/runs/{id}`, `GET P/runs/{id}/steps` | State, input snapshot, attempts/results |
| `POST P/runs/{id}/cancel` | Idempotently set cancellation request |
| `GET P/events`, `GET P/runs/{id}/events` | Authorized durable SSE observations |
| `GET P/runs/{id}/logs` | Bounded live/retained log retrieval |
| `GET, POST P/variables`, `GET, PATCH, DELETE P/variables/{id}` | Project nonsecret configuration |
| `GET, POST P/secrets`, `GET, DELETE P/secrets/{id}` | Secret metadata/create/deletion |
| `GET, POST P/secrets/{id}/versions` | Metadata list/new encrypted value version |
| `POST P/secrets/{id}/versions/{version}/revoke` | Set revocation timestamp |
| `GET P/artifacts/{id}` | Scoped metadata/download |
| `GET, POST P/agent-profiles` | M2 immutable profile versions |
| `GET, PUT P/model-settings` | M2 endpoint/model/credential reference and limits |
| `GET, POST P/mcp-servers`, `PATCH, DELETE P/mcp-servers/{id}` | M2 endpoint/tool registrations |
| `GET P/gates`, `GET P/gates/{id}`, `POST P/gates/{id}/decisions` | M2 review and immutable decision |
| `GET, POST P/environments`, `GET, PATCH P/environments/{id}` | M2 explicit target/configuration |
| `GET, POST P/deployments`, `GET P/deployments/{id}` | M2 durable deployment record before apply |
| `POST P/deployments/{id}/cleanup` | M2 scoped cleanup request |
| `GET, POST P/work-items/{id}/dependencies`, `DELETE P/work-items/{id}/dependencies/{dependencyId}` | M3 dependency links |

M3 metrics are SQL views exposed as summary fields/query options on work lists. Defects use work-item routes and evidence links. There are no operation, candidate, finding, session, handoff, revision, or generic connection resources.

Membership/bootstrap routes implement standalone authority or explicitly supported hosted SDLC bindings only. They cannot grant platform membership or entitlements. [Platform integration](../specs/platform-integration.md) governs hosted mapping and credentials; a missing upstream operation is not permission to implement it under a local route. Conformance reports use existing artifact/evidence references, not a new resource.

## Work and execution examples

Create work:

```json
{
  "kind": "story",
  "title": "Resolve inventory exceptions",
  "spec": "# Plan\nAdd a scoped review flow.\n\n# Behavior\nReviewers record ERP acknowledgments.",
  "acceptanceCriteria": [{"id": "AC-1", "text": "A reviewer resolves an exception with an audit record."}]
}
```

Creation returns `201`, state `backlog`, `version:1`, `contentRevision:1`. PATCH replaces supplied content fields under `If-Match`, appends history, and increments content revision when spec/criteria change. Transitions increment resource version but not content revision.

Start a run with `Idempotency-Key`:

```json
{
  "workItemId": "wi_1042",
  "definitionId": "pd_01",
  "contentRevision": 2,
  "sourceCommit": "1111111111111111111111111111111111111111",
  "inputs": {}
}
```

The source commit must belong to the project's configured repository and retained workspace history. The server resolves and pins spec, configuration, credentials, and profile. The candidate for a read-only delivery run is this commit; a writer can produce a new commit output before gates. No redundant candidate/checkpoint IDs are submitted.

Cancel returns `202` with the run's current status and `cancelRequestedAt`. Poll/watch that run until terminal. Workspace provisioning similarly returns the workspace, not an operation handle.

## Gate decision

```json
{
  "decision": "approve",
  "subjectDigest": "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
  "reason": "Reviewed the identified source and test evidence."
}
```

Use `If-Match` and `Idempotency-Key`. The server derives the human actor. The [delivery spec](../specs/delivery-and-quality-gates.md) owns subject construction and applicability. A stale or expired subject returns `409 stale_gate_subject` without changing the prior decision.

## Runtime and observation

Internal runtime endpoints are defined in the [step entrypoint contract](../implementation/step-entrypoint.md), with an attempt-token audience distinct from human access. Runtime deployment creation uses the same underlying module with restricted authorization.

SSE uses a durable project event sequence/cursor retained with outbox/event history. `Last-Event-ID` resumes within retention. An expired cursor returns `410 cursor_expired`; snapshot reads return a new observation cursor so clients can resynchronize. NATS delivery is not itself a client cursor.

Live logs proxy the Kubernetes API; finished logs are artifacts. Log loss is visible separately from command outcome. Responses never expose arbitrary Kubernetes selectors or raw storage credentials.

## Errors and implementation

Errors use `{error:{code,message,requestId,details}}`. In addition to the codes above, use 400 invalid request/cursor, 401 invalid credential, 403 authorized-scope denial, 404 hidden/not found, 409 writer/WIP/conflicting result, 422 graph/transition/type failure, 429 throttling with `Retry-After`, and 503 unavailable dependency.

Each module owns its OpenAPI fragment and examples. Integration tasks assemble/validate a published document and generate types with `oapi-codegen`; runtime schemas are frozen before server/entrypoint tasks run in parallel. Detailed request schemas are part of the owning task's deliverables, not guessed independently by clients.

T-027 also generates a portable public Go HTTP client under `api/client/`, with pinned generator/configuration and regeneration-drift checks; later composition tasks regenerate it as the API evolves. The published OpenAPI lets Platform generate a client in its own repository. Server selection, HTTP transport and authentication are injected; no internal server/domain packages or runtime-audience routes leak into the public client. Run-submission retries preserve the idempotency key and request, then observe the same run resource. Client generation does not select hosted delegation or credential custody.
