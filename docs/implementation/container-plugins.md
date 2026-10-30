# Stateful container plugins and deployment-core retirement

**Status:** proposed implementation plan, requested by the owner on 2026-10-30.
The selected direction is ordinary image-based plugins, generic persistent
state, one shared deployment engine with provider drivers, and retirement of
application Environment/Deployment behavior from the Kodama server. The wire
contracts below are proposed; P0 must freeze them in executable schemas before
implementation. This document does not enable a runtime, change existing
contracts, schedule the factory, or claim provider qualification.

## 1. Baseline, scope and design disposition

| Baseline field | Value |
| --- | --- |
| Repository | `github.com/optimiware/kodama` |
| Inspected revision | `74096e2623168db4502c3f2eff64721377be578e` on `main` |
| Contribution branch | `container-plugins` |
| Current owners | [Execution](../specs/pipeline-execution.md), [delivery and gates](../specs/delivery-and-quality-gates.md), [secrets](../specs/variables-and-secrets.md), [identity](../specs/identity-tenancy-and-rbac.md), [software design](../specs/software-design.md) |
| Wire baseline | [Pipeline v1alpha2](../contracts/pipeline-format-v1alpha2.md), [runtime v2](../contracts/runtime-v2.md), [HTTP](../contracts/http-api.md), [events](../contracts/event-catalog.md) |
| Contribution procedure | [Design conformance](design-conformance.md), [ownership/seams](repository-layout.md), [test layers](development-and-testing.md) |
| Disposition | Intentional contract change. P0 supersedes O-12 and the affected DQ/PX requirements explicitly, preserving their historical IDs and evidence |

Current code has ordinary commands, direct secret bindings, artifacts, agents,
gates and shared-run machinery. It also has Kubernetes-specific application
environments, deployment records, runtime callbacks, a cleanup scheduler, gate
and QA joins, and qualified-script effect reconciliation. These concerns cross
`internal/delivery`, `internal/deliverycleanup`, execution, gates, QA, artifacts,
API/CLI and composition. Deleting only `internal/delivery` is insufficient.

Two additional prerequisites are visible in code:

- `internal/execution/admission.go` requires a work item for public runs;
  source-free admission is currently a deployment-bound cleanup operation.
- `internal/integrations/gitwebhook/adapters/planner.go` refuses a missing work
  item even though a trigger can represent one. A CI-only project needs actual
  project-run admission, not a dummy task hidden in an example.

`run_shared` serializes executable steps. Its code is merged in this baseline,
but installed-profile acceptance remains separately tracked by T-117/T-121.
Package availability is not live deployment evidence.

### 1.1 End state

```text
Kodama server
  projects / work / repositories / pipelines / runs / gates
  identities / credentials / variables / artifacts / generic state
                           |
                 ordinary container attempt
                           |
                 shared deployment engine
                  /          |          \
         drivers/kubernetes  |     drivers/gcp-cloud-run
                    drivers/cloudflare-workers
```

| Core owns | Deployment plugin owns |
| --- | --- |
| Execution, cancellation and runtime-resource cleanup | Application release lifecycle and external-resource cleanup |
| Verified ownership and immutable artifact metadata | Provider-specific interpretation and verification of artifacts |
| Scoped credential delivery | Use of those credentials through provider tools |
| Conditional state storage and artifact retention | Deployment state schema, journal, history and expiry policy |
| Generic human gates and evidence provenance | Provider readiness, drift, rollback and reconciliation |
| Kubernetes as an execution backend | Kubernetes as an application deployment destination |

Application Target, Environment and Deployment entities are absent from the new
core model. `stage` and `production` are recipe configuration or separate
pipeline definitions. `KODAMA_ENV`, tenant execution namespaces, `internal/kube`,
run-resource cleanup and installation Helm charts retain their distinct roles.

### 1.2 Constraints and upstream ownership

- Keep one Go module and the existing execution port. A plugin is not a new step
  kind, server extension process, CRD, provider registry or nested scheduler.
- Ordinary scripts remain valid without the deployment engine or state access.
- Credentials remain in the existing secret system; KV values are not secrets.
- Preserve Platform-owned identity, credentials and provider authority. P0 must
  amend the Kubernetes-only delivery paragraph in [platform integration](../specs/platform-integration.md)
  without authorizing plugins to bypass hosted provider policy. Its reviewed
  upstream baseline remains `optimiweb/platform@05dec0d81a15f9c3bcac826508f0342761343d38`;
  this plan does not claim a newly qualified hosted delegation contract.
- Current execution policy denies the execution cluster's Kubernetes API.
  Kubernetes-driver qualification uses an explicitly authorized destination,
  initially a separate target cluster. Same-cluster deployment needs an explicit
  scoped egress/authority amendment and real network tests, not a broad allow rule.

## 2. Proposed requirements

`CP-*` identifiers belong to this proposed container-plugin contract. P0 registers
the owning specification and preserves these IDs and their acceptance mapping.

| ID | Requirement |
| --- | --- |
| CP-01 | Plugins MUST execute as ordinary digest-pinned container steps through the existing entrypoint and result protocol. |
| CP-02 | Runtime state access MUST be bound to current attempt authority, tenant, project, declared namespace/prefix and read/write mode. |
| CP-03 | State MUST support strongly consistent per-key conditional mutation, bounded storage, audit and safe delete/recreate semantics. |
| CP-04 | Inputs, credentials and artifact descriptors MUST be pinned at their appropriate admission/dispatch boundary; mutable state reads MUST remain distinguishable from immutable invocation inputs. |
| CP-05 | State artifact references MUST protect content from retention and MUST NOT grant access to arbitrary artifact IDs. |
| CP-06 | Deployment drivers MUST share one engine for journal, reservations, release identity, history, receipts and recovery policy. |
| CP-07 | Intent/checkpoints MUST be acknowledged durably before external mutations; an uncertain effect MUST be observed or explicitly reconciled before another mutation. |
| CP-08 | Readiness, rollback and deletion MUST use provider observations and owned identities, never successful logs alone. |
| CP-09 | Project/source-free runs MUST work without application deployment records or artificial work items. |
| CP-10 | Human review and QA MUST bind immutable, ownership-checked evidence; a plugin or KV value MUST NOT grant human approval. |
| CP-11 | Existing delivery invariants MUST be transferred to named plugin/core tests or explicitly retired with a replacement behavior before legacy removal. |
| CP-12 | Cutover MUST preserve historical hashes, outstanding external-effect ownership and artifact retention; no destructive migration runs while legacy dependencies remain. |
| CP-13 | The development and website examples MUST run through composed product interfaces; the website publishing definition contains only build and deploy steps. |
| CP-14 | A non-deployment plugin MUST use the same state/artifact interfaces without provider-specific changes in core. |

## 3. Repository and dependency layout

Proposed paths, created by their implementation owners:

```text
internal/state/                    # sole new core state capability
  postgres/                       # RLS, CAS, quota and retention joins
  memory/                         # same contract over memdb
  http/                           # public read and runtime scoped APIs
contracts/runtime/v3/             # generic context additions; no deployment DTOs
contracts/state/v1/               # state wire schema, fixtures and OpenAPI
pkg/pluginruntime/                # thin HTTP client; no deployment semantics
images/plugins/deploy/
  cmd/deploy/                     # one shared command line
  engine/                         # journal, reservation, releases, recovery
  driver/                         # narrow driver values/ports and contract suite
  drivers/
    kubernetes/                   # mandatory replacement of legacy deploy tools
    cloudflare-workers/           # Wrangler adapter
    gcp-cloud-run/                # Cloud Run adapter
  recipes/                        # versioned defaults/input schemas
  contracts/v1/                   # deploy config, state, receipt, driver protocol
  containers/
    kubernetes.Containerfile
    cloudflare.Containerfile
    gcp.Containerfile
images/plugins/sync-cursor/        # small non-deployment reference plugin
templates/agent-development/
templates/cloudflare-website/
test/integration/container-plugins/
test/integration/deploy-plugins/
test/integration/legacy-delivery-upgrade/
```

All deploy images copy the same engine build, adding their pinned CLI and driver
dependencies. A combined image is possible but not required. Image splitting
must not fork the lifecycle implementation. Drivers can initially be compiled
into the same binary and invoke pinned tools with argument arrays.

Dependency checks must reject imports from core packages into plugin engine or
driver packages, and reject server-private imports by the plugin client/driver.
The engine may use the generic public runtime client and its own contracts.
Provider dependencies remain below `images/plugins/deploy/`. A provider's input
schema is never added to the server's pipeline schema.

## 4. Contract A: ordinary container plugin and runtime

