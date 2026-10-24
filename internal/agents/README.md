# internal/agents

Agent profiles (task T-030): immutable, versioned defaults that map an `agent` pipeline step
onto the existing container runtime, following the [agent runtime specification](../../docs/specs/agent-runtime.md)
(AR-01–AR-04, AR-10, AR-12, AR-13; supporting AR-03, AR-05, AR-06, AR-14) with SD-01–SD-08.

Status: implemented and tested (domain, expansion and codecs without infrastructure; the PostgreSQL
store, registration and both admission paths against real PostgreSQL). Nothing registers these
routes, migrations or adapters yet; that is the integration owner's job (T-039), with the
confirmations in [review.md](review.md). It does **not** claim an OpenCode wrapper or its
compatibility smoke result (T-031), the AI gateway or tool proxy enforcement of limits (T-032/T-033),
human gates (T-034), hosted delegation or credentials (O-18/O-19), sessions, continuation or a
scheduler. An agent step is a container step: same attempt, entrypoint, publication, result,
cancellation and recovery machinery.

## Layout

| Path | Purpose |
| --- | --- |
| `profile.go`, `values.go`, `errors.go` | `Draft` → validated, canonical `Content` (digest-addressed), immutable `ProfileVersion`; actors, permissions, controls, `name@version` references |
| `catalogue.go`, `ports.go`, `service.go` | Injected compatibility catalogue; consumer-owned ports (`Repository`, `Authority`, `Compatibility`, `Approvals`); `Register`/`List`/`Stored`/`Resolve` use cases |
| `payload.go`, `expansion.go` | Pure expansion into image, wrapper command and the canonical wrapper payload; the shared encoder/decoder; pinned `Policy` |
| `postgres/` | `Store` (`agents.Repository`), `T030_001_agent_profiles.sql`, `Migrations()` |
| `http/` | The two routes, DTO/error mapping, generated types (`agents.gen.go`) |
| `adapters/` | `Lookup` (`pipeline.ProfileLookup`), `Resolver` (`execution.AgentResolver`), `Authority` (identity), admission decorators and `Chain`, `Planner`, `PolicyReader`, `MetadataReader` |
| `agentstest/` | Fixtures: builders, a table authority and approvals, an in-memory repository. Fixture catalogue entries never establish production support |
| `integration/` | Real-PostgreSQL tests of pinning, unknown wrapper, `needs_input`, admission recovery and limits |
| `../../api/agents.yaml` | OpenAPI 3.0.3 fragment with examples |
| `../execution/agent-resolver.go` | Execution-owned `AgentResolver`, `AgentExpansion` and `ExpandAgents` (the seam execution consumes) |

Dependencies point inward: `agents` imports `domain` and the runtime contract; `postgres`, `http` and
`adapters` import `agents`. `adapters` also imports `execution`, `execution/http`, `pipeline` and
`identity` as a bridge; `execution` imports none of this module.

## Model

A profile version pins: the image digest, an exact wrapper id/version/entrypoint and harness
id/version, instruction text, an approved model name and credential-free endpoint, a sorted tool
allow-list, claimed limits (`wallSeconds` required; `maxRequests`, `maxTokens`, `maxToolCalls` only
when claimed), a transcript policy and an immutable [design baseline](../../docs/implementation/design-conformance.md).
Content is canonical JSON; its SHA-256 is the digest. There is no update or delete: changed content
is the next version of its name, identical content returns the existing version, and nothing else
transitions. Provider and tool credentials are never part of a profile: the gateway and proxy own them.

**Reference syntax.** An agent step names the server-returned immutable `name@version`. A bare name is
a moving alias and is rejected by validation, lookup and admission. (`pipeline.ProfileLookup`
explicitly leaves this syntax to the profile owner.)

