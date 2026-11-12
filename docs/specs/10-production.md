# S10 — Environments, production, and operations

Source: [implementation and operation](../../KODAMA.md#5-implementation-operation-and-acceptance). Owners: P16–P19, P28, Q02–Q04. AC: 6, 7, 9, 15, 20.

## 1. Explicit compositions

| Environment | State/events | Runtime | Keys |
|---|---|---|---|
| Development | In-memory | Process | Ephemeral |
| Test | PostgreSQL + NATS | Podman or Kubernetes | Keyfile |
| Production | PostgreSQL + NATS | Kubernetes Jobs | Explicitly selected cloud KMS |

Production startup refuses memory/process/keyfile/ephemeral adapters and incomplete required settings. Production requires verified TLS on every declared transport: the NATS URL must be a single `tls://` broker URL (plain `nats://` and every other scheme, userinfo including the empty `tls://@host` form, comma lists and fragments including an empty `#` marker are refused; the host is required and the port must be 1-65535 when present, while a missing port keeps the nats.go default), and all four PostgreSQL DSNs (runtime, controller, publisher and auth) must be strict URL-form `postgres://` DSNs with exactly one `sslmode=verify-full` option. The PostgreSQL rule is an allowlist validator, not `net/url`: the scheme must be exactly lower-case `postgres://` (the `postgresql://` spelling is refused), the raw string may carry no `#`, whitespace or control byte, exactly one non-empty host (no comma list, empty host element or Unix-socket path) and an optional port in 1-65535, the path names one database, and the query allowlist is `sslmode`, `sslrootcert`, `connect_timeout` and `application_name` at most once each with `sslmode`'s percent-decoded value exactly `verify-full`; every other key and every percent-encoded or upper-case key is refused. A differential test runs pgx's own `pgconn.ParseConfig` over every DSN the validator accepts and requires verification-on TLS with a server name on the primary and every fallback and no Unix-socket host. `validateEndpoints` also requires a single named host for the database, NATS and public URLs in the test environment, so `postgres:///db?host=/sock` and `https:///x` are refused there. Production refuses a key reference outside the declared key adapter family (an exact case-sensitive prefix and, from C-ADAPTER r17, the AWS or GCP provider grammar; Scaleway keeps the prefix-only r14 rule until its structural grammar is revised as r16). The AWS forms are a KMS ARN (`arn:<partition>:kms:<region>:<account>:key/<key-id>` or `alias/<alias-name>`) or `alias/<alias-name>` with an explicit configured region; a bare ambiguous id, a foreign service or partition, a bad region or account, an AWS ARN/configured-region disagreement, traversal, escapes, query strings and fragments are refused before credential acquisition. The GCP form is `projects/<project>/locations/<location>/keyRings/<ring>/cryptoKeys/<key>`; an incomplete key ring and a `cryptoKeyVersions` reference are refused. A reference is never normalized into a different persisted identity, and an alias reassignment is not KEK rotation. The test environment accepts the Podman and Kubernetes runtimes; there is no fourth environment. Optional Git, gateways, agents, and services are explicitly enabled. Plain task-free pipelines must work without any optional companion. Shipped entrypoints are `kodama` (`server`, `git serve`, `git hook`, `git provision-tenant`, `git provision-repository`, `agent`, `services`, `migrate`, `bootstrap-admin`) and `kodactl`.

P28 implements configuration parsing/preflight as a unit-testable package before G2. Concrete composition and command dispatch arrive in I01. No hidden dependency initialization or environment auto-detection may silently select weaker adapters.

Database URL credentials must be URL-safe: letters, digits, `.`, `_`, `~` and `-`. Percent-encoded credentials are refused (for example `p%40ss`, `%23x`, `%25` and `p%40ss%2Fw%3Ard`); generated passwords are hex. The strict validator stays percent-free on purpose even though pgx would percent-decode userinfo, and `scripts/bootstrap-secrets` will refuse an operator-supplied password outside `[A-Za-z0-9._~-]` instead of percent-encoding it. A NATS URL refuses a comma, `#` or `@` anywhere, including after a `/` path separator, after a `?` query separator and inside the query, because nats.go splits the comma list and applies per-element userinfo. `sslrootcert=system` is accepted because it still verifies against the system roots, and `connect_timeout=0` is accepted.

## 2. Deployment contract

Initial Helm deployment uses one control-plane replica, tenant namespaces, workload resource limits, and tenant-isolating network policies. Jobs do not automatically replay commands. Workloads can reach Kodama, configured Git and policy-allowed services; Git verifies/reports over authenticated HTTPS. Registered endpoint identity must match transport trust configuration.

AWS, GCP, and Scaleway are supported installation targets; each advertised target requires its own KMS/runtime/deployment evidence. One successful provider test cannot qualify all three. The threat model is trusted project engineers in regular containers; no hostile-workload isolation claim is inferred.

## 3. Health and observability

Expose separate process liveness and dependency/readiness state. Include scoped logs/audit, authorization and verification results, queued/running progress, active requests/waiting tasks, pending cleanup with resource identities, event publication backlog, consumer lag, refused pushes/reports, and dependency errors.

NATS outage reports degraded publication while preserving persisted execution progress. Runtime uncertainty appears explicitly, not as idle/healthy. Usage attribution follows tenant/project/principal/execution where applicable. Do not put token values or forwarded actor credentials in metrics labels or logs.

## 4. Backup and restore

Back up PostgreSQL, retained logs, encryption keys, and JetStream event/consumer state. Coordinate Git repositories/journals/rules and external registry/checkpoint retention with their owners. Workspaces are disposable and excluded.

Restore order:

1. Migrate the restore target first: `kodama migrate` creates the application roles the dump's ownership and grants reference; a missing role is a typed refusal before any restore effect.
2. Preflight the backup; missing encryption keys refuse before fencing.
3. Fence old controllers, enforcing services and consumers before new actors can effect changes.
4. Restore database/key/log/event state and coordinate service-owned journals/storage.
5. Preserve bindings, verification records, pipeline IDs, conversations, decisions, receipts and queued executions.
6. Reconcile existing workloads and external effects from stable identities before enabling dispatch/report retries.
7. Fail executions with unrecoverable workspaces visibly; permit authorized fresh attempts.
8. Resume publication/consumers with deduplication; verify no duplicate effects or approval reuse.

The coordinated run is preflight → fence → restore → reconcile → enable dispatch.

P28 unit-tests restore planning/preflight and failure behavior with fake command/storage ports. Real destructive restore drills happen only in Q03 after integration. Missing fencing or unresolved effect evidence must block dispatch, not trigger replay.

## 5. Retention and measured targets

- Logs: 30 days. Ordinary evidence: 90 days. Audit metadata: one year.
- Active references extend retention; referenced commits/artifacts require owner coordination.
- Operation receipts remain until project deletion; do not apply the ordinary-evidence TTL to them.
- Raw model/tool content: off by default; authorized opt-in starts at seven days.
- Recommended recovery objectives: RPO ≤15 minutes, RTO ≤4 hours, 30-day PostgreSQL recovery. Kodama does not enforce them; the production deployment guide explains how to reach them.

A timestamped restore drill of the reference deployment measures and reports them. A failed restore is a failure; a missed recommended objective is a documented limitation of that deployment, reported in the release report and the guide. Record environment, revisions, backup ages, failure/recovery timeline, lost data, and unresolved resource state. A green unit suite cannot satisfy operational acceptance.
