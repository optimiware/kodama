# Pipeline format `kodama/v1alpha2`

**Status:** normative contract frozen by T-109 (P0 of the [run workspace plan](../implementation/run-workspace-services-builders.md)); executable schema/parser by T-110; **not enabled and not qualified**. A document of this version describes the `run_shared` run model of [ADR 0010](../adr/0010-run-workspace-services-builders.md). Until the installation enables it (T-116) and the profile is qualified (T-111/T-117), every installation executes only [`kodama/v1alpha1`](pipeline-format.md) with the `attempt_private` model. Schema representability is not installation support.

`v1alpha2` is a separate document version, not an extension flag. A `v1alpha1` document is never reinterpreted: its canonical bytes, digests, `workspace.from` semantics and per-attempt checkout behavior are unchanged. A `v1alpha2` document is never accepted by the `v1alpha1` validator. IDs introduced here (PX-17–PX-22, WS-13–WS-16, IT-14–IT-16, VS-14, DQ-16) are listed in the owning specifications; where a `v1alpha1` requirement is replaced, the older ID stays valid for `v1alpha1` runs and is marked by format, never renumbered.

## Document

**Subsequent refinement:** [ADR 0012](../adr/0012-container-sdlc-and-artifact-releases.md) keeps shared workspace/services and ordinary containers while adding artifact/release inputs and generic delivery. DQ-18/DQ-19 replace the blanket DQ-16 evidence rule only after the [new semantic contract](artifacts-and-releases.md) has an explicit executable version/cutover. This document's canonical vectors, defaults and old gate behavior are not reinterpreted.

```yaml
apiVersion: kodama/v1alpha2
kind: Pipeline
metadata:
  name: integration-build
spec:
  timeoutSeconds: 3600
  runWorkspace: {sizeMiB: 4096}
  inputs: {}
  services:
    db:
      image: registry.example.invalid/postgres-rootless@sha256:1111111111111111111111111111111111111111111111111111111111111111
      env:
        POSTGRES_USER: kodama
        POSTGRES_DB: app
        POSTGRES_PASSWORD_FILE: /kodama/secrets/postgres-password
      secretFiles:
        postgres-password: "secret://project/test-db-password"
      command: ["/usr/local/bin/docker-entrypoint.sh", "postgres"]
      ports:
        sql: {port: 5432}
      resources: {cpuMillis: 500, memoryMiB: 512, ephemeralMiB: 256}
      storage:
        - {path: /var/lib/postgresql/data, sizeMiB: 1024}
      readiness:
        exec: ["pg_isready", "-U", "kodama", "-d", "app"]
        timeoutSeconds: 60
    browser:
      image: registry.example.invalid/headless-browser@sha256:2222222222222222222222222222222222222222222222222222222222222222
      ports:
        devtools: {port: 3000}
      resources: {cpuMillis: 1000, memoryMiB: 1024, ephemeralMiB: 512, shmMiB: 512}
      egress: [preview]
      readiness:
        http: {port: devtools, path: /json/version}
        timeoutSeconds: 60
  network:
    edges: []
  steps:
    - id: generate
      kind: container
      image: registry.example.invalid/go-tools@sha256:3333333333333333333333333333333333333333333333333333333333333333
      workspace: {access: write}
      command: ["/bin/sh", "-ec"]
      args: ["cd /workspace && go generate ./..."]
      timeoutSeconds: 600
    - id: integration
      kind: container
      needs: [generate]
      image: registry.example.invalid/test-tools@sha256:4444444444444444444444444444444444444444444444444444444444444444
      env:
        PGHOST: "${{ services.db.host }}"
        PGPORT: "${{ services.db.ports.sql }}"
        PGUSER: kodama
        PGDATABASE: app
        PGPASSWORD: {secretRef: "secret://project/test-db-password"}
        BROWSER_HOST: "${{ services.browser.host }}"
        BROWSER_PORT: "${{ services.browser.ports.devtools }}"
      command: ["/bin/sh", "-ec"]
      args: ["cd /workspace && go test ./..."]
      timeoutSeconds: 1200
      outputs:
        report: {type: artifact, path: /kodama/outputs/tests.json}
```

