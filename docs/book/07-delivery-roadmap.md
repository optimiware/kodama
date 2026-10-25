# 7. Delivery roadmap

Kodama is built in milestones. Each one is first written and unit-tested in memory (`development`), then proven against real services and containers (`test`, using Podman and no cluster), and only then qualified on Kubernetes (`production`, first on kind, then on a stage cluster). See [ADR 0009](../adr/0009-environments.md). Unit-tested work may merge before its real-service checkpoint, but a finished package never counts as an accepted milestone.

What works today is on the [project status](../status.md) page. What is still open is the [task backlog](../tasks/README.md).

## M1: local CI server (done)

Think of a small CI server: submit a pipeline, run it in containers, keep results and logs, and inspect or cancel a run. It covers orchestration, the step entrypoint, results and logs, the HTTP API and the CLI.

**Checkpoint.** One prepared Podman stack and the Podman runtime, with no cluster. It proves source to container to report and log, cancellation, restart and replay, publication compare-and-swap, and tenant denial against real PostgreSQL, JetStream, S3, Git and OIDC ([report](../acceptance/m1.md)). This is local acceptance, not cloud KMS, hosted or production qualification.

**Kubernetes component conformance (done).** The real Kubernetes runtime, its restricted baseline and the Helm chart's refusal of invalid production configuration, on kind ([report](../acceptance/kube.md)). It does not claim an installed, ready server.

## M2: agentic core (done)

Agent profiles and model budgets, tool authority, human gates, environments and deployments, the delivery CLI and the native Git push trigger, all wired into the one server. Model and tool responses are deterministic, so the loop stays local and reproducible: no live credentials, model spending or wrapper-image builds in the merge gates.

**Checkpoint.** The installed agent, review and preview loop against Podman services, with kind only as the deployment target ([report](../acceptance/m2.md)). Real model, provider and wrapper smoke tests are not part of it (T-085).

## M3: local business loop (done)

Work dependencies, flow summaries, evidence-to-defect mapping, and authenticated gate and notification links, qualified against real local persistence and replay ([report](../acceptance/m3.md)). The report records one gap: the assembled server cannot yet move an item from review to done by explicit human completion. Browser-agent QA and further application stacks are scheduled separately.

## Next

- **Full Kubernetes readiness (T-104).** An installed server that becomes ready on Kubernetes, with live KMS validation and the original M1 round trips. It follows the production startup wiring for cloud KMS and the known M1 defect cleanup, both merged.
- **Trusted-engineer staging (T-090).** The deployment to the stage cluster, after full readiness.
- **Recovery and release (T-029, T-056).** Source, data and key recovery drills, measured recovery objectives and scale, and a supported release.
- **Hosted Platform integration (T-061).** Hosted authority and credentials, qualified against the real Platform caller. The [platform integration contract](../specs/platform-integration.md) stays the source of truth for it; local acceptance does not certify it.

## Later

Each of these needs explicit scheduling: Markdown chassis guidance (T-059), the Supabase blueprint (T-047), browser QA and profile qualification (T-069), live agent and wrapper qualification (T-085), additional harnesses (T-052 to T-054), an optional Platform UI, stronger isolation for untrusted hosted execution (T-055), and operator automation. Completed wrapper, blueprint and adapter packages are kept and need not be rewritten while the core is finished.

## Engineering rules

One Go module with disjoint package ownership, and a single owner for shared app, API and client files. Small consumer-owned seams keep domain policy unit-testable, while SQL, concurrency and provider behavior keep named integration checks. See [testing](../implementation/development-and-testing.md).

Scoped unit checks and independent review gate Go changes. Integration suites run at the milestone checkpoints and reuse their prepared stack. Image wrappers and templates are qualified by their owners, not as portable core packages.
