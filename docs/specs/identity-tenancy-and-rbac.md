# Identity, tenancy, and RBAC

**Scope:** O-07 selects trusted-engineer staging on regular Kubernetes containers with the existing M1 shared-tenancy restrictions. Stronger/untrusted execution is a deferred, separately qualified extension.

**Authority profile:** local OIDC/membership storage and bootstrap below define the standalone baseline. O-18 selects the Optimi OAuth server for hosted authentication, with signed role and UUID `tenant_id` claims under the [platform integration](platform-integration.md) contract. Platform tenant status, current permissions and entitlements remain authoritative; local bindings cannot create core access. T-061 qualifies actual claims, role mapping and current revocation behavior. Runtime predicates/RLS remain required in both profiles.

## Identity and roles

OIDC establishes human identity; the CLI uses OAuth authorization code with PKCE. The first deployment uses a configurable issuer supporting JWT access tokens for the factory audience. Verify access-token issuer, audience, signature, expiry, and subject with `github.com/coreos/go-oidc/v3` and `golang.org/x/oauth2`, the same libraries platform uses. Do not accept a hand-parsed JWT; the scaffold probe's `golang-jwt` check is a fixture, not the product verifier. An ID token is not a general API bearer credential. Map `(issuer, subject)` to a user, then check local membership.

| Fixed role | Grants |
| --- | --- |
| `admin` | Tenant/project membership, settings, credential metadata/management |
| `analyst` | Read project work/evidence; edit specification and acceptance |
| `engineer` | Technical edits, workspace/run management, project deployment |
| `reviewer` | Human gate decisions and `work_item:complete` |
| `viewer` | Authorized read-only access |

Bindings can compose at tenant/project scope. Admin does not implicitly become a reviewer or obtain secret plaintext. An operator uses a distinct administrative identity for bootstrap/installation operations. Workload identities get explicit attempt-scoped actions, never human roles. Custom roles and policy engines are later work.

For the hosted profile, verify the OAuth access token before reading its actor/roles/tenant claims. Validate and canonicalize `tenant_id` as a UUID and use that identity for tenant ownership and transaction-local RLS. Missing/malformed claims, a foreign tenant, a wrong audience and untrusted role/tenant headers deny access. Existing standalone opaque-ID fixtures do not establish hosted UUID acceptance; required compatibility constraints or migrations belong to T-061 and its integration owner.

## Isolation baseline

Use tenant/project keys, composite foreign keys, application authorization, and PostgreSQL RLS. A `WithTenant` transaction helper establishes validated context locally. Runtime roles lack table ownership and `BYPASSRLS`; migrations use a separate owner. Discovery of memberships filters by authenticated principal. Workers enumerate tenant IDs through a restricted path, then enter ordinary tenant transactions.

`[attempt_private]` (current) Kubernetes uses one tenant namespace, Pod Security `restricted`, default-deny NetworkPolicies with explicit required egress, per-tenant service accounts, and ResourceQuota. Attempt scratch volumes are private. Tenant code receives no control-plane database/broker credentials. Rootless build images must fit this baseline or fail preflight. This remains the only executable profile until the `run_shared` cutover; it is not weakened by the profile below.

These controls support the selected trusted-engineer staging and qualified trusted-engineer installation profile. Regular containers are the current runtime; gVisor, Substrate and other isolation/security options are later evaluation candidates. Public execution of mutually untrusted code still requires a separately selected and validated boundary before that later profile is offered. Namespace separation alone does not establish that guarantee. Optimi-hosted identity/tenant integration can be qualified independently on the trusted-engineer profile.

## Run-shared isolation profile

Normative design for `run_shared` runs ([ADR 0010](../adr/0010-run-workspace-services-builders.md), [run-resources contract](../contracts/run-resources.md)); **not delivered, not qualified**, and consistent with O-07's trusted-engineer scope. It does not select or imply gVisor, Substrate or untrusted-code isolation.

