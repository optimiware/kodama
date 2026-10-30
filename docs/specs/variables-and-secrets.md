# Variables and secrets

**Scope:** M1 project variables and encrypted secrets. Production requires the installation cloud's KMS; T-028 implements AWS, GCP, and Scaleway adapters.

**Authority profile:** O-19 selects the existing secrets module and KMS-encrypted PostgreSQL storage, with immutable version pins and scoped audited read-for-use. Standalone custody is the implemented local baseline; hosted core/provider credentials follow [platform integration](platform-integration.md) and T-061. The reviewed Platform current-value revision is not an immutable value version; hosted integration still needs the supported version interface rather than a second uncontrolled copy.

## Model

Variables are typed nonsecret values stored as plaintext in PostgreSQL. Secret values are envelope-encrypted, versioned, and write-only through the management interface. A secret has `kind` (`generic`, `git`, `model`, `mcp`, or `kubeconfig`), version, scope, and `revoked_at`. Typed secret payloads carry provider credentials; ordinary configuration remains in project/profile/environment settings. There is no generic connection resource.

Precedence is pipeline input default → project variable of the declared name → explicit run input. Unknown names and type mismatches fail admission. Runs pin immutable effective values and secret versions. Environment configuration in M2 is an explicit snapshot on a deployment, not another implicit variable-inheritance level.

## Encryption

