# S05 — Identity, authority, and verification

Source: [authorization](../../KODAMA.md#authorization-and-verification), [human identity](../../KODAMA.md#human-identity), [tokens](../../KODAMA.md#bearer-token-security-model). Owners: P01–P03, P17–P19, P22. AC: 5, 7, 19.

## 1. Principals and grants

Principals are verified humans, enabled services, or eligible executions. URL IDs, environment variables, body roles, network location, and correlation strings are not proof of identity. Resource scope comes from relational ownership; creation scope comes from the owning tenant/project.

One `Authorize(principal, operation, resource)` implementation serves Shell capabilities and public edges. C01 freezes an explicit operation/grant matrix with all unknown operations/resources refused. Baseline semantics:

| Principal | Authority |
|---|---|
| Human Admin | Configuration/membership administration; no implicit Reviewer authority |
| Human Engineer | Authorized project pipelines, tasks, and execution operations |
| Human Reviewer | Current assigned human-role decisions in scope |
| Human Operator | Authorized runtime follow-up/retry/cancel and operational inbox |
| Human Viewer | Authorized read access |
| Service | Current scoped roles excluding Admin; answers only explicit `service/<id>` assignments |
| Execution | Current running container's admitted permissions: task content/metadata, project task creation, own-task ask, own outputs/events, configured models/tools |

Execution authority excludes request resolution, assigned task replies, membership/pipeline/binding administration, and plaintext secret-management reads. Enforcing services decide their own resource access using returned facts.

## 2. Human verification

OIDC/OAuth access tokens must match configured issuer, signature algorithm/key, Kodama audience, and expiry. Resolve current memberships on every authorized call. Test stale/unknown keys, rotated keys, bad issuer/audience, malformed claims, expiry and current role removal. Identity-provider disablement is bounded by short access-token lifetime; do not claim instantaneous IdP revocation unless implemented by the provider.

Hosted authority/entitlement checks use the configured Platform contract. Failure refuses hosted authorization; no fallback to standalone grants. Cached discovery/key material may improve availability within its declared bounds but cannot replace current Kodama membership/service/execution state.

## 3. Derived tokens

```text
kodex.<execution-id>.<base64url-no-padding(HMAC-SHA256(tenant-key,
  encode("kodama-execution-v1", tenant-id, execution-id, nonce, expiry)))>

kodsvc.<service-id>.<base64url-no-padding(HMAC-SHA256(tenant-key,
  encode("kodama-service-v1", tenant-id, service-id, nonce, expiry)))>
```

C01 freezes an unambiguous canonical binary encoding and exact test vectors, including lengths/time representation. IDs are lookup hints until constant-time MAC verification. Wrong token kind/domain, missing nonce/expiry, malformed encoding, expired token, and inactive principal are refused.

Each tenant key and principal nonce has at least 256 random bits. The Shell stores an encrypted tenant derivation key and one current nullable nonce/expiry pair per execution/service; it stores neither token plaintext nor token hashes. Plaintext exists only for delivery/use, with redacted formatting and no serialization into events/receipts.

| Lifecycle event | Required behavior |
|---|---|
| Container activation | Issue if absent/expired; TTL ≤8 hours and ≤ action deadline |
| Consecutive container | Reuse still-valid token; current action/deadline governs authority |
| Long-running renewal | Atomically replace nonce/expiry, then replace mounted file; retry delivery without restarting workload |
| Request entry | Clear fields; workload has no authority during wait |
| Pass then next container | New eligible credential in same execution |
| Cancel/terminal/task handoff | Clear fields atomically with state transition |
| Service issue/rotate | Explicit configured expiry; replace fields; securely return new token |
| Service revoke/disable | Clear fields and invalidate established connections |
| Tenant key rotation | Immediately invalidate that tenant's tokens; replace eligible execution mounts; service owner reissues |

Authentication checks current nonce/key/expiry, principal state, scope, running container, and action deadline. Credential refresh never revives terminal executions. NATS connections/subscriptions enforce changes and expiry, not just initial authentication.

## 4. Verification protocol

`POST /api/v1/verify` authenticates the **calling service**, then independently authenticates the **actor credential**. The service binding must cover the resource and attempted operation. Service permissions never substitute for actor authority.

Input names the actor credential, registered resource, typed operation/effect subject, and optional `approval: role/<role>`. Output contains verified principal, current roles in resource scope, approval fact when requested, and a verification record ID. It is not an access decision or bearer grant.

Approval is true only if all of these hold:

1. Actor is an eligible execution; humans/services cannot carry execution approval evidence.
2. Evidence is an earlier succeeded request in **that same execution**.
3. Assignment is exactly the required human `role/<role>`; a service with that role is insufficient.
4. Recorded decision is pass, authorized at resolution, with intact immutable subject metadata.
5. Operation, resource, and every effect subject field supplied by the enforcing service match exactly.

Extra request subject fields are immutable display context, not verified facts, and are ignored by matching. C01 fixes typed equality; do not stringify unequal JSON types or compare only a caller-supplied digest of selected fields.

The verification record captures service, actor, execution/action, resource, operation/subject, requested approval, result, and time. It excludes the forwarded credential. Every new attempt/retry rechecks current state. A previous verification ID can link a report to an accepted effect but cannot authorize a new effect.

## 5. Transport and trust boundaries

Tokens use HTTPS Bearer. Git helper supplies a current token as password for username `kodama` only to registered trusted Git origins/paths. Reject origin/path confusion, insecure redirects, and unregistered targets. Tokens never enter URLs, repository configuration, logs, task metadata, or upstream providers. Read the mounted token file for each authentication so renewal takes effect.

## 6. Unit test obligations

- Golden MAC vectors; tampering, wrong kind/tenant, absent fields, expired token, deterministic replacement/rotation.
- Every operation's allow/deny matrix and cross-scope combinations; Admin is not Reviewer.
- Concurrent revocation/renewal and request-wait/terminal lifecycle preconditions through fake atomic state.
- Separate service and actor identities; unknown binding/operation; no service impersonation.
- Prior/foreign execution, wrong role/service assignment, failed/expired/cancelled request, wrong resource/operation/candidate/base, forged output strings, display-field handling.
- Current role removal/cancellation/service disablement invalidates a previously allowed attempt.
- Redaction and secret-bearing field rejection across verification journals/log serializers.
- Trusted-origin helper and live broker invalidation state-machine cases with fake transports.

Real OIDC transport, NATS connections, Git forwarding, database races and tenant isolation are delayed integration obligations, not substitutes for these unit cases.