- **Kept.** Tenant namespaces, RLS and scoped credentials, default-deny ingress/egress, per-tenant ResourceQuota/service accounts, no automounted Kubernetes API token, no control-plane database/broker credentials, no host PID/IPC/network, host paths or engine sockets, and no arbitrary pipeline-supplied security, mount, network or selector controls.
- **Replaced.** The fixed `restricted` baseline is replaced for new `run_shared` workloads by a versioned, operator-installed [execution policy](../contracts/run-resources.md#execution-policy) pinned at admission and verified at dispatch/adoption: Pod user namespaces, non-root users, `SETUID`/`SETGID` mapping helpers inside the user namespace only, qualified seccomp/LSM, bounded writable mounts and storage, and Pod Security `baseline` at a tested version pin. Kodama's template and adoption checks verify the actual security fields, not only labels or fingerprints. Pod Security labels apply per namespace; T-116 owns namespace/node-pool layout and documents the effect on `attempt_private` workloads.
- **Network.** The pinned policy selects the model ([O-30](../decisions.md#revised-scope--2026-10-29-o-30)). The stage model is tenant-level: a tenant's workloads may reach the run services of that tenant, while other tenants, the control plane, the Kubernetes API, node and cloud-metadata addresses are denied; public HTTP/HTTPS egress is allowed and private destinations need named [egress profiles](../contracts/run-resources.md#egress-profiles). The stricter run model additionally matches run identity (a run's workloads reach only that run's declared service ports and edges, other runs of the tenant denied). Independently, `publicEgress: false` limits all external access to named profiles enforced after name resolution. DNS always goes to the installation resolver. Service discovery names are not authorization. Kubernetes NetworkPolicies are additive, so broad tenant allow policies are replaced, not merely supplemented. Nested rootless networking stays inside the Pod boundary and never bypasses default deny; `hostNetwork` is never used.
- **Builders.** ko and Buildah run as ordinary steps under the policy's `step` role: every `run_shared` step gets that builder-capable shape (O-30). BuildKit and builder services are deferred to [T-123](../tasks/T-123-buildkit-builder-service.md); the design retained for it: BuildKit uses a qualified rootless mode where it works; otherwise a run-exclusive builder service with its own process namespace, credentials, bounded storage and authenticated run-only endpoint, torn down completely with the run. A host-privileged mode requires an exclusive disposable isolated worker with no unrelated workloads or node/cloud credentials; a namespace or label is not that scope. The installation selects and pins one qualified mode and a failed rootless build never silently widens privilege.
- **Resource identities.** Preparation and service workloads have their own narrow audience (`kodama-runtime-resource`); they cannot call step source/result/artifact/begin, model/MCP proxies or another resource's secrets, and neither token type works on the other's routes.
- **Trust limits.** All attempts of a run share one filesystem trust domain: later commands can read what earlier ones wrote, and a command explicitly given a secret can copy it. These controls support mutually trusting engineers; running mutually untrusted code in one run or tenant still needs a separately selected and validated boundary.

## Requirements

| ID | Requirement |
| --- | --- |
| IT-01 | Credentials and local membership MUST be validated independently. |
| IT-02 | All product relations MUST enforce tenant/project ownership. |
| IT-03 | Authorization MUST cover every access path, including logs, streams, artifacts, and source. |
| IT-04 | Runtime database access MUST use tenant predicates and RLS without owner/bypass privileges. |
| IT-05 | Pool reuse MUST clear transaction-local tenant context; foreign keys MUST include ownership. |
| IT-06 | Attempt tokens (and `run_shared` resource tokens, which use a distinct audience) MUST be short-lived, audience-bound, and checked against current attempt/resource/cancellation state; signing-key rotation MUST invalidate old tokens after restore. |
| IT-07 | `[attempt_private]` The fixed execution baseline MUST be enforced and its trust limits documented. `[run_shared]` replaced by the pinned execution policy (IT-14, IT-15); the fixed baseline remains for `attempt_private` runs. |
| IT-08 | Workloads MUST use scoped product interfaces instead of privileged platform connections. |
| IT-09 | Membership/settings changes MUST be audited and invalidate affected authorization caches. |
| IT-10 | Gates/completion MUST authenticate a human with the required current role. |
| IT-11 | Cross-tenant references MUST return nondisclosing not-found responses. |
| IT-12 | Production offboarding MUST revoke admission/access, settle execution (including `run_shared` resources and scratch), and track backend retention/deletion. |
| IT-13 | Hosted tenant ownership/RLS context MUST derive from the verified Optimi OAuth UUID `tenant_id`; role claims MUST follow the qualified mapping and current authority checks, not caller headers or local role widening. |
| IT-14 | `[run_shared]` Network enforcement MUST implement the pinned policy's network model, verified with real CNI traffic. Every model MUST deny other tenants, the control plane, the Kubernetes API, node and cloud-metadata addresses, and private destinations without a named egress profile. Reachability: the tenant model (O-30) MAY let a tenant's workloads reach that tenant's run services; the run model MUST additionally deny other runs of the same tenant and enforce declared service edges. External access, independently: `publicEgress: true` MAY allow public HTTP/HTTPS; `publicEgress: false` MUST deny every destination outside the named profiles. |
| IT-15 | `[run_shared]` ko and Buildah MUST actually build and push under the pinned policy; arbitrary privilege, host paths/namespaces/devices, engine sockets, security overrides and a wider shape after a failed build MUST be refused. |
| IT-16 | `[run_shared]` **Deferred to T-123 (O-30).** A selected BuildKit mode MUST run real `RUN` builds, authenticate run clients and remove daemon/process/cache with the run; a host-privileged mode MUST prove exclusive worker provisioning and disposal and MUST NOT be advertised without that isolation. |

## Recovery

Rotate workload signing keys, fence old controllers/publishers, revoke old runtime sessions, and reconcile external effects before dispatch. Already delivered credentials and running remote requests require explicit revocation/settlement; a key rotation only invalidates subsequent token use.

## Acceptance

| ID | Scenario | Requirements |
| --- | --- | --- |
| IT-A01 | Wrong audience/issuer or absent membership denies access | IT-01, IT-11 |
| IT-A02 | Composite ownership checks reject cross-tenant/project writes | IT-02, IT-04, IT-05 |
| IT-A03 | Shared pooled connection never leaks prior tenant context | IT-04, IT-05 |
| IT-A04 | Copied log/artifact/cursor/source references disclose no foreign content | IT-03, IT-11 |
| IT-A05 | Cancelled/rotated attempt tokens cannot start actions or finalize results | IT-06 |
| IT-A06 | The declared baseline denies privileged Pods and platform data access | IT-07, IT-08 |
| IT-A07 | Role removal immediately prevents new gate decisions/completion | IT-09, IT-10 |
| IT-A08 | Offboarding preserves visible incomplete deletion while denying new access | IT-12 |
| IT-A09 | Missing/malformed/foreign UUID tenant claims, wrong-audience tokens and forged role/tenant headers fail; the authorized UUID scopes real PostgreSQL/RLS resources | IT-01, IT-02, IT-04, IT-13 |
| IT-A10 | Nested build and service traffic obeys the pinned network model: same-tenant service access and public egress under the tenant model; foreign-tenant, platform, Kubernetes API, metadata and profile-less private destinations denied; under the run model also other same-tenant runs and undeclared edges; without public egress also unnamed public destinations | IT-08, IT-14 |
| IT-A11 | ko and Buildah really build and push; privilege, host path/namespace/socket, security override and policy-widening attempts are refused | IT-07, IT-15 |
| IT-A12 | Deferred to T-123 (O-30). The selected BuildKit mode builds real `RUN` instructions, authenticates run clients and leaves no daemon/process/cache; privileged mode, if selected, proves worker disposal | IT-16 |
