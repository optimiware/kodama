# API, CLI, and integrations

**Scope:** CLI/HTTP API/MCP/webhook-first product access; M2 native Git push notifications; M3 outbound ChatOps review links. Optional UI is deferred to Platform under O-15, not a local dashboard.

## Interface rules

HTTPS JSON under `/api/v1`, tenant/project scope, bounded cursor lists, JSON CLI output, and resource-based status reads are the baseline. An asynchronous action returns the target resource and its desired/observed state; there is no generic operation resource. SSE observes durable events with cursor expiry/resynchronization.

Use `If-Match` for content edits as well as transitions/decisions: removing it from editable content would reintroduce lost updates. Idempotency keys are required for run/deployment creation and gate decisions. Workspace provisioning is naturally idempotent by unique work-item ownership; pipeline versions deduplicate by content digest. Other CRUD retries reread their resource.

## Requirements

| ID | Requirement |
| --- | --- |
| AC-01 | Product behavior MUST have a scoped public interface; runtime-only authority has a separate audience. |
| AC-02 | Run/deployment creation and gate decisions MUST support principal/scope/route/key idempotency with a stored request hash. Different content for the same key MUST conflict. |
| AC-03 | Content updates, transitions, and decisions MUST check expected versions; errors have stable codes and request IDs. |
| AC-04 | Cursor lists MUST bind scope/filter/sort and apply authorization. |
| AC-05 | CLI MUST support explicit contexts, deterministic JSON, noninteractive invocation, and documented exit codes. |
| AC-06 | Observation streams MUST support retained cursors and an explicit snapshot-resync path; log queries are resource-authorized. |
| AC-07 | Native Git push hooks and later provider webhooks MUST verify authenticity and delivery IDs before triggering authorized exact-commit runs. |
| AC-08 | Notifications MUST use durable event delivery and expose retry/failure without rolling back business state. |
| AC-09 | M3 review links MUST reload current authorization/subject; a reaction or link possession MUST NOT approve. |
| AC-10 | Interface/schema evolution MUST preserve declared compatible clients. |
| AC-11 | Rate limits MUST be scoped and return retry guidance. |
| AC-12 | Implemented routes/examples MUST have machine-readable contracts and meaningful acceptance tests. |

The idempotency lookup key excludes the request hash; the hash is a value compared on conflict. Including the hash in the unique key would accidentally accept changed requests. Replays recheck current permission before returning prior content. Minimum proposed retention is 24 hours.

## CLI and integrations

Command groups are `auth`, `context`, `work`, `workspace`, `pipeline`, `run`, `gate`, `environment`, `deployment`, `variable`, and `secret`. `workspace` provisions, inspects, archives/restores, and prints Git clone information; it does not launch an IDE/session. `pipeline register` validates inherently; a local `pipeline validate` command needs no validation HTTP endpoint.

Commands use the standard-library `flag` package, as platform CLIs do. Do not add Cobra. Exit codes: 0 success, 1 remote/operation failure, 2 usage/validation, 3 authentication/authorization, 4 conflict, 5 timeout/interruption. `run watch --exit-on-terminal` returns 1 for failed/cancelled runs; reading such a record succeeds normally. Secret input uses stdin/protected files. PKCE credentials use `github.com/zalando/go-keyring`. `--credential-store file` is an explicit opt-in to a `0600` file; there is no silent fallback and no nonpersistent plaintext store.

M1 Git uses HTTPS and typed token secrets. Under O-03, M2 first consumes authenticated server-side receive-hook notifications from a controlled Git host and maps the exact accepted commit to a registered definition. Git transport alone does not notify Kodama; configure the hook explicitly. Use a bounded payload, credential and delivery identity, then revalidate repository/project/trigger authority and commit availability. A later GitHub/provider adapter normalizes its verified event into the same seam; no Git wire-protocol server or provider-specific first implementation is required. Untrusted fork execution is outside the M1 trust profile. M3 ChatOps sends durable notifications plus authenticated review links; provider-interactive approvals are later work.

## Acceptance

| ID | Scenario | Requirements |
| --- | --- | --- |
| AC-A01 | Core journey uses public routes and CLI JSON | AC-01, AC-05 |
| AC-A02 | Retry returns original create/decision; changed body conflicts; stale edits fail | AC-02, AC-03 |
| AC-A03 | Copied cursors cannot change scope/query | AC-04 |
| AC-A04 | Reconnect resumes or requests snapshot resynchronization | AC-06 |
| AC-A05 | Forged/duplicate native push hooks or provider webhooks create no unauthorized/duplicate run; later branch movement cannot replace the selected commit | AC-07 |
| AC-A06 | ChatOps outage preserves review records and retry history | AC-08 |
| AC-A07 | Forwarded/stale links cannot approve on behalf of another actor | AC-09 |
| AC-A08 | Additive response fields preserve supported clients; throttling reports retry | AC-10, AC-11 |
| AC-A09 | Published route/CLI examples match generated schemas and tests | AC-12 |
