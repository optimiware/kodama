# test/integration/harness-opencode — acceptance of the OpenCode wrapper image (T-031)

Resource prefix `t031`. Acceptance checks `opencode_real_smoke`, `structured_result_required`, `cancel_wrapper`,
`tool_scope_denied`, `design_baseline_pinned` and `design_conflict_reported`, run by
`Test_opencode_real_smoke`, `Test_structured_result_required`, `Test_cancel_wrapper`, `Test_tool_scope_denied`,
`Test_design_baseline_pinned` and `Test_design_conflict_reported`.

```bash
source .harness/test-env.sh        # PostgreSQL, S3, registry, kind kubeconfig; KODAMA_REQUIRE_* = 1
go test -race -count=1 ./test/integration/harness-opencode/...
```

Nothing skips: a missing cluster, registry, PostgreSQL, S3, container provider, OpenCode binary (`OPENCODE_BIN`, default
`$HOME/.opencode/bin/opencode`, checksum-verified) or image build **fails** the test. `kubectl` is taken from `PATH`
or from the pinned `dev/.tools/kubectl-*` next to the kubeconfig. The first test builds and pushes the wrapper image
with `images/harness-opencode/build.sh` and the entrypoint injector, and every Job runs by digest.

## The composition (test-only)

`world_test.go` wires the merged modules the way the composition root eventually will; production wiring
(T-027/T-039) is not touched.

| Real | Notes |
| --- | --- |
| PostgreSQL | every dependent schema (execution, runtime authority, gateway, MCP proxy, agents, artifacts, workspaces, secrets) |
| Kubernetes executor, observer, reconciler | kind cluster from `.harness/test-env.sh`; the actual `kodama step` entrypoint runs in the Jobs |
| runtime authority, results, artifacts, workspace publication | S3 (LocalStack) and a real HTTPS Git remote |
| agent profile service and admission chain | profile → wrapper payload, design baseline, frozen step |
| model gateway | real `internal/gateway` over its PostgreSQL store, forwarding over TLS |
| MCP proxy | real `internal/mcpproxy` in front of a real Streamable HTTP MCP server (`test/support/mcp`) |
| OpenCode 2.1.2 and the wrapper | in the image, in the Job |

| Double / fixture | Why |
| --- | --- |
| model upstream (`model_test.go`) | a deterministic OpenAI-compatible HTTPS server scripting text, tool calls, a stalled stream and failures, recording every request. **A control, not a provider or model claim** — there is no approved external model in this environment, so `opencode_real_smoke` runs against it through the real gateway |
| MCP tools (`echo`, `mutate`, `slow`, hidden `admin_secret`) | `test/support/mcp` |
| profile approvals, MCP registration authority | the hosted sources are T-039's |
| three TLS listeners under one test CA | the platform origin (runtime API + gateway + MCP proxy on one server), a Pod-reachable S3 front (upload instructions must name an address Pods can reach) and a read-only Git front for Pods (the Job now delivers the checkout credential and Git CA, T-079; the front injects no authorization). The host reaches the same listeners on loopback because the container provider's host alias is not routable from the host |

## Acceptance map

| Test | What it proves |
| --- | --- |
| `Test_opencode_real_smoke` | An unqualified OpenCode version cannot register (AR-A01). A pinned profile expands to a Job; the entrypoint admits and checks out; the wrapper runs OpenCode through the gateway (the upstream only ever sees the gateway's provider credential) and the MCP proxy; a tool result reaches the model; the agent writes a file and a structured result; the entrypoint publishes exactly the wrapper's commit (branch tip == candidate == `sourceCommit`), the result file is not in it; the conformance report is bound to the baseline digest and candidate and — honestly — blocked (M-6); usage rows are recorded; the redacted transcript contains no runtime token although the agent read the token file; the retained command log carries no model or tool content |
| `Test_structured_result_required` | prose-only success, a missing summary, an unknown member, an unknown outcome, non-JSON, a failing model and an agent-reported failure each fail the attempt with the wrapper's reason, publish nothing and fail the run; `needs_input` fails with its question retained, publishes nothing and is not retried |
| `Test_cancel_wrapper/cluster` | a run cancelled mid model stream: attempt and run `cancelled`, the upstream stream cancelled, the interrupted request never `succeeded`, the attempt's own token refused by the gateway (no late request reaches the upstream) and the MCP proxy, the Job gone, nothing published |
| `Test_cancel_wrapper/process_tree` | the **real** wrapper and **real** OpenCode on the host in their own process group, signalled like the entrypoint does: the agent's `setsid` escapee and OpenCode's private server (neither in the group) are descendants of the wrapper (subreaper) and all die; exit 143, no verdict, no conformance artifact |
| `Test_tool_scope_denied` | OpenCode offers only the pinned MCP tool and none of web/skill/subagent/question/execute; a registered-but-unpinned tool and an unregistered one never run; a direct call with the attempt's own token to the proxy for a hidden tool is denied while the pinned tool works |
| `Test_design_baseline_pinned` | the upstream design branch moves and a newer profile version pins the new commit: the admitted attempt's prompt, pinned context and report keep the old digest and commit; the new commit reaches only a new run; a bare profile name is refused |
| `Test_design_conflict_reported` | an unresolved material deviation yields a retained, sorted, candidate- and baseline-bound **blocked** report that `CheckConformance` refuses with `conformance_blocked`; a stale baseline is refused too |

## Limits

See the image README's mismatch table (M-1…M-8) and the list of unproven controls. In particular no run here outlives
a token lifetime, no external provider or model was used, and no wrapper report can be `conforms` until evidence
binding exists. The wrapper's pure parts have unit tests next to it (`images/harness-opencode/wrapper`).
