# OpenCode wrapper protocol evidence

Evidence index and synthetic boundary cases for the T-074 early OpenCode wrapper protocol checkpoint. The narrative, compatibility matrix, unresolved gaps and ordered prerequisites are in the [protocol document](../../docs/implementation/opencode-wrapper-protocol.md). This directory holds no code, schema or adapter, and it freezes only the formats below.

| File | Contents |
| --- | --- |
| [`sources.json`](sources.json) | `version`, `sources`: every upstream, measured, local-contract, synthetic and unresolved evidence item |
| [`cases.json`](cases.json) | `version`, `cases`: synthetic positive and negative boundary cases mapped to the runtime and profile envelopes |

## What the evidence is

- **Candidate, not support.** OpenCode `2.1.2` is the candidate the coordinator measured on 2026-10-21. Nothing here selects a production version, provider, flag or image, and nothing populates the compatibility catalogue. T-031 pins, builds and tests the real image after the gateway and MCP tasks merge.
- **Provenance.** Documented sources are OpenCode V2 pages and its served OpenAPI schema (retrieved 2026-10-21). Measured items are the coordinator's observations of the local binary and were not reproduced by the T-074 implementer. Upstream pages are living pages with no immutable revision (`revision` is `null` unless a content digest or version was recorded); that is gap U-11.
- **Synthetic fixtures.** Every input in `cases.json` is invented from the local contracts. Nothing in it was captured from OpenCode, a provider or an MCP server, and its `expected` values (`basis: contract-derived`) are expectations of Kodama's existing contracts, not measurements of OpenCode.

## `sources.json`

`kind` is one of `documented`, `coordinator-measured`, `local-contract`, `synthetic-fixture`, `unresolved`. Entries carry `id`, `kind`, `location`, `revision`, `observedAt`, `claim`, `provenance`, `limitations`. Unknown revisions and locations are explicit `null`. Local contracts use the repository commit `80cd84f`. Unresolved gaps `wp-gap-*` carry the statements U-01 to U-12 used by the protocol document; an assumed control without a primary source must be one of them.

## `cases.json`

Case ids are stable: `wp.<boundary>.<positive|negative>.<name>`. Entries carry:

| Field | Meaning |
| --- | --- |
| `id` | Stable case id |
| `requirements` | Requirement and acceptance ids (AR-nn, GM-nn, AR-Ann, GM-Ann) |
| `sourceRefs` | Ids in `sources.json` |
| `contractRefs` | Authoritative `path` and `selector`: a fixture `name=` in the runtime fixtures, a file in the gateway manifest, or a Go identifier |
| `input` | Synthetic scenario input. Malformed raw input is a string; JSON payloads stay valid |
| `expected` | Contract-derived expectation; not a production HTTP schema |
| `evidenceKind` | Always `synthetic-fixture` |
| `qualificationGate` | The task that must confirm the expectation against real behavior (T-031 preparation, T-032 owner, T-033 owner, T-031 qualification) |

Placeholders such as `sha256:aaaa…`, commit `1111…` and `<…>` are not real values.

## Mapping

| Boundary | Envelope reused |
| --- | --- |
| Profile and configuration | `agents.WrapperConfig` (`kodama.agent-config/v1`, 3072-character chunks), catalogue tuple, runtime `Context` |
| Completion, input needed, publication | entrypoint results manifest, runtime `ResultRequest`, `SourceProposal*` ([runtime contract](../runtime/README.md)) |
| Conformance | runtime `DesignBaseline` and `ConformanceReport` |
| Gateway | `chat-completions-stream/1` fixtures in `internal/gateway/testdata/chat-completions/` |
| MCP | `internal/mcpproxy` pinned SDK/protocol and proxy session behavior |

## Checks

These files are checked for JSON syntax by `python3 scripts/dev/check.py config` (all JSON under `contracts/`) and for links and fences by `make docs-check`. Neither validates the semantics or executes a case; the semantic review is recorded in the protocol document. They are not Go tests and do not satisfy `opencode_real_smoke`.
