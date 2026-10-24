# OpenCode wrapper protocol checkpoint

Task T-074 records, early, what an OpenCode wrapper, the model gateway and the MCP proxy must agree on, so T-031, T-032 and T-033 can proceed without rediscovering it. It is a **documentation checkpoint**: it selects no flag, no schema, no provider and no pin, and it changes no adapter or frozen contract. C-15 and O-04 select the direction (OpenCode headless over a configurable OpenAI-compatible endpoint); the exact version, invocation and compatibility evidence remain T-031's to produce after gateway and MCP merge.

Machine-readable evidence lives beside this document in [`contracts/harness-opencode/`](../../contracts/harness-opencode/README.md): [`sources.json`](../../contracts/harness-opencode/sources.json) (source index, including each unresolved gap) and [`cases.json`](../../contracts/harness-opencode/cases.json) (synthetic boundary cases). This document and those files are self-contained; a dependent task does not need the coordinator's gitignored `.harness/` packet.

Requirements served: AR-02, AR-05, AR-06, AR-09, AR-11, GM-12, with supporting AR-01, AR-04, AR-10, AR-13, AR-14, GM-05, GM-07, GM-08, GM-10, GM-11. Acceptance scenarios: AR-A01, AR-A02, AR-A04, AR-A06, AR-A07, GM-A05. The [agent runtime](../specs/agent-runtime.md) and [gateway and MCP proxy](../specs/ai-gateway-and-mcp-proxy.md) specifications own the requirements.

## Status and baseline

- Repository baseline: `80cd84f`, branch `task/T-074-wrapper-protocol`. No live tenant, project, provider or image was used.
- Candidate for qualification: **OpenCode `2.1.2`**, the build the coordinator measured on 2026-10-21. It is not a production pin, a qualified image, or a compatibility-catalogue entry (`agents.Catalogue` stays empty for it).
- Retrieval date for every upstream source: **2026-10-21**. Upstream pages are unversioned living pages and give no release or commit, so no immutable upstream revision is recorded (gap U-11).
- Disposition: a conforming checkpoint **with open qualification gaps**. The gaps block dependent qualification, not this checkpoint.

## Evidence classes

Every claim below carries one of these labels, and every id resolves in `sources.json`.

| Class | Meaning | Examples (ids) |
| --- | --- | --- |
| **documented** | Stated on an official OpenCode V2 page or in its served OpenAPI schema. Read, not run. | `oc-doc-cli`, `oc-doc-permissions`, `oc-doc-mcp`, `oc-doc-migrate`, `oc-doc-openapi`, `oc-doc-index` |
| **coordinator-measured** | Observed by the coordinator running the local OpenCode `2.1.2` binary. Second-hand: the T-074 implementer did not run the binary. | `oc-meas-version`, `oc-meas-run-help`, `oc-meas-removed-flags`, `oc-meas-ndjson` |
| **local-contract** | A merged Kodama specification, frozen contract or implementation at `80cd84f`. | `kodama-spec-*`, `kodama-runtime-contract`, `kodama-entrypoint`, `kodama-agents`, `kodama-gateway`, `kodama-mcpproxy` |
| **synthetic-fixture** | An invented, contract-derived expectation in `cases.json`. Never captured from OpenCode. | `wp-synthetic-cases` |
| **unresolved** | No primary source and no measurement. Recorded as a gap, not filled by guesswork. | `wp-gap-*` (U-01 to U-12) |

The coordinator's packet also reported the OpenAPI schema as a "measured HTTP surface". It is a served schema; no route was exercised, so this document classes it as documented. The implementer re-fetched the five documentation pages and the OpenAPI schema on 2026-10-21 (all HTTP 200) and confirmed the statements marked "confirmed" in `sources.json`; the migration-guide statements are taken from the packet and were not re-verified.

## Documented upstream facts

All from `https://opencode.ai/v2/…`, retrieved 2026-10-21, attributed to the OpenCode project's V2 documentation.

