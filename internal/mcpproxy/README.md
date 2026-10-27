# internal/mcpproxy

The Streamable HTTP MCP proxy (task T-033): it exposes only approved remote tools to the active
attempt, under the attempt's identity, using integration-owned credentials that never reach the
workload. Specification: [AI gateway and MCP proxy](../../docs/specs/ai-gateway-and-mcp-proxy.md)
(GM-01, GM-02, GM-07–GM-12; supporting GM-06 cancellation) with AR-04/AR-05, IT-02–IT-06/IT-11 and
SD-01–SD-08.

Status: implemented and tested (entities without infrastructure; the PostgreSQL store, SDK adapters,
destination policy, bridges and HTTP routes against real PostgreSQL, the real runtime-authority and
secrets modules and a local TLS MCP server). Nothing mounts these routes or adapters yet;
that is the integration owner's job (T-039), with the confirmations in [review.md](review.md).

It does **not** claim: OpenCode compatibility (T-031 qualifies the real harness; the fixture proves
only the declared SDK/protocol combination), delegated end-user OAuth or Optimi Platform tool access
(O-18/T-061 own the recipient-bound hosted integration), an argument-policy language, other
transports, automatic retries of any tool effect, or production egress isolation (the adapter checks
do not deploy Kubernetes network policy).

## Layout

| Path | Purpose |
| --- | --- |
| `registration.go`, `session.go`, `invocation.go`, `policy.go` | Entities and pure rules: `Registration` (expected-version edits, tombstone), `RemoteSession` (caller-tuple binding, expiry, terminal states), `Invocation` (admitted → dispatching → succeeded/failed/unknown), qualified-tool `ToolPolicy` |
| `ports.go`, `service.go`, `errors.go` | Consumer-owned ports (`Store`, `RuntimeAuthority`, `HumanAuthority`, `AttemptPolicy`, `CredentialSource`, `RemoteTools`, `Destinations`, `SecretChecker`, optional `ContentRecorder`) and the use cases |
| `postgres/` | `Store`; the proxy tables are in the baseline schema (`internal/storage/migrations/001_schema.sql`) |
| `protocol/` | Pinned SDK/protocol (`compatibility.go`), approved-destination policy and TLS transport, the SDK remote client (`RemoteTools`) and the workload-facing Streamable HTTP handler |
| `adapters/` | Bridges: `RuntimeAuthority` (runtimeauth), `HumanAuthority` (identity), `Policy` (execution + agents `PolicyReader`), `Credentials` and `Pins` (secrets), `ToolApprovals` (agents approvals contribution) |
| `http/` | The four registration routes, DTO/error mapping, generated types (`mcp.gen.go`) |
| `mcptest/` | Test-only stack: migrated database, runtime authority, secrets, fixtures. Imported only by tests |
| `mcptest/mcpcontract/` | The one exported `Contract(t, open)` suite of the `Store` port (SD-13): scoped registrations, frozen bindings, caller-bound sessions, protected use, idempotent admission, the run ceiling, final outcomes, stale-receipt recovery |
| `mcptest/memory/` | Development-only memory adapter (T-084): `Store` over `memdb` with fake current runtime authority, MCP secrets and pins, a scripted deterministic remote and policy/human/destination ports. Runs the contract and the `unit_tool_authority` test with no service, network or key provider. Not modelled: SQL, locks and lock order, RLS, the SDK/Streamable HTTP protocol, DNS/TLS destination checks, real key unwrapping. `mcptest.PostgresHarness` runs the same contract on PostgreSQL via `integration/contract_test.go` (skipped in the unit layer) |
| `integration/` | The five named acceptance tests plus cancellation, credential, recording and sweep tests |
| `../../test/support/mcp/` | Local TLS MCP server with fault controls (see its README) |
| `../../api/mcp.yaml` | OpenAPI 3.0.3 fragment: registrations and the runtime MCP endpoint |

Dependencies point inward: `mcpproxy` imports `domain` and `secrets.ValidateSecretName` only; the SDK,
`net/http`, pgx and every peer-module bridge stay in the sub-packages.

