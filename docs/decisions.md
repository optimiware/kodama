# Decision register

**Baseline:** initial design plus the owner-approved KISS review. Detailed code choices remain proposed until their task validates them.

## Confirmed direction

| ID | Decision |
| --- | --- |
| C-01 | Kubernetes, Go, NATS, PostgreSQL |
| C-02 | Native Jobs for executable steps; [ADR 0001](adr/0001-native-kubernetes-jobs.md) |
| C-03 | Shared control plane and tenant execution namespaces; [ADR 0003](adr/0003-shared-control-plane.md) |
| C-04 | Actionable work item paired with durable logical workspace |
| C-05 | Separate AI exploratory QA and human review |
| C-06 | CLI, HTTP API, MCP and webhooks are the primary interfaces; human editors use Git. An optional UI belongs in Platform, not a separate Kodama dashboard |
| C-07 | Application chassis is Markdown guidance for tenant/project scopes, versioned in Git: architecture/microservices, approved stacks/libraries and cloud deployment methods. It is not a shared application runtime library |
| C-08 | On-premises and hosted offerings share product contracts |
| C-09 | PostgreSQL variables/secrets; platform envelope (scope-key wrap, HKDF-SHA-256, AES-256-GCM). Production KEK is the installation's cloud KMS |
| C-10 | Built-in work management, agent runtime, AI gateway, MCP proxy |
| C-11 | Retain outbox → JetStream → inbox deduplication; NATS is a durable event bus |
| C-12 | Git commits are checkpoints; runtime scratch is disposable; [ADR 0005](adr/0005-git-backed-workspaces.md) |
| C-13 | Collapse derived states/resources; human completion; no M2 agent continuation |
| C-14 | Build and deploy are ordinary container steps with recorded outputs |
| C-15 | Start with OpenCode headless, OpenAI-compatible HTTP, HTTPS Git, S3-compatible artifacts; local keyfile KEK for development |
| C-16 | Prefer lightweight hexagonal ports/adapters and module-owned DDD entities; [software design](specs/software-design.md), [ADR 0006](adr/0006-hexagonal-domain-design.md) |
| C-17 | Optimi Platform is the company core; Kodama is its agent layer and autonomous SDLC for platform tenants. Platform owns billing, commercial usage/access policy and entitlements; Kodama retains focused SDLC logic, operational usage evidence and execution bounds. Hosted operation preserves core authority through explicit adapters; [platform integration](specs/platform-integration.md), [ADR 0007](adr/0007-platform-core-and-agent-layer.md) |
| C-18 | Deploy on AWS, GCP, or Scaleway. A supported installation requires Kubernetes, PostgreSQL, and that cloud's KMS. Same chart and envelope; the installation selects one provider and does not fall back. Platform's `fr-par` residency does not choose Kodama's cloud |
| C-19 | First staging target is Platform's stage Kubernetes cluster, with a separate Kodama control-plane namespace and scoped execution namespaces. Kodama stays an independently deployable appliance; Platform integration uses its public API and may address installations in multiple clusters. Co-location does not resolve hosted authority/custody |
| C-21 | One backend-neutral runtime port for execution: Process for fast tests, Podman for local end-to-end runs, Kubernetes for staging and production; backends declare guarantees and runs pin them; [ADR 0008](adr/0008-runtime-seam.md) |
| C-22 | Adapters are selected by one required `KODAMA_ENV`: `development` (in memory, ephemeral, Process runtime), `test` (dependency stack and Podman, no cluster) and `production` (Kubernetes; kind for conformance, then the stage cluster). `production` refuses development and test adapters; SQLite is rejected; [ADR 0009](adr/0009-environments.md) |
| C-20 | Public product name is Kodama; repository and target public Go module are `github.com/optimiware/kodama`; license is Apache-2.0 under O-02. The current local module path is not yet migrated |

## Implementation baseline

