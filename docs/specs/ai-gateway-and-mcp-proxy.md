# AI gateway and MCP proxy

**Scope:** M2 one OpenAI-compatible HTTP protocol and MCP Streamable HTTP. More provider protocols and transports are later extensions.

## Model gateway

Configure one approved project endpoint, a typed credential secret, allowed models, and request/token limits. Expose the exact protocol verified against the selected OpenCode wrapper; the initial target is streaming chat completions. A compatibility smoke test must demonstrate the actual chosen model/harness combination before claiming support.

Enforce a maximum request count atomically and stop new requests once observed token usage reaches the run allowance. Permit only one in-flight model request per run across all server replicas. Bound request input and maximum output tokens. This admits at most one bounded request of token overshoot; it is not a monetary billing guarantee. Unknown provider usage is recorded and conservatively charged at the request bound before admitting more requests. No reservation/settlement ledger is required.

Persist usage and request outcomes; record the actual endpoint/model. Do not silently fall back after streaming starts. Propagate cancellation and report uncertain remote completion/billing.

O-13 places subscription billing, commercial usage/access and entitlements in Platform. The request/token counters above are operational admission bounds and usage evidence, not a second commercial ledger. Under O-19, the gateway resolves the authorized pinned provider secret server-side; an agent needs scoped runtime authority rather than the provider credential.

## MCP proxy

A project registration contains an approved endpoint, typed secret reference, and enabled tool names. A profile names allowed registrations/tools. Authenticate both discovery and invocation, bind remote sessions to tenant/project/caller, and negotiate a pinned supported protocol version. Tool responses never expand permissions.

The first transport is Streamable HTTP with integration-owned credentials. Protocol negotiation uses `github.com/modelcontextprotocol/go-sdk`, the same SDK platform uses for its MCP resource. Delegated end-user OAuth, argument-policy languages, and other transports are later. Tokens must be valid for the intended recipient; factory bearer tokens are not forwarded as remote credentials.

### Optimi Platform tools

The generic integration-owned credential baseline does not authorize calls to Platform's user-authorized MCP resource. [Platform integration](platform-integration.md) owns this boundary and pins the reviewed upstream contract. The reviewed tool set is read-only, uses a separate resource audience and repeats core tenant/scope/entitlement/audit checks. O-18 selects Optimi OAuth UUID tenant/role authority; T-061 qualifies the actual actor/delegation/current-access contract before unattended Platform use is claimed. No new OAuth grant is implied by an active Kodama attempt token.

A tool allow-list narrows operations; it cannot grant platform membership or a provider mutation. Send only the tenant-data projection and destination approved for model/transcript use under PI-08. A successful tenant read does not authorize copying its result into a channel, prompt, or retained artifact.

## Requirements

| ID | Requirement |
| --- | --- |
| GM-01 | Every request MUST validate active workload identity and project scope. |
| GM-02 | Provider credentials MUST stay behind the gateway/proxy and out of diagnostics/events. |
| GM-03 | Request count and one-in-flight admission MUST be atomic; token caps MUST publish their bounded-overshoot semantics. |
| GM-04 | Usage MUST retain run/attempt, actual model, tokens or conservative estimate, and uncertainty. |
| GM-05 | Retry MUST be explicit; partially delivered streams MUST NOT be silently replaced. |
| GM-06 | Cancellation/deadlines MUST stop local forwarding and propagate remotely where supported. |
| GM-07 | MCP discovery and invocation MUST enforce the profile's server/tool allow-list. |
| GM-08 | Remote sessions MUST be bound to the current caller/scope and expire; cross-caller reuse is denied. |
| GM-09 | Endpoints, redirects, and DNS/network routes MUST obey approved destination and credential-audience restrictions. |
| GM-10 | Side-effecting tools MUST NOT be retried automatically without a known-safe idempotency/reconciliation contract. |
| GM-11 | Raw content retention MUST be independently configurable and authorized. |
| GM-12 | Supported protocol/harness versions MUST be pinned and compatibility-tested. |

A fixed allow-list is authorization, not an attempt to prove arbitrary remote tools safe. The initial trusted-engineer execution profile still applies. A raw model/proxy endpoint is not a generic unscoped tenant API.

## Acceptance

| ID | Scenario | Requirements |
| --- | --- | --- |
| GM-A01 | Foreign project model/MCP references fail authorization | GM-01, GM-02 |
| GM-A02 | Two replicas cannot admit concurrent requests for one run; limits/overshoot are bounded | GM-03, GM-04 |
| GM-A03 | Interrupted streaming exposes partial failure and cancellation | GM-05, GM-06 |
| GM-A04 | Hidden tools also fail direct invocation | GM-07 |
| GM-A05 | Copied/expired remote sessions are denied; pinned protocol passes smoke tests | GM-08, GM-12 |
| GM-A06 | Redirects cannot exfiltrate credentials to another destination | GM-02, GM-09 |
| GM-A07 | Unknown tool outcome is surfaced without a blind retry | GM-10 |
| GM-A08 | Content recording can be disabled while usage metadata remains | GM-04, GM-11 |