- **CLI** (`/docs/cli/`): `opencode run` is the non-interactive entry for scripts and CI. By default OpenCode starts or discovers one shared background server per user account that "owns sessions, configuration, integrations, permissions, and tool execution"; `--standalone` runs a private server and `--server <url>` connects to an explicit one. The page lists **no** `run` flags (`--dir`, `--pure`, `--format` and `--auto` do not appear on it) and says nothing about exit codes, signals or stdin.
- **Permissions** (`/docs/permissions/`): top-level `permissions` is an ordered rule list (`action`, `resource`, `effect`); MCP tools match `<server>_<tool>`; `shell` runs with the host user's filesystem, process and network authority and "directory inference from command text is best effort". `permission`, `bash` and `task` are V1 names.
- **MCP servers** (`/docs/mcp-servers/`): servers are declared under `mcp.servers`; remote servers use **MCP Streamable HTTP** with an absolute URL; `oauth: false` is for header credentials; Code Mode is on by default and groups tools as `tools.<server>.<tool>`; permissions can hide or deny tools; server instructions are injected into the system prompt; `tools/call` carries `params._meta["ai.opencode/sessionID"]`, an opaque correlation value that must not be used alone for authentication or authorization; `protocol` accepts `legacy` (default), `auto` or `2026-07-28`. Its "Protocol version" section documents that `legacy` (the default) sends `initialize` and speaks protocol revisions up to `2025-11-25`, while `auto` probes with `server/discover` for `2026-07-28` and falls back to `legacy` when that is unsupported.
- **Migration** (`/docs/migrate-v1/`, packet summary): V1 `.opencode/` definitions are intended to stay compatible; API and plugin APIs are intentional breaks.

### HTTP session surface (served OpenAPI, documented)

Source `https://opencode.ai/v2/openapi.json`: OpenAPI 3.1.0, title `opencode HttpApi`, version `0.0.1`, description "Experimental HttpApi surface for selected instance routes"; sha256 `c45c6cfc…efa511b5` (full value in `sources.json`). Routes relevant to a wrapper:

| Need | Route |
| --- | --- |
| Create / list sessions | `POST` / `GET /api/session` |
| Send a prompt | `POST /api/session/{sessionID}/prompt` |
| One-shot generate | `POST /api/session/{sessionID}/generate` |
| Event stream | `GET /api/event` |
| Read messages | `GET /api/session/{sessionID}/message` |
| Interrupt | `POST /api/session/{sessionID}/interrupt` |
| Active sessions | `GET /api/session/active` |
| Delete a session | `DELETE /api/session/{sessionID}` |
| Diff / context | `GET /api/session/{sessionID}/diff`, `GET /api/session/{sessionID}/context` |
| Permission prompts | `GET`/`POST /api/session/{sessionID}/permission`, `POST …/permission/{requestID}/reply` |
| Form prompts | `GET`/`POST /api/session/{sessionID}/form`, `POST …/form/{formID}/reply` (relation to `question` unread) |
| MCP status | `GET /api/mcp`; `PUT`/`DELETE`/`POST …/connect`/`…/disconnect` under `/api/experimental/mcp/{server}` |
| Server info | `GET /api/info` |

Message and event types distinguish user, assistant (text, reasoning, tool) and shell messages, tool states `streaming`, `running`, `completed`, `error`, and a session outcome `succeeded`, `failed` or `interrupted`. These are structured JSON, not a terminal transcript, and they are not the Kodama result envelope. The schema says nothing about authenticating the server, process lifetime, or whether `/interrupt` stops child processes.

## Coordinator-measured observations (OpenCode 2.1.2)

Recorded in the packet on 2026-10-21; not reproduced by the T-074 implementer.

- `opencode --version` reports `2.1.2`.
- `opencode run --help` advertises `--standalone`, `--server`, `--continue/-c`, `--session/-s`, `--fork`, `--model/-m`, `--agent`, `--format default|json`, `--file/-f`, `--title`, `--thinking` and `--auto`. It does **not** advertise `--dir` or `--pure`.
- `opencode run --pure …` and `opencode run --dir …` each exit 1 and print the run usage (unknown flags). The retired Python harness invocation that uses them is therefore not runnable on 2.1.2.
- Top-level subcommands: `run`, `serve`, `session`, `api`, `mcp`, `plugin`, `models`, `service`, `acp`, `debug`, `reload`, `upgrade`. The background service is the default.