| ID | Default and rationale |
| --- | --- |
| P-01 | PostgreSQL owns state; outbox and periodic reconciliation complement NATS; ADR 0002 accepted |
| P-02 | One binary (`server`, `step`, CLI); one server process hosts modules; default one replica |
| P-03 | Helm and ordinary Jobs first. Operator assessment/implementation are explicitly deferred under O-16 |
| P-04 | OAuth/OIDC/PKCE, five fixed composable SDLC roles, tenant/project bindings. Hosted identity comes from the Optimi OAuth server's verified actor, UUID `tenant_id` and role claims under O-18; local authority remains an explicit standalone profile |
| P-05 | Tenant predicates, composite ownership keys, transaction-local RLS context |
| P-06 | Private attempt clones and publisher-owned Git CAS; Pod deletion is not a filesystem fence |
| P-07 | Git retains source; S3 retains artifacts/logs; no persistent source PVC is required |
| P-08 | Immutable API-managed pipeline/profile versions |
| P-09 | No pipeline CRDs required |
| P-10 | Kubernetes live logs plus retained log artifacts; Loki is optional |
| P-11 | Ordinary rootless build images must fit the fixed execution restrictions |
| P-12 | Performance claims require a measured named workload; remove speculative sub-200-ms promise |
| P-13 | Project plaintext variables, KMS-encrypted versioned PostgreSQL secrets through the existing secrets module, and explicit deployment configuration. Prefer proxy-mediated agent credentials; ordinary script steps may explicitly bind declared secret versions to child-process ENV. Hosted custody/read-for-use qualification is T-061 under O-19 |
| P-14 | Trusted-engineer staging first on regular Kubernetes containers with the existing restricted execution baseline. Stronger isolation/security backends are deferred; their later profile requires its own qualification rather than blocking the current trusted-engineer scope |
| P-15 | Narrow scopes keep concurrency control on editable content as well as transitions/decisions |
| P-16 | Foundation library choices follow platform, without importing `github.com/optimiweb/platform`: stdlib `net/http` and `flag`, `log/slog`, `coreos/go-oidc` plus `golang.org/x/oauth2`, `zalando/go-keyring`, `oapi-codegen`, `go.yaml.in/yaml/v3`, `modelcontextprotocol/go-sdk`, `pgx`, and `nats.go`. Kodama keeps Go 1.27 (currently 1.27.2); exact dependency versions are qualified for this module rather than forced to Platform's older toolchain. No chi, Cobra, GORM, or hand-rolled JWT verifier |

## Open decisions and resolved selections