Images and domains are placeholders, and service names are arbitrary: `browser` here is a generic HTTP service, not a supported browser capability (browser support is out of scope under [O-30](../decisions.md#revised-scope--2026-10-29-o-30)). This is a shape example, not an executable qualified build: images must contain the declared probe/command dependencies and implement their endpoints. The document carries no raw Pod, security, mount, node, volume, network, selector or kubeconfig control; those come from the pinned [execution policy](run-resources.md#execution-policy).

## Fields

Rules not repeated here are those of [`v1alpha1`](pipeline-format.md#fields) (names, inputs, `needs`, `when`, `environment`, `retry`, `outputs`, secret references, agent/gate forms, validation).

| Field | Rule |
| --- | --- |
| `spec.timeoutSeconds` | **Required.** Total run deadline, integer 60–604800, covering preparation, service startup, every attempt, retry backoff **and gate waits**. Installation policy may lower the ceiling. It is distinct from a step's execution `timeoutSeconds`; a step's value must not exceed it (`step_timeout_exceeds_run`) |
| `spec.runWorkspace.sizeMiB` | Optional bound of the run scratch filesystem, integer 64–1048576. **Absent means the pinned policy's default**, recorded at admission; it is not filled into the canonical form |
| `spec.services` | Optional map of at most **8** services keyed by `^[a-z][a-z0-9-]{0,29}$`. Names share no namespace with step IDs |
| `spec.network.edges` | Optional list of at most 32 declared service-to-service edges |
| `step.workspace` | `{access: none \| read \| write}`. **No `from`** (`workspace_from_removed`). Absent is not filled (see [source defaults](#source-backed-and-source-free-defaults)). Gates have none |
| `step.egress`, `service.egress` | Optional list of at most 8 names (`^[a-z][a-z0-9-]{0,62}$`) of installation/project-authorized [egress profiles](run-resources.md#egress-profiles). Container steps and services only. Never hosts, CIDRs or ports |
| every executable step | Receives `/workspace` implicitly. Container steps keep `image`, `command`, `args`, `env`, `secretFiles`, `timeoutSeconds`, `retry`, `outputs`; the image remains digest-pinned and the executed process runs under the pinned policy, not document-supplied security settings |

Images (steps, services, agent profile expansion) must be digest pinned; a tag alone, `latest` or a missing digest is `mutable_image`.

### Services

Each service is a run-lifetime support workload, started with the run, ready before any user command, stopped when the run ends. It is neither a step nor a sidecar of any attempt.

| Field | Rule |
| --- | --- |
| `image` | Required, digest-pinned |
| `role` | `general` (default) or `builder`. A builder service is the run-exclusive daemon of the installation's pinned builder-service mode (for example BuildKit); at most **one** per document. The shape carries no engine/mode selection, privilege or process-sandbox field: the installation selects and pins its qualified mode, and a run needing a role/mode the installation does not offer is refused at admission (`capability_unsupported`). Builder services are deferred to T-123 (O-30): until then every installation refuses `role: builder`. Per-run client authentication material is generated by execution and delivered through the protected secret path as the qualified builder-service profile defines; it is not a document field |
| `command`, `args` | Optional argv arrays with the [argv matrix](#argv-semantics) below. `command`, when present, must have 1–64 elements (`command: []` is rejected, `invalid_value`); `args` may be omitted or `[]` (identical meaning and identical canonical form) or hold up to 256 elements, each at most 8192 characters. With neither, the image defaults run and the runtime preserves argv and signal handling (SIGTERM, then SIGKILL after the policy grace period). A service with any secret binding **must** declare `command` (`service_secret_requires_command`) because supervision cannot rewrap unknown image defaults |
| `env` | At most 64 entries, names `^[A-Za-z_][A-Za-z0-9_]{0,127}$`. Values: nonsecret scalars, whole-scalar `${{ inputs.NAME }}`, whole-scalar endpoint expressions of **another** service the service has a declared edge to, or `{secretRef: "secret://project/NAME"}` |
| `secretFiles` | At most 16 entries; same rules as steps. Delivered under the service's `/kodama/secrets`, never in manifests or shared files |
| `ports` | Required map of 1–8 ports keyed by `^[a-z][a-z0-9-]{0,14}$`; `{port: 1–65535, protocol: "TCP"}` (`protocol` defaults to `TCP`; no other value exists). Port numbers are unique per service. The declared container port is the endpoint port: there is no remapping, host port, NodePort, LoadBalancer or ingress |
| `resources` | **Required.** `cpuMillis` 50–16000, `memoryMiB` 16–65536, `ephemeralMiB` 16–262144 (writable layer, logs and `/tmp` combined), optional `shmMiB` 0–4096 (default 64, bounded writable `/dev/shm`). Values above the pinned policy ceiling are refused at admission (`limit_exceeds_policy`) |
| `storage` | Optional list of at most 4 `{path, sizeMiB}` run-owned ephemeral volumes (`sizeMiB` 1–262144). `path` is an absolute normalized path of at most 512 characters, with no `..`, no trailing slash, no overlap with another declared path and none at or under `/workspace`, `/kodama`, `/proc`, `/sys`, `/dev` or `/run/secrets`. Contents die with the service. A service never mounts `/workspace`, `.git`, attempt identity or another workload's secrets |
| `readiness` | **Required.** Exactly one of `tcp: PORT_NAME`, `http: {port: PORT_NAME, path}` (path begins with `/`, at most 512 characters, no whitespace/control characters/fragment; success is HTTP status 200–399 over plain HTTP, no headers), `exec: [argv]` (1–16 elements of at most 1024 characters, run inside the service container without a shell, with the service's environment but no secrets expanded into argv). Plus `timeoutSeconds` 5–600 (startup budget until first ready), `periodSeconds` 1–60 (default 2), `probeTimeoutSeconds` 1–60 (default 5, at most `timeoutSeconds`) and `lossSeconds` 5–600 (default 30: continuous failure after first ready longer than this is service loss) |

An image used for `exec` must contain its program; Kodama does not inject one. Probe origin is runtime-private and must not require network access for tenant workloads. A service that exits for any reason before the run settles is lost, even with exit code 0.

#### Argv semantics

Canonical form fills an absent `args` as `[]`, so **absent `args` and `args: []` are one and the same value** with one digest; the format has no way to say "explicitly empty CMD while keeping the image ENTRYPOINT" (declare `command` instead). `command` is never filled: its absence is meaningful and is preserved. The effective process of a service (and of a step, where `command` is required and only the right-hand rows apply) is:

| `command` | `args` (absent or `[]` / non-empty) | Effective argv |
| --- | --- | --- |
| absent | absent or `[]` | Image ENTRYPOINT then image CMD, unchanged |
| absent | non-empty | Image ENTRYPOINT, then `args` (replacing the image CMD) |
| present | absent or `[]` | `command` only; the image CMD is not used |
| present | non-empty | `command` then `args`; neither image ENTRYPOINT nor CMD is used |

A service with no `command` and no secret bindings keeps image defaults; admission records `command: null` in the [resource context](runtime-v2.md#resource-subjects) (never a guessed argv, since the image's defaults are not part of the document) and the adapter applies the matrix against the pinned image. An image with neither ENTRYPOINT nor CMD and no declared `command` fails startup as `service_not_ready` (it exits), not at validation, because validation never reads images. Fixtures: the same document with `args` omitted and with `args: []` canonicalizes to identical bytes; the four rows each have a valid fixture (`db` with all defaults, `args` only, `command` only, `command` plus `args`); `command: []` is rejected.

### Network edges and egress

Reachability follows the pinned policy's [network model](run-resources.md#execution-policy) ([O-30](../decisions.md#revised-scope--2026-10-29-o-30)). Under `isolation: run`, steps reach exactly the declared ports of their own run's services and a service accepts connections from nothing else unless an edge declares it. Under `isolation: tenant` (the stage model), the tenant's workloads may reach any run service of that tenant; edges then remain a document rule for endpoint expressions, not an enforced boundary. Edges are declared the same way in both:

```yaml
network:
  edges:
    - {from: app, to: db, ports: [sql]}
```

`from` and `to` name different declared services, `ports` is a non-empty unique list of ports declared by `to`, and a `(from, to)` pair appears once. Edges are directed and carry no start-order meaning: services start independently, edges may form a cycle, and the endpoint addresses are fixed at admission. A service `env` endpoint expression `services.NAME.host` or `services.NAME.ports.PORT` requires an edge from this service to `NAME` (and to that port); otherwise `edge_undeclared`. A service may not reference its own endpoints (`service_self_reference`) or any `steps.*` value (`service_step_reference`).

`egress` names external destination sets authorized by installation/project policy, pinned at admission and enforced after name resolution. A workload gets only the profiles it names; a service does not inherit a step's registry access. Under `publicEgress: false` all other external traffic is denied; under `publicEgress: true` (the stage model) public HTTP/HTTPS is also allowed, while private, cluster and platform destinations still need a profile. Names the pinned policy does not define are refused at admission (`egress_profile_unknown`). The document cannot widen egress: the policy alone selects the model.

## Expressions

All `v1alpha1` expressions remain whole-scalar `${{ ... }}` forms; the marker anywhere else is `interpolation`. `v1alpha2` adds two typed endpoint references:

| Expression | Type | Meaning |
| --- | --- | --- |
| `${{ services.NAME.host }}` | string | Run-unique DNS-label endpoint host of service `NAME`, resolved at admission from the run identity; its spelling is opaque and at most 63 characters |
| `${{ services.NAME.ports.PORT }}` | integer | The declared port number |

Sites: container-step `env` values and `args` elements (a typed integer is rendered as a decimal string in an environment variable or argument), and service `env` values subject to the edge rule. `command`, `image`, `readiness`, `storage`, `gate.evidence`, outputs and paths are not sites. Service endpoints need no `needs` path: a service has no outputs and exists from run start. A reference to an undeclared service or port is `unknown_service`/`unknown_port`. Admission resolves every endpoint expression into the immutable [attempt context](runtime-v2.md#context) (`command.env`/`command.args` hold final strings and `endpoints` the typed map); the entrypoint never evaluates expressions. The endpoint host is run-unique; it is reachable by that run's workloads under the run model and by the tenant's workloads under the tenant model.

## Source-backed and source-free defaults

Whether a run has source is a property of the **run**, not the definition: `purpose=work` runs are source-backed (a pinned repository commit exists); internal `purpose=cleanup` runs are source-free. The same definition digest is valid for both, so canonicalization must not depend on it.

| Document | Source-backed run | Source-free run |
| --- | --- | --- |
| Step `workspace` absent | Effective `{access: read}` | Effective `{access: none}` |
| `workspace: {access: none}` | **Refused** (`workspace_none_source_backed`): `none` is publication authority only, and a mounted scratch tree would be misleadingly labeled isolated | Allowed |
| `workspace: {access: read\|write}` | Allowed | **Refused** (`workspace_access_source_free`): no repository exists |
| Agent step | Allowed | Refused (`agent_source_free`); cleanup runs admit container steps only |
| `/workspace` | Scratch prepared from the pinned commit | Empty scratch, no checkout credential, no preparation workload |

`access` expresses publication authority, not filesystem mutability: even `read` can modify scratch; the single write step may publish. Admission records the **effective** access of every step in the run snapshot so replay, API and context never recompute it. Absent `workspace` stays absent in the canonical definition.

## Canonical form

Same serialization as [`v1alpha1`](../../contracts/pipeline/README.md#canonical-form-and-digest) (UTF-8 JSON, byte-ordered keys, no insignificant whitespace, no HTML escaping, array order preserved, plain integers, `sha256:` + lowercase hex). The digest includes `apiVersion`, hence never collides with a `v1alpha1` digest of similar content. Normalization applies documented **contract-constant** defaults and nothing else:

| Scope | Filled when absent |
| --- | --- |
| spec | `inputs: {}`, `network: {edges: []}` |
| service | `role: "general"`, `args: []`, `env: {}`, `secretFiles: {}`, `storage: []`, `egress: []`, `ports.*.protocol: "TCP"`, `resources.shmMiB: 64`, `readiness.periodSeconds: 2`, `probeTimeoutSeconds: 5`, `lossSeconds: 30` |
| container step | `needs: []`, `args: []`, `env: {}`, `secretFiles: {}`, `egress: []`, `outputs: {}`, `retry: {maxAttempts: 1, backoffSeconds: 0, on: []}` |
| agent step | `needs: []`, `outputs: {}`, `retry` as above |
| input | `required: false` |
| gate step | As in `v1alpha1` (no workspace, image, retry or outputs) |

**Not filled, so the digest is independent of run source and installation policy:** step `workspace`, `spec.runWorkspace`, service `command` (absence is meaningful; see [argv semantics](#argv-semantics)), `environment` and `when` when absent. Source-dependent defaults are resolved at run admission; policy-dependent defaults (scratch size, ceilings) at admission against the pinned policy. Both are recorded in the run snapshot, never written back to the definition. A later default change needs a new format version.

Reference vector (derive independently; this was computed with Python `json`/`hashlib`). Input:

```yaml
apiVersion: kodama/v1alpha2
kind: Pipeline
metadata: {name: service-test}
spec:
  timeoutSeconds: 1800
  services:
    db:
      image: registry.example.invalid/postgres-rootless@sha256:1111111111111111111111111111111111111111111111111111111111111111
      env: {POSTGRES_USER: kodama, POSTGRES_DB: app}
      ports: {sql: {port: 5432}}
      resources: {cpuMillis: 500, memoryMiB: 512, ephemeralMiB: 256}
      storage: [{path: /var/lib/postgresql/data, sizeMiB: 1024}]
      readiness: {tcp: sql, timeoutSeconds: 60}
  steps:
    - id: test
      kind: container
      image: registry.example.invalid/go-tools@sha256:2222222222222222222222222222222222222222222222222222222222222222
      command: ["/bin/sh", "-ec"]
      args: ["cd /workspace && go test ./..."]
      env: {PGHOST: "${{ services.db.host }}", PGPORT: "${{ services.db.ports.sql }}"}
      timeoutSeconds: 600
```

Canonical bytes (one line) and digest `sha256:368c9636851797bb0812589cae5297122b3d799fc96522194764de5a574910e2`:

```json
{"apiVersion":"kodama/v1alpha2","kind":"Pipeline","metadata":{"name":"service-test"},"spec":{"inputs":{},"network":{"edges":[]},"services":{"db":{"args":[],"egress":[],"env":{"POSTGRES_DB":"app","POSTGRES_USER":"kodama"},"image":"registry.example.invalid/postgres-rootless@sha256:1111111111111111111111111111111111111111111111111111111111111111","ports":{"sql":{"port":5432,"protocol":"TCP"}},"readiness":{"lossSeconds":30,"periodSeconds":2,"probeTimeoutSeconds":5,"tcp":"sql","timeoutSeconds":60},"resources":{"cpuMillis":500,"ephemeralMiB":256,"memoryMiB":512,"shmMiB":64},"role":"general","secretFiles":{},"storage":[{"path":"/var/lib/postgresql/data","sizeMiB":1024}]}},"steps":[{"args":["cd /workspace && go test ./..."],"command":["/bin/sh","-ec"],"egress":[],"env":{"PGHOST":"${{ services.db.host }}","PGPORT":"${{ services.db.ports.sql }}"},"id":"test","image":"registry.example.invalid/go-tools@sha256:2222222222222222222222222222222222222222222222222222222222222222","kind":"container","needs":[],"outputs":{},"retry":{"backoffSeconds":0,"maxAttempts":1,"on":[]},"secretFiles":{},"timeoutSeconds":600}],"timeoutSeconds":1800}}
```

Endpoint expressions stay as written in the definition; they resolve at admission. The same vector with `workspace: {access: read}` added to the step is a different digest by design.

## Deterministic execution order

The executable order is a pure function of the canonical definition, so it needs no stored digest but is recorded in the run snapshot (`executionOrder`, step IDs including gates). Let steps be indexed by declaration order. Repeat until every step is emitted: among not-yet-emitted steps whose `needs` are all emitted, select the **smallest declaration index** and append it. (This is the lexicographically smallest topological order by declaration index; it is not breadth-first.)

| Declaration (`needs`) | Order | Note |
| --- | --- | --- |
| `a`, `b(a)`, `c`, `d(c)` | `a b c d` | After `a`, candidates are `b`(1) and `c`(2) |
| `b(c)`, `c`, `a` | `c b a` | `b` unlocks once `c` is emitted and outranks `a` |
| `test`, `review`(gate, `test`), `build(review)`, `lint` | `test review build lint` | `lint` has no dependency but is declared last: it waits for the gate |

A gate occupies its position. Every node after it waits until the gate resolves, whether or not it depends on the gate; authors place independent work before the gate. `needs` keeps controlling logical dependency, skipping propagation, writer ordering, output availability and expression validity; the order only serializes. A node is eligible when its predecessor in the order is **settled**: terminal step state (`succeeded`, `failed`, `skipped`, `cancelled`) with workload/mount released and result/publication/uncertain effects reconciled. A false `when` or skipped ancestor settles a step without ever holding the workspace. A failed/cancelled node ends dispatch through the existing fail-fast rule. Retry backoff keeps the step's slot. Details in the [pipeline-runtime simplification plan](../implementation/pipeline-runtime-simplification.md).

## Validation

Everything in [`v1alpha1` validation](pipeline-format.md#validation) applies, with these changes. Rejected: unknown fields anywhere (including `workspace.from`, raw Pod/security/mount/volume/network/selector/hostPort fields), `latest`/tag-only/missing-digest images for steps **and services**, a service reference to a step output, services without `ports`/`readiness`/`resources`, probes naming undeclared ports, duplicate or out-of-range ports, unsupported protocols, storage paths violating the rules above, more than 8 services, more than one builder service, unknown or conflicting edges, endpoint expressions without a declared edge, `egress` entries that are not names, `spec.timeoutSeconds` absent or out of range, step `timeoutSeconds` above the run deadline, and conditional gates. Writers must still be totally ordered by `needs` (`parallel_writers`), keeping the author's intent explicit even though execution is serialized. Each writer still has an implicit `commit` output. A write after approval requires a new applicable gate before protected successors, and a changed scratch tree does not make the original approval apply ([DQ-16](../specs/delivery-and-quality-gates.md#requirements)).

New diagnostic codes (stable, with the existing `code` + `path` + constant `message`, no document content echoed; syntax stage unless noted): `workspace_from_removed`, `unknown_service`, `unknown_port`, `duplicate_port`, `invalid_probe`, `invalid_resources`, `invalid_storage`, `invalid_edge`, `invalid_egress`, `invalid_role`; semantic stage: `edge_undeclared`, `service_self_reference`, `service_step_reference`, `service_secret_requires_command`, `step_timeout_exceeds_run`. Existing codes (`mutable_image`, `unknown_field`, `limit_exceeded`, `interpolation`, `type_mismatch`, `secret_denied`, ...) keep their meaning.

### Format availability and admission refusals

Registration/validation carries an explicit **format capability** that is off by default: `format_disabled` is returned for a `v1alpha2` document while the installation has not enabled it. Run admission refuses, with a stable code and an actionable message that names only the cause (never document content):

| Code | Cause |
| --- | --- |
| `format_retired` | The installation closed new `v1alpha1` runs after cutover; register a `v1alpha2` definition. Historical definitions stay readable |
| `capability_unsupported` | The selected runtime's resource capabilities (or installation's pinned builder mode) lack a required capability; `details.missing` lists the names ([capabilities](run-resources.md#capabilities)) |
| `policy_unavailable` | No installed execution policy matches the admission pin |
| `limit_exceeds_policy` | Run deadline, scratch size, service resources/storage or service count exceed the pinned policy ceiling |
| `egress_profile_unknown` | A named egress profile is undefined or not authorized for the project |
| `workspace_none_source_backed`, `workspace_access_source_free`, `agent_source_free` | [Source defaults](#source-backed-and-source-free-defaults) |

## Migration from `v1alpha1`

Mechanical changes: add `spec.timeoutSeconds`; delete every `workspace.from`; drop `workspace` where `read` is intended; set `none` only for source-free definitions; give every step a pinned image of its own, containing the tools it needs (a reader no longer needs Git merely for checkout, a writer still needs Git for publication); move per-attempt clones' assumptions to the shared scratch (a prior step's untracked/ignored files are now visible). Anything depending on branch isolation between steps (`from: <writer>` reading a published commit while a later step changed files) must be re-expressed with `needs`/explicit commits. Registering the rewritten document creates a new definition; the old one, its digest and its runs are untouched. This historical model is superseded by the [pipeline-runtime simplification plan](../implementation/pipeline-runtime-simplification.md).