### 4.1 Version boundaries

Propose `kodama/v1alpha3`, runtime context v3 and gate subject v2. They retain
`run_shared` execution semantics but remove application `environment`,
`gate.deploymentId` and deployment-bound cleanup admission from the new format.
Older definitions, contexts, canonical bytes and result hashes remain frozen.
Do not add a new run model merely to add plugin bindings.

P0 freezes executable JSON Schemas, module OpenAPI, canonical examples, Go DTOs,
positive/negative fixtures and compatibility refusal codes. Old controllers
must refuse unsupported context/definition versions before executing a command.
The existing result envelope/hash can remain unchanged if its fields suffice;
plugin output content is independently versioned.

### 4.2 Step declarations

The following are proposed additions to executable steps, including explicitly
authorized agent-profile expansion. There is no `kind: plugin`.

```yaml
state:
  release:
    namespace: deploy
    prefix: website/stage/
    access: read-write
artifactFiles:
  bundle: "${{ steps.build.outputs.bundle }}"
```

- `state` is a bounded alias map (initial ceiling eight bindings). Namespace is
  a lowercase identifier, prefix is a normalized relative key prefix ending in
  `/`, and access is `read` or `read-write`. There are no wildcard grants or
  runtime-supplied tenant/project fields. Literal bindings suffice initially;
  input-driven prefixes can wait for a demonstrated need.
- A declared binding is a request. Admission checks current project authority
  and intersects it with installation ceilings. Use the fixed role model:
  `state:read` follows project evidence-read authority; `state:write` is an
  engineer permission, without implicit admin-to-engineer elevation. Prefixes
  restrict workload disclosure; they do not establish stronger isolation
  between engineers already authorized to author project pipelines.
- Persist the authorized binding with admission. Recheck authority at dispatch
  and every API operation. Agents, services and preparation do not inherit
  another step's state or credentials.
- `artifactFiles` maps a validated filename alias to a whole-scalar reference to
  a declared artifact input or successful ancestor artifact output. It does not
  accept URLs or arbitrary storage keys. OCI metadata is supplied as an artifact
  descriptor rather than downloaded as a file.
- Run-input artifacts resolve at admission. Ancestor outputs resolve only after
  finalization and before consumer dispatch. Freeze ID/digest/size/provenance
  in the consuming attempt context and retain them through its lifetime.
- The entrypoint materializes verified bytes under `/kodama/inputs/<alias>`,
  outside shared scratch. It does not extract archives. Plugins enforce archive
  traversal, symlink, file-count and expanded-size limits before use.

### 4.3 Invocation context and process contract

The entrypoint supplies trusted, non-overridable values:

| Name | Proposed meaning |
| --- | --- |
| `KODAMA_CONTEXT_FILE` | Private, immutable context-v3 JSON file |
| `KODAMA_RUNTIME_URL` | Server-selected runtime origin; no user-directed bearer forwarding |
| `KODAMA_TOKEN_FILE` | Refreshed attempt token; clients reread for each request |
| Existing run/step/attempt variables | Correlation, never user-asserted authority |
| Existing results/outputs/secrets paths | Retain current entrypoint semantics |

The context contains scope, attempt/run/step, stable `actionKey`, deadline,
resolved inputs, image/definition/policy pins, permitted state aliases and
verified artifact descriptors. It contains credential references/versions, not
secret values. A live state value is not silently snapshotted at admission:
reads return revisions and the plugin must record the revisions it relies on.

Credentials use the existing declared ENV/file delivery. The child is trusted
with explicitly delivered secrets; shared scratch is not an isolation boundary
between that run's steps. Cancellation revokes runtime calls and stops the
process tree. Already-issued provider requests can outlive that process.

Use the existing `/kodama/results.json` manifest and declared scalar/artifact
outputs. Retain the current wrapper exit convention: a valid `failed` manifest
is submitted through the entrypoint's documented structured-result path, not
replaced accidentally by an unrelated nonzero-process-exit interpretation.
Any change to that convention is an explicit runtime compatibility change.

A successful `status` command means observation completed; it does not mean the
resource is healthy. `apply` succeeds only after its declared verification.
Unknown external outcome is a failed invocation with a diagnostic receipt and
an unresolved plugin operation. Failed outputs do not become successful inputs
to descendants. Diagnostic artifacts remain inspectable.

### 4.4 Generic artifact reads

Add a runtime artifact-read operation under the attempt's versioned routes.
It authorizes exactly declared input descriptors or explicit references obtained
through an authorized state binding. Do not reuse a human bearer or make every
project artifact readable merely because its ID appears in plugin JSON.

A read request names its authority source (input alias, or state binding/key and
ETag). Atomically validate that reference and acquire an attempt-lifetime hold
before issuing download instructions; a concurrent state update cannot delete
content midway through authorized consumption. Receipts store IDs/digests, not
expiring download URLs or object-storage credentials.

## 5. Contract B: generic persistent state

### 5.1 Model and consistency

Use existing PostgreSQL through `internal/state`, with a memory twin over
`memdb`. No Redis service, raw database credentials or deployment tables are
required. The key is `(tenant, project, namespace, key)`.

Entry metadata: generation, revision, creation/update time, last writer
run/step/attempt, opaque JSON value and explicit artifact references. State
survives runs and image upgrades. Namespace identity is not an image digest.

Initial bounds to freeze in P0: 64 KiB encoded value, 512-byte normalized relative
key, 32 artifact references per entry, bounded project bytes/entry count, and
default/max list page sizes 50/200. Operators may lower policy limits; quota
reservation must be transactional under concurrent writes. Plugins store large
manifests/reports in artifacts, not growing JSON documents indefinitely.

Reads and conditional writes are strongly consistent per key. There is no
multi-key transaction, watch stream, query language or automatic TTL deletion.
List uses an authenticated, scope/filter-bound cursor over keys and is not a
snapshot or reservation; consumers revalidate each entry before mutating it.

### 5.2 HTTP operations

Let `S` mean `/runtime/v3/attempts/{attemptId}/state/{binding}`. The binding alias
resolves server-side to its authorized namespace/prefix.

| Method | Request | Semantics |
| --- | --- | --- |
| `GET S/entries?key=resource` | No write precondition | Read entry and ETag |
| `GET S/entries?prefix=operations/&cursor=...` | Exclusive with `key` | Paginated relative-key list |
| `PUT S/entries?key=resource` | `If-None-Match: *` | Create only |
| `PUT S/entries?key=resource` | `If-Match: <etag>` | Conditional replacement |
| `DELETE S/entries?key=resource` | `If-Match: <etag>` | Conditional deletion |

An ETag encodes generation plus revision. This is an explicit state-API exception
to the existing quoted-integer convention: a deleted/recreated key must never
accept an old writer's ETag. No unconditional writes or `If-Match: *` updates.

Use the existing error envelope: `428 version_required`, `412 version_conflict`,
`400 invalid_key`, `403 state_binding_denied`, nondisclosing `404`, bounded-size
rejection, `429` with `Retry-After` for quotas/rates, and `503` for dependencies.
P0 freezes exact size/quota codes. Reject ambiguous encodings, empty path
segments, `.`/`..`, absolute keys and prefixes outside the binding. Never join a
key to a host filesystem path.

Proposed write body (identifiers/digests are placeholders):

```json
{
  "value": {"schemaVersion": 1, "releaseId": "release_example"},
  "artifactReferences": [
    {"artifactId": "art_bundle", "digest": "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
  ]
}
```

The server adds writer/timestamp/version metadata. It never interprets
`releaseId` or the plugin's schema version. Audit/outbox contain safe metadata,
not complete values. Public CLI/API initially offer scoped read/list for
inspection; normal mutations happen through authorized runs. Administrative
restore/import remains a separately audited tool, not anonymous KV editing.

### 5.3 Transactions, references and replay

- Current runtime-session authorization, state CAS, quota accounting, artifact
  reference changes and audit/outbox join one transaction, following the existing
  run/attempt-to-authority lock order. Cancellation racing a mutation is tested.
- Only artifacts the invocation may already consume, that it produced, or that
  are referenced by permitted state may be attached. The referenced ID/digest
  must agree with verified artifact metadata and ownership.
- Reuse artifact pin/unpin mechanisms with a generic state-reference owner.
  Reference removal does not bypass other gate/run/history pins. No parsing
  arbitrary JSON to discover retention roots.
- After a lost conditional-write response, read and compare operation identity
  and payload. An unprovable acknowledgment remains uncertain. Do not blindly
  retry with a new ETag and overwrite intervening work.
- Restore uses the existing backup/authority-revocation procedure. Plugin state
  may lag provider effects; reconcile before mutating restored resources. A
  restored `idle` value alone is not evidence that no later effect happened.
