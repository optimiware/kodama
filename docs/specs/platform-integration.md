# Platform integration and ownership

**Direction:** owner-confirmed: Optimi Platform is the company core; Kodama is its agent layer and autonomous SDLC for platform tenants. O-18/O-19 select Optimi OAuth tenant/role authority and KMS-encrypted versioned PostgreSQL secrets with proxy-first delivery. **Contract status:** implementation/hosted qualification remains T-061; selecting these directions does not establish a deployed connection between repositories.

## Purpose and authority

Kodama turns tenant intent into bounded agent work, source changes, evidence, and reviewed delivery. Platform owns the company-wide tenant and service authority on which that work depends. Autonomy concerns execution inside granted limits; it does not replace the explicit human decisions in [delivery](delivery-and-quality-gates.md).

The reviewed upstream baseline is `optimiweb/platform` at commit `05dec0d81a15f9c3bcac826508f0342761343d38`, reviewed on 2026-10-19. These links pin the review evidence; integration qualification must record the actual supported platform revision and recheck changes since this baseline.

| Upstream authority | What Kodama must preserve |
| --- | --- |
| [Index and authority](https://github.com/optimiweb/platform/blob/05dec0d81a15f9c3bcac826508f0342761343d38/docs/specs/00-index.md), [RFC process](https://github.com/optimiweb/platform/blob/05dec0d81a15f9c3bcac826508f0342761343d38/docs/specs/01-process.md) | One owner per fact; executable contracts own routes, schemas, names, and implemented behavior |
| [Identity](https://github.com/optimiweb/platform/blob/05dec0d81a15f9c3bcac826508f0342761343d38/docs/specs/04-identity.md), [authorization](https://github.com/optimiweb/platform/blob/05dec0d81a15f9c3bcac826508f0342761343d38/docs/specs/05-authorization.md) | Verified actor, authoritative tenant membership, current effective access, entitlements, and auditable tenant selection |
| [Secrets](https://github.com/optimiweb/platform/blob/05dec0d81a15f9c3bcac826508f0342761343d38/docs/specs/16-secrets.md), [settings](https://github.com/optimiweb/platform/blob/05dec0d81a15f9c3bcac826508f0342761343d38/docs/specs/22-settings.md) | Core credential custody and runtime-setting ownership; read-for-use authority is distinct from administration |
| [MCP](https://github.com/optimiweb/platform/blob/05dec0d81a15f9c3bcac826508f0342761343d38/docs/specs/19-mcp.md), [API](https://github.com/optimiweb/platform/blob/05dec0d81a15f9c3bcac826508f0342761343d38/docs/specs/08-api-dashboard.md) | Audience-bound access to bounded tenant operations; protocol adapters preserve operation authorization and audit |
| [Platform](https://github.com/optimiweb/platform/blob/05dec0d81a15f9c3bcac826508f0342761343d38/docs/specs/02-platform.md), [privacy](https://github.com/optimiweb/platform/blob/05dec0d81a15f9c3bcac826508f0342761343d38/docs/specs/06-privacy.md), [release](https://github.com/optimiweb/platform/blob/05dec0d81a15f9c3bcac826508f0342761343d38/docs/specs/11-release.md) | Core data placement, residency, retention, provider policy, and production change authority |
| [Foundation roadmap](https://github.com/optimiweb/platform/blob/05dec0d81a15f9c3bcac826508f0342761343d38/docs/specs/12-roadmap.md) | F1 delegation, F2 jobs, F3 communications, and F5 agents are integration planning inputs, not delivered service contracts |

## Ownership boundary

| Concern | Platform owns | Kodama owns |
| --- | --- | --- |
| Tenant and human identity | Optimi OAuth identity and signed UUID tenant/role claims, tenant lifecycle, current membership, scopes and entitlements | Validated tenant UUID for scoped resources/RLS; project bindings and SDLC-specific permissions within current core authority |
| Credentials and settings | Core/provider/integration credentials and core runtime settings | Declared credential use, project variables, pinned execution configuration, and SDLC credential-version requirements |
| Work and execution | Existing platform jobs and domain-operation outcomes | Work/spec history, workspaces, pipeline/step/attempt lifecycles, harnesses, budgets, cancellation, and recovery |
| Tenant operations | Analytics, provider resources/actions, support, subscription billing, commercial usage/access, entitlements and their authorization/audit | Authorized invocation, run correlation, runtime usage/outcome evidence and bounded execution; no independent billing/pricing/entitlement authority |
| Channels and tools | Platform channel bindings and domain operation catalogue | Agent tool allow-lists, MCP mediation, and SDLC event production |
| Delivery | Platform-owned targets retain their own mutation/release policy | Candidate, build/deployment evidence, previews, and human gates for tenant software |

Separate state machines are legitimate when they have different subjects: a platform export job is not a Kodama pipeline run. Correlate their identifiers; do not independently complete or retry the same external operation through both systems. Platform's ID-only jobs broker and ingestion lanes are not Kodama's outbox/event transport. Reusing a NATS technology choice grants no access to their streams.

Kodama is an independently deployable appliance. It runs on Kubernetes on AWS, GCP, or Scaleway and requires its own PostgreSQL database/roles and that cloud's KMS. The first staging target is Platform's existing stage cluster, with Kodama's control plane in a separate namespace and scoped execution namespaces. Subsequent installations may run in other clusters and connect to the central Platform. Co-location does not share database authority, processes, private Go packages or credentials. Platform's `fr-par` residency does not select every Kodama installation's cloud. Hosted adapters implement consumer-owned ports and supported service interfaces. Cross-repository SQL, imports of private platform packages, and copies of core policy are not substitutes for that contract. Local foreign keys/RLS protect Kodama records; they do not establish core authorization.

## First integration shape

Platform may submit work through a selected Kodama installation's public API and observe the returned run. Use the versioned OpenAPI/generated client from T-027, preserving tenant scope, idempotency and operation correlation. Platform chooses the installation endpoint; Kodama owns attempt scheduling and recovery within that installation. The prototype's embedded Go runtime is not the integration target. T-061 freezes the selected O-18 OAuth caller contract and endpoint registration; no global dispatcher is introduced here.

CLI, HTTP API, MCP and webhooks are the primary product surfaces. O-15 selects Platform as the only intended host of an optional UI, likely its existing `packages/dashboard`; T-050/T-051/T-060 remain deferred. Their local `web/` ownership is removed. An upstream-owned task must agree the actual shell, accessibility and API/auth handoff before UI work is scheduled; no second dashboard is implied.

Platform remains the edge/cloud delivery layer for customers' built artifacts. Cloudflare, Fastly and GCP worker-style targets are possible later Platform operations, not new Kodama executors. Whether invocation uses a supported Platform HTTP operation or MCP mutation is still open; the reviewed MCP surface is read-only. Keep the first Kodama deployment contract (namespace-scoped Kubernetes plus ordinary scripts) until an upstream mutation/delegation contract is accepted.

## Shared libraries

Follow platform's foundation library choices, recorded in [P-16](../decisions.md#implementation-baseline), without importing `github.com/optimiweb/platform`. Kodama's public repository/module is `github.com/optimiware/kodama`; this does not rename the existing Platform repository. Kodama-local secret envelopes follow the upstream secrets contract, and the hosted use of that store is qualified through T-061.

O-10 retains Go 1.27 for Kodama (currently 1.27.2); Platform will update independently from its reviewed 1.26.0 baseline. Qualify dependency compatibility and record intentional pin differences rather than forcing a downgrade. `go.yaml.in/yaml/v4` remains the actionlint exception. Product code must not adopt the probe's JWT parser, Cobra, chi or GORM merely because a dependency exists in the module graph.

## Hosted and standalone profiles

In Optimi-hosted operation, the Optimi OAuth server authenticates actors and can issue signed UUID `tenant_id` and role claims. T-061 validates the actual issuer, audience, signature, expiry and claim schema, including role mapping. The verified tenant UUID is the ownership identity for tenant-scoped PostgreSQL resources and transaction-local RLS context. A factory tenant refers to one authoritative Platform tenant; any compatibility mapping is explicit, installation-bound and unique, never inferred from names, repositories or caller-supplied tenant/role headers.

Project bindings may narrow SDLC permissions inside that current authority. Local `admin` is not Platform administration and does not implicitly grant human review. Current revocation, tenant disablement and entitlement policy are checked before new dispatch, tool/secret use, gate decisions and completion; a retained token or restored database cannot recreate access. T-061 names the supported OAuth/current-access mechanism instead of assuming that an unexpired claim alone proves current authorization. Settle already-started effects under the owning lifecycle.

The standalone/on-premises profile may use Kodama-local OIDC membership and credential adapters. These implement the same SDLC ports and isolation requirements but do not become a second authority for an Optimi-hosted tenant. The installation selects one authority profile explicitly; failure of the core adapter never selects the local one. Existing local schemas, roles, key wrapping, and bootstrap plans describe this profile unless hosted integration explicitly adapts them.

## Current gaps and implementation checkpoints

| Gap found in the existing design | Required checkpoint |
| --- | --- |
| Local membership/RBAC could grant hosted access independently of platform membership | Selected O-18 authority is Optimi OAuth with verified UUID tenant/role claims; T-010 supplies the consumer port, and T-061 freezes the actual claim/current-access contract and qualifies the adapter |
| Kodama retains immutable secret versions; the reviewed Platform store retains the current value only and has no general remote plaintext-read API | Selected O-19 storage is KMS-encrypted PostgreSQL through the secrets module, with version pins and audited read-for-use. T-061 coordinates the missing supported version interface; do not substitute the newest value or copy private rows |
| T-033's integration-owned MCP credentials do not provide platform user delegation | The current platform MCP resource is read-only and uses its own audience. T-033 remains a generic integration adapter; T-061 needs an accepted recipient-bound credential/delegation flow. Do not forward a factory/CLI token or invent a platform machine grant |
| Core roadmap and Kodama both discuss agent execution and communications | Core retains tenant/channel/domain authority; Kodama owns agent/SDLC execution. T-061 records the supported first vertical slice and correlations; adding a channel or mutation needs its owning core contract |
| Detailed diagrams and local schemas could be mistaken for a qualified hosted release | T-056 depends on T-061 as well as recovery/isolation evidence; the current local M1/M2 tracers prove only their declared profile |

Stage co-location, OAuth authority and API-first integration are selected direction, not evidence of a deployed connection. No unimplemented token exchange, scope name, synchronization service or provider-mutation API is asserted here. T-061 records the actual supported claim, current-access and secret-version contracts and separately owned upstream prerequisites before enabling the hosted profile.

## Credential delivery and commercial ownership

Reuse the existing secrets module's KMS-encrypted PostgreSQL envelope and immutable logical versions. Model/tool proxies are the default way to keep provider credentials out of agent processes and prompts: the caller receives attempt-scoped authority to use an approved operation, not the provider token. Ordinary script/build steps may explicitly bind an authorized pinned secret to the child process environment; this is deliberate plaintext disclosure to that script, not a leak-prevention guarantee. Resolve just in time, audit use, keep plaintext out of Pod manifests and retained logs/artifacts, and block new use after revocation. Protected files remain available through the existing delivery contract.

Platform owns the subscription add-on, commercial metering/usage access, billing and entitlements. Kodama owns runtime request/token/tool/concurrency bounds and attributable usage/outcome evidence. Consuming a Platform allowance does not introduce a second pricing, invoicing, payment or subscription authority in Kodama.

## Requirements

| ID | Requirement |
| --- | --- |
| PI-01 | Optimi-hosted tenant identity, membership, entitlements, and core permissions MUST have the platform as their sole authority; any local projection MUST have explicit mapping and revocation semantics. |
| PI-02 | Every hosted action MUST resolve one core-authorized tenant and apply SDLC-specific permissions without widening that authority. |
| PI-03 | Hosted integrations MUST use reviewed ports/adapters and supported contracts; they MUST NOT bypass core policy through private tables, packages, storage, or brokers. |
| PI-04 | Standalone authority MUST be explicitly configured; core failure or denial MUST NOT fall back to local grants, credentials, or another tenant. |
| PI-05 | Credential integration MUST preserve declared versions, custody, current authorization, revocation, audit, and restore semantics; unavailable pinned versions MUST fail closed. |
| PI-06 | Platform tool use MUST preserve resource audience, current tenant access, entitlements, bounds, and core audit; a profile allow-list alone MUST NOT grant a core operation. |
| PI-07 | External platform operations MUST retain originating actor/workload, tenant, run/attempt, core operation reference where supplied, and result/unknown-outcome provenance without sensitive payloads in events. |
| PI-08 | Tenant data sent to models, transcripts, artifacts, or channels MUST have an explicit allowed projection, destination, retention, and deletion policy; authorization to read MUST NOT imply permission to redistribute. |
| PI-09 | Integration changes MUST name reviewed upstream revisions/contracts and prove compatibility against the supported platform; roadmap text and fakes MUST NOT establish hosted support. |
| PI-10 | Core tenant disablement or authority loss MUST prevent new protected actions and trigger attributable settlement of running work; retained history MUST NOT authorize new execution. |

## Failure, recovery, and acceptance

Core unavailability is a classified dependency failure, not empty tenant discovery, successful authorization, or a reason to replay an uncertain mutation. Independent source/history inspection follows its declared access policy. Integration recovery revalidates the mapping and current authority before dispatch; a restored Kodama database cannot resurrect core access. Preserve core operation identity when reconciling a lost response.

| ID | Scenario | Requirements |
| --- | --- | --- |
| PI-A01 | Local role/header/mapping changes cannot grant a foreign or inactive core tenant; revoking core membership blocks tool use and human decisions despite retained local bindings | PI-01, PI-02, PI-10 |
| PI-A02 | Core denial/outage and stale authority refuse protected actions without selecting a local adapter; standalone tests still use explicit local configuration | PI-03, PI-04 |
| PI-A03 | Value rotation, revocation, missing pinned version, and restore preserve the agreed custody/version contract; no direct platform table access or plaintext management read is used | PI-03, PI-05 |
| PI-A04 | Wrong-audience tokens, missing entitlements, direct hidden-tool calls, and audit failure disclose no data; read diagnostics cannot mutate a provider | PI-02, PI-06 |
| PI-A05 | A lost core response retains correlation and unknown outcome; retry observes/reconciles the same operation rather than blindly creating another | PI-07 |
| PI-A06 | Model/transcript/channel collection rejects unapproved tenant data and destinations and exercises the declared retention/deletion policy | PI-08 |
| PI-A07 | The first tenant intent → authorized tool → candidate/evidence → human decision slice passes against a named real platform revision; a changed upstream contract is detected by conformance review | PI-09 |
| PI-A08 | Core tenant disablement during a run and a restore with stale mappings block new actions, settle external effects, and retain attributable history | PI-01, PI-10 |

The [decision register](../decisions.md) owns O-18/O-19. [T-061](../tasks/T-061-platform-integration.md) owns integrated acceptance; module and standalone tests alone cannot satisfy it.
