# T-033 review notes

Candidate-specific evidence lives in the harness handoff; this file records what a reviewer should check
and the assumptions the integration owner confirms.

## Baseline

* **Subject:** T-033 generic Streamable HTTP MCP proxy, based on `deff2e17233f4ec13506f2dc460f9179c7da90b4`.
  Runtime records carry tenant/project ownership; no tenant is selected by this implementation.
* **Requirements:** GM-01, GM-02, GM-07–GM-12; supporting GM-06 cancellation, AR-04/AR-05, IT-02–IT-06/IT-11,
  applicable VS exact-version and revocation requirements, SD-01–SD-08.
* **Acceptance:** GM-A01, GM-A04–GM-A07; the cancellation and recording portions of GM-A03/GM-A08. Named tests:
  `hidden_tool_direct_call`, `foreign_session`, `remote_token_audience`, `unknown_side_effect`, `protocol_smoke`
  (Go: `Test_hidden_tool_direct_call` etc. in `integration/acceptance_test.go`).
* **Decisions:** confirmed C-01, C-09, C-10, C-15–C-17; ADR 0006 and ADR 0007. O-18/O-19 have resolved directions
  (Optimi OAuth authority; KMS-encrypted PostgreSQL secrets with pins and proxy-first delivery) but no hosted
  evidence: Platform delegation, hosted credential custody/version interface and a recipient-bound hosted token
  remain excluded here and are qualified by T-061. Executable assembly is T-072, the full M2 join T-039.
  O-10/O-11 production qualification is not resolved here.
* **Boundaries:** `mcpproxy` owns registrations, run bindings, remote sessions, call admission/outcomes and the
  proxy's audit. Membership, attempts, profile pins and encrypted secret versions remain with identity,
  execution/runtimeauth, agents and secrets. Composition is T-039.

## What to check

* `mcpproxy` (the package itself) imports no SDK, `net/http` or pgx; peer modules are imported only under
  `adapters/`, `postgres/`, `protocol/` and `http/`.
* Every runtime request authenticates `tool.invoke` before the SDK sees it; `tools/list` and `tools/call`
  authorize again and compare the complete caller tuple; the proxy-local session ID is the only one exposed.
* Discovery and invocation use the same `Visible` intersection; a hidden tool never reaches the remote.
* No transaction contains a network call. The dispatch intent is durable (`Admit`) before any remote I/O; a lost
  or uncertain admission commit never dispatches; a lost outcome is `unknown`, final (database guard) and
  never retried.
* The run-wide call counter is charged at admission and never refunded; duplicates charge nothing.
* A registration edit or deletion invalidates sessions in the same transaction; frozen run bindings are refused,
  not replaced.
* Current use (authority, session, registration revision, exact credential version and run pin) is checked from
  durable state, never cached, before discovery reaches the remote, inside `Admit` before any charge, and while a
  call or listing is in flight. A definitive verdict ends the session; an outage denies that use and records
  nothing; an in-flight call stops locally and becomes `unknown` (`authority_ended`/`authority_unavailable`).
  Unit evidence: `service_use_test.go`, `protocol/server_use_test.go`; SQL/locking evidence needs the real-database
  suites.
* An ended or unconfirmed credential is never presented to the remote, including for session termination: only a
  deliberate close after a successful current-use check sends the terminate request; every other end erases the
  credential first (`RemoteConn.Revoke`) and lets the remote session lapse; a call stopped by a failed check
  erases the credential before cancelling (the SDK's cancel notification would otherwise carry it), except when
  the check could not be answered. Failed initialization closes the same way. Residual windows (a request already
  in flight when the credential ends; check-to-request) are not claimed away. Evidence:
  `integration/credential_close_test.go` (direct close, workload terminate and sweep after revocation and release,
  a control where a usable close does terminate, and an in-flight cancellation after revocation/release),
  `service_use_test.go` (failed initialization, revoke-before-cancel ordering) and the credential lifecycle suite.
* No relation holds a credential, argument, result or remote session identifier; audit metadata holds identifiers
  and classified outcomes only.
* Destination policy: exact approved host:port, validated DNS answers dialed directly, no redirects, no ambient
  proxies, credential only to the approved origin.

## Assumptions (for the integration owner to confirm)

1. **Tool addressing.** T-030 stores `Tools []string` with no encoding. This module defines the exact qualified name
   `<registrationID>/<toolName>`, parsed by `mcpproxy.ParseQualified`; bare names and wildcards grant nothing.
   T-030's string bounds (≤128 bytes, `[A-Za-z0-9][A-Za-z0-9_.:/-]*`) admit it. T-031/T-039 confirm.
2. **Integration credential binding.** T-012's `VersionSource` requires a retained run pin, while MCP registrations
   are not execution secret declarations. On first authorized use the proxy freezes the registration revision and
   exact credential version in `mcp_run_bindings` and registers the run pin through `RegisterPins` in the same
   transaction, not through workload-delivered bindings. T-012/T-039 confirm.
3. **Credential payload.** Strict bounded JSON with an explicit recipient (see README). T-012 leaves provider
   payloads to consumers; the codec lives in `adapters/credentials.go`.
4. **Operational policy.** Session lifetime, call deadline, recheck interval, recovery age, content bounds,
   discovery pages and approved destinations are injected typed configuration. SDK sessions are process-owned; a
   different process or restart rejects a lost session. T-039 confirms configuration and routing.
5. **Management authorization.** `project:read` lists; `project:settings` AND `credential:manage` mutate. No new
   role or permission. Isolated in `adapters/identity.go`; T-010/T-039 confirm.
6. **Claimed limits and recording.** `MaxToolCalls` is enforced run-wide at admission (retries do not reset it,
   uncertain calls stay charged). Raw recording is off unless a recorder is configured, `Options.RecordRaw` is
   set and the pinned transcript policy allows it. T-030/T-039 confirm.
7. **Secret metadata read.** Registration validation and `CheckSecret` read the secrets module's `secrets` and
   `secret_versions` rows (kind, deletion, revocation) with a tenant-scoped, read-only SQL query in the proxy's
   store, because the secrets module exposes no by-name kind lookup. It reads metadata only. T-012 may offer a
   narrower reader later.
8. **Request identity for duplicates.** The SDK does not expose the JSON-RPC request ID to handlers, so the
   workload-facing handler assigns a fresh receipt ID per `tools/call`. Duplicate protection therefore covers the
   service API, crash recovery and lost admission replies; a workload that deliberately re-sends a call over MCP
   makes a new, separately admitted, separately charged call. The proxy itself never re-sends.
9. **Run foreign key.** `mcp_run_bindings.run_id` and `mcp_run_calls.run_id` have no foreign key to `runs`, so the
   migration does not require the execution contribution; the integration owner may add it.
10. **Local session identifiers.** Session IDs are `mcs_` plus 128 random bits and are bearer-like only together
    with the full caller tuple; they are not logged, audited or stored beyond `mcp_sessions.id`.

## Support exclusions

Compatibility is shown for the Go SDK `v1.8.0` client and server on revision `2025-11-25` only. Claims about
OpenCode, other MCP SDKs, other revisions, SSE-only or stdio transports, resource/prompt/sampling relay, delegated
OAuth, Platform tools and production egress isolation are not made here.
