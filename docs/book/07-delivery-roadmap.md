# 7. Delivery roadmap

Work climbs three environments ([ADR 0009](../adr/0009-environments.md)): policy in `development` (in memory, no containers), real services and containers on `test` (Podman, no cluster), then Kubernetes on `production` (kind, then the stage cluster). Each milestone is first built and unit-tested in `development`, then checkpointed on `test`; Kubernetes conformance and staging follow M1 and M3 explicitly.

The [task graph](../tasks/README.md) is the executable backlog. Preserve merged packages; complete local Go behavior before spending time on integration or production qualification. Unit-tested contributions may merge before their real-service checkpoint, but package completion never implies milestone acceptance.

## M1 — local CI server

Finish orchestration, entrypoint, results/logs and their unit-test seams in parallel (T-080/T-081/T-082), then wire the executable/API/CLI (T-071 → T-027). Think small Drone-like CI: submit a pipeline, execute containers, retain results/logs and inspect/cancel a run. Use existing consumer-owned ports, controlled time, in-memory adapters and httptest; retain real adapters in production constructors.

**Checkpoint T-026:** one prepared Podman dependency stack and the Podman runtime, with no cluster. Prove actual source → container → report/log, cancellation, restart/replay, publication CAS and tenant denial. Real SQL/RLS/rollback/provider behavior is verified here, not inferred from mocks. This is local development acceptance, not cloud KMS/hosted/production qualification.

**Kubernetes component conformance T-102 → T-089:** the real Kubernetes runtime, restricted baseline and chart installation/fail-closed configuration checks on kind (O-24/O-25). This checkpoint does not claim a ready installed server or full Kubernetes M1 acceptance. **Full readiness T-104** retains the assembled M1 roundtrips and ready chart after production startup wiring (T-105 under O-26) and known-defect cleanup (T-103); it includes live KMS acceptance and remains deferred. **Staging T-090** (explicitly scheduled) depends on that full readiness before deploying the trusted-engineer appliance to Platform's stage cluster.

## M2 — agentic core business logic

After M1, build/test native-trigger policy, environment pinning, cleanup, profiles/model budgets, tool authority, human gates and delivery CLI in independent Go packages. T-072 → T-039 wires them. Deterministic model/tool responses keep the implementation loop local and reproducible; no live credentials, model spending or wrapper-image builds in component merge gates.

**Checkpoint T-065/T-073/T-067 → T-038:** qualify ordinary deployment scripts, real gate transactions, pinning/reconciliation/cleanup and the installed agent/review/preview loop against Podman services and the Podman runtime, with kind only as the deployment target. The model/agent adapter remains deterministic in this local checkpoint. Actual pinned wrapper/model/provider smoke is explicit deferred T-085 scope; do not claim it from fixture output.

## M3 — local business loop

T-075 → T-046 wires evidence-to-defect mapping, authenticated gate/notification links, work dependencies and flow summaries with unit tests. T-044 qualifies actual local persistence/replay/human completion. Browser-agent QA and application-stack expansion remain separately scheduled.

## After M3 — scheduled expansion
O-26 schedules [T-105 secrets startup wiring](../tasks/T-105-secrets-startup.md), extracted from T-029. Full Kubernetes readiness T-104 now follows T-105/T-103 and includes live KMS validation during deployment; complete disaster recovery remains T-029. T-104/T-090 retain explicit scheduling requirements.


Defer chassis guidance (T-059), Supabase blueprint (T-047), browser QA/profile qualification (T-069), live agent/wrapper qualification (T-085), additional harnesses (T-052–T-054), optional Platform UI, stronger isolation and operator work. Keep completed wrapper/blueprint/adapter packages; they need not be rewritten or repeatedly qualified while finishing the core.

The 2026-10-22 owner interview confirmed the blueprint/chassis deferrals (O-22) and scheduled only the runtime-contract prerequisite T-102 and remaining M1 defect cleanup T-103 alongside core acceptance. Full Kubernetes readiness T-104 and its production prerequisites still require explicit scheduling.

Recovery/release (T-029/T-056), hosted Platform authority/credentials (T-061), external provider adapters and production operations require explicit scheduling. The [platform integration contract](../specs/platform-integration.md) remains the hosted authority source; local acceptance does not certify it. Deferred work is scope, not a reason to block M1/M2 local Go progress.

## Integration rule

One module, disjoint package ownership and a single ordered owner of shared app/API/client files. Small consumer-owned seams make domain policy unit-testable; actual SQL/concurrency/provider semantics retain named integration owners. See [testing](../implementation/development-and-testing.md).

Scoped unit checks plus independent review gate Go contributions. Every five task merges and at drain, batch-check main once. Start with core-package unit tests (Make's `GO_CORE_PACKAGES`), add bounded M1/M2 integration at their checkpoints and reuse the stack. Image wrappers/templates remain explicit owner qualification, not portable core packages. OpenForge/development Python tests run only for their inputs. No automatic full-suite merge gate or age trigger.