**Registration** (`Service.Register`) authorizes first, then validates, requires compatibility
evidence for the exact image/wrapper/entrypoint/harness tuple and for every claimed control, requires
the model and tools to be approved for the project, and registers atomically with one audit row. An
unqualified tuple, an unsupported claimed control (for example a token allowance nothing enforces)
or an unapproved reference fails before anything is written (AR-02). A missing catalogue or approval
policy fails closed; an empty catalogue qualifies nothing.

**Compatibility evidence.** `agents.Catalogue` is an injected list of tested combinations, each with
mandatory evidence and the controls the installation enforces for it. Production entries come from
T-031 (wrapper smoke evidence) and T-032/T-033 (gateway and proxy enforcement). T-039 must not enable
a profile whose claimed controls those tasks do not implement.

## Expansion and pinning

`execution.ExpandAgents` (invoked through `adapters.Expander`) freezes every pending agent step before
the run is durable: the step keeps its ID, dependencies, condition, retry, outputs, workspace access and
action key, and receives the profile's image, wrapper command, payload arguments and an effective
timeout (the smaller of the step's and the profile's wall time; an explicit step timeout narrows,
never widens). The run still pins the original pipeline definition ID and digest. A run's frozen steps
and attempt contexts are immutable in the database, so a later registration, restart, retry or redrive
cannot change them. Request and token allowances are run-wide, so every agent step of one run must
declare the same allowances (including none); different declarations are refused rather than merged.

**Wrapper payload** (`payload.go`): the canonical, versioned, nonsecret JSON of the pinned profile,
base64url-encoded and cut into command arguments after the marker `kodama.agent-config/v1`. It carries
no identity, credential or token, never contains an expression marker, and decodes strictly (bounded,
canonical chunking, unknown fields, trailing data and a digest that does not match the content are
rejected). The wrapper (T-031) decodes it with `agents.DecodeArgs`; the control plane uses the same code.

**Planner.** `adapters.Planner` reads only the frozen payload of the attempt context, verifies it against
the image and command, sets the runtime `Profile` and `DesignBaseline`, and delegates to the configured
planner. It performs no I/O. Ordinary container commands pass through unchanged, and a container step
whose arguments open with the marker is refused at admission.

## Wiring (integration owner)

```go
store, _ := agentspg.NewStore(pool, agentspg.Options{})      // migrations: agentspg.Migrations() after storage.BaseContribution()
svc, _ := agents.NewService(agents.ServiceConfig{Repository: store, Authority: agentAuth /* adapters.NewAuthority(identity, reader) */,
    Compatibility: catalogue, Approvals: approvals})
resolver, _ := adapters.NewResolver(svc)
chain, _ := adapters.NewChain(adapters.ChainConfig{Resolver: resolver, Store: execStore, Creator: execKeyed})
// execution.Dependencies: Store: chain.Store, Planner: chain.Planner, Hooks: chain.Hooks,
//                         Capabilities: chain.Capabilities(pipeline.KindGate)
// executionhttp.Config:   Creator: chain.Creator
lookup, _ := adapters.NewLookup(svc)                         // pipeline.Config.Profiles
h, _ := agentshttp.NewHandler(svc, cursorCodec); h.RegisterRoutes(router)
```

`Chain` is the only source of agent-enabled hooks and capabilities: the store decorator (ordinary
admission), the keyed-creator decorator (HTTP admission) and the planner must be used together.
Keyed creation expands the plan before the idempotency check, so a replay repeats the (deterministic)
expansion; it never creates or changes a run.

## Authority and routes

`GET` and `POST /api/v1/tenants/{tenantId}/projects/{projectId}/agent-profiles` only. The identity
adapter maps reading to `run:read` and registration to `run:manage`; no role or permission is added.
Identity policy denies every workload, service and operator actor, so an agent can never satisfy a human
permission (AR-12), and profile instructions grant nothing: authority comes only from the workload
identity. Profile policy narrows what a profile may name; it never establishes authority.

## Limits, results and metadata