- Project deletion revokes new access and follows generic state/artifact
  retention. It cannot promise to delete provider resources automatically;
  run cleanup first or retain an explicit outstanding-resource export.

## 6. Contract C: shared deployment engine

### 6.1 Commands and recipe configuration

One executable provides `plan`, `apply`, `status`, `reconcile`, `rollback`,
`cleanup`, `prune` and explicit `import-legacy`. These are plugin commands, not
server endpoints or new core run purposes.

```sh
deploy apply --config /workspace/deploy.yaml --state release --bundle /kodama/inputs/bundle
deploy status --state release --resource resource_example
deploy reconcile --state release --resource resource_example --operation op_example
deploy rollback --state release --resource resource_example --release release_example
deploy cleanup --state release --expired
```

For source-free maintenance, `--resource` identifies a resource in the permitted
binding. Load its retained normalized configuration and recipe identity from
state/artifacts; no source checkout or `/workspace/deploy.yaml` is required.
Credentials are newly authorized bindings of the maintenance definition. They
are not reconstructed from retained secret values. `cleanup --expired` lists
only the granted prefix and revalidates each resource/expiry under CAS; it never
uses an installation-wide provider inventory as deletion authority.

`plan` validates/render inputs and emits an immutable intent artifact without
provider mutation. An optional gate can approve that artifact; `apply --plan`
must consume the exact approved bytes and enforce its expected resource/provider
revision. A changed target, artifact or expectation requires a new plan.

Recipes are versioned input schemas/defaults bound to the plugin image digest.
They select a driver and its options; they are not server-managed profiles. The
same engine accepts, for example:

```yaml
apiVersion: deploy.kodama/v1
recipe: cloudflare-workers
resource:
  accountId: example-account
  worker: website-stage
verification: {path: /health, timeoutSeconds: 120}
retention: {keepReleases: 10}
```

```yaml
apiVersion: deploy.kodama/v1
recipe: kubernetes-manifests
resource:
  cluster: example-target-cluster
  namespace: website-stage
  release: website
credentials: {kubeconfigFile: /kodama/secrets/kubeconfig}
verification: {timeoutSeconds: 180}
retention: {keepReleases: 10}
```

```yaml
apiVersion: deploy.kodama/v1
recipe: gcp-cloud-run
resource:
  project: example-project
  region: europe-west1
  service: website-stage
verification: {path: /health, timeoutSeconds: 180}
retention: {keepReleases: 10}
```

Recipe-specific schemas reject unknown fields. Explicit argv options may supply
fixed resource settings instead of a file; both normalize to one effective JSON
configuration, persisted before effects. No ambient working-directory config,
moving recipe download, implicit shell interpolation or unpinned plugin install.
Application bundles are data: reject custom build hooks and paths that could
execute application-supplied code with deployment credentials.

### 6.2 Identity and versions

| Identity | Rule |
| --- | --- |
| Plugin implementation | Digest-pinned image plus driver-contract version |
| Resource key | Driver-normalized provider/account/cluster/namespace/service identity; immutable once the slot is created |
| Release ID | Hash of verified deployable artifact descriptors and canonical effective application configuration |
| Operation ID | Derived from runtime action key, normalized resource key and command kind; stable across attempts |
| Request fingerprint | Operation kind, resource, desired release, expectations and implementation identity; changed input under one operation ID conflicts |
| Provider revision | Provider-native version/revision or Kubernetes observed object identities |

Operational token rotation does not create a new application release. Deployed
application secret-version references, if supported by a recipe, are part of
application configuration, never raw secret values. Every invocation still pins
the credentials it actually uses; revocation applies to new use.

Hash canonical UTF-8 JSON with byte-sorted object keys, preserved array order,
explicit contract defaults and the repository's integer-only numeric rules.
P0 supplies independent hash vectors. Driver normalization happens before
hashing; a later normalization/default change needs a new recipe/schema version.
Source lineage comes from verified artifact descriptors, not plugin-supplied
commit strings. Work-bound application releases must match the approved
candidate/source set. Rollback explicitly uses retained historical descriptors
rather than claiming they were built from the maintenance run's source.

One external resource must have one owning project and state binding. Normalize
known aliases in the driver; different projects/prefixes do not share a global
lock. Provider RBAC and declared ownership enforce this constraint. A separate
cross-project target registry is not introduced to disguise this limitation.

### 6.3 Plugin-owned records

Within the binding, use `resources/<resourceHash>`,
`operations/<resourceHash>/<operationId>` and
`releases/<resourceHash>/<releaseId>`. Records have a plugin `schemaVersion`.

The authoritative resource document contains:

- immutable normalized provider identity and ownership marker;
- desired release, observed provider revision and last verified healthy release;
- active operation ID/fingerprint, origin run/attempt, checkpoints and outcome;
- expected previous provider/state revision;
- expiry, deletion observations and pending-history publication;
- explicit artifact references for its currently required inputs.

Operation records preserve request, observations and receipt; release records
preserve immutable artifact/configuration descriptors and provider references.
Large manifests and detailed observations are artifacts. Bound journal growth;
fail before an effect when the configured checkpoint budget is exhausted.

Plugin operation states are `pending`, `applying`, `succeeded`, `failed`, `unknown`.
`unknown` can become `succeeded` or `failed` only from sufficient observation;
an operation that definitively failed is not silently restarted. Rollback and
cleanup are new operations. Readiness failure after apply can leave a newer
version serving: retain `observed` separately from `lastHealthy`.

### 6.4 Mutation and history protocol

1. Verify inputs, schema compatibility, resource identity and current authority.
2. Read the authoritative resource; refuse an unresolved competing operation.
3. CAS the resource to reserve the operation and persist complete pre-effect
   intent. A duplicate matching operation is observed; changed input conflicts.
4. Before each external sub-operation, persist a named checkpoint and wait for
   its durable acknowledgment. Missing acknowledgment means do not issue it.
5. Invoke the driver with operation identity, prior checkpoints and expectations.
6. Record provider IDs and observations; verify the intended artifact/version
   and required readiness, not just command exit.
7. CAS the authoritative document to the observed outcome with a pending-history
   payload. Keep the reservation until operation/release history is published.
8. Create history keys conditionally; existing keys must match their immutable
   content. If interrupted, reconciliation finishes this publication from the
   authoritative document before another mutation can start.
9. Clear the active operation by CAS only after publication. Emit the normal
   result and immutable receipt artifact.

This is a recoverable single-key state machine with secondary records, not an
assumed atomic transaction across unrelated KV keys. Pruning never removes an
active/unknown operation, required checkpoint or the inputs of the retained
rollback window. Status can list old records, but releases lacking usable
provider versions/artifacts are explicitly marked unavailable for rollback.

### 6.5 Recovery boundaries

- No automatic reservation expiry/takeover. A lease expiring does not fence an
  old command or a provider request already in flight.
- Provider idempotency keys/CAS are used where available. An absent object from
  an eventually consistent lookup is not proof that a request never happened.
- A new `reconcile` run references the old operation and uses freshly authorized
  bindings. It observes and completes safe bookkeeping; it never blindly calls
  `Apply` again. Unprovable effects stay `unknown` for explicit resolution.
- New-format core execution does not inspect plugin state to establish replay
  safety. Preserve the command-begin rule: only proven pre-command failures may
  automatically retry. A killed/uncertain begun command fails without automatic
  replay; a separately authorized run resolves its plugin operation.
- Core workload stop/removal and source-publication reconciliation retain their
  own fences. Provider uncertainty does not require the core to keep stopped
  compute resources alive indefinitely. Never report a late reconciliation as
  success of the historical failed/cancelled attempt.
- New cleanup/reconciliation runs may use a newly rotated authorized credential
  for the same recorded resource. They do not resurrect a revoked old token or
  let mutable current settings retarget the resource. This intentionally replaces
  legacy cleanup's mandatory use of a retained environment credential version.
- State-schema upgrades are explicit, CAS-protected migrations with retained
  backups. Older images refuse newer schemas before effects. Resolve outstanding
  operations with a compatible pinned implementation before migrating them.

### 6.6 Receipt contract

Version the deployment receipt separately from runtime results. Required facts:
schema version, operation ID/kind, resource identity, release/artifact/config
digests, plugin image, source lineage, provider revisions, outcome/readiness,
observation timestamps and originating run/attempt. URLs and detailed resource
lists are optional, bounded fields. No token values or expiring download URLs.

The core verifies producer/artifact provenance. It does not independently
certify a plugin's provider claims. Gates and QA consume this immutable receipt,
not a mutable KV field labelled `approved` or `ready`.

