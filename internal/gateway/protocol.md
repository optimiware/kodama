# Model gateway protocol: chat-completions-stream/1

This file freezes the one protocol the first model gateway speaks. It is the
contract between a workload (OpenCode or any harness), the gateway, and an
OpenAI-compatible provider. Machine-readable forms: [`api/gateway.yaml`](../../api/gateway.yaml)
(request and event schemas), `testdata/chat-completions/` (accepted and rejected
request fixtures, stream fixtures and `manifest.json`), and the code in
`openai/protocol.go` and `openai/stream.go`. A change to any accepted member or
to the stream grammar needs a new revision (`openai.ProtocolRevision`) and new
fixtures; `Test_chat_completions_compatibility` fails if the manifest and the
code disagree.

**Scope.** Streaming chat completions only. Excluded and rejected, not
forwarded: Responses API, embeddings, completions, images/audio, multimodal
content parts, non-streaming requests, multiple choices, response formats,
logprobs, seeds, `user`, `store`, `metadata`, legacy `functions`, automatic
provider or model fallback.

**Qualification status.** The fixtures are deterministic and prove that this
module implements the subset below. They are **not** evidence that a real
provider, model or harness version works. T-031 selects the OpenCode version and
runs the real harness smoke test; until then GM-12 is satisfied only for the
frozen contract and its fixtures, and `manifest.json` records
`"harness": "pending T-031"` and `"provider": "none claimed"`.

## Request (workload to gateway)

`POST /gateway/v1/attempts/{attemptId}/chat/completions`, `Content-Type:
application/json`, `Authorization: Bearer <runtime token>` of that attempt. The
tenant, project, run and attempt come from the verified token and its durable
grant. Body limit: 4 MiB, structural depth 16, 50,000 JSON nodes; each run's
`maxRequestBytes` is tighter.

Accepted top-level members (anything else is `400 invalid_request`):

| Member | Rule |
| --- | --- |
| `model` (required) | model name pattern; must be on the run's allow-list |
| `messages` (required) | 1 to 256 messages |
| `stream` (required) | must be `true` |
| `stream_options` | only `{"include_usage": true}`; the gateway always asks the provider for usage |
| `n` | only `1` |
| `max_tokens` / `max_completion_tokens` | alternatives, 1 to 1,000,000; absent selects the run's output limit; above it is `403 limit_exceeded` |
| `temperature` | 0 to 2 |
| `top_p` | above 0, at most 1 |
| `stop` | one string or up to four strings, 1 to 64 bytes each |
| `tools` | 1 to 64 function declarations; names unique; `parameters` a JSON object of at most 32 KiB |
| `tool_choice` | `none`, `auto`, `required` (the last two need tools) or `{"type":"function","function":{"name":<declared>}}` |
| `parallel_tool_calls` | boolean; needs tools |

Messages are text only: roles `system`, `user`, `assistant`, `tool`; `content` is a
string (up to 512 KiB) and may be null or absent only on an assistant message with
`tool_calls`; `name` is optional; `tool_calls` (1 to 32 function calls with
`id`, `type: function`, `function.name` and `function.arguments` text) belong to
assistant messages; `tool_call_id` is required on, and only allowed on, tool
messages. Unknown members inside messages, tools and tool calls are rejected.

## Outbound request (gateway to provider)

Built from the validated fields only. The URL is `<endpoint>/chat/completions`
of the run's bound endpoint. The body carries `model`, `messages`, `stream:
true`, `stream_options.include_usage: true`, the **enforced** `max_tokens` (the
caller's value or the run's limit), `temperature`, `top_p`, `stop`, `tools`,
`tool_choice` and `parallel_tool_calls`. Headers: `Content-Type`, `Accept:
text/event-stream`, `Accept-Encoding: identity`, `Authorization: Bearer <the
run's pinned provider key>` and a fixed `User-Agent`. No caller header, cookie,
URL, query or member is copied. No environment proxy is used and no redirect is
followed (a same-origin redirect included): a redirect response is a failure, and
its target receives no request and no credential.

## Stream (provider to gateway to workload)

Server-sent events. `data:` lines carry one JSON object each; comment lines
(starting with `:`) are ignored as keep-alives; `[DONE]` ends the stream. Any
other SSE field, a multi-line event or a malformed event is a protocol failure.

Each event is validated and **re-encoded** from the members of the supported
subset; unknown provider members (`system_fingerprint`, usage details,
obfuscation) are dropped, never forwarded. A chunk has `id`, `object:
chat.completion.chunk`, optional `created`, `model` and `choices` (at most one,
index 0) whose `delta` holds `role` (only `assistant`), `content` and up to 32
function tool-call deltas (`index` 0 to 31, `id`, `type`, `function.name`,
`function.arguments`); `finish_reason` is `stop`, `length`, `tool_calls` or
`content_filter`. A final `usage` object with integer `prompt_tokens` and
`completion_tokens` (and an optional `total_tokens` that must equal their sum) is
valid usage; a usage object that is present but malformed or inconsistent is
reported as invalid usage.

