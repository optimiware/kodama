# T-030 review notes

Candidate-specific evidence lives in the harness handoff; this file records what a reviewer should
check and the assumptions the integration owner confirms.

## Baseline

* **Subject:** T-030 agent profile expansion, repository candidate based on
  `9f045af6775e2ae6e3199220e1d7d6455a4b8fdf`. Runtime records carry tenant/project ownership; no
  tenant is selected by this implementation.
* **Requirements:** AR-01, AR-02, AR-03, AR-04, AR-10, AR-12, AR-13; supporting SD-01–SD-08 and the
  existing tenant, secret-resolution and runtime-result invariants.
* **Acceptance:** `profile_pinning`, `unknown_wrapper`, `needs_input_failure`, `agent_no_human_role`;
  the applicable parts of AR-A01, AR-A02, AR-A03, AR-A05, AR-A07 and the pinned-context rule of AR-A08.
  Real OpenCode compatibility is T-031's acceptance.
* **Decisions:** confirmed C-01–C-03, C-05, C-09–C-13, C-15–C-17; baseline P-01, P-04–P-08, P-13–P-16.
  ADRs 0001, 0003, 0005, 0006, 0007. O-18/O-19 are not resolved and no hosted delegation or credential
  support is claimed.
* **Boundaries:** agents owns profile registration, immutable versions, validation, expansion and
  adapter contributions. Execution owns run/step/attempt transitions, admission transactions, result
  acceptance and recovery. Secrets owns exact-version resolution; artifacts owns transcript storage and
  access; the gateway and proxy own request admission and usage. Infrastructure imports stay in adapters.
  Composition belongs to T-039.

## What to check

* `agents` imports no pgx, net/http, provider or execution package; `execution` imports nothing of
  `agents` (`agent-resolver.go` is the consumer-owned seam).
* Profile versions are append-only: no update or delete path, forced RLS with no UPDATE/DELETE policy,
  runtime grants `SELECT, INSERT` only, digest and name checked against the stored body.
* Authorization precedes parsing, qualification and persistence in `Service.Register`.
* No transaction contains a network call; `Planner.Plan` performs no profile lookup.
* The chain (`adapters.Chain`) is complete or refused: store decorator, keyed-creator decorator, planner,
  hooks and capabilities.
* A frozen step's payload round-trips (`agents.DecodeArgs`, `execution.EncodeStep/DecodeStep`) and is
  recomputable against its digest.

## Assumptions

1. **Execution integration.** T-020's `Hooks` only enables step kinds; it does not resolve profiles.
   The bridge is three decorators under `adapters/` (store, keyed creator, planner), expanding before
   admission persists the immutable `StepRun.Spec`; registered pipeline definitions are never rewritten.
   T-030 review and T-039 must confirm and wire all three together (`adapters.NewChain`).
2. **Wrapper configuration transport.** Runtime v1 has no instruction/model/tool-budget payload, so a
   bounded, versioned, nonsecret payload travels in the step's command arguments after the marker
   `kodama.agent-config/v1` (base64url chunks). No environment variable or OpenCode flag is invented.
   T-031 confirms consumption before a production compatibility entry is enabled.
3. **Compatibility and enforcement.** An explicit compatibility catalogue (image, wrapper, entrypoint,
   harness, controls, mandatory evidence) is injected; an empty one rejects registration, and fixtures
   never establish production support. T-031 supplies wrapper evidence; T-032/T-033 supply model, request,
   token and tool enforcement. T-039 must not enable profiles whose claimed controls lack those.
4. **Reference syntax.** `name@version` (the server-returned immutable reference); moving aliases are
   rejected. Recorded in `api/agents.yaml` and the README.
5. **Management permissions.** Reads map to `run:read` and registration to `run:manage` through an injected
   identity adapter, with no new role. T-030 review and the integration owner must confirm this mapping.
6. **Metadata linkage.** Attempt results, diagnostic artifact IDs and the pinned context link transcript
   and result metadata; T-032 stays authoritative for model usage. There is no agent-session table and no
   second usage ledger. `MetadataReader` and `PolicyReader` payloads are module-local and need confirmation
   with T-031/T-032.

### Additional assumptions made during implementation

7. **Run-wide allowances.** One run has one request and one token allowance, so all pending agent steps of a
   run must declare identical `maxRequests`/`maxTokens` (including none); any difference, including claimed
   versus unclaimed, refuses admission. This is conservative and is the module's reading of AR-04 plus
   the gateway's "run allowance"; T-032 confirms.
8. **Replay re-expansion.** `CreateKeyed` takes its plan up front, so the keyed decorator expands before the
   idempotency lookup. A replay therefore repeats a deterministic expansion; if the profile were withdrawn or
   unqualified between the request and its replay, the replay would be refused instead of returning the stored
   response. Making replay independent of the profile store needs a lazy plan hook in `execpg.Keyed`
   (T-020's file); recorded for the integration owner.
9. **Resolution re-checks policy.** `Service.Resolve` (lookup and admission) re-qualifies the tuple and
   re-checks the model/tool approvals, so a withdrawn approval or catalogue entry fails closed for new runs
   without changing the stored version. Lookup reports such a version as not immutable.
10. **Container steps and the marker.** A container step whose arguments start with the payload marker is
    refused at admission so it cannot claim a profile pin or poison dispatch.
11. **Audit and events.** Registration writes one audit row (name, version, digest; never content) and no
    event; no new event type is introduced.
12. **Instruction hygiene.** Instructions may contain any plain text up to 64 KiB; the payload transport
    (base64url) keeps expression markers out of resolved arguments. Credentials in text are not detectable and
    are the author's responsibility; the model endpoint is checked by syntax (no userinfo, query or fragment),
    not by path content.