## 7. Contract D: deployment drivers

### 7.1 Small interface and ownership

The shared engine owns state access, reservations, deadlines, history, artifact
retention, retries and result emission. Drivers own provider validation,
mutation and observation. They do not write core KV directly or schedule runs.

| Method | Required behavior |
| --- | --- |
| `Identify(config)` | Validate inputs and return normalized resource identity and capabilities; no provider mutation |
| `Apply(request, journal)` | Apply immutable desired state, using acknowledged checkpoints and provider preconditions |
| `Observe(request)` | Read-only provider observation of the particular operation/resource/version |
| `Destroy(request, journal)` | Delete only recorded owned identities, with provider preconditions |
| `Rollback(request, journal)` | Optional native rollback; otherwise the engine can use an explicitly supported reapply-previous-artifact strategy |

`Identify` reports accepted artifact forms, rollback strategy
(`native`, `reapply`, `unsupported`), and supported observations/preconditions.
These capabilities live inside the plugin. They are not execution-backend
capabilities and are not advertised by the server as provider guarantees.

`request` carries protocol version, request/operation ID, deadline, desired
release, normalized configuration, verified artifact descriptors/local paths,
previous inventory, expected remote revision and retained checkpoints.
Credentials are supplied through declared environment variables/private files,
not embedded in journal or protocol JSON.

P0 freezes these typed driver values; fields marked nullable are present with
`null`, not omitted:

| Value | Required fields and constraints |
| --- | --- |
| `IdentifyRequest` | Recipe ID/version and effective configuration object; no mutation authority |
| `Identity` | Driver ID/version, canonical resource key, normalized configuration, accepted artifact forms, rollback strategy and observation/precondition capabilities |
| `Operation` | ID, command kind, immutable request fingerprint and original action key; current invocation identity remains separately attributable |
| `DesiredRelease` | Release ID, configuration digest, verified artifact descriptors and normalized provider settings; nullable for lookup/deletion without desired content |
| `ArtifactDescriptor` | Artifact ID, digest, kind, producer/source lineage; verified local path for a file or digest-addressed registry reference for OCI |
| `DriverRequest` | Deadline, identity, nullable operation/desired release, previous inventory, expected provider revision and checkpoints; protocol/request ID/method come from the outer request envelope |
| `MutationResult` | Effect certainty (`none`, `possible`, `confirmed`), nullable provider operation/version IDs, observed resource identities and bounded details; always followed by observation before success |
| `Observation` | State enum, observation time, nullable observed release/provider revision, owned resource identities, verification checks and bounded diagnostic code |
| `DriverError` | Stable code, effect certainty, safe message and optional bounded retry-after for observation; no permission to replay a mutation |

Every method respects the supplied deadline and cancellation. File paths must
refer to verified inputs or plugin-private scratch. `Observe` cannot emit a
mutation-intent checkpoint or change the provider. A driver implementation may
perform its own bounded read retries, but mutation retry requires a documented
provider idempotency/precondition guarantee and the same persisted operation.

`Observe` returns one of `absent`, `progressing`, `ready`, `unhealthy`, `conflict`
or `unknown`, plus observed version/artifact identity, owned resources and a
timestamp. `ready` requires matching desired identity and all recipe checks.
An accepted mutation is not a ready resource. A mutation response records
provider operation/version IDs and whether an effect is `none`, `possible` or
`confirmed`; partial application must never be reported as `none`.

Errors distinguish invalid input, permission denial, conflicting resource,
unsupported capability, observation unavailability and uncertain mutation. A
driver's transient error classification never by itself authorizes replay.

An expected remote revision is enforced atomically only when the provider offers
that precondition. Checking before and after a CLI call is not remote CAS and
cannot fence an out-of-band administrator. A recipe must disclose that capability
and refuse an action requiring a stronger guarantee than the driver provides.
The engine's state reservation serializes cooperating plugin operations only.

### 7.2 Future executable-driver protocol

First ship compiled drivers sharing the interface above. Freeze a matching
`kodama.deploy-driver/v1` protocol so a future custom executable can implement
the same operations without copying the engine. P0 publishes schemas/fixtures;
the external-executable launcher can follow the three built-in drivers.

Protocol rules:

1. The engine starts the recipe's image-packaged executable by argv; it does not
   fetch code, evaluate shell strings or start nested containers.
2. One UTF-8 JSON request line is written to stdin. Stdout carries only bounded
   JSON-line protocol frames; stderr is diagnostic logging subject to redaction.
3. A driver emits `checkpoint` frames with request ID, monotonic sequence, stable
   checkpoint name and bounded data. The engine persists and returns an `ack`
   frame on stdin. The driver MUST wait for that acknowledgment before the
   associated external mutation. Repeated checkpoint sequence/data is idempotent;
   conflicting data is rejected. Names and payloads are schema-validated.
4. Exactly one final `result` or `error` frame ends the call. Request IDs must
   match; unsupported versions, malformed/oversized frames, premature exit or
   missing result fail the protocol. After mutation may have started, these
   failures mean uncertain effects, not evidence of no action.
5. The engine owns timeout/signal propagation and process-tree termination.
   Driver helpers share the journal client; drivers never implement KV retries.

Request methods are `identify`, `apply`, `observe`, `destroy`, `rollback`.
`identify` uses `IdentifyRequest`; all others use `DriverRequest` as `payload`.
Each final frame includes the same protocol/request ID and a method-appropriate
`payload`; fields outside the published schema are rejected. Stdout may contain
zero or more checkpoint frames followed by one final frame, with no extra text.
Use a 256 KiB frame ceiling, a bounded total frame count, and an acknowledgment
deadline no later than the attempt deadline. These transport limits are separate
from the smaller durable KV-value limit.

Illustrative envelopes, with abbreviated payload data rather than executable
schema fixtures. Each is transmitted as one line:

```json
{"protocol":"kodama.deploy-driver/v1","type":"request","requestId":"identify_example","method":"identify","payload":{"recipe":"cloudflare-workers","recipeVersion":1,"configuration":{"accountId":"example-account","worker":"website-stage"}}}
```

```json
{"protocol":"kodama.deploy-driver/v1","type":"checkpoint","requestId":"apply_example","sequence":1,"name":"upload-intent","data":{"operationMarker":"op_example"}}
```

```json
{"protocol":"kodama.deploy-driver/v1","type":"ack","requestId":"apply_example","sequence":1,"revision":"generation:12"}
```

The checkpoint/ack pair illustrates a separate mutating request; an `identify`
call cannot issue an upload checkpoint. Matching and ordering are enforced
across every frame of one call.

The driver is trusted code in the plugin's security domain, not a sandboxed
subtenant. Suppressing runtime credentials from its environment is defense in
depth, not a claim that a same-user process cannot read mounted files.

### 7.3 Kubernetes driver: mandatory migration target

Implement under `drivers/kubernetes/`, using the shared engine and an ordinary
namespace-scoped kubeconfig file. It is a first-class driver in this delivery,
not a deferred replacement.

- Normalize configured cluster authority, namespace and release name. Validate
  the kubeconfig endpoint/context against the pinned configuration. Keep one
  stable operator-selected cluster identity across credential rotation; do not
  infer cluster identity from a mutable local context name alone.
- Render a bounded manifest inventory; reject cluster-scoped resources,
  foreign namespaces, `generateName`, unknown unsupported kinds and unpinned
  workload images before apply. Namespace creation is outside this driver.
- Verify the main OCI digest against core artifact provenance and bind every
  workload image to it or an explicit digest-pinned auxiliary allow-list. The
  main image must actually remain deployed; healthy sidecars do not prove it.
- Put ownership/resource/operation/release markers on objects and preserve
  Kubernetes UIDs and observed resourceVersions in checkpoints/receipts.
- Observe structured workload status and generation, not kubectl text. Initial
  readiness handlers cover the kinds the legacy tools qualify; other kinds
  require explicit recipe verification before they can be reported ready.
- Apply must refuse unmanaged/conflicting existing objects unless an explicit,
  auditable import/adoption operation proved ownership. Avoid silent force
  takeover. Updating a release may delete old manifest objects only from the
  previously recorded owned inventory under the same deletion guards.
- Delete with UID and resourceVersion preconditions after fresh ownership
  observation. A same-named replacement, changed owner or recreated object
  survives. A benign resourceVersion change requires a fresh observation.
- Rollback reapplies the retained previous manifests/configuration and image
  descriptors, then verifies the resulting resource set. Handle resources added
  by the superseded release explicitly; do not simply assume the old manifest
  deletes them. Storage/data rollback is not implied.