## Model

**Registration.** Tenant, project and ID are immutable. Content: a normalized `https` endpoint (no
userinfo, query or fragment), an exact secret reference (the name of an MCP-kind project secret and
one exact value version, never "current") and sorted enabled tool names. Create, expected-version edit
and tombstone deletion are the only transitions. Creation and edits require the endpoint to be an
approved destination and the secret to be a live, non-revoked MCP-kind secret of the same project.

**Tool addressing.** A profile names tools as `<registrationID>/<toolName>`. A bare name or a wildcard
grants nothing. Discovery and invocation both expose exactly the intersection of the registration's
enabled tools and the profile's pinned list; a tool outside it fails like an unknown tool.

**Run binding.** The first authorized use of a registration by a run freezes the registration revision
and exact credential version in `mcp_run_bindings` and registers the matching run pin through the
secrets module's `RegisterPins`, in one transaction. A later registration change makes the binding
unusable (`ErrBindingInvalid`): the credential is never silently replaced. The proxy-owned pin is not
part of the secret bindings delivered to the workload.

**Credential payload.** The MCP secret's value is strict JSON `{"version":1,"recipient":"https://host[:port]","token":"..."}`.
The recipient must equal the registration endpoint's origin. This is integration-owned bearer
configuration; it does not prove a JWT audience and is not Platform delegation.

**Session.** `initializing → active → closed | expired | invalid` (or `failed`). The workload only ever
sees the proxy-local session ID; the remote server's own ID never leaves the proxy. Every request
authenticates the attempt token for `tool.invoke`; tools methods authorize again and compare the
complete caller tuple (tenant, project, run, attempt, grant session). Copied, expired, closed,
invalidated, credential-ended and process-lost sessions are all the same not-found. SDK sessions are process-owned: a
session recorded by another process incarnation is invalidated, never adopted or replayed.

**Current use.** Cached authority never extends use of a session. `Store.CheckSessionUse` answers from
durable state, with no network or key-provider call: runtime authority, session ownership/expiry/process,
the frozen registration revision and the exact frozen credential version (not revoked, run pin
unreleased; a deleted secret whose pinned version remains is still usable by that run). It runs before
discovery contacts the remote, inside `Admit` before anything is charged, and on every recheck interval
while a call or slow listing is in flight. A definitive verdict (`ErrCredential`, `ErrBindingInvalid`,
`ErrSessionInvalid`) invalidates the session and drops its live handle; authority loss denies without a
session verdict; a check that cannot be answered (`ErrUnavailable`) denies that use only and records
nothing, so the session works again when the dependency recovers. An in-flight call or listing is
cancelled on any of these, and a dispatched call becomes `unknown` with code `authority_ended` or
`authority_unavailable`: never claimed rolled back, never replayed.

**Ending a session.** A credential is only ever sent when something has just confirmed it may be used.
The remote session is therefore terminated (with the credential) only by a deliberate close from the
authorized caller whose current-use check succeeds at that moment. Every other end (expiry, the sweep,
shutdown, an invalidation, an abandoned initialization, or a close whose check fails or cannot be
answered) erases the credential copy first and drops the connection without any request; the remote
session then lapses on the remote's own timer. The local session is closed either way. A call or
listing stopped because its check found the credential or authority ended erases the credential before
it is cancelled, so the SDK's cancellation notification is not sent with it. Two windows remain and are
not claimed away: a request already past the header copy when the credential ends, and the interval
between a successful check and the request it permits (which includes the cancellation notification
the SDK sends when the caller cancels or the call deadline passes: the last check was at most one
recheck interval earlier). Loss of the attempt's authority during a call erases the handle's
credential without recording a session verdict; the session ends with the attempt and that handle is
not usable again. When the check merely cannot be answered the
handle is left intact (an outage proves nothing about the credential), so that stopped call's
cancellation notification does carry it; the check had not found it unusable.