| ID | Status / decision | Needed by |
| --- | --- | --- |
| O-01 | Resolved: Kodama; public repository and target module `github.com/optimiware/kodama` (C-20). Public module/import migration remains a coordinated implementation prerequisite; installation hostnames are deployment configuration | T-027/T-056 |
| O-02 | Resolved: Apache License, Version 2.0; see [LICENSE](../LICENSE). Commercial use is permitted under that license without a separate commercial-use fee. Contributions follow Section 5 unless explicitly stated otherwise; third-party licenses/notices remain applicable. This supersedes the earlier TBD/free-use/commercial-license intent; paid Platform hosting/support remains a separate offering | Public release / T-056 |
| O-03 | Resolved direction: native Git push notifications first, using an authenticated server-side receive hook on a controlled Git host. Keep ordinary HTTPS/token Git; do not implement a new Git transport/server. GitHub and other provider webhook adapters follow later | T-036 |
| O-04 | OpenCode + configurable OpenAI-compatible endpoint selected; pin/test exact versions/protocol | T-031/T-032 |
| O-05 | Resolved: production KMS is AWS KMS, GCP Cloud KMS, or Scaleway Key Manager, selected per installation (C-18). All three wrap scope keys. The local keyfile is development only. No cross-provider fallback | T-028 |
| O-06 | S3-compatible artifacts, LocalStack development (owner-selected replacement for retired MinIO), Git checkpoints selected | T-001/T-013/T-015 |
| O-07 | Resolved current scope: start with trusted-engineer staging on regular Kubernetes containers. Defer support/evaluation for gVisor, Substrate and other isolation/security options; none is selected, implemented or qualified by this decision. Preserve the existing restricted-container/tenant-access controls. T-055 is a later extension, not a prerequisite for the current trusted-engineer release | Current trusted profile; deferred T-055 |
| O-08 | Resolved release scope: full air-gap support is excluded from the first release; a future disconnected profile needs a separate dependency/key-management contract | Later profile only |
| O-09 | Generic outbound webhook is initial ChatOps transport; provider-specific interactive actions remain later | T-041 / later adapters |
| O-10 | Resolved direction: retain Go 1.27 (current pin 1.27.2); Platform will update independently. Qualify compatible dependency pins without a forced downgrade. Reference workload: 20 tenants, 100 projects, 20 concurrent attempts, declared per-tenant limits and concurrent API/SSE/model/webhook activity. Measurements and supported capacity remain release acceptance | T-056 |
| O-11 | Resolved policy: owner accepted the [retention/recovery baseline](operations/upgrades-backup-and-recovery.md#approved-retention-and-recovery-baseline). RPO 15 minutes, RTO 4 hours, 30-day PITR and the specified Git/object/image/key/deletion windows; these are objectives to qualify, not proven service guarantees | T-029/T-056 |
| O-12 | Kubernetes namespace + scoped kubeconfig + ordinary scripts selected | T-035 |
| O-13 | Resolved ownership/model: Platform subscription add-on with bounded compute/AI usage. Platform owns billing, commercial metering/access, entitlements and subscription policy. Kodama emits operational usage/correlation evidence and enforces execution limits, without its own pricing/payments/entitlement subsystem. Exact prices/support commitments remain Platform business configuration | Platform commercial launch |
| O-14 | Resolved: OS keyring via `github.com/zalando/go-keyring`. A `0600` file store is an explicit opt-in; no silent fallback and no nonpersistent plaintext path | T-017 |
| O-15 | Resolved product direction: optional UI exists in Platform, likely `packages/dashboard`; Kodama is primarily CLI/API/MCP/webhook driven. No local `web/` implementation is scheduled. Exact upstream UI work/ownership remains an explicit deferred handoff for T-050/T-051/T-060 | Deferred Platform UI work |
| O-16 | Deferred by owner; an operator is not expected to be needed. Keep Helm/ordinary Jobs, with both assessment and implementation unscheduled unless the owner revisits the decision | Deferred T-057/T-058 |
| O-17 | Resolved: chassis is Markdown-only guidance, Git-versioned per tenant/project scope, covering microservices, approved stacks/libraries and deployment methods. T-059 supplies guidance-repository conventions and pinned-context examples, not extraction of runtime chassis libraries | T-059 |
| O-18 | Resolved authority direction: Optimi OAuth server authenticates actors and can carry roles plus UUID `tenant_id`; tenant-owned PostgreSQL resources/RLS use that verified tenant identity. T-061 freezes issuer/audience/claim validation, role mapping, current revocation/entitlement behavior and any required upstream contract before hosted acceptance. Claims alone do not bypass current authorization | T-061 |
| O-19 | Resolved custody/delivery direction: PostgreSQL encrypted with KMS through the existing secrets module, immutable version pins and scoped audited read-for-use. Prefer model/tool proxies that retain provider credentials server-side; explicit secret-to-ENV binding is allowed for ordinary build/script steps. T-061 qualifies hosted custody/version/rotation/revocation and secure runtime delivery; the reviewed current-value-only Platform store still needs the required interface | T-061 |
| O-20 | **Resolved by the orchestrator 2026-10-22 under standing authority to maximize throughput; owner may override.** T-067 (deployment qualification) failed review because two production pieces it must qualify did not exist: the cleanup run received no deployment input from the scheduler (T-067 had to stand in a per-deployment `CleanupDefinition`), and `internal/delivery` had no production `EffectReconciler` (T-067 had a test-local one). Chosen: option A, two bounded prerequisite tasks that depend only on finished work and run in parallel with T-027 — T-092 (cleanup-run deployment input handoff) and T-093 (delivery effect reconciliation adapter). T-067 and T-072 now depend on both; T-072 binds them in the process assembly. T-067 stays parked until both merge, then replaces its stand-ins with the real implementations. Alternatives not taken: widening T-067 to build product code; merging T-067 under human review with the gaps documented | T-067, T-072, T-038, T-069 |
| O-21 | Resolved 2026-10-22 (owner): ownership of `deploy/helm/kodama/` transfers from T-018 (done) to T-027 for a bounded chart join: select the real assembly and environment explicitly, consume the rendered settings, supply the secret keys the assembly needs, and add a no-cluster render test. Production key providers stay with T-029 and installed-chart qualification with T-089/T-056; a profile that cannot yet start the core assembly must fail closed and never weaken the `production` guards | T-027 |
| O-22 | **Resolved 2026-10-22 (owner interview): keep both deferred.** T-047 (Supabase/Hono/Vite blueprint) and T-059 (Markdown chassis guidance) remain unscheduled. Concentrate on core acceptance and Kubernetes work. Browser QA T-069 also retains its separate scheduling requirement; spare capacity does not authorize scope expansion | Deferred T-047, T-059, T-069 |
| O-23 | **Resolved 2026-10-22 (owner interview): schedule remaining cleanup.** T-103 fixes missing artifact/log spool creation, preservation of specific output-failure reasons, and the stale `assembly-core` integration suite, with focused regression checks. Artifact output naming was already repaired by T-099; workspace-free claims by T-094. Cleanup is required before full Kubernetes/staging acceptance, without blocking the component checkpoint selected in O-24 | T-103, T-104, T-090 |
| O-24 | **Resolved 2026-10-22 (owner interview): component checkpoint now.** T-089 qualifies the real Kubernetes runtime, restricted-container controls, chart installation and fail-closed rejection of invalid production configuration. Production KMS adapters exist (T-028), but startup wiring and HTTPS dependencies remain prerequisites for full server readiness. [T-104](tasks/T-104-kubernetes-full-readiness.md) retains the fully running installed server and assembled Kubernetes M1 roundtrips after T-029; T-090 depends on it. This follow-up remains deferred until explicitly scheduled. Component evidence does not satisfy full readiness, and correct startup refusal is not a working installation. Preserve `production` guards. O-25 separately governs the shared runtime contract | T-089, T-104, T-090 |
| O-25 | **Resolved 2026-10-22 (owner interview): declare uncertainty as a backend capability.** Gate the shared contract's never-created `LaunchNotStarted` expectation on an explicit runtime guarantee. Kubernetes does not advertise that guarantee and returns `LaunchUnknown` when missing records cannot prove whether creation happened; stronger backends retain their proven guarantee. Preserve reconciliation before retry/resource release and the lost-record/uncertain-create regression tests. Do not add durable launch-tracking objects or weaken the lost-Secret expectation. T-102 updates the runtime contract/capability declarations under ADR 0008 before T-089 runs | T-102, T-089 |

T-001 uses the owner-selected [oauthsonas](https://github.com/optimiweb/oauthsonas) development OIDC fixture. Its pinned version and configuration are recorded in the [scaffold contract](../dev/configuration.md). O-18 selects the Optimi OAuth authority; its actual hosted issuer, audience and claim/revocation contract are qualified by T-061, not inferred from the fixture.

## Owner update — 2026-10-20

The owner approved the selections above; the response's “D-01” refers to the existing O-01. Preserve the original decision IDs. Follow-up approvals select Apache-2.0 for O-02 and trusted-engineer regular-container staging for O-07, with stronger isolation deferred. O-16 remains explicitly deferred. Selecting O-18/O-19 establishes direction, not evidence that upstream interfaces or hosted acceptance are already delivered. Existing implementation statuses are not retroactively completed by these decisions.

## Secrets startup update — 2026-10-23 (O-26)

The owner approved `secrets.adapter: keyfile | kms` with cloud providers `aws`, `gcp`, `scw`, startup client construction and cleanup, and live-provider validation during Kubernetes deployment. T-105 extracts startup wiring from T-029. T-104 depends on T-105 and retains full installation/assembled acceptance plus the live KMS procedure; T-029 retains full recovery. This updates O-21/O-24's startup ownership and prerequisite ordering. T-104/T-090 remain explicitly scheduled deployment work; component completion is not deployment evidence. The persisted Scaleway identity remains compatible.

## Asynchronous decisions

The 2026-10-22 owner interview resolved every explicitly open owner request (O-22–O-25). T-102 and T-103 are scheduled implementation work; T-089 waits on T-102 rather than an unanswered decision. T-104 preserves full Kubernetes readiness before staging. Other deferred tasks still require explicit scheduling; their provider, upstream-contract and live-qualification details remain with their existing owners rather than becoming new local-core blockers.

Autonomous runs never wait on a person. When a task needs an owner decision the harness parks it, the orchestrator records an **Open** row above with the options and a recommendation, and unrelated work continues. Answer by replying to the orchestrator or by editing the row to `Resolved`; the orchestrator then applies the outcome (task `owns`/dependencies, new prerequisite tasks) and unparks the task.

## Change discipline

Preserve IDs and historical ADR rationale. Mark superseded requirements explicitly; do not silently repurpose their numbers. Tasks blocked by owner/provider decisions remain blocked until evidence or a recorded decision resolves them. See the [review](review-kiss.md) for corrections to the initial proposal and the [task graph](tasks/README.md) for execution order.