Bounds: 64 KiB per line, 8 MiB per stream, 20,000 events, a 60 s idle timeout
between events by default (constructor inputs); the request deadline and the
transport's total timeout bound the whole call.

## Usage and charging

The gateway charges, per request, exactly one of:

| Observation | Basis | Charge | Uncertainty |
| --- | --- | --- | --- |
| complete stream, valid usage within the verified bounds | `observed` | `prompt + completion` | `none` |
| complete stream, no usage | `estimated` | request bound | `usage_missing` |
| complete stream, invalid usage | `estimated` | request bound | `usage_invalid` |
| usage above the request's input bound or output limit | `estimated` | `max(reported, bound)`, request fails `bound_exceeded` | `bound_exceeded` |
| stream truncated, idle, malformed or oversized | `estimated` | request bound | `stream_interrupted`, `protocol_violation`, `bound_exceeded` |
| client disconnect, deadline, attempt cancelled after a possible send | `estimated` | request bound | `client_cancelled`, `deadline_exceeded`, `attempt_cancelled` |
| client disconnect, deadline, attempt cancelled before any send | `estimated` | 0 (request `failed`, reason code kept) | `not_sent` |
| provider 5xx, protocol or timeout after a possible send | `estimated` | request bound | `provider_error`, `protocol_violation`, `deadline_exceeded` |
| provider 3xx/4xx/429 before any stream byte | `estimated` | 0 | `provider_rejected` |
| nothing sent (destination, connection, credential or authority failure) | `estimated` | 0 | `not_sent` |
| worker lost while forwarding (reconciliation) | `estimated` | request bound, slot kept | `worker_lost` |

Every uncertain row except a *complete* stream with missing or invalid usage leaves
the provider's completion unknown: the request becomes `unknown` and keeps the run's
slot until an authorized operator attests the completion through `SettleUnknown`, so no further request of the run
is admitted (`run_busy`). A complete stream without usable usage is charged the bound
and releases the slot. Rows charging 0 release it.

The **request bound** is `input bound + enforced max_tokens`, where the input bound
is the conservative, model-specific estimate (`openai/estimate.go`: bytes divided by
the configured bytes per token, plus fixed framing overheads; the default is one
token per byte). Admission requires charged tokens below the allowance and at most
one request is unresolved per run, so charged tokens never exceed
`allowance - 1 + maxInputTokens + maxOutputTokens`. That formula applies to
charges within the verified provider/input/output bounds. It is a token-overshoot
bound for operational admission, not a monetary guarantee, price, payment or
entitlement (those belong to Platform). A provider that reports usage above the
verified bounds is a protocol failure, not an expansion of the contract: the
request fails `bound_exceeded`, the larger of the reported usage and the bound is
charged and the evidence is kept as reported, never clamped.

## Failure semantics

**Before the first byte reaches the workload** the response is an ordinary JSON
error with the shared envelope (`code`, fixed `message`, `requestId`, `details`)
and the status in `api/gateway.yaml`. Messages are fixed; provider output,
credentials and prompts are never quoted. `limit_exceeded` is `403`, not `429`:
OpenAI-compatible clients retry rate limits, and a run's limit never clears by
waiting.

**After bytes were delivered** the gateway writes one in-stream event
`{"error":{"code","message","type":"kodama_gateway"}}`, never writes `[DONE]`, and
aborts the response so the transport shows an incomplete body. Codes:
`stream_truncated`, `stream_idle`, `protocol_violation`, `response_too_large`,
`bound_exceeded`, `client_cancelled`, `attempt_cancelled`, `authority_unavailable`,
`deadline_exceeded`, `unavailable`. A partially delivered stream is never replaced
by another response and the provider call is never repeated.

**Cancellation.** A client disconnect, the request deadline, run/attempt
cancellation or revocation (checked every `Recheck` interval against current
authority) or a failed authority check cancels the upstream request. Closing an
HTTP connection is not proof that the provider stopped, so the request keeps its
conservative charge and the usage record says so. A cancellation before any
send (for example while the credential is prepared) sends nothing: the request
ends `failed` with the cancellation reason code, `not_sent` and no charge, and
releases the slot. The provider stream is also
closed as soon as forwarding ends for any reason (including a failed write to the
workload) and before the outcome is persisted.

**Retry.** The gateway never retries a transport error, a rate limit or a partial
stream. A retry is the workload's explicit decision and is a new request: a new
admission, another request allowance, and impossible while an earlier request of
the run is unresolved.

## Compatibility manifest

`testdata/chat-completions/manifest.json` lists the frozen fixtures: three
accepted requests (`minimal`, `full`, `tool_loop`), the rejected requests with the
reason each is rejected, and nine streams (complete with and without usage and with
invalid usage, tool calls, truncated, malformed, unknown SSE field, two choices,
bad finish reason) with their expected outcome, frame count, usage, finish reason
and model. It is the evidence T-031 extends with the real harness version.