The `--continue`, `--session` and `--fork` flags are not a continuation mechanism for Kodama: AR-07 and AR-08 are retired, and a resume is a fresh run from the last accepted Git commit.

Measured `run --format json` sample, a trivial prompt with no repository, tool, MCP server, gateway or error; ids abridged by the coordinator, `timestamp` values as measured:

```bash
opencode run --format json --model opencode/deepseek-v4.1-flash-free \
  "Reply with the single word OK and nothing else."
```

```jsonl
{"type":"step_start","timestamp":1792609788374,"sessionID":"ses_...","part":{"id":"prt_...","sessionID":"ses_...","messageID":"msg_...","type":"step-start"}}
{"type":"text","timestamp":1792609788400,"sessionID":"ses_...","part":{"id":"prt_...","sessionID":"ses_...","messageID":"msg_...","type":"text","text":"OK","time":{"start":1792609788384,"end":1792609788400}}}
{"type":"step_finish","timestamp":1792609788416,"sessionID":"ses_...","part":{"id":"prt_...","sessionID":"ses_...","messageID":"msg_...","type":"step-finish","reason":"stop","cost":0,"tokens":{"input":114,"output":2,"reasoning":0,"cache":{"read":5120,"write":0}}}}
```

Exit status was 0. Observed properties: NDJSON with `type`, `timestamp`, `sessionID` and a `part`; text is incremental; `step_finish` carries a stop reason, cost and token usage; **no terminal result envelope**. The retired parser's rule "the last text-producing step is the answer" is not adopted: prose is not completion (AR-06).

## Kodama contracts the wrapper meets

These are local contracts at `80cd84f`; this checkpoint does not alter them.