Kodama-local values use the Optimi Platform envelope in [runtime secret storage](https://github.com/optimiweb/platform/blob/05dec0d81a15f9c3bcac826508f0342761343d38/docs/specs/16-secrets.md). Implement that contract in this module. Do not import `github.com/optimiweb/platform`.

The key provider holds a KEK and wraps one random 32-byte scope key per tenant generation. It does not wrap a data key for every secret value. Go derives a per-secret-revision AES-256-GCM key with HKDF-SHA-256. Associated data binds installation identity, tenant, project, secret ID, immutable name, and value version. Persist the ciphertext, algorithm, scope-key ID, and KEK reference. Value rotation creates a new value version. Rewrapping a scope key preserves the value version. Keep KEK versions required by retained backups.

A supported installation runs on Kubernetes and requires PostgreSQL plus one KMS. Supported clouds are AWS, GCP, and Scaleway; the provider is AWS KMS, GCP Cloud KMS, or Scaleway Key Manager. The installation selects one provider. Startup fails if it is missing, and it never falls back to another cloud or to the development keyfile. Platform's `fr-par` residency does not select Kodama's cloud.

Scope-key wrapping lives in `pkg/secretkeys`: ephemeral memory, a local keyfile, and Scaleway, AWS, and GCP on one wrap/unwrap contract. The binding is authenticated data. T-028 selects one of those providers behind the `KeyWrapper` port. It does not reimplement wrap/unwrap. Package tests are the adapter evidence for VS-06–VS-10. A live cloud run is production qualification (T-029), not a second implementation and not a harness blocker for T-028.

Development uses a local keyfile of 32-byte KEKs supplied outside the database. The scaffold `master-key` file is that development KEK, not a value key. Replicas of a development profile share one keyfile. A plaintext database fallback is never used. Matching this envelope does not copy Platform secret rows or treat its current-value revision as a pinned version; T-061 qualifies selected O-19 hosted custody.

## Runtime delivery
### Installation startup

Configuration document version 2 selects `secrets.adapter: keyfile | kms`. Production requires KMS and one `secrets.kms.provider: aws | gcp | scw`; test uses keyfile and development injects its ephemeral wrapper. T-105 constructs the selected workload-authenticated client, validates key mappings and performs the existing bounded startup wrap/unwrap probe before serving. The `scw` configuration name maps to the established Scaleway persisted KEK identity. Invalid settings, denied credentials and probe failures stop startup. Client resources close on construction failure and after drained shutdown. Live cloud/deployment qualification is T-104; full retained-data recovery remains T-029 under VS-10.

`secrets.keyfile.current/files` selects pre-provisioned 32-byte files. `secrets.kms.kekId/current/resources` selects pre-provisioned keys and retained labels; label mappings and installation identity remain stable across rollouts. Startup never rotates or creates a cloud key. See [chart configuration](../../deploy/charts/kodama/README.md#kms-selection) for fields and credential projections.

### Workload delivery

Prefer the AI gateway and MCP proxy for agent provider credentials: the server resolves the approved pinned secret and performs the operation, while the agent receives only scoped runtime authority. Do not put provider tokens in prompts, tool arguments, source or ambient agent environments merely to enable a proxy call.

An ordinary container step (harness, build, publish, deploy or another command) may explicitly declare `env: {NAME: {secretRef: "secret://project/name"}}` under the pipeline contract. Admission pins the version and checks scope/purpose. Resolve it just in time into the authorized child environment, not plaintext Kubernetes Job/Pod fields; protected secret files outside source/output roots remain the preferred direct-delivery form where supported. A child receiving ENV or a file can read that secret, so the binding is an explicit disclosure grant, not a guarantee against a malicious script. A directly invoked harness uses this same declared-binding rule, never implicit inheritance or mandatory proxy setup ([ADR 0012](../adr/0012-container-sdlc-and-artifact-releases.md)).

The ADR 0012 implementation must resolve the authorized union of environment and explicit step bindings, rejecting ambiguous name/reference/version/delivery collisions. Declaring an environment must not silently discard the step's own bindings. Each admitted run retains exact versions; a new rollback/cleanup run may select current authorized versions while retaining the original target configuration, with the new credential pins recorded separately. Revoked or missing versions never silently fall back. Source publication by a secret-using writer requires the existing disclosure scan to be wired, including prior run disclosures; an unbound scan is an implementation gap, not a permanent prohibition on harness containers.

**Services, preparation and shared scratch (`run_shared`, designed, not delivered).** A [service](../contracts/pipeline-format-v1alpha2.md#services) may declare its own `env` secret references and `secretFiles`; admission pins their versions like a step's. Preparation and each service authenticate with a resource-subject token ([runtime v2](../contracts/runtime-v2.md#resource-subjects)) and can resolve only that resource's pinned bindings: a binding made for a step is not a binding for a service, a service cannot call step `source`/`result`/`artifacts`, and copying one subject's token to another resource, attempt or publication action fails. Plaintext stays out of manifests and shared mounts: injection is just-in-time into the intended process or a private memory-backed file. Registry/build credentials needed by a build use a private auth file or a scoped build session and are destroyed on stop; push credentials normally stay with the build client. Because all steps of a run share `/workspace`, a command explicitly given a secret can copy it there. Mount exclusion and known-value scanning do not prevent that; source/output redaction also scans for values delivered to earlier attempts of the run, and the residual risk is documented rather than denied. The checkout credential belongs only to the preparation workload and never reaches a step or service.

Revocation prevents new resolution/proxy use. Redact known injected values and exclude secret files from bundles/artifacts; do not claim arbitrary output is leak-proof. T-061 qualifies hosted delivery and requests a bounded owner prerequisite if an existing runtime delivery seam needs extension; no frozen wire contract is silently changed here.

## Requirements

| ID | Requirement |
| --- | --- |
| VS-01 | Values MUST be scoped/versioned; secret values MUST use authenticated envelope encryption. |
| VS-02 | Resolution MUST follow declared precedence and pin effective values/versions. |
| VS-03 | Runtime resolution MUST be restricted to declared references and active attempt authority (`run_shared`: or the resource's own pinned bindings and active resource authority). |
| VS-04 | Management reads/events/errors MUST omit secret values. |
| VS-05 | Runtime delivery MUST be scoped and audited; prefer files outside source/output roots. |
| VS-06 | Tampered ciphertext or ownership context MUST fail authentication. |
| VS-07 | Rewrap and value rotation MUST retain pinned-version semantics. |
| VS-08 | `revoked_at` MUST block new resolution; active consumers follow explicit termination/issuer-revocation procedures. |
| VS-09 | KMS failure MUST surface a classified error without plaintext persistence/fallback. |
| VS-10 | Production restore MUST demonstrate decryptability with retained key versions. |
| VS-11 | Injected secret files MUST be excluded from source bundles/artifacts; diagnostics access and redaction remain separate controls. |
| VS-12 | Deletion MUST account for pinned runs and backup retention and retain metadata audit. |
| VS-13 | Agent provider credentials SHOULD stay behind scoped proxies. Direct ENV/file delivery MUST be explicitly declared, version-pinned, purpose-scoped and audited; plaintext MUST NOT be placed in Pod manifests or implicitly inherited by other steps. |
| VS-14 | `[run_shared]` Preparation and services MUST resolve only their own pinned secrets just in time; a grant MUST NOT work for another resource, attempt or publication action; checkout credentials MUST reach only preparation; manifests MUST omit plaintext; revoked bindings MUST block new resolution; redaction MUST account for earlier attempts' disclosures in the shared scratch. |
| VS-15 | `[ADR 0012]` Explicit step and environment secret bindings MUST compose without silent loss or ambiguous collisions; every use MUST match the new run's authorized immutable pin, including rollback/cleanup and direct harness commands. |

## Interface and authorization

Use project variable CRUD and secret create/metadata/version/revoke operations. Administrative management authority does not imply plaintext read. Resolve is an internal attempt interface. Long-lived Git write credentials are reserved for the control-plane publisher; runtime checkout uses read-only access. Internally hosted repositories ([ADR 0011](../adr/0011-internal-tenant-git-service.md), designed) need no stored Git secret at all: people and attempts receive short-lived Git grants, so `git`-kind secrets remain only for external repositories.

An already delivered secret cannot be recalled from memory. Revocation prevents further resolution and can trigger consumer termination and external credential rotation. Plain variables may appear in logs; confidential values must be secrets.

## Acceptance

| ID | Scenario | Requirements |
| --- | --- | --- |
| VS-A01 | Defaults/project values/run inputs resolve deterministically and reject unknown types | VS-01, VS-02 |
| VS-A02 | Undeclared or cross-tenant secret resolution fails | VS-03, VS-05 |
| VS-A03 | Management/event/error paths return no secret plaintext | VS-04 |
| VS-A04 | Ciphertext/context swapping fails authentication | VS-06 |
| VS-A05 | Rewrap preserves pinned values; new versions do not silently replace run inputs | VS-07 |
| VS-A06 | Revocation blocks further resolution and records consumer handling | VS-08 |
| VS-A07 | KMS outage preserves encrypted state and reports retryability | VS-09 |
| VS-A08 | Restore decrypts required values; commits/artifacts exclude injected files | VS-10, VS-11 |
| VS-A09 | Deletion reports outstanding references/backup retention | VS-12 |
| VS-A10 | A proxy call succeeds without exposing the provider token to the agent; an authorized script ENV binding receives only its pinned value, while undeclared/foreign/revoked bindings and plaintext Pod-manifest injection fail | VS-02, VS-03, VS-04, VS-05, VS-08, VS-13 |
| VS-A11 | A service and the preparation workload resolve only their pinned secrets; foreign, copied, revoked or cross-resource grants fail on other resources, attempts and publication actions; manifests contain no plaintext and a prior step's copied secret is rejected from later source/output | VS-03, VS-05, VS-08, VS-11, VS-14 |
| VS-A12 | A deploy step receives both its environment credential and explicit registry secret; conflicting bindings fail, rotation does not alter an admitted run, and a new rollback binds authorized versions without modifying historical pins | VS-02, VS-03, VS-08, VS-13, VS-15 |
