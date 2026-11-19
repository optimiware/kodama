# S09 — Userland, Git, agents, and optional capabilities

Source: [Userland](../../KODEMA.md#4-userland-agents-and-software-delivery). Owners: P10, P11, P22–P27. AC: 5, 8, 10, 14, 20.

## 1. Public client and CLI

`client` depends on public DTOs, not internal domain packages. It supports caller-supplied idempotency keys, expected versions, context cancellation, bounded responses, consistent errors, and current credential sourcing. Never automatically retry an ambiguous mutation with a new key.

The client commands of `kodema` expose served operations, pipeline apply, inspect/watch, task/request commands, login, the Git credential helper, and `output push` (S07 §2). They are linked into the one `kodema` binary and keep the Userland import boundary of S01 §3: `internal/userland/cli` reaches the product only through `client` and the public schema. CLI logic accepts injected client/session/filesystem/output ports for unit tests. Human sessions and mounted execution tokens are separate sources; service automation uses its own service credential.

`kodema git credential <get|store|erase>` speaks Git's credential-helper protocol: Git passes the operation as the argument and the request attributes (`protocol`, `host`, `path`) on standard input; a URL argument replaces that request. Helper requests are restricted to registered trusted Git origins/paths; `store` and `erase` do nothing; a refusal is an empty answer, so Git falls through to its next helper; credentials are absent from diagnostics.

Dispatch is by command name. A name the control plane owns (`serve`, `up`, `down`, `status`, `git serve`, `git hook`, `git provision-tenant`, `git provision-repository`, `auth`, `agent`, `services`, `migrate`, `bootstrap-admin`) never reads the client configuration or a human session; every other name is a client command and never opens a store or a listener. `server` remains accepted for `serve`.

## 2. Independent Git service

`kodema git serve` owns Git binaries, smart HTTP, provisioning, hooks, access rules, storage, durable receive journal, reporting, retention and backups. Its code cannot import Shell/Kernel or access the factory DB/tenant keys. It authenticates as a service and uses public verification/report/query contracts.

Configuration is flags with environment defaults. Required: `--tenant` (`KODEMA_GIT_TENANT`), `--storage-root` (`KODEMA_GIT_STORAGE_ROOT`), `--journal-dir` (`KODEMA_GIT_JOURNAL_DIR`), `--base-url` (`KODEMA_BASE_URL`), `--credential-file` (`KODEMA_CREDENTIAL_FILE`). Optional: `--listen` (`KODEMA_GIT_LISTEN`, default `127.0.0.1:8081`), `--timeout` (`KODEMA_GIT_TIMEOUT`), repeatable `--resource repository=resource-id`. There is no `--policy` flag or `KODEMA_GIT_POLICY`: the access policy is the committed `conf/git.conf` of the tenant's `admin` repository. Native Git runs in an isolated environment: inherited `GIT_*` variables are dropped and global/system configuration is disabled.

Smart HTTP verifies actor and applies read rules. The managed `pre-receive` hook verifies every exact ref update of the whole push batch immediately before acceptance (no `update` hook is installed); after-the-fact webhooks are insufficient. Tenant workers and durable volumes are isolated; execution containers never mount repository storage. Repositories may bind their registered resource identity through `option resource = <resource-id>` in `admin/conf/git.conf` (with on-demand bare-repository and hook initialization on first authenticated access, without restarting `git serve`), or through startup `--resource` pins and `kodema git provision-repository`.

### Native Git minimum version

`git serve` requires Git **2.44.0** or newer. It probes the exact executable it will serve and run hooks with (`git --version`) at startup, before installing hooks, creating the journal or opening a listener, and refuses a missing, failed or unparsable probe. 2.44.0 is the first release that carries `rev-parse --show-ref-format` ([v2.44.0 `builtin/rev-parse.c`](https://github.com/git/git/blob/v2.44.0/builtin/rev-parse.c)); the option is absent from 2.43.0 ([v2.43.0 `builtin/rev-parse.c`](https://github.com/git/git/blob/v2.43.0/builtin/rev-parse.c)), and tenant and repository provisioning both use it to verify the files ref backend of the repositories they publish. Vendor suffixes such as `2.44.0.windows.1` and `2.45.1 (Apple Git-155)` are accepted; the numeric major/minor/patch components decide the comparison.

### Tenant provisioning

`kodema git provision-tenant --tenant <id> --storage-root <dir> --journal-dir <dir>` creates the tenant's trusted `admin` repository. It runs independently of `git serve`, requires the same Git minimum, and probes the executor before any filesystem effect. It builds the repository in a private sibling staging directory with the files ref backend, `main` HEAD, managed `pre-receive`/`post-receive` hooks (no `update` hook) and exactly one parentless commit whose `conf/git.conf` is `repo admin` / `RW = @admin`; the receive-journal directory is prepared empty. The complete, flushed directory is published by one same-filesystem rename.

Provisioning is idempotent and never repairs. A missing admin repository or trusted ref means "not provisioned"; an existing valid repository is validated and preserved byte-for-byte (`Created=false`), while an existing empty, damaged, symlinked or foreign-hook destination is refused without reseeding. A failure before publication leaves no canonical repository; a failure after publication reports uncertainty and keeps the repository, and a retry validates it. The committed repository, not a marker file or local store, is the durable provisioning state. The tenant directory is made private (0700) to the provisioning user; run the Git worker as that user.

Decision 31(a) records the multi-role limit of the initial `RW = @admin` policy: because evaluation is ordered first-match-wins, a human holding admin plus another role can still be locked out of the policy ref when an earlier rule denies that other role.

### Rule evaluator

Rules live in tenant `admin/conf/git.conf`. Repository names are tenant-relative. Principals are current scoped roles, `@execution`, `@all`, or explicit service IDs.

| Permission | Effect |
|---|---|
| `R` | Read |
| `RW` | Create or fast-forward ref |
| `RW+` | Also rewind/delete |
| `-` | Deny |

Evaluate in order: first ref/principal match whose permission covers the attempt allows; matching deny refuses; no match refuses. Ref-less rule covers all refs; other refs match exactly or by prefix only when ending `/`. No regex or implicit Admin bypass.

`option resource = <resource-id>` binds the repository stanza to its registered Kodema resource identity (refusing duplicate `option resource` lines in the same stanza and refusing any mismatch against a static `--resource` pin). `option approve.<ref> = <role>` additionally requires same-execution passed human-role request matching `git.ref.update`, resource, exact `ref`, `expected_base`, and `candidate`. Humans/services cannot directly update approval-guarded refs. Rule changes apply after accepting the `admin` push, affecting the next attempt.

Provision defaults follow KODEMA.md: append-only Admin `admin` history; repository read for `@all`, work-branch `RW+` for Engineer/execution, default-branch `RW` for execution with Reviewer approval.

### Receive/recovery sequence

1. Verify current actor and effect, evaluate current rules and required approval.
2. Persist intent with stable source-event ID and verification ID; never forwarded credential.
3. Atomically update only if current ref equals expected base; require fast-forward unless `RW+`.
4. Observe Git result and durably record acceptance.
5. Report verified `git.ref.updated`; retry report with identical identity/content.

Uncertain mutation is reconciled from repository/journal evidence before any retry. A failed response is not permission to push again. Serialization is service-owned; Kodema has no cross-store transaction. Retain commits referenced by retained executions, discovering them through authorized queries and using events only as hints.

Unit seams include native-command executor, verification/report client, durable journal storage, ref observer and clock. Test rules, compare-and-swap intent, journal crash boundaries, reporting-only retry, retention, and no credential persistence using fakes. Actual Git protocol/hooks/ref atomicity require I03 after G2.

## 3. Agents and durable sessions

External harnesses run as ordinary container commands. Native `kodema agent` adds planning/decomposition, tools, review/validation, Git operations, knowledge/Markdown skills, and usage/outcome/session records. No agent gets extra authority or a private execution engine.

P24 defines versioned external checkpoint references and restoration errors. Session bytes/transcripts live in Userland-controlled storage, not factory tables. P25 uses fake model/tool/client/checkpoint ports before integration; provider protocol/version and resource bounds freeze in C03.

Planned decisions use declared requests. For unplanned input: save durable context → ask with references → exit/cancelled execution → assigned reply → fresh execution → restore from task snapshot. No live agent process is preserved across waits. Missing context fails the restoring action. Old session results or approvals do not authorize the fresh attempt.

Raw model/tool content is off by default; authorized opt-in retention starts at seven days. Always record attributable usage/outcomes without leaking credentials. Deterministic model fixtures gate correctness; live model quality is separate qualification.

## 4. Independent service examples

Email/webhook/timer adapters call public APIs with stable keys. Request responders combine current-request scans, retained events, and new notifications; no missed earlier event may strand an assignment. Services answer only their explicit service-ID assignments.

A merge coordinator owns durable per-repository/ref serialization and uncertain-effect recovery. A broadcast/reservation is neither exclusive proof nor approval. Do not release an uncertain operation solely on lease expiry; reconcile it. Responses describe observed merge/rebase results and cannot satisfy human-role review. Factory database access and command-over-NATS remain forbidden.

## 5. Optional Shell gateways

Model gateway and external MCP proxy are Shell capabilities calling the same authorizer. Resolve upstream credentials internally on use; never return them to callers. Model forwarding implements the frozen OpenAI-compatible subset, allowlists, request/token bounds, cancellation, streaming limits, and usage attribution. MCP proxy enforces configured upstream servers/tools under current caller authority.

The Kodema MCP edge exposes Kodema operations; the external proxy invokes outside tools. Neither is an engine or required for plain pipelines. Unit-test protocol errors, denied models/tools, scope, credential redaction, usage, upstream failure and stream cancellation through fake transports.

## 6. Delivery recipes and artifacts

Artifacts are strings referencing external registry bytes. There is **no artifact table, registration API, catalog package, or Shell artifact owner**. Build publishes bytes first, then submits a digest-pinned declared output; lost output response retries the same value without rebuilding. OCI indexes represent multi-platform images.

Recipes test the exact candidate/image they claim, pass digest forward, write a deploy-repository work-branch candidate, request approval of exact ref/base/candidate, and integrate that protected update. Later action/GitOps rollout verifies readiness. Split build/promotion to retry promotion without rebuild; rollback is a new promotion of an older digest, not automatic data rollback.

P27 validates examples with parser fixtures/static expectations in isolation. End-to-end publication, testing, promotion and readiness proofs wait for I06/Q01. An output proves submission attribution, not independently confirmed publication or healthy deployment.
