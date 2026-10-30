# Agent runtime

**Scope:** managed profile conveniences over ordinary containers. [ADR 0012](../adr/0012-container-sdlc-and-artifact-releases.md) also makes direct harness commands with explicit secrets a first-class path. Managed native, Claude Code and Codex profiles remain later tasks; generic container use does not wait for them.

## Model

An agent step is a container step resolved from an immutable profile: image digest, command wrapper, instruction template, model endpoint, allowed tools, and limits. It uses the same attempt, entrypoint, source publication, result, and cancellation machinery as any command. Transcript and usage references live on the attempt; there is no independent agent-session state machine.

A user can instead declare `kind: container`, a harness image/command and an API-key secret binding. Registration needs the ordinary image, secret, runtime and network checks, not a managed wrapper catalogue, model gateway or MCP proxy. The entrypoint still captures exit status and validates declared outputs. A native Go agent packaged with ko uses the same primitive; it does not run tenant agent loops inside the server. Profiles remain optional pinned expansion, not another executor.

Managed profile requirements below apply to controls the profile selects and claims. A direct credential permits calls outside Kodama's proxies: provider usage/tool restrictions cannot then be represented as proxy-enforced, and cancellation cannot recall an already disclosed API key or undo remote effects. This distinction must be visible in profile/run metadata, without requiring a second agent lifecycle.

The wrapper obtains the run's pinned specification/acceptance context, runs a pinned supported OpenCode version, and emits a result. Exact headless flags and provider compatibility must be verified by the adapter task against that version's primary documentation and a smoke test.

Managed instruction context also identifies applicable specification/decision/blueprint references at immutable revisions, following [design conformance](../implementation/design-conformance.md). For hosted integrations it names the reviewed core contract baseline. When the pinned profile/project requires conformance, the wrapper produces its declared candidate report and records material conflicts as unresolved rather than inventing architecture to make the task pass. Repository instructions and this context describe intent, never tool authority.

O-17 defines application chassis as Markdown guidance held in a tenant/project-scoped Git repository, normally one repository per selected scope. Pin repository identity, immutable commit and selected Markdown paths in the instruction baseline; any tenant/project composition is explicit. Guidance may describe microservices, approved stacks/libraries and cloud deployment methods, but cannot grant runtime permissions or become a shared application SDK. T-059 supplies conventions/examples; missing repository-consumption seams return to the profile/wrapper owner.

## Requirements

| ID | Requirement |
| --- | --- |
| AR-01 | Profile image/version, instruction context, tools, and limits MUST be pinned for the run. |
| AR-02 | A Kodama-supported managed wrapper/version combination MUST be integration-tested; unsupported managed profiles fail registration. Generic container commands MUST NOT require that catalogue. |
| AR-03 | Agent execution MUST reuse normal attempt identities and workspace publication authority. |
| AR-04 | Wall time, request/token allowance, and tool-call limits MUST be enforced where the declared profile claims them. |
| AR-05 | Tool/proxy/runtime authorization MUST come from the workload identity, not repository or model instructions. |
| AR-06 | A managed wrapper MUST produce its declared structured result; generic commands use the entrypoint's ordinary result and declared-output validation. A prose success claim is insufficient for either. |
| AR-09 | Cancellation MUST stop the process tree and revoke new Kodama-mediated provider/tool/publication access; direct external credentials follow VS-08 and cannot be recalled from a process or provider. |
| AR-10 | Attempts MUST retain attributable result metadata and available usage/transcript references; unavailable direct-provider usage MUST remain unknown rather than fabricated. |
| AR-11 | Source changes MUST become a validated commit published through the workspace protocol. |
| AR-12 | Agent identity MUST NOT satisfy a human review/completion permission. |
| AR-13 | `needs_input` MUST fail the attempt with a recorded question; a human revises intent and starts a new run. |
| AR-14 | Managed agent instructions MUST use their pinned design baseline; when the profile/project requires conformance they MUST return candidate-bound evidence identifying affected requirements, checks and unresolved deviations. Design references MUST NOT silently advance during execution. |
| AR-15 | `[ADR 0012]` A direct harness command with explicit secrets MUST use the ordinary container path without mandatory profile/gateway/proxy registration; native managed agents MUST also run in containers. |
| AR-16 | `[ADR 0012]` Usage, budget, transcript and tool-control claims MUST identify whether they are runtime-enforced, proxy-enforced or merely tool-reported; direct credentials MUST NOT be presented as mediated access. |

AR-07 and AR-08 (handoff/continuation contracts) are retired from the initial scope. There is no capability-negotiation framework or continuation token in M2.

## Result and recovery

Wrapper outcomes are `completed`, `needs_input`, or `failed`. Runtime cancellation/timeout is supplied by the entrypoint. `needs_input` preserves its question and diagnostic artifacts but does not publish incomplete source as a successful candidate. A completed profile must return declared evidence and any proposed commit.

An interrupted attempt can lose uncommitted scratch work; in the designed `run_shared` model agent steps share the run's scratch with other steps, and a lost scratch fails the run rather than being reconstructed. Resume means a fresh run from the last accepted Git commit. Harness-native resumability may be added through a separately verified later contract.

The mediated profile routes provider traffic through the AI gateway and tool traffic through authorized tools, keeping provider credentials server-side. Direct harness/build/publish/deploy commands explicitly bind their own declared pinned secrets under [secrets delivery](variables-and-secrets.md#runtime-delivery); they never inherit another step's bindings. Transcripts are sensitive ordinary artifacts; content recording is independently configurable. Shell/network access follows the runtime policy in [identity](identity-tenancy-and-rbac.md).

## Acceptance

| ID | Scenario | Requirements |
| --- | --- | --- |
| AR-A01 | Pinned wrapper/profile passes compatibility smoke tests; unknown version fails | AR-01, AR-02 |
| AR-A02 | Repository instructions cannot grant another tenant's tool/source access | AR-03, AR-05 |
| AR-A03 | Budget exhaustion stops new admission and preserves result/usage | AR-04, AR-10 |
| AR-A04 | A managed wrapper missing its required structured output fails finalization; an ordinary command succeeds with exit zero when all of its declared outputs are valid | AR-06 |
| AR-A05 | Missing business input records a question and fails; revised intent starts a new run | AR-13 |
| AR-A06 | Cancellation revokes actions and stops the wrapper/process tree | AR-09 |
| AR-A07 | Valid commit/evidence are published; agent identity cannot approve | AR-11, AR-12 |
| AR-A08 | Moving an upstream/blueprint branch during execution does not change instruction context; an architectural conflict is reported in the candidate's conformance evidence and cannot become a prose-only success claim | AR-01, AR-06, AR-14 |
| AR-A09 | A harness-shaped container receives its declared API key and produces ordinary outputs with gateway/proxy/catalogue disabled; an optional managed profile expands to the same execution primitive | AR-02, AR-03, AR-06, AR-15 |
| AR-A10 | Direct-provider access is labeled as such, cannot claim proxy-enforced token/tool bounds, and cancellation revokes Kodama grants without claiming recall of a disclosed key | AR-04, AR-09, AR-16 |