**Call.** Admission is one transaction: current runtime authority (execution lock before grant lock),
registration `FOR SHARE`, session validity, the frozen credential version and pin (`FOR SHARE`, in the
secrets module's version-then-pin order), the receipt insert (a duplicate request ID never charges or
dispatches), the run-wide call counter against the pinned `MaxToolCalls`, and the audit entry. A commit
of unknown outcome means no dispatch. The remote is called outside any SQL transaction; the outcome is
recorded in a second short transaction. A reply that is lost, cancelled or timed out is `unknown`, final,
surfaced as an `isError` result with `_meta` (`kodama/outcome`, `kodama/requestId`, `kodama/code`), never
retried and never described as rolled back. A failed outcome write leaves a `dispatching` receipt that
`Sweep`/`RecoverStale` turns into `unknown`. Every tool is treated as side-effecting regardless of
remote "read-only" hints.

**Network.** HTTPS only; approved `host[:port]` entries only; the DNS answer is validated (any
prohibited address rejects the whole answer) and the validated IP is dialed directly while TLS still
verifies the approved host name; redirects are not followed; ambient proxies are disabled; the
credential is added only to requests for the approved origin. Private-network destinations need the
explicit `AllowPrivate` installation policy; link-local, metadata, unspecified and multicast ranges are
always refused.

**Compatibility pin (GM-12).** `protocol/compatibility.go` records the official Go SDK
(`github.com/modelcontextprotocol/go-sdk v1.8.0`) and the single supported revision `2025-11-25` over
Streamable HTTP. Both legs are narrowed to that revision; the SDK's newer default (`2026-07-28`) is not
used. Only the `tools` capability is advertised; resources, prompts, sampling, elicitation and logging
are not relayed.

## Integration obligations (T-039)

1. **Migrations.** Nothing to register: the proxy tables are in the baseline schema
   (`internal/storage/migrations/001_schema.sql`) applied by `storage.BaseContribution()`.
   Consider adding a foreign key `mcp_run_bindings(tenant_id, project_id, run_id) → runs` as a later
   contribution after the baseline; the baseline does not declare it.
2. **Construction.** `postgres.NewStore(db, postgres.Options{Recheck: runtimeAuthority.Recheck, Pin: adapters.Pins(secretsStore)})`;
   `mcpproxy.NewService(Config{...})` with `adapters.NewRuntimeAuthority` (same `AttemptAuthority` and signing
   generation as runtimeauth's store), `adapters.NewHumanAuthority`, `adapters.NewPolicy`,
   `adapters.NewCredentials(secretspg.NewVersionSource(...))`, `protocol.NewDestinations` and `protocol.NewRemote`.
3. **Routes.** `mcpproxyhttp.NewHandler(service, cursorCodec).RegisterRoutes(router)` under `/api/v1`, with the
   router authorizer mapping `mcp_server.list` → project:read and `mcp_server.mutate` → project:settings and
   credential:manage. Mount `protocol.NewHandler(...)` on `protocol.RoutePattern` outside `/api/v1` with
   `protocol.MaxHeaderBytes`; the attempt token is its only credential.
4. **Session affinity.** Live remote handles are process-local. Route all requests of a session to the process
   that opened it (or accept that another replica answers 404 and the session is invalidated). Do not add a
   retry that re-sends `tools/call`.
5. **Sweep.** Call `Service.Sweep(ctx, tenant, limit)` periodically per tenant (expires sessions, closes handles,
   recovers stale `dispatching` receipts as `unknown`); call `Service.Shutdown()` on exit.
6. **Approvals.** Compose `adapters.ToolApprovals` with the model approval adapter via `adapters.ComposeApprovals`
   for `agents.Approvals`. T-030's compatibility catalogue may claim `tool_calls` and `tool_allowlist` only for
   installations that run this proxy.
7. **Configuration.** `mcpproxy.Options` (session lifetime, call deadline, recheck interval, recovery age, content
   bounds, discovery pages, raw recording) and `protocol.DestinationConfig` are typed, with no permissive defaults
   and no new environment variables. The destination allow-list is the installation's approved-destination policy.
8. **Egress.** The adapter checks do not deploy Kubernetes network policy; deploy egress controls separately.