* **Wall time** reaches the ordinary execution deadlines; the execution limit still applies.
* **Requests, tokens, tool calls, model and tool allow-list** are claims enforced by the gateway and
  proxy. `adapters.PolicyReader.PolicyOf(scope, run, step, attempt)` gives them the immutable pinned policy
  of one attempt; a foreign, mismatched or non-agent attempt is not found. This module keeps no budget ledger.
* **Results** use `execution.Service.CommitResult` unchanged: `needs_input` is a failed attempt that keeps
  its question and authorized diagnostic artifacts, exposes no candidate and is not retried; a revised
  intent is a new run. `adapters.MetadataReader.MetadataOf` links an attempt to its pinned profile, outcome,
  question and diagnostic artifact IDs (the transcript is one of those artifacts; its access is the artifact
  module's). Transcript recording is a profile switch; metadata stays attributable either way and no
  transcript content enters audits or events.

## Unit qualification and fixtures (T-083)

Everything below runs without a service (`go test ./internal/agents/...`; the
PostgreSQL run of the same suites is `Test_agents_repository_contract` behind
`layer.SkipService`). It is deterministic: profiles live in `agentstest.MemRepo`, time
is fixed, and attempts are driven through the real `execution.Aggregate` transitions,
the real `adapters.Chain` and planner. No provider, wrapper image or model is involved,
so none of it is OpenCode or provider evidence (T-031/T-085).

| Test | Proves |
| --- | --- |
| `Test_unit_agent_profile_pin` (`adapters/`) | AR-14, AR-01/AR-02: an admitted attempt keeps its profile version, instructions, tools, limits, model and design baseline however the profile is edited, re-registered, re-approved or mutated through aliases; only `name@version` admits; a branch/tag/abbreviated baseline revision is refused; conformance evidence is bound to the pinned baseline and the candidate |
| `Test_unit_agent_outcomes` (`adapters/`) | AR-06, AR-13: success needs a result that passes `runtimev1.CheckResult` (prose, a bare exit code, a missing/undeclared/wrong-type output never completes); `needs_input` fails the attempt, keeps its question and stops dispatch; cancellation, a deadline and a reported failure leave no completed outcome and a late result cannot rewrite them |
| `Test_unit_agents_gateway_contract` (`agentstest/`) | SD-13/SD-A11: `agentstest.Contract(t, open, scopes)` on `MemRepo`: content-addressed registration, per-name versions, immutability, project/tenant isolation, ordered paging, racing identical registrations |

**Fixture examples for T-072** (`agentstest` plus the adapters' own constructors):

```go
repo := &agentstest.MemRepo{}                       // agents.Repository over memory
draft := agentstest.Draft(t, "coding")              // a valid draft; Entry/Catalogue qualify it (fixture evidence only)
auth := agentstest.NewAuthority()
auth.Grant(scope, "u_engineer", agents.ActorUser, agents.PermRegister, agents.PermList)
svc, _ := agents.NewService(agents.ServiceConfig{Repository: repo, Authority: auth,
    Compatibility: agentstest.Catalogue(t, draft), Approvals: agentstest.NewApprovals(), Clock: clock})
resolver, _ := adapters.NewResolver(svc)
chain, _ := adapters.NewChain(adapters.ChainConfig{Resolver: resolver, Store: execStore, Creator: execKeyed})
// Outcomes and metadata need only the consumer port adapters.AttemptSource:
meta, _ := adapters.NewMetadataReader(source)       // source: anything returning execution.Attempt by scope/run/step/attempt
```

`MemRepo` is a plain mutex-guarded slice that mirrors the PostgreSQL adapter's
content-addressed semantics; it is not a `memdb` adapter. Moving it onto `memdb` is
possible but changes no behavior the contract can observe, so it was not done. It is
development evidence of policy only: no SQL, lock-order or isolation claim.

## Verification

```
go generate ./internal/agents/...
go test -race ./internal/agents/... ./internal/execution/... ./contracts/runtime/...
make test-task TASK=T-030
```

PostgreSQL tests use `KODAMA_TEST_POSTGRES_ADMIN_URL` (see `test/support/postgres`) and the resource
prefix `t030`.