- Port useful rendering/readiness/ownership logic from
  `images/deployment-tools/scripts/`, but remove its delivery callback client.
  The driver has no imports from `internal/delivery` and no `/deployments` calls.
- Use a separate kind target for local driver evidence. Runtime Kubernetes
  security, chart and run-resource tests remain tests of Kodama infrastructure.

### 7.4 Cloudflare Workers driver

Implement under `drivers/cloudflare-workers/` with pinned Wrangler. Initially
support ES-module Worker code and static assets, including a qualified OpenNext
bundle recipe for the website example.

- Validate the requested account and Worker identity before mutation; reject
  conflicting account/name settings in bundled configuration.
- First creation uses `wrangler deploy`. Subsequent versioned releases can use
  `wrangler versions upload` then `wrangler versions deploy`; persist the uploaded
  version before activation. Correlate provider metadata with the operation.
- Routes/domains/triggers are separate provider effects and need their own
  checkpoints when enabled. Start the example with `workers.dev`; NS1 or other
  external DNS management is a separately scoped recipe/plugin, not implicit.
- Observe provider version and an application build marker. A successful HTTP
  response from an old version is not successful deployment of the new bundle.
- Native rollback selects a recorded available provider version. Cloudflare
  version limits, deleted bindings and Durable Object migrations can make it
  unavailable. Report that explicitly; no automatic data/schema rollback.
- Deletion is bounded to the recorded owned Worker and explicitly owned routing
  resources. Never delete shared KV/R2/D1 resources simply because they are bound.

