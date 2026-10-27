# internal/gateway: the project model gateway (T-032)

The gateway mediates **one** verified OpenAI-compatible endpoint (streaming chat
completions, [protocol.md](protocol.md)) for attempts, with simple bounded usage.
It owns project model settings, the immutable per-run policy binding, atomic
request admission (a request cap and one unresolved request per run across
replicas), request outcomes and usage records. Provider credentials stay behind
it. Specification: [AI gateway and MCP proxy](../../docs/specs/ai-gateway-and-mcp-proxy.md)
(GM-01 to GM-06, GM-09, GM-11, GM-12; the MCP proxy is T-033). Wire contract:
[`api/gateway.yaml`](../../api/gateway.yaml).

**Operational accounting only.** Request counts and token charges are admission
and execution-limit accounting for a run. They are not pricing, payments,
subscriptions or entitlement authority: Platform owns those (O-13), and this module
only preserves attributable operational usage evidence and the atomic run budgets.
Billing being external never weakens a bound.

**Credential custody (O-19).** The selected direction is that model provider
credentials stay server-side behind this gateway: they are KMS-encrypted secrets in
the existing secrets module, pinned to an exact version per run, and read for use
only here, audited, and never delivered to the agent's prompt, files or ambient
environment. Hosted custody, rotation and revocation qualification is T-061's
responsibility; this module's tests qualify the standalone secrets adapter only.

