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

Kubernetes uses one tenant namespace, Pod Security `restricted`, default-deny NetworkPolicies with explicit required egress, per-tenant service accounts, and ResourceQuota. Attempt scratch volumes are private. Tenant code receives no control-plane database/broker credentials. Rootless build images must fit this baseline or fail preflight.

These controls support the selected trusted-engineer staging and qualified trusted-engineer installation profile. Regular containers are the current runtime; gVisor, Substrate and other isolation/security options are later evaluation candidates. Public execution of mutually untrusted code still requires a separately selected and validated boundary before that later profile is offered. Namespace separation alone does not establish that guarantee. Optimi-hosted identity/tenant integration can be qualified independently on the trusted-engineer profile.

## Requirements

| ID | Requirement |
| --- | --- |
| IT-01 | Credentials and local membership MUST be validated independently. |
| IT-02 | All product relations MUST enforce tenant/project ownership. |
| IT-03 | Authorization MUST cover every access path, including logs, streams, artifacts, and source. |
| IT-04 | Runtime database access MUST use tenant predicates and RLS without owner/bypass privileges. |
| IT-05 | Pool reuse MUST clear transaction-local tenant context; foreign keys MUST include ownership. |
| IT-06 | Attempt tokens MUST be short-lived, audience-bound, and checked against current attempt/cancellation state; signing-key rotation MUST invalidate old tokens after restore. |
| IT-07 | The fixed execution baseline MUST be enforced and its trust limits documented. |
| IT-08 | Workloads MUST use scoped product interfaces instead of privileged platform connections. |
| IT-09 | Membership/settings changes MUST be audited and invalidate affected authorization caches. |
| IT-10 | Gates/completion MUST authenticate a human with the required current role. |
| IT-11 | Cross-tenant references MUST return nondisclosing not-found responses. |
| IT-12 | Production offboarding MUST revoke admission/access, settle execution, and track backend retention/deletion. |
| IT-13 | Hosted tenant ownership/RLS context MUST derive from the verified Optimi OAuth UUID `tenant_id`; role claims MUST follow the qualified mapping and current authority checks, not caller headers or local role widening. |

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