Provider references reviewed 2026-10-30:
[deployment management](https://developers.cloudflare.com/workers/versions-and-deployments/deployment-management/),
[versions](https://developers.cloudflare.com/workers/versions-and-deployments/),
[rollbacks](https://developers.cloudflare.com/workers/versions-and-deployments/rollbacks/).
Pin and test actual command/output behavior; documentation is not qualification.

### 7.5 GCP Cloud Run driver

Implement under `drivers/gcp-cloud-run/`. Consume a verified OCI image digest;
project, region and service are ordinary recipe inputs. Explicitly authenticate
gcloud with the authorized credential form. Prefer workload federation where
the installation supports it; do not assume setting an application-default
credential variable alone authenticates the CLI.

Persist operation/revision identifiers, verify readiness and traffic routing to
the desired revision, and record the previously serving revision. Native
rollback switches to an available recorded revision and verifies it; optional
reapply of a retained image/config is a distinct recorded operation. Cleanup
deletes only the owned service, not project-wide dependencies. Use structured
API/CLI output and provider preconditions where supported. Contract tests with
controlled responses precede separately scoped live GCP qualification.

## 8. Generic project runs, review and future uses

### 8.1 Project and maintenance admission

Add project-level run admission with a definition, origin, optional work item,
optional pinned source set, inputs and an idempotency key. Reuse the source/origin
direction of [internal Git](internal-git-service.md), T-126/T-131/T-132; do not
create a competing repository or trigger model. Work-item origins still require
the specification/content revision and retain business WIP rules. Source-backed
CI origins verify exact project repository commits without task workspaces;
source-free maintenance has no checkout grant. Project-only runs are initially
read/no-publication; grants to update Git refs remain separately authorized.

Every origin consumes runtime quotas even when it consumes no business WIP.
Source/no-source and publication authority determine workspace behavior, not a
deployment-specific `purpose=cleanup` flag. Source-free container runs can invoke
status, reconciliation, rollback and cleanup without inventing a business task.

Push redelivery deduplicates by trusted trigger/event/ref. Recheck the trigger
installer's current authority. The initial recipe serializes same-resource
operations and reports conflicts; this is not an automatic latest-push-wins
guarantee. Explicit supersession ordering is a separate generic trigger policy.

Automatic expiry is a periodic ordinary maintenance run. Ship an external
scheduler/CronJob example calling public run admission with a supported scoped
identity and a stable tick idempotency key. Its credential onboarding is an
explicit prerequisite; do not seed identity tables or pretend an unavailable
service-account flow exists. Manual maintenance remains usable independently.

### 8.2 Gates and QA

Gate subject v2 removes deployment IDs/configuration joins and includes source
identity, optional work-item revision, definition digest, effective execution
snapshot digest and sorted immutable evidence IDs/digests. The snapshot digest
covers effective inputs, authorized state bindings, profile/tool pins and secret
version references, never secret values. Resolve referenced ancestor artifacts
before constructing the subject; their final descriptors must be covered.

Gates keep current human authority, expiry, subject matching and source/content
invalidation. A plugin cannot approve a gate. Approval of a plan/receipt is
approval of that immutable evidence, not a guarantee that a remote resource
stays unchanged. A protected apply verifies its approved plan's expected remote
state and refuses drift before effects.

Replace `internal/qa` deployment lookups with scoped receipt/evidence references
and their producer/source provenance. QA reports identify the inspected receipt,
observed version and time. QA may file defects, not complete work. The core need
not parse a Kubernetes resource or Cloudflare version to validate evidence
ownership. Direct provider truth remains the responsibility of the qualified
plugin/QA observation, not a synthetic core deployment row.

### 8.3 Non-deployment reference plugin

Ship `sync-cursor`: fetch a fixture service's next page, produce a retained
artifact, and CAS a cursor/document with that artifact reference. A second run
resumes from the checkpoint. It exercises the same runtime/state/retention
contracts without deployment vocabulary. It does not claim exactly-once
external writes merely because it checkpoints a cursor. Future backup,
analysis-baseline and release-note plugins can use the same primitives.

## 9. Runnable project examples

These complete example definitions use proposed syntax. Repeated-character
digests and `registry.example.invalid` are placeholders. P7 publishes qualified
images and bootstrap manifests; these snippets are not executable on current
main. Both examples use normal registration/run APIs and public clients.

### 9.1 Development project with agents and review

Deliver `templates/agent-development/` with a small application, acceptance
criteria, pinned design baseline, bootstrap instructions, four agent profiles,
test script and this pipeline. Profiles emit the declared paths, consume their
input artifacts and return failure for blocking findings. Their image/model/tool
bindings are explicit and qualified; a deterministic harness can prove flow but
does not substitute for live-agent acceptance.

```yaml
apiVersion: kodama/v1alpha3
kind: Pipeline
metadata: {name: develop-and-review}
spec:
  timeoutSeconds: 14400
  steps:
    - id: plan
      kind: agent
      agent: {profile: planner-v1}
      workspace: {access: read}
      timeoutSeconds: 900
      outputs:
        plan: {type: artifact, path: /kodama/outputs/plan.json}
    - id: implement
      kind: agent
      needs: [plan]
      agent: {profile: implementer-v1}
      workspace: {access: write}
      artifactFiles:
        plan: "${{ steps.plan.outputs.plan }}"
      timeoutSeconds: 2400
    - id: test
      kind: container
      needs: [implement]
      image: registry.example.invalid/project-tools@sha256:1111111111111111111111111111111111111111111111111111111111111111
      command: ["/bin/sh", "-ec"]
      args: ["cd /workspace && ./ci/test.sh /kodama/outputs/tests.json"]
      timeoutSeconds: 1200
      outputs:
        report: {type: artifact, path: /kodama/outputs/tests.json}
    - id: code-review
      kind: agent
      needs: [test]
      agent: {profile: code-reviewer-v1}
      workspace: {access: read}
      artifactFiles:
        tests: "${{ steps.test.outputs.report }}"
      timeoutSeconds: 900
      outputs:
        report: {type: artifact, path: /kodama/outputs/review.json}
        conformance: {type: artifact, path: /kodama/outputs/conformance.json}
    - id: security-review
      kind: agent
      needs: [code-review]
      agent: {profile: security-reviewer-v1}
      workspace: {access: read}
      artifactFiles:
        review: "${{ steps.code-review.outputs.report }}"
      timeoutSeconds: 900
      outputs:
        report: {type: artifact, path: /kodama/outputs/security.json}
    - id: approve
      kind: gate
      needs: [security-review]
      gate:
        evidence:
          - "${{ steps.test.outputs.report }}"
          - "${{ steps.code-review.outputs.report }}"
          - "${{ steps.code-review.outputs.conformance }}"
          - "${{ steps.security-review.outputs.report }}"
```

`run_shared` serializes these steps. Read access prohibits publication but does
not make scratch immutable; reviewers write reports outside source, and evidence
must bind the clean published candidate. Changed tracked/untracked source cannot
silently retain an earlier approval. Required conformance output names/profile
checks must match the owning gate contract. Rejection/needs-input creates a new
run after revised intent; agents never perform the human decision/completion.
Separate-run parallel agents can later use the existing Git branch design.

### 9.2 Website project: two-step build and Cloudflare deploy

Deliver `templates/cloudflare-website/` with a minimal application, pinned
OpenNext/Wrangler-compatible build, package lock, build script, publish pipeline,
and one-step `status`, `rollback`, `reconcile`, `cleanup` maintenance definitions.
The publish pipeline has exactly two steps and requires no work item.

```yaml
apiVersion: kodama/v1alpha3
kind: Pipeline
metadata: {name: website-stage}
spec:
  timeoutSeconds: 2400
  inputs:
    account_id: {type: string, required: true}
  steps:
    - id: build
      kind: container
      image: registry.example.invalid/website-builder@sha256:2222222222222222222222222222222222222222222222222222222222222222
      command: ["/bin/sh", "-ec"]
      args: ["cd /workspace && ./ci/build-cloudflare.sh"]
      timeoutSeconds: 1800
      outputs:
        bundle: {type: artifact, path: /kodama/outputs/worker.tar.gz}
    - id: deploy
      kind: container
      needs: [build]
      image: registry.example.invalid/deploy-cloudflare@sha256:3333333333333333333333333333333333333333333333333333333333333333
      artifactFiles:
        bundle: "${{ steps.build.outputs.bundle }}"
      state:
        release:
          namespace: deploy
          prefix: website/stage/
          access: read-write
      env:
        CLOUDFLARE_API_TOKEN: {secretRef: "secret://project/cloudflare-stage"}
      command: ["/usr/local/bin/deploy"]
      args:
        - apply
        - --recipe
        - cloudflare-workers
        - --account-id
        - "${{ inputs.account_id }}"
        - --worker
        - website-stage
        - --state
        - release
        - --bundle
        - /kodama/inputs/bundle
      timeoutSeconds: 600
      outputs:
        release_id: {type: string}
        provider_version: {type: string}
        url: {type: string}
        receipt: {type: artifact, path: /kodama/outputs/deployment.json}
```

The builder runs locked dependency installation/build, then produces a validated
archive with a manifest, Worker modules, static assets, nonsecret configuration
and a served build identifier. It needs no Cloudflare credential. The deploy
step consumes the finalized artifact rather than trusting mutable scratch,
normalizes configuration, deploys and verifies within that step. It does not
rerun the application build with the token.

Stage/production are separate immutable definitions with distinct credentials
and state bindings. No pipeline input can turn a stage credential into production
authority. Git push integration initially uses an authenticated supported native
hook or manual run API; GitHub webhook automation requires its explicitly
qualified adapter, not an undocumented assumption.

Maintenance definitions bind the same state prefix, appropriate current
credential and a pinned engine image. `status` uses state read access; mutating
commands use read-write. They take recorded release/operation IDs, are source-free,
and can run after the original work/run has finished. Rollback does not rebuild.

## 10. Decommission the hardcoded Kubernetes delivery model

### 10.1 Removal and replacement inventory

| Existing surface | Replacement/disposition |
| --- | --- |
| `internal/delivery/` entities, services, persistence and HTTP handlers | Shared deploy engine, generic state API and immutable receipts; remove active package after cutover |
| `internal/deliverycleanup/` scheduler/bindings | Ordinary maintenance runs plus an explicit generic external trigger |
| `internal/execution/environment.go`, `adapters/environments.go`, `adapters/txdelivery.go` | Generic effective input/secret/state bindings; remove environment admission joins |
| Environment branch in `internal/execution/adapters/secretroute.go` and `internal/app/pinned_context.go` | One direct, declared secret-resolution path for new-format attempts |
| `PrepareCleanup`, `CleanupCommand`, deployment-bound run fields/constraints | Generic project/source-free admission with current authority and explicit inputs |
| `internal/app/delivery.go`, delivery joins/configuration and tool qualifications | Separate retained agent/gate wiring; remove delivery-owned effect reconciliation and cleanup workers |
| `deployment.call` runtime action and callbacks | Scoped `state.read`, `state.write` and artifact-read operations; never a public arbitrary-provider endpoint |
| `internal/gates` deployment snapshot adapter/subject fields | Versioned generic evidence subject and applicable immutable invocation inputs |
| `internal/qa/adapters/deployments.go` and `DeploymentFacts` | Receipt/evidence producer and source provenance |
| Deployment-specific artifact protection | State-reference retention owner; transfer pins before removing old owners |
| `api/delivery.yaml`, generated clients/CLI/help and app route capabilities | Retire active Environment/Deployment endpoints; regenerate public contracts and capability reporting |
| Deployment event consumers, metrics, ChatOps links, QA/profile schemas | Generic run/evidence observations; retain historical event decoding without live deployment lookups |
| `environments`, `environment_snapshots`, `deployments`, `deployment_history` and referencing SQL | Audited export/import and appended retirement migrations; preserve needed immutable history |
| `images/deployment-tools/` | Port Kubernetes provider behavior into `drivers/kubernetes/`; retire callback scripts/image after tests migrate |
| `templates/go-postgres` delivery commands and acceptance fixtures | Use the generic deploy CLI with Kubernetes recipe and scoped state |
| Glossary, DQ/PX/VS contracts, examples and status pages | Mark intentional replacements and retired semantics; no stale claim of active core deployment support |

Inventory all fields/FKs/check constraints/permissions, not only Go imports.
Audit execution `purpose`, runtime action arrays, artifact pin-owner checks,
gate canonical subjects, QA reports, API snapshots, acceptance manifests,
development scripts and chart delivery-tool settings. Keep unrelated message
delivery, secret delivery, runtime Kubernetes tests and installation configuration.

### 10.2 Compatibility and live resource transfer

Use a bounded transition with one active owner of each resource:

1. Ship new state/runtime/engine and qualified Kubernetes driver alongside legacy
   paths. A feature switch governs new-format admission; production refuses
   missing bindings rather than installing permissive fallbacks.
2. Inventory nonterminal runs, pending gates, deployments, cleanup requests,
   environments, snapshots and retention roots. Freeze new legacy admission and
   edits. Existing runs drain under their original context/authority semantics.
3. Settle unknown legacy effects using the legacy implementation. An unresolvable
   resource blocks its transfer; do not mark it failed merely to satisfy a migration.
4. Export history with original IDs, canonical bytes/hashes, resource UIDs,
   configuration, provenance, cleanup state and artifact references. Exports
   contain secret references, never decrypted values. Verify completeness and
   digests under a retained audit/retention policy.
5. For live resources, run explicit `import-legacy` using the Kubernetes driver:
   verify live endpoint/namespace, annotations, UIDs and artifact identity; create
   shared-engine records and generic retention references idempotently; preserve
   legacy IDs as historical metadata. Do not redeploy or claim uncertain state.
6. Transfer cleanup ownership only after imported state/reference checks succeed.
   Disable the legacy sweeper for transferred resources. At no point may two
   sweepers mutate the same resource. Resources not imported must be explicitly
   cleaned up or retained as outstanding dependencies.
7. Re-register definitions/profiles against the new format. Pending legacy gates
   finish under old semantics or are explicitly invalidated and replaced; their
   decision/subject bytes are never rewritten into a new approval.
8. Remove active legacy callbacks/services/workers, then referencing constraints
   and tables through appended migrations. A migration preflight refuses live
   legacy dependencies, untransferred pins or incomplete exports. Keep minimal
   read-only historical decoders/archives, not an active old deployment engine.
9. Regenerate API/CLI docs and tests. Publish the breaking release/cutover notes.
   Legacy mutations return a documented retired-capability response during the
   transition; final route removal is explicit. History remains available through
   preserved runs/artifacts and the documented archive path.

Do not edit `001_schema.sql`/`002_grants.sql` to hide applied objects. Append
creation/retirement contributions and update the migration manifest through the
schema owner. Fresh install and upgrade must reach the same active schema.

Before destructive retirement, rollback means disabling new admission and
restoring the previously selected owners after settlement. After retirement,
rollback requires a tested backup/export restore and provider reconciliation;
restarting an old binary against the new schema is not supported.

### 10.3 Explicit semantic changes

Record these in P0 rather than calling every old test obsolete:

- Delivery records no longer establish core replay safety. Begun/uncertain
  plugin operations recover in a separate run; there is no automatic retry
  justified by a missing or settled legacy deployment row.
- A fresh cleanup run uses current authorized credentials for the recorded
  resource; it need not reuse a revoked/expired old environment credential.
- Legacy `cleanup_requested`/deployment-bound callback permission disappears.
  Explicit authorized maintenance definitions, declared state scope and provider
  credentials are the new authority boundary. An arbitrary script already given
  a provider credential is not prevented from using that credential by KV policy.
- The server does not independently validate provider readiness. Artifact
  provenance is core-verified; resource identity/readiness is tested plugin logic.
- A human approval binds immutable evidence and expected action inputs. Current
  remote-state checks move into the plugin's apply/verification protocol.

## 11. Test migration: preserve behavior, replace the execution path

Adapt tests that currently instantiate delivery services, call `/deployments`,
create environments or run `apply.sh`/`delete.sh`. Their replacement must run the
shared deploy engine plus Kubernetes driver through ordinary admitted steps.
Do not merely rename fixtures while keeping a private legacy callback server.

| Existing suite/assertion | Replacement owner/path | Required disposition |
| --- | --- | --- |
| `internal/delivery/*_test.go` and `internal/delivery/integration/` | `internal/state` contract suite; deploy engine/driver tests; upgrade suite | Transfer scope/CAS/provenance/lifecycle tests; retain old schema/hash cases only as explicit upgrade fixtures |
| `internal/deliverycleanup/*_test.go` | Generic project-run/trigger tests and deploy engine cleanup tests | Preserve duplicate tick/run admission, resource scoping, expiry and operation recovery; remove hardcoded cleanup scheduler assertions |
| `internal/execution/adapters/environments*_test.go`, `txdelivery_test.go`, `test/integration/environment-pinning-contract/` | Generic binding/admission tests | Verify inputs, credential versions, state grants and rollback/revocation instead of environment snapshots |
| `test/integration/environment-pinning-runtime/` | `test/integration/container-plugins/bindings/` | Preserve edit/rotation/restart pinning, RLS, cross-project denial and all-or-nothing admission; new cleanup credential semantics get separate tests |
| `test/integration/delivery/stack/` | `test/integration/deploy-plugins/stack/` | Real state/runtime/secret/artifact/result composition; remove `txdelivery`, legacy callback and deployment-effect stand-ins |
| `Test_apply_ack_loss` | Shared engine fault suite plus real Kubernetes plugin suite | Lose intent/checkpoint/result responses and kill the container; no blind replay; reconcile same operation without resetting live objects |
| `Test_duplicate_deployment_key` | Engine concurrent operation/CAS suite | Same operation/input observes one intent; changed input conflicts; reservation and history survive restart |
| `Test_wrong_image_source`, `Test_deployment_provenance_join` | Generic artifact binding + Kubernetes image verification | Wrong owner/source/digest or unaccepted artifact cannot deploy; rotation does not alter an already admitted credential binding |
| `Test_deployment_cleanup_join`, `Test_cleanup_after_work_done` | Project maintenance + Kubernetes driver | Expired resource cleaned without a work item/WIP; retained identity/history, current scoped credential, no bystander deletion |
| `Test_cleanup_scope`, `Test_cleanup_duplicate_claim` | State-prefix authorization, trigger deduplication and engine cleanup concurrency | Foreign prefix/resource/input denied; one owner; uncertain cleanup never becomes an automatic second deleter |
| `test/integration/deployment-tools/` | `test/integration/deploy-plugins/kubernetes/` and image/driver contract tests | Port actual restrictions, manifest/image checks, annotations, observation and conditional-delete race tests |
| `internal/app/delivery*_test.go`, `m2_joins_delivery_test.go`, `runshared_v2_test.go` | App generic-state/artifact/gate wiring tests plus legacy compatibility fixtures | Prove real constructor/transaction wiring; preserve old v2 vectors separately rather than rewriting them |
| `internal/gates/adapters/deployments*_test.go`, subject fixtures | Subject-v2/evidence tests plus frozen subject-v1 compatibility | Keep stale approval, changed candidate/config, role removal and exact historical hashes |
| `internal/qa/integration/wrong_deployment_test.go`, QA fixtures | Wrong-receipt/evidence/source tests | Foreign or stale evidence cannot file a misleading defect; plugin claims are not human decisions |
| `test/e2e/m2/preview_test.go`, `example_test.go`, `kit/erp*.go` | M2 ordinary plugin preview flow | Agent/review/build/preview uses a real admitted plugin and receipt, not a core deployment service |
| `test/e2e/m3/expiry_test.go`, `business_loop_test.go`, `kit/stage.go` | M3 receipt review, maintenance and QA flow | Same artifact promotion, explicit human acceptance, receipt-bound QA, expiry cleanup and notifications |
| `test/e2e/kube/helpers_test.go` | Separate execution fixtures from application deployment fixture | Application deployment helpers use the Kubernetes driver; runtime Job/chart/security assertions stay core |
| `templates/go-postgres/acceptance/`, `app/internal/delivery/`, `app/test/realdelivery/` | Template wrapper invoking deploy plugin and migrated acceptance | Preserve build-to-image provenance, promotion, e2e URL, recovery and cleanup without `internal/delivery` imports |
| CLI/client/OpenAPI/ChatOps/metrics fixtures referencing delivery | Generic state/run/evidence views and explicit retired-interface fixtures | No active lookup depends on deleted delivery tables or routes |

### 11.1 Kubernetes assertions that must survive verbatim in meaning

- Namespace-scoped credentials fail on other namespaces and cluster resources.
- Intent exists before the first apply; a lost intent acknowledgment causes no
  unconfirmed mutation. No provider call while state persistence is unavailable.
- A live primary image must equal the verified digest, including after replay
  or reconciliation; permitted auxiliary images do not substitute for it.
- `kubectl` printing `created` for an unschedulable workload never reports ready.
- Unknown/partial effects are observed, not repaired through a hidden reapply.
- A resource with another owner, a new UID, or changed ownership between read
  and delete survives; UID/resourceVersion conditions are enforced by real API.
- A benign resourceVersion change can converge after fresh observation.
- Cleanup preserves history and touches only the test's recorded resources.
- Image restrictions, token redaction and process cancellation remain tested
  under the selected runtime policy; no fallback to privileged execution.
- Teardown removes only workloads/resources created by that fixture, not every
  container sharing the plugin image.

### 11.2 Evidence levels

Shared engine tests use an injected deterministic driver for faults and exact
call counts. Every real driver runs the same driver contract suite plus
provider-specific tests. State memory/PostgreSQL adapters share one contract
suite; real SQL tests prove RLS, lock order, rollback, quota and reference joins.

The assembled Kubernetes plugin test uses PostgreSQL, artifacts, real runtime
authority/entrypoint, the actual plugin image and a real kind target. Local
Podman/shared-resource availability must be qualified or the equivalent suite
must use the real Kubernetes runtime; a missing local port is a prerequisite,
not a permissive test wrapper. Cloudflare/GCP use controlled protocol tests and
bounded real account/project checks before claiming provider support. Missing
required infrastructure is reported blocked/failed, never passing by skip.

Maintain an old-test-to-new-acceptance ledger in the migration handoff. Each old
assertion is `preserved`, `replaced` (with the approved new rule) or `retired`
(with rationale). Delete an old suite only after its replacements pass. Update
milestone runners and evidence documentation so removal cannot silently shrink
the tested product.

## 12. Acceptance and traceability

| ID | Scenario | Requirements |
| --- | --- | --- |
| CP-A01 | Plain command and stateful container use the same entrypoint; incompatible versions are refused before command begin | CP-01, CP-04 |
| CP-A02 | Foreign tenant/project/prefix, read-only, revoked and service-subject state calls fail without disclosure/mutation | CP-02, CP-03 |
| CP-A03 | Concurrent create/update/delete, lost acknowledgment and delete/recreate preserve one valid revision; quota/audit failure rolls back all changes | CP-03 |
| CP-A04 | Inputs/credential pins survive edit/restart; ancestor artifact binding waits for successful finalization; archive escape/tampered downloads fail | CP-04 |
| CP-A05 | Concurrent state reference updates and artifact GC retain usable inputs; rollback works after original scratch removal | CP-05, CP-08 |
| CP-A06 | Kubernetes, Cloudflare and GCP adapters use the same engine/contract; no lifecycle fork or core provider import | CP-06 |
| CP-A07 | Crash before effect, after provider acceptance, during checkpoint/history publication and after receipt produces no blind mutation replay | CP-07 |
| CP-A08 | Kubernetes live-image/readiness and UID/resourceVersion races preserve the assertions in section 11.1 | CP-08, CP-11 |
| CP-A09 | Cloudflare create A, release B, observe B, rollback A and cleanup succeed; unsupported bindings/version rollback fails explicitly | CP-08 |
| CP-A10 | GCP image A/B, observed traffic, rollback and owned-service cleanup pass against a named real project before support is advertised | CP-08 |
| CP-A11 | Public CI/source-free maintenance runs need no work item/deployment; duplicate trigger delivery admits one run and respects current authority/quotas | CP-09 |
| CP-A12 | Agent cannot approve; changed source/input/config or revoked reviewer invalidates progression; foreign/stale receipt fails QA evidence checks | CP-10 |
| CP-A13 | Both example projects execute through public registration/admission and real runtime contracts; website definition has exactly two steps | CP-13 |
| CP-A14 | Two sync-cursor runs resume state and retain artifacts without deployment-specific core changes | CP-14 |
| CP-A15 | Upgrade/import interrupted at every boundary resumes without duplicate ownership, lost history/pins or rewritten hashes; unresolved legacy effects block retirement | CP-11, CP-12 |
| CP-A16 | New-format server and test fixtures have no active legacy deployment callbacks/tables; runtime Kubernetes/chart/cancellation tests still pass | CP-11, CP-12 |

## 13. Implementation packages and ordered ownership

These are planning packages, not scheduled task IDs. New task records must name
owned paths, prerequisite contract checkpoints, scoped checks and acceptance IDs.

| Package | Owns/delivers | Dependencies | Exit evidence |
| --- | --- | --- | --- |
| P0 Contracts and decision | ADR superseding O-12; amend owning DQ/PX/VS/IT/PI contracts; pipeline/runtime/state/driver/receipt/gate schemas and fixtures; complete retirement/test ledger | Owner direction in this plan | Reviewed contract checkpoint, examples validate, compatibility/retirement semantics explicit |
| P1 State capability | `internal/state`, generic artifact-reference join, appended storage contribution through schema owner, API fragment | P0 | CP-A02/A03/A05 with memory and real PostgreSQL |
| P2 Runtime bindings | Runtime v3, direct secret path, state grants, artifact inputs/read helper, thin public client | P0, P1 | CP-A01/A02/A04 and real entrypoint roundtrip |
| P3 Generic admission | Execution/project/source-free origins, Git trigger planner and source retention; coordinate T-131/T-132 | P0 | CP-A11; no dummy work item, no source authority for source-free runs |
| P4 Shared engine | Deployment contracts, journal/release engine, fake driver contract suite, receipts/history/pruning | P0, P2 | CP-A05/A07, protocol/cancellation/schema tests |
| P5K Kubernetes driver | Mandatory `drivers/kubernetes`, image/recipe and migrated deployment-tools tests | P4 | CP-A08 and real kind/ordinary-runtime suite |
| P5C Cloudflare driver | `drivers/cloudflare-workers`, pinned Wrangler image/recipe | P4 | CP-A09, including first-create and ambiguous effects |
| P5G GCP driver | `drivers/gcp-cloud-run`, pinned tooling image/recipe | P4 | CP-A10; controlled tests do not close live qualification |
| P6 Review and assembly | Ordered `internal/app`, gates/QA, API/client/CLI composition; generic state inspection, schema manifest, chart/settings joins | P1, P2, P3, P4 | CP-A12, M1/M2/M3 composition and denial/rollback joins |
| P7 Examples and migrated tests | Both templates, sync-cursor, M2/M3/template plugin paths, integration drivers, documented image build evidence | P5K, P5C, P6 | CP-A13/A14 plus replacement test ledger; CP-A06 after P5G |
| P8 Cutover and retirement | Export/import tool, freeze/drain, historical readers, removal of old services/routes/schema, upgrade tests | P5K, P6, P7 | CP-A15/A16 and no active legacy dependencies |

P5 drivers can proceed independently after the shared contract, but shared
engine files have one owner. The composition/schema/API/client owner serializes
shared changes. Coordinate existing T-117/T-121 acceptance and T-134 local
runtime wiring instead of assuming unfinished infrastructure is available.
Full three-driver support closes only after P5G's live acceptance; a narrower
release must explicitly describe its qualified drivers.

### 13.1 Checks and publication

Documentation-only planning uses `make docs-check` and `git diff --check`.
Implementation uses `make check-go PKGS="..."` on each package's actual owned
scope and `make check-go-integration PKGS="..."` with isolated `DEV_SCOPE` for
real state/runtime/driver joins. New image packages and templates must be named
explicitly: the default core Go suite does not include them.

Composition runs relevant `make test-m1`, `make test-m2`, `make test-m3`,
`make api-check` and chart checks when their inputs change. Update those runners
to exercise plugins rather than quietly dropping deployment coverage. Run
live-provider checks only in their named isolated scope; retain provider/tool
versions and observed resource identifiers without credentials.

Each task follows [repository review and worktree rules](../../AGENTS.md): scoped
passing checks and independent approval before merge, with commands/results in
the handoff. Integrated unit validation runs every five merges and at queue
drain, not another redundant full suite per task. Test retirement is reviewed
against the acceptance ledger as part of the same candidate.

## 14. Completion criteria and deferred extensions

The transition is complete when:

- The core has no active application Environment/Deployment service, callback,
  cleanup scheduler, provider readiness parser or mandatory Kubernetes credential.
- Kubernetes deployment is implemented and qualified under `drivers/kubernetes/`
  using the same engine/state path as Cloudflare and GCP.
- Existing Kubernetes deployment tests have named passing plugin replacements;
  infrastructure Kubernetes tests still qualify runtime behavior separately.
- Reusable versioning, status, rollback and cleanup are owned by one plugin
  engine, with a documented driver contract and no per-image lifecycle copies.
- Both project examples run, including CI without work items and maintenance
  after the originating run/work item has finished.
- Credentials, artifact provenance/retention, tenant isolation, human decisions,
  source publication and uncertain-effect handling retain explicit owners.
- Historical records/hashes are accessible, live resource ownership has been
  transferred or settled, and no old table/route is needed for new operations.
- A non-deployment plugin proves the generic contract without a core change.

Deferred: plugin marketplace/registry, runtime installation of drivers, arbitrary
server plugins, universal cloud schema, cross-project resource locks, workflow
DSL inside the deploy engine, automatic database rollback, and a deployment UI.
Executable custom-driver launching can follow the frozen protocol after the
three built-in adapters establish its real needs. Nothing requires those
extensions to deliver the two example projects.

## 15. Implementation status in this branch

The following slice is implemented and exercised locally:

| Area | State |
| --- | --- |
| `contracts/state/v1` | Implemented with bounded values, keys, artifact references and ETags |
| `internal/state` | Memory and PostgreSQL stores, binding-relative keys, conditional lifecycle, project quota trigger, runtime HTTP handler and live PostgreSQL lifecycle/RLS tests |
| `contracts/pipeline/v1alpha3` | State/artifact-file bindings and no v1alpha3 environment field |
| Runtime bindings | State and artifact descriptors are pinned into v1/v2 contexts; `/runtime/v3` state route is composed and checks the run_shared wire |
| Artifact inputs | Admission pins the verified descriptor, the entrypoint materializes it under `/kodama/inputs`, and the runtime download route checks the pinned attempt context |
| `pkg/pluginruntime/state` | HTTP client with create/read/update/delete/list and token-file refresh |
| `images/plugins/deploy` | Shared engine, CAS operation claim, durable checkpoints, apply/status/reconcile/cleanup/rollback CLI and result-manifest emission |
| `drivers/kubernetes` | Bounded manifest validation, ownership annotations, image checks, readiness observation and UID/resourceVersion-preconditioned deletion via kubectl proxy |
| `drivers/cloudflare` | Wrangler apply/observe/delete/rollback driver with controlled-command tests |
| `drivers/gcp` | Cloud Run apply/observe/delete/rollback driver with controlled-command tests |
| Project runs | Source-free `kodama/v1alpha3` admission service and `POST /runs/project`, with a `project` purpose and no work item/workspace/deployment |
| Templates | Agent-development and Cloudflare-website project shapes |
| Real local test | `test/integration/deploy-plugins` creates/reuses kind, builds the plugin, applies a dummy nginx Deployment through the plugin, observes readiness and cleans it up |

Not yet implemented in this branch:

- State audit/outbox events, artifact retention pin/unpin joins and an atomic state-CAS plus runtime-session transaction.
- A distinct runtime context v3 wire; v1alpha3 currently extends the v2 context additively. The state route is versioned separately.
- Source-backed CI origins and the Git trigger planner's no-work-item path (source-free project admission exists, but the planner still requires a work item).
- Generic gate/QA evidence integration and the legacy delivery-core removal. The legacy delivery composition is still constructed by `internal/app`.
- Real Cloudflare/GCP account qualification; their tests use controlled provider commands.

The local test is intentionally explicit about this boundary. It proves the
engine and Kubernetes provider path, not the full planned cutover.