Not implemented and not claimed: hosted/platform model access and authority
(O-18, qualified by T-061), billing, pricing or settlement ledgers, MCP, other
provider protocols, provider/model fallback, content storage and its retention or
deletion (the recorder owner's responsibility: raw content is off by default and
authorized opt-in retention starts at seven days under the approved
[retention baseline](../../docs/operations/upgrades-backup-and-recovery.md#approved-retention-and-recovery-baseline)),
and qualification of any real OpenCode or provider version (T-031; the fixtures
here qualify the protocol subset only).

## Layout

| Path | Role |
| --- | --- |
| `settings.go`, `budget.go`, `request.go`, `usage.go`, `values.go` | Entities and values with their invariants: `ModelSettings` (revisions), `RunBinding`, `RunBudget`, `ModelRequest` (state machine), `Usage`, `Endpoint`, `Limits`. No HTTP, SQL or provider types. |
| `ports.go`, `errors.go`, `service.go`, `forward.go`, `reconcile.go`, `recording.go` | Use cases and the consumer-owned ports: `Store`, `Workloads`, `Authority`, `Credentials`, `Provider`, `Destinations`, `Estimator`, `Decoder`, `Recorder`, `Sink`, `TenantSource`. |
| `postgres/` | PostgreSQL adapter: settings, admission, finalization, reconciliation. The gateway tables are in the baseline schema (`internal/storage/migrations/001_schema.sql`). |
| `adapters/` | Bridges to merged modules: identity (management permissions), runtime authority and execution attempts, secrets (exact version, pins), events. |
| `openai/` | Provider adapter: strict request decoding, restricted TLS transport, destination policy, SSE validation, token estimator. |
| `http/` | HTTP adapter: management route, workload streaming route, error mapping, generated types. |
| `gatewaytest/` | Test fixture over the real stack (imported only by tests). |
| `testdata/chat-completions/` | Frozen protocol fixtures and compatibility manifest. |

## Settings and the run binding

`ModelSettings` (per project, integer revision): `enabled`, one approved HTTPS
`endpoint`, a `secret://project/<name>` credential at an **exact version**, a
non-empty model allow-list, `Limits` (request cap, token allowance, request
bytes, input bound, output bound) and `recording` (default off). A project without
stored settings has the virtual disabled revision 1, so the first PUT carries
`If-Match: "1"` and no wildcard creation exists; the first stored revision is 2.
Each change appends an immutable `model_settings_revisions` row, an audit entry
and a `project.settings_changed` event. Edits need `project:settings`, rechecked
inside the write transaction through `identitypg.NewTxAuthority`; reads need
`config:read`.

At a run's **first model admission** the gateway captures an immutable `RunBinding`
(settings revision, endpoint, models, limits, exact credential version, recording)
and registers the credential as a run pin through the secrets module's pin API in
the same transaction. Later settings edits and secret rotation affect later runs
only; the run keeps its binding and its pinned version, and a newer secret version
is never substituted. The credential is never added to an attempt's file or
environment bindings, and the run pin alone does not let the workload resolve the
plaintext: the secrets resolver releases only bindings the attempt's immutable
context declares (`Test_credential_lifecycle` proves both). Two exceptions narrow in the safe direction: disabling the
project, or turning recording off, takes effect on the next request of runs bound
earlier.

## Request state machine

```text
admitted --begin--> forwarding --finalize--> succeeded | failed | cancelled
   |                    |
   | finalize(unsent)   | finalize (remote completion uncertain) or abandon (worker lost)
   v                    v
 failed               unknown --settle (operator attestation)--> settled
```

A cancellation **before any send** (the interruption lands while the credential is
being prepared or before forwarding begins) is an unsent finalization: the only
exit from `admitted` besides forwarding is `failed`, so the request ends `failed`
with its cancellation reason code (`client_cancelled`, `attempt_cancelled`), the
`not_sent` uncertainty and a zero charge. The request count stays consumed and
the slot is released. A cancellation **after a possible send** follows the
`forwarding` path: the bound is charged and the request becomes `unknown`, holding
the slot.

`admitted`, `forwarding` and `unknown` **hold the run's single slot**; the
database enforces it with a partial unique index (`model_requests_one_unresolved`).
`RunBudget.Admit` is the one place the admission rules live: one slot, the request
cap, charged tokens below the allowance. Counters only increase (triggers), usage
is write-once, and a settled request is immutable.

**Admission** is one transaction: recheck the workload's current authority
(`runtimeauth.CheckSession` through `runtimeauth/postgres.Bind` and the execution
owner's `execpg.LockAttempt`, which reads the attempt and locks its run row), capture or read the binding, validate model and
limits, increment the request count, take the slot, insert the request. Nothing
reaches the provider before it commits; an unknown commit outcome means **do not
send**, and the orphan is reconciled as unsent (free of charge, the allowance it
consumed stays consumed). **Finalization** commits usage, the token charge, the
request outcome, an audit row and `usage.recorded` together, exactly once; an
unknown commit is reread by request identity, never repeated as a provider call.

**Lock orders.** There are two, and each is followed everywhere.

- *Workload admission, begin-forwarding and heartbeat:* the execution **run** row
  (`FOR SHARE`, taken by `execpg.LockAttempt` inside the workload recheck; the
  attempt is read, not locked independently), the runtime grant (`FOR SHARE`), the
  gateway binding/budget row (`FOR UPDATE`), the request row (`FOR UPDATE`), secret
  rows (pin registration), event append last. Finalization and reconciliation
  need no workload authority and start at the budget row.
- *Human settlement (`SettleUnknown`):* the actor's identity authorization
  revision (`FOR SHARE`, taken by the transaction-bound management binder, which
  rereads current bindings), then the gateway budget row, the request row, and the
  audit insert. No workload authority is taken: finishing an already started
  effect never requires the attempt to be live. A revocation committed before the
  transaction locks the revision is seen and refuses the settlement; one racing it
  waits for the settlement to commit.

Provider HTTP, credential decryption and key-provider calls, and content
recording never run inside a transaction.

## Tenant ownership

Every table is tenant owned: forced row level security with the `kodama.tenant_id`
setting, composite foreign keys to the project, run and attempt, column-scoped
grants to the non-owner runtime role (no `DELETE`, no `UPDATE` of policy or
identity columns) and triggers for the transitions grants cannot express.
Statements run inside `storage.WithTenant` and carry explicit tenant and project
predicates. No table has a column for prompts, completions, tool arguments,
provider error text or content digests. There is no hosted-to-local fallback: the
management authority is whatever `identity.Authority` the composition selects.

## Recovery

| Situation | Behavior |
| --- | --- |
| Provider stream truncated, idle, malformed, oversized; provider 5xx or timeout after a possible send | bound charged, request `unknown` and **the slot kept** until `SettleUnknown`, in-stream error, never retried |
| Client disconnect, deadline, attempt cancelled or revoked before any send | nothing sent; request `failed` with the cancellation reason, `not_sent`, zero charge, slot released |
| Client disconnect, deadline, attempt cancelled or revoked after a possible send | upstream cancelled, bound charged, request `unknown` and **the slot kept** (closing a connection is not proof the provider stopped) |
| Workload write failure or provider protocol failure while forwarding | the provider stream is closed **before** any outcome persistence starts, so a slow or unavailable database never keeps upstream work running; the failure classification, conservative charge and held slot are unchanged |
| Complete stream with missing or invalid usage | bound charged, request `succeeded`, slot released (the completion is known; only the token count is not) |
| Admission commit unknown | nothing sent; reconciled as unsent |
| Finalization fails or its commit is unknown | database-only retries (reread by identity); if never recorded, reconciliation settles it as `unknown` |
| Worker lost while `forwarding` | `Reconcile` charges the bound and makes it `unknown`; it **keeps the slot**. Lease expiry alone never reopens it |
| Late worker after reconciliation | `Finalize`/`Heartbeat` return `ErrSuperseded`; nothing changes |
| Event backlog full | the settlement rolls back (no usage row, no charge); the request stays `forwarding` until reconciliation |

`SettleUnknown(actor, scope, id)` is an internal management operation (there is no
HTTP route). It records an **authorized operator's attestation** that the remote
completion has been established outside the gateway, which does not itself
discover or verify it (a closed connection or expired lease is never such
evidence), and releases the `unknown` request's slot. It requires
`project:settings`, checked once beforehand and again inside the settlement
transaction. State change, slot release and the audit row (`model.request.settled`,
attributed to the human actor, not to `model-gateway`) commit together; a denial,
audit failure or duplicate leaves the request `unknown`, the slot held and the
usage record, charge and `usage.recorded` event untouched, and never calls the
provider. The charge stays. This sacrifices availability for safety: an ambiguous
remote completion blocks the run rather than permitting parallel overshoot.

## Credentials and destinations

A `kind=model` secret holds `{"version":1,"bearer":"<key>","origin":"https://host[:port]"}`
(`adapters.ModelCredentialPayload`). The origin is the credential's audience and
must equal the origin of the run's endpoint, or the key is refused rather than
sent elsewhere. `adapters.Credentials.Open` reads exactly the pinned version
(`secrets.VersionSource`), finalizes its audit and revocation check, then returns
a redacting `gateway.Credential` that the transport reveals once and destroys. If
the audit finalization fails or its commit is uncertain, nothing is released and no
provider request is sent; the request ends unsent and free of charge.

**Legacy delivery discriminator.** The existing secrets interfaces carry only the
runtime file/env delivery values and have no proxy-use discriminator. The adapter
therefore passes `runtimev1.DeliveryEnv` in `secrets.FinalizeInfo` and in the run
pin it registers. This is an adapter-local compatibility representation: it neither
declares an agent environment binding nor performs agent ENV delivery (the
plaintext goes only to the provider transport). The secrets/integration owner must
confirm this interpretation before assembly; no new shared enum is introduced here.

Endpoints must be approved by `openai.Policy` (installation-supplied origins with
an optional path prefix, no wildcard). The transport requires HTTPS with a valid
certificate, validates **every resolved address** at every connection against an
address policy (default: public unicast only, so loopback, private, link-local,
metadata, CGNAT and reserved ranges are refused; an operator may add explicit
prefixes), dials the validated address while keeping TLS server-name verification,
uses no environment proxy, keeps no idle connections and follows no redirect.

## Recording

Raw content recording is independent of usage and **off by default**. It is
effective for a request only when the run's binding, the current settings and a
wired `Recorder` all say yes; a workload field cannot enable it. With it off the
gateway never copies, hands over or stores content. With it on, the recorder gets
the bounded validated frames and the decoded request together with run, attempt and
request ownership after settlement; its failure is stored as
`recording_outcome = failed` and returned in `InvokeResult.RecordingFailed`, and
never changes the workload's stream or the usage record. Storage, retention and
deletion belong to the recorder's owner.

## Constructing the module (integration owner)

The composition root (not edited by T-032) supplies:

```go
// the gateway tables are in the baseline schema; no module contributes a migration
storage.NewManifest(storage.BaseContribution())

store, _ := gatewaypg.NewStore(gatewaypg.Config{
    DB: runtimePool, Events: adapters.EventsFrom(eventStore),
    Workload: txAuthority,            // adapters.NewTxAuthority(executionAttempts, signer.Generation)
    Management: adapters.BindManagement, Pins: pins, // adapters.NewPins(secretsStore)
})
svc, _ := gateway.NewService(gateway.Config{
    Store: store, Workloads: workloads,   // adapters.NewRuntimeWorkloads(runtimeService)
    Authority: authority,                 // adapters.NewAuthority(identityAuthority)
    Credentials: creds,                   // adapters.NewCredentials(secretspg.NewVersionSource(...), secretsStore)
    Provider: client,                     // openai.NewClient(openai.Config{Policy, TLS, Timeouts, Stream})
    Estimator: estimator, Decoder: openai.NewDecoder(), Destinations: policy,
    Recorder: nil /* optional */, Tenants: tenantSource,
})
h, _ := gatewayhttp.NewHandler(gatewayhttp.Config{Service: svc})
h.RegisterRoutes(router)                  // GET, PUT .../model-settings
mux.Handle("/gateway/v1/", h.Workload())  // POST .../chat/completions
```

All of these are required except the recorder and the tenant source; a missing
adapter fails construction and there is no allow-all fallback. `Tenants` is
required for `Reconcile` (which fails without it) although `NewService` accepts its
absence, so a composition that omits it must not schedule reconciliation and would
leave abandoned requests holding their slots. Approved destinations, address ranges, model
token accounting, timeouts and stream limits are explicit constructor inputs, not
environment variables.

**Reconciliation scheduling.** Call `Service.Reconcile(ctx, quiet)` periodically on
every replica (it is idempotent and safe concurrently). `quiet` must be at least
three `Timeouts.Recheck` intervals; use a value comfortably above the longest
credential preparation (default `Recheck` is 15 s).

**Management binder.** `adapters.BindManagement` and `identity.NewStandaloneAuthority`
are the standalone management authority only. Hosted authorization (O-18) is not
claimed and no hosted-to-local fallback exists; T-061 supplies the hosted binder.

**Transactions and locks.** PostgreSQL owns admission and the usage/outbox
atomicity: admission and finalization are single tenant transactions with the lock
order above. Provider HTTP, credential decryption and recording stay outside them.
The provider stream is closed before finalization so database latency cannot extend
upstream work, and finalization uses a bounded context detached from the caller.

**Recorder obligations.** A `Recorder` must persist or reject within `Timeouts.Record`,
must apply the authorized retention (an authorized opt-in; the initial policy is seven days under the approved retention baseline), must never
log content, and owns deletion. The gateway keeps no content itself.

**Execution seam.** `adapters.ExecutionAttempts` maps execution's attempt records
into the runtime authority's state. Execution does not yet expose the Job namespace
or a per-attempt action set, so both are installation inputs; replace them when it
does. Execution's secret snapshots pin step-delivered secrets, not gateway policy,
so the gateway registers its own exact-version pin under the run lock at first
admission (the integration owner confirms this timing).

**HTTP server.** Mount the workload handler on a server whose `WriteTimeout` is 0 or
generous: the sink sets a per-frame write deadline itself. A failed stream is
aborted with `panic(http.ErrAbortHandler)`, which `net/http` handles silently;
recover-all middleware must re-panic with it.

## T-031 obligations

Three things are kept distinct: the fixture manifest here (protocol-subset
compatibility against a controlled provider, not a qualified endpoint), T-074's
wrapper-protocol checkpoint, and T-031's real harness smoke test. T-031 owns the
choice of the OpenCode version, the smoke test against a selected provider and
model, and the evidence for it; because its evidence lives in its own paths, a
`manifest.json` change needs a handoff to this module's owner.
The harness must send only the accepted members, set `stream: true`, treat
`403 limit_exceeded` as non-retryable, treat a body that ends without `[DONE]` (or
with a `kodama_gateway` error event) as a failed request, and never retry a failed
stream automatically.

## Unit qualification and fixtures (T-083)

`go test ./internal/gateway/...` runs the gateway's unit gate without a service: the real
`gateway.Service` over `gatewaytest.MemStore` (a `gateway.Store` over `memdb`), a controlled
clock and scripted provider streams (`gatewaytest.ScriptedProvider`, no network). Nothing
here qualifies a provider, a model or OpenCode (T-085, T-031); the frozen protocol fixtures
remain protocol-subset evidence only.

| Test | Proves |
| --- | --- |
| `Test_unit_model_budget` (`unit_budget_test.go`) | GM-03/GM-04/GM-05/GM-06/GM-12: bounds refuse before provider traffic and consume nothing; observed usage is charged exactly and admission stops at the allowance within the published overshoot; request cap; uncertain outcomes (no/invalid usage, truncation, provider error/timeout, protocol violation, usage above bounds) charge the bound and, when remote completion is unknown, hold the single slot until an operator settles; failures before any send are free; cancellation, deadline and a revoked attempt stop the upstream; usage, events and audit rows are attributed to the verified workload and tenant and carry no content or credential; a run keeps its exact credential version; reconciliation; lost/rolled-back commits never repeat the provider call; rejected protocol fixtures never reach the provider and no support is claimed |
| `Test_unit_agents_gateway_contract` (`gatewaytest/`) | SD-13/SD-A11: `gatewaytest.Contract(t, open)` on `MemStore`: settings revisions with audit and event, atomic admission with the one slot, request cap and allowance, charge by outcome, exactly-once finalization, reconciliation, operator settlement, tenant/project isolation, workload authority, all-or-nothing writes |

`MemStore` applies the same entity transitions as the PostgreSQL store inside one `memdb`
tenant transaction (settings revisions, run binding with its credential pin, budget,
request state machine, write-once usage, audit entry and `usage.recorded` event commit
together or roll back together). Rows are copied on the way in and out, so mutating a
supplied or returned allow-list, binding, usage or request pointer, or an inspected event
or audit payload, cannot change stored policy or history; admission only inserts, so a
request identity that exists in the project (another run's, or one already settled) is
refused with `ErrInvalid` and changes nothing (the PostgreSQL primary key, mapped to the
same class). It does **not** model SQL, row level security, locking, lock order or restart
persistence.

The same `Contract` runs on PostgreSQL as `Test_gateway_store_contract`
(`integration/contract_test.go`, behind `layer.SkipService`) over `gatewaytest.PGHarness`:
a fresh database per case with real runtime-authority attempts as workloads (`Revoke`
cancels the run), the real identity authority, the model secret at versions one and two in
each fixture project, and an event appender that can be made to fail. It runs at the
integration checkpoint (`KODAMA_REQUIRE_POSTGRES=1 go test ./internal/gateway/integration/
-run Test_gateway_store_contract` with `KODAMA_TEST_POSTGRES_ADMIN_URL` set), not in the
unit gate.

**Fixture example for T-072:**

```go
u := gatewaytest.NewUnit(t, gatewaytest.UnitOptions{})   // Service + MemStore + clock + scripted provider
u.Configure(gatewaytest.ScopeA, nil)                    // enable model access as the project admin
a := u.NewAttempt(gatewaytest.ScopeA)                   // an attempt of a new run, authorized for model.invoke
u.Provider.Script(gatewaytest.Reply("gpt-a-2026", 40, 10, "hello"))
res, err := u.Invoke(a, gatewaytest.Body("gpt-a", "hi", 64), &gatewaytest.Sink{})
admitted, charged := u.Store.Budget(a.Workload.Scope, a.Workload.RunID)
u.Clock.Advance(10 * time.Minute)                       // then u.Svc.Reconcile(ctx, 5*time.Minute)
```

Composition tests that need only the store use `gatewaytest.NewMemStore(workloadCheck, management)`
(`MemAuthority.Check` and `MemManagement` are the table authorities); step builders are
`Reply`, `ReplyNoUsage`, `ReplyInvalidUsage`, `ReplyTruncated`, `ReplyHang`, `ReplyStatus` and `ReplyFault`.
Faults: `MemStore.FailCommits`/`LoseNextCommits` (unknown commit outcomes) and `FailEvents` (a full outbox).

## Tests

Run `KODAMA_REQUIRE_POSTGRES=1 go test -race -count=1 ./internal/gateway/...` with
`KODAMA_TEST_POSTGRES_ADMIN_URL` set (resource prefix `t032`).

| Test | Proves |
| --- | --- |
| `Test_cross_replica_admission` | GM-03/GM-A02: single slot and cap across replicas and attempts; rollback consumes nothing |
| `Test_bounded_token_overshoot` | GM-03/GM-04: limits, observed and uncertain charging, the published overshoot bound |
| `Test_pre_forward_cancellation` | GM-05/GM-06: cancellation or deadline before any send persists `failed`/`not_sent`, zero charge, slot released |
| `Test_partial_stream` | GM-05/GM-06/GM-A03: visible partial failure, no retry, cancellation and deadlines, upstream stopped before a blocked finalization is released |
| `Test_redirect_credential` | GM-02/GM-09/GM-A06: no request or credential beyond the approved destination |
| `Test_content_retention_off`, `Test_recording_authorization` | GM-11/GM-A08: no content kept, authorized recording |
| `Test_gateway_authority`, `Test_credential_lifecycle` | GM-01/GM-02/GM-A01: fail-closed identity, exact-version credentials, proxy pins that neither alter the attempt context nor authorize plaintext resolution, audit-failure closure |
| `Test_gateway_happy_path` | attributable operational usage, one audit row and `usage.recorded`, no commercial member or content (not real-harness qualification) |
| `Test_gateway_recovery` | crashes, lost commits, duplicate finalization, stale workers, event backlog |
| `Test_gateway_storage_isolation`, `Test_gateway_settings_revisions` | RLS, ownership, grants, immutability, migration reapplication, atomic rollback |
| `Test_gateway_transitions` | entity transitions and overflow |
| `Test_gateway_http_contract`, `Test_gateway_workload_route` | OpenAPI/DTO agreement, strict input, ETags, safe errors, separate authentication, streaming |
| `Test_chat_completions_compatibility` | frozen protocol fixtures (not the real harness smoke test) |