- **Attempt context and profile envelope.** The runtime `Context` is immutable and carries tenant, project, run, step, attempt, command, profile, design baseline, workspace and declared outputs ([runtime contract](../../contracts/runtime/README.md)). The profile reaches the wrapper as `agents.WrapperConfig` `{version, profile{name, version, digest}, content}`: canonical JSON, unpadded base64url, cut into 3072-character arguments after the marker `kodama.agent-config/v1`, decoded with `agents.DecodeArgs` (`internal/agents/payload.go`). It holds no identity, credential or token. Authority comes from the verified workload token, never from payload text, repository instructions or model output (AR-05).
- **Entrypoint.** `kodama step` runs the wrapper as argv in its own process group, stdin closed, stdout and stderr merged into one bounded log, an allowlisted environment, SIGTERM then SIGKILL on cancel or timeout, and writes the renewing runtime token to `--token-file` ([step entrypoint](step-entrypoint.md), `internal/entrypoint/README.md`). An optional `/kodama/results.json` manifest carries `outcome` (`completed`, `failed`, `needs_input`), typed outputs, and `reason` or `question`; file outputs are collected from declared paths. Exit zero alone never completes a step with a declared output missing. Without a manifest and with no declared output, the outcome defaults to `completed`.
- **Result envelope.** The entrypoint normalizes to the frozen `ResultRequest` `{outcome, exitCode, reason, question, sourceCommit, outputs, diagnosticArtifactIds}` plus `resultHash`; identical replay returns the original acknowledgment, a different result is `result_conflict`; the server derives attempt status and cancellation or timeout can supersede `completed`. `needs_input` fails the attempt, keeps its question and diagnostics, publishes no candidate, and is not retried (AR-13).
- **Conformance.** The report is an ordinary declared artifact bound to the pinned baseline digest and the candidate commit; disposition is derived and `conforms` only with no unresolved deviation and every required check passed ([design conformance](design-conformance.md), [runtime contract](../../contracts/runtime/README.md#design-baseline-and-conformance-report)).
- **Gateway.** `chat-completions-stream/1` only, per `internal/gateway/protocol.md`: bearer runtime token on `POST /gateway/v1/attempts/{attemptId}/chat/completions`; a strict member set (`model`, `messages`, `stream:true`, `stream_options` `include_usage` only, `n`=1, one of `max_tokens`/`max_completion_tokens`, `temperature`, `top_p`, `stop`, `tools`, `tool_choice`, `parallel_tool_calls`); text-only messages; a re-encoded SSE stream ending in `[DONE]`; after bytes were delivered a failure is one in-stream error and an aborted body; `limit_exceeded` is `403`, not `429`; the gateway never retries, falls back or replaces a stream. The fixtures prove the module, **not** that any real provider, model or harness works (`manifest.json`: `harness: pending T-031`, `provider: none claimed`).
- **MCP proxy.** Streamable HTTP only, official Go SDK `v1.8.0`, the single revision `2025-11-25` on both legs (the SDK's newer default `2026-07-28` is not used), `tools` capability only. Tools are `<registrationID>/<toolName>`; a bare name or wildcard grants nothing; every request authenticates the attempt token for `tool.invoke`; sessions are bound to the complete caller tuple and expire, with copied, expired and closed sessions all the same not-found; a lost, cancelled or timed-out call is `unknown`, never retried and never called rolled back. The module claims no OpenCode compatibility.
- **Catalogue controls.** A profile may claim only `model`, `requests`, `tokens`, `tool_calls`, `tool_allowlist`, `transcript`, each backed by compatibility evidence for the exact image/wrapper/entrypoint/harness tuple. Wall time is an execution deadline, not a control; there is no continuation or negotiation framework.

## Compatibility matrix

Columns: boundary/control; Kodama requirement and owner; upstream evidence (documented, D; coordinator-measured, M); local contract (L); observation; unresolved mismatch (`U-nn` in [Unresolved mismatches](#unresolved-mismatches)); qualification prerequisite. "Cases" name rows of `cases.json` by the id prefix.

| Boundary / control | Kodama requirement and owner | Upstream evidence | Local contract | Observation | Unresolved mismatch | Qualification prerequisite |
| --- | --- | --- | --- | --- | --- | --- |
| Headless invocation, project directory, stdin | AR-02, AR-A01. Wrapper (T-031); entrypoint runs it with stdin closed in the workspace | D `oc-doc-cli`: `run` is the automation entry. M `oc-meas-run-help`, `oc-meas-removed-flags`: `--dir` and `--pure` rejected (exit 1) on 2.1.2 | L `kodama-entrypoint`: argv, workspace cwd, stdin closed. Cases `wp.profile.*` | The retired invocation does not run on 2.1.2. No documented flag list and no replacement found | U-01, U-05 | T-031 preparation chooses an invocation from primary evidence or measurement and records it; no flag is guessed here |
| Permission and question behavior, `needs_input` | AR-05, AR-13, AR-A05. Wrapper produces `needs_input`; execution owns the failed attempt | D `oc-doc-permissions`: rule vocabulary incl. `question`, `shell`; D `oc-doc-openapi`: `/permission` and `/form` routes. M: `--auto` advertised, unread | L `kodama-spec-agent-runtime`, `kodama-runtime-contract` (`result_needs_input`). Cases `wp.input-needed.*` | Unanswered-prompt behavior under `run` with stdin closed is unknown; shell runs with host authority and permissions are a guardrail only | U-03 | T-031 measures no-human behavior; a hang or auto-approval blocks qualification |
| Private process or server lifetime; no native continuation | AR-01, AR-09; AR-07/AR-08 retired. Wrapper and entrypoint | D `oc-doc-cli`: shared background server by default, `--standalone` private. M: `--continue/-s/--fork` exist | L `kodama-entrypoint`: process group, SIGTERM then SIGKILL. Cases `wp.cancel.*` | A shared per-user service could escape the group; private-server child handling unmeasured; session flags are not a Kodama continuation | U-04 | T-031 shows a private, attempt-scoped OpenCode server whose children die with the group |
| Structured completion and result | AR-06, AR-A04. Wrapper to entrypoint manifest to `ResultRequest` | M `oc-meas-ndjson`: `step_start`, `text`, `step_finish`, no result envelope. D `oc-doc-openapi`: session outcome `succeeded\|failed\|interrupted`, message types | L `kodama-entrypoint` manifest; `kodama-runtime-contract` `CheckResult`. Cases `wp.completion.*` | OpenCode events or messages are not the Kodama envelope; an absent manifest defaults to `completed` when nothing is declared | U-02, U-10 | T-031 validates OpenCode output in the wrapper, always writes an explicit manifest, and fails closed on exit zero or prose alone |
| Command manifest and declared outputs | AR-06, AR-11. Entrypoint (T-016), results (T-022) | none (Kodama-only) | L `kodama-entrypoint`: `outcome`, `outputs`, `reason`/`question`; `output_invalid`. Cases `wp.completion.negative.*` | Reason vocabulary beyond `exit_nonzero`/`timeout` awaits T-022 adoption | U-10 | T-031 selects reason codes matching `^[a-z][a-z0-9_]{0,63}$` and T-022 maps them |
| Transcript and diagnostics | AR-10, GM-11, AR-A07. Wrapper builds; artifacts authorize; entrypoint captures its log | M `oc-meas-ndjson`: text events carry model output on stdout. D `oc-doc-openapi`: `GET …/message`, `GET /api/event` | L `kodama-agents` (transcript policy), `kodama-entrypoint` (always-captured bounded command log). Cases `wp.transcript.*` | Transcript source, upload path and non-leakage into the command log when recording is off are undecided | U-09 | T-031 proves disabled recording keeps metadata but not raw content, including in the command log |
| Candidate-bound conformance evidence | AR-14, AR-A08. Wrapper produces the declared artifact; T-034 binds it | none | L `kodama-runtime-contract` (`ConformanceReport`, `CheckConformance`), `kodama-conformance-procedure`. Cases `wp.conformance.*` | The wrapper must carry the context baseline digest and candidate commit unchanged | none beyond U-10 | T-031 emits a report from real runs with no moving reference |
| Cancellation: process tree and revocation | AR-09, AR-A06, GM-06. Entrypoint, gateway, MCP proxy, runtime auth | D `oc-doc-openapi`: `POST …/interrupt`. M: CLI signal behavior not measured | L `kodama-entrypoint` (`Test_cancel_child_tree`), `kodama-gateway`, `kodama-mcpproxy`. Cases `wp.cancel.*` | HTTP interrupt alone does not prove the tree stopped; late actions must still be denied by the control plane | U-04 | T-031 cancellation smoke through the real wrapper and OpenCode tree, with gateway and MCP revocation observed |
| Model route: request subset | GM-12, GM-05, AR-04. Gateway (T-032) | D `oc-doc-cli`/`oc-doc-openapi`: no provider-configuration or request-member schema read. M `oc-meas-ndjson`: hosted model, not the gateway | L `kodama-gateway`: strict member set, text-only. Cases `wp.gateway.positive.*`, `wp.gateway.negative.unsupported-request-member` | Actual OpenCode request members, retry, per-request auth and endpoint configuration are unknown | U-06, U-07 | T-032 compares real candidate traffic with the subset; a required change returns to T-032 for a new revision and fixtures |
| Model route: streaming, tool calls, usage, errors, retry | GM-04, GM-05, GM-A03. Gateway; the wrapper never retries silently | M `oc-meas-ndjson`: usage on `step_finish`. D: none for provider streaming | L `kodama-gateway`: in-stream error, aborted body, `403 limit_exceeded`, usage table. Cases `wp.gateway.negative.stream-failure-or-limit` | OpenCode's handling of an aborted stream, `403` and `unknown` charge is unmeasured | U-06 | T-032 and T-031 observe the real client treat a truncated stream or limit as failure with no automatic retry |
| MCP transport and protocol negotiation | GM-12, AR-02, GM-A05. MCP proxy (T-033) | D `oc-doc-mcp`: Streamable HTTP; `protocol` `legacy`\|`auto`\|`2026-07-28`, default `legacy` | L `kodama-mcpproxy`: SDK `v1.8.0`, only `2025-11-25`, tools only. Cases `wp.mcp.positive.allowed-call`, `wp.mcp.negative.protocol-or-name-mismatch` | Documented: `legacy` (default) sends `initialize` and speaks revisions up to `2025-11-25`; `auto` probes `server/discover` for `2026-07-28` and falls back to `legacy`. That is a documented client intent, not a measured exchange: whether OpenCode and the pinned `2025-11-25` proxy actually interoperate is unmeasured, and `auto`/`2026-07-28` are not evidence of it | U-08 | T-033 measures real negotiation against the pin; mismatch is rejected, never adapted |
| MCP naming, scope and permissions | GM-07, AR-05, AR-A02. MCP proxy; profile allow-list | D `oc-doc-mcp`, `oc-doc-permissions`: `<server>_<tool>` normalization, Code Mode, hide/deny, `_meta` session id is correlation only, server instructions go into the prompt, resource helpers exist | L `kodama-mcpproxy`: `<registrationID>/<toolName>`, bare or wildcard grants nothing, caller-tuple sessions. Cases `wp.mcp.negative.hidden-or-foreign-use`, `wp.authority.negative.workspace-configuration` | OpenCode names must map unambiguously to profile entries; ambiguous names fail closed; resource helpers and injected instructions grant nothing | U-05, U-08, U-12 | T-033 confirms naming, tools-only behavior, hidden-tool denial and token delivery/renewal |
| MCP unknown effect | GM-10, GM-A07. MCP proxy | none | L `kodama-mcpproxy`: `unknown` receipt, `isError`, never retried. Case `wp.mcp.negative.unknown-effect-no-retry` | OpenCode must not blindly retry a `tools/call` that returned `unknown` | U-08 | T-033 and T-031 observe the real client leave an `unknown` result as final |
| Source publication and replay | AR-11, AR-A07, AR-12. Entrypoint and control plane | none | L `kodama-runtime-contract` (source, result, `publication_conflict`, `result_conflict`), `kodama-entrypoint`. Cases `wp.publication.*` | The wrapper does not write the branch; pending or conflicted publication never succeeds | none | T-031 produces a commit only inside the workspace; the entrypoint proposes it |
| Credential delivery and renewal | AR-05, GM-02. Entrypoint writes `--token-file`; gateway/proxy own provider and tool credentials | D `oc-doc-mcp`: `headers`, `{env:NAME}` substitution (static) | L `kodama-entrypoint`: token only in memory and the token file; command environment is an allowlist | A static value does not renew; whether OpenCode re-reads a file per request is unknown | U-07 | T-031 and the composition owner confirm renewal without the token entering config, logs or transcript |
| Catalogue controls `model`, `requests`, `tokens`, `tool_calls`, `tool_allowlist`, `transcript` | AR-02, AR-04. Catalogue entry per exact tuple; T-032/T-033 enforce | D: none that proves enforcement | L `kodama-agents`: catalogue, claimed controls, `PolicyReader`. Cases `wp.profile.negative.unknown-tuple-or-control` | No control may be claimed for 2.1.2 yet; wall time stays an execution deadline | U-11, U-12 | Only actual T-031 evidence populates the catalogue |

## Unresolved mismatches

These are explicit gaps, each with a `wp-gap-*` entry in `sources.json`. Absence of evidence is not support, and no replacement is invented.

| Gap | Statement | Owner of the next step |
| --- | --- | --- |
| U-01 | `--pure` and `--dir` are absent from 2.1.2 `run --help` and fail with exit 1. Absence from help does not prove removal and no replacement is documented for `run`. A working project-directory and ambient-configuration setting is unknown. | T-031 preparation |
| U-02 | `--format json` has no documented stable terminal-result schema; the OpenAPI event and message log is not the Kodama envelope. The wrapper validates OpenCode output itself. | T-031 preparation |
| U-03 | Behavior of an open permission or question with stdin closed and no human, and the meaning of `--auto`, are unmeasured. `needs_input` must come from the wrapper, not from an unanswered prompt. | T-031 preparation |
| U-04 | CLI signal handling and private-server process containment are unmeasured. The shared background service is the default. HTTP interrupt alone does not prove the tree stopped. | T-031 preparation |
| U-05 | Whether workspace configuration (`.opencode/`, instructions, plugins, MCP servers) is loaded, and how an attempt-private configuration overrides it, is unknown. Treat it as untrusted. | T-031 preparation, composition owner |
| U-06 | Custom OpenAI-compatible endpoint configuration, actual request members, retry controls and handling of a gateway error or aborted stream are not established. | T-032 owner |
| U-07 | Per-request provider and MCP bearer delivery and renewal from `--token-file`, without the token entering config, logs or transcripts, is not established. | T-031 preparation, T-032, T-033 |
| U-08 | MCP: actual negotiation and interoperability of the documented `legacy` behavior (`initialize`, revisions up to `2025-11-25`) against the pinned `2025-11-25` proxy, and the `auto` fallback; clean rejection of other revisions; tools-only behavior; header token with `oauth: false`; Code Mode naming versus `<registrationID>/<toolName>`; normalization collisions; no blind retry of `unknown`. | T-033 owner |
| U-09 | Transcript source and upload; no raw model text in the always-captured command log when recording is disabled. | T-031 preparation |
| U-10 | Entrypoint manifest is optional and defaults to `completed`; the wrapper must always write an explicit manifest and fail closed. | T-031 preparation |
| U-11 | No immutable upstream revision, binary checksum or raw artifact was captured; pages are living pages. | T-031 preparation |
| U-12 | Whether OpenCode can be held to the approved model and the profile's tools, with no fallback provider, `webfetch` or `websearch`, is unknown. | T-031 qualification |

## Case index

`cases.json` holds 25 synthetic cases. They state contract-derived expectations and map each to the runtime or profile envelope it exercises. Fixtures reuse existing vectors by `name` rather than copying them.

| Boundary | Positive case | Negative case(s) | Envelope exercised |
| --- | --- | --- | --- |
| Profile and configuration | `wp.profile.positive.pinned-payload` | `wp.profile.negative.unknown-tuple-or-control`, `wp.profile.negative.payload-tamper`, `wp.profile.negative.command-or-alias-mismatch` | `WrapperConfig`, catalogue tuple, `Context` |
| Completion | `wp.completion.positive.manifest-to-result` | `wp.completion.negative.exit-zero-or-prose-only`, `wp.completion.negative.malformed-manifest` | results manifest, `ResultRequest` |
| Input needed | `wp.input-needed.positive.question-retained` | `wp.input-needed.negative.no-wait-or-success` | `needs_input` result |
| Conformance | `wp.conformance.positive.baseline-bound-report` | `wp.conformance.negative.moving-stale-or-blocked` | `DesignBaseline`, `ConformanceReport` |
| Cancellation | `wp.cancel.positive.process-group-and-access` | `wp.cancel.negative.late-or-surviving-access` | attempt revocation, entrypoint exit codes |
| Gateway | `wp.gateway.positive.accepted-subset` | `wp.gateway.negative.unsupported-request-member`, `wp.gateway.negative.stream-failure-or-limit` | `chat-completions-stream/1` |
| MCP | `wp.mcp.positive.allowed-call` | `wp.mcp.negative.hidden-or-foreign-use`, `wp.mcp.negative.protocol-or-name-mismatch`, `wp.mcp.negative.unknown-effect-no-retry` | MCP proxy session and tool receipts |
| Publication and replay | `wp.publication.positive.cas-and-replay` | `wp.publication.negative.pending-or-conflict` | `SourceProposal*`, `ResultRequest` |
| Transcript | `wp.transcript.positive.authorized-recording` | `wp.transcript.negative.recording-disabled` | profile transcript policy, diagnostic artifacts |
| Authority | none (covered by the positive cases above) | `wp.authority.negative.workspace-configuration` | workload token, pinned profile |

Negative payloads in `cases.json` remain syntactically valid JSON; malformed raw input appears as string values. Every `expected.basis` is `contract-derived`.

## Ordered prerequisites for qualification

1. **T-031 preparation.** Obtain an immutable upstream revision and raw evidence for the chosen candidate (U-11); choose an invocation and configuration without `--pure` or `--dir` assumptions (U-01, U-05); establish structured output, no-input behavior, private-process containment, transcript handling and credential refresh (U-02, U-03, U-04, U-07, U-09, U-10).
2. **T-032 owner.** Compare actual candidate model traffic with the merged `chat-completions-stream/1` subset and confirm routing, authentication and stream-failure handling (U-06). A required protocol change returns to this owner for a new revision and fixtures.
3. **T-033 owner.** Confirm `2025-11-25` negotiation, tools-only behavior, naming, runtime-token delivery and renewal, scope and session denial, and no blind tool retry (U-08). An incompatibility returns to this owner for revision and fixtures.
4. **T-031 qualification.** After the prerequisite adapter changes merge, pin and build the real image and test the complete wrapper, gateway and MCP combination. Only actual evidence may populate the compatibility catalogue, and only for controls actually enforced.

Gateway and MCP reviews may proceed independently, but both precede final qualification. Composition remains its owner's responsibility; this checkpoint edits no task graph and no shared file.

## Boundaries

- Only this document and `contracts/harness-opencode/` change. No spec, frozen runtime schema, active adapter, task frontmatter or task index is edited.
- Existing modules keep ownership of state, authorization, credentials, budgets and publication. The checkpoint adds no runtime layer, authority service or adapter, and no new dependency.
- Tenant authority: ownership across model calls, tools, sessions, artifacts and publication comes from the verified workload token and durable grants. Repository instructions, OpenCode session correlation, injected server instructions and tool responses grant nothing.
- State transitions: wrapper outcomes (`completed`, `failed`, `needs_input`) are distinct from entrypoint normalization and server-derived attempt status. `needs_input` fails the attempt; cancellation supersedes completion; recovery is a fresh run from accepted Git state.
- This docs profile cannot satisfy `opencode_real_smoke` and certifies no version, provider or image.

## Review record

Documentation acceptance checks `wrapper_protocol_sources` and `wrapper_protocol_fixtures` (AR-02, AR-05, AR-06, AR-09, AR-11, GM-12). They validate syntax and links; the semantic review below is by reading.

**Source and case review (wrapper_protocol_sources, wrapper_protocol_fixtures).** Reviewed by the implementer against the local owners and the upstream pages named in `sources.json`:

- Every row of the matrix cites a source id or states "none (Kodama-only)" with a local contract, and every assumed control has either a primary source (URL and attribution) or an explicit gap U-01 to U-12. Documented, coordinator-measured, local-contract, synthetic and unresolved claims are labelled separately.
- Every `sourceRefs` entry in `cases.json` resolves to an id in `sources.json`. Each fixture selector `name=…` resolves in the cited runtime or gateway fixture file, and each cited Go test name exists in the cited package directory (checked with a one-off script during authoring, not a repository check).
- Every boundary in the task's pairing table has a positive and a negative case, except authority, which is deliberately negative-only.
- Unresolved items block qualification: each case names its `qualificationGate`.

**Commands run in the worktree** (2026-10-21, after the final edit of the owned files).

| Command | Result |
| --- | --- |
| `python3 scripts/dev/check.py config` | passed: `PASS configuration and lock validation (8 images)` (parses all JSON under `contracts/`, including both new files) |
| `make docs-check` | passed: `PASS documentation links, anchors, JSON and shell fences (139 files)` and `PASS task graph, ownership and index (72 tasks)` |

Neither command validates semantics or executes the cases; no Go test was run because no Go file changed.
