# Agent runtime

**Scope:** M2 OpenCode headless image and profile defaults. Native, Claude Code, and Codex harnesses are later tasks.

## Model

An agent step is a container step resolved from an immutable profile: image digest, command wrapper, instruction template, model endpoint, allowed tools, and limits. It uses the same attempt, entrypoint, source publication, result, and cancellation machinery as any command. Transcript and usage references live on the attempt; there is no independent agent-session state machine.

The wrapper obtains the run's pinned specification/acceptance context, runs a pinned supported OpenCode version, and emits a result. Exact headless flags and provider compatibility must be verified by the adapter task against that version's primary documentation and a smoke test.

Instruction context also identifies the applicable specification/decision/blueprint references at immutable revisions, following [design conformance](../implementation/design-conformance.md). For hosted integrations it names the reviewed core contract baseline. The wrapper produces a declared conformance artifact for the candidate; it reports material conflicts as unresolved rather than inventing architecture to make the task pass. Repository instructions and this context describe intent, never tool authority.

O-17 defines application chassis as Markdown guidance held in a tenant/project-scoped Git repository, normally one repository per selected scope. Pin repository identity, immutable commit and selected Markdown paths in the instruction baseline; any tenant/project composition is explicit. Guidance may describe microservices, approved stacks/libraries and cloud deployment methods, but cannot grant runtime permissions or become a shared application SDK. T-059 supplies conventions/examples; missing repository-consumption seams return to the profile/wrapper owner.

## Requirements

| ID | Requirement |
| --- | --- |
| AR-01 | Profile image/version, instruction context, tools, and limits MUST be pinned for the run. |
| AR-02 | The supported wrapper/version combination MUST be integration-tested; unsupported profiles fail registration. |
| AR-03 | Agent execution MUST reuse normal attempt identities and workspace publication authority. |
| AR-04 | Wall time, request/token allowance, and tool-call limits MUST be enforced where the declared profile claims them. |
| AR-05 | Tool/proxy/runtime authorization MUST come from the workload identity, not repository or model instructions. |
| AR-06 | Completion MUST produce a valid structured result; a prose success claim is insufficient. |
| AR-09 | Cancellation MUST stop the process tree and revoke new provider/tool/publication access. |
| AR-10 | Attempts MUST retain attributable result/usage metadata and authorized transcript references. |
| AR-11 | Source changes MUST become a validated commit published through the workspace protocol. |
| AR-12 | Agent identity MUST NOT satisfy a human review/completion permission. |
| AR-13 | `needs_input` MUST fail the attempt with a recorded question; a human revises intent and starts a new run. |
| AR-14 | Agent instructions MUST use the pinned design baseline and return candidate-bound conformance evidence identifying affected requirements, checks, and unresolved deviations; design references MUST NOT silently advance during execution. |

AR-07 and AR-08 (handoff/continuation contracts) are retired from the initial scope. There is no capability-negotiation framework or continuation token in M2.

## Result and recovery

Wrapper outcomes are `completed`, `needs_input`, or `failed`. Runtime cancellation/timeout is supplied by the entrypoint. `needs_input` preserves its question and diagnostic artifacts but does not publish incomplete source as a successful candidate. A completed profile must return declared evidence and any proposed commit.

An interrupted attempt can lose uncommitted scratch work. Resume means a fresh run from the last accepted Git commit. Harness-native resumability may be added through a separately verified later contract.

The approved profile routes provider traffic through the AI gateway and tool traffic through authorized tools, keeping provider credentials server-side. Ordinary build/script steps can explicitly bind a declared pinned secret to their child environment under [secrets delivery](variables-and-secrets.md#runtime-delivery); agents do not inherit those values by default. Transcripts are sensitive artifacts; content recording is independently configurable. Shell/network access follows the M1 trust model in [identity](identity-tenancy-and-rbac.md).

## Acceptance

| ID | Scenario | Requirements |
| --- | --- | --- |
| AR-A01 | Pinned wrapper/profile passes compatibility smoke tests; unknown version fails | AR-01, AR-02 |
| AR-A02 | Repository instructions cannot grant another tenant's tool/source access | AR-03, AR-05 |
| AR-A03 | Budget exhaustion stops new admission and preserves result/usage | AR-04, AR-10 |
| AR-A04 | Exit zero without structured outputs fails finalization | AR-06 |
| AR-A05 | Missing business input records a question and fails; revised intent starts a new run | AR-13 |
| AR-A06 | Cancellation revokes actions and stops the wrapper/process tree | AR-09 |
| AR-A07 | Valid commit/evidence are published; agent identity cannot approve | AR-11, AR-12 |
| AR-A08 | Moving an upstream/blueprint branch during execution does not change instruction context; an architectural conflict is reported in the candidate's conformance evidence and cannot become a prose-only success claim | AR-01, AR-06, AR-14 |
