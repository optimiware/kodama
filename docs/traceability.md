# Capability traceability

Active requirements and design preferences are in the [specifications](README.md#contributors); each has local acceptance mappings. AR-07/AR-08 and DQ-12/DQ-A08 are retired by the [KISS review](review-kiss.md). Historical ADR text is rationale, not an additional current requirement.

| Capability | Requirements | Task ownership / integration acceptance |
| --- | --- | --- |
| Platform core and hosted agent/SDLC integration | PI-01–PI-10, IT-13, VS-13 | T-010/T-012/T-033 consumer ports; selected O-18 OAuth UUID/roles and O-19 secret delivery → T-061 real-platform acceptance → T-056 |
| Vertical capability packages, ports/adapters, entities, and design conformance | SD-01–SD-15 | Capability baseline/review/tests; T-003 fixtures; T-087/T-088 memory kernel and contracts; T-062/T-073 transaction prerequisites; T-070/T-071/T-072/T-075 incremental assembly; T-027/T-039/T-046/T-060 final joins |
| OIDC, tenancy, fixed RBAC, audit | IT-01–IT-11 | T-002, T-010, T-019, T-026 |
| Shared run workspace, support services and rootless builders (O-28; `run_shared` model, designed — not delivered or qualified) | WS-13–WS-16, PX-17–PX-22, IT-14–IT-16, VS-14, DQ-16; acceptance WS-A09–WS-A12, PX-A13–PX-A18, IT-A10–IT-A12, VS-A11, DQ-A12 | T-109 contracts → T-110 executable pipeline/runtime v2 → T-111 profile qualification → T-112 lifecycle/persistence → T-113 entrypoint/authority/publication ∥ T-114 Kubernetes ∥ T-115 Podman/Process → T-116 assembly/chart/migration → T-117 integrated acceptance. Existing WS-05, PX-09, PX-14 and IT-07 are labeled `[attempt_private]`, not renumbered |
| Work/spec history, WIP, human completion | WM-01–WM-04, WM-06–WM-11 | T-011, T-025, T-034, T-044 |
| Dependencies and flow summaries | WM-05, WM-12 | T-045, T-042, T-044 |
| Git workspaces and CAS publication | WS-01–WS-12 (`attempt_private`; WS-13–WS-16 for `run_shared` above) | T-013, T-016, T-022, T-026 |
| Durable execution and JetStream | PX-01–PX-16 (PX-17–PX-22 `run_shared` above) | T-005, T-014, T-020, T-021, T-022, T-086, T-026; O-25 launch-evidence contract/PX-A12: T-102 → T-089 real Kubernetes component qualification |
| API/CLI, logs, event streams | AC-01–AC-06, AC-10–AC-12 | T-004/T-015/T-017; T-023 service → T-063 upload → T-064 qualification; T-024/T-025; T-070/T-071/T-027 |
| Variables and secrets | VS-01–VS-13 (VS-14 `run_shared` above) | T-012, T-028, T-029; T-061 qualifies hosted proxy/script delivery |
| OpenCode profile/entrypoint | AR-01–AR-06, AR-09–AR-14 | T-003/T-030; T-074 early protocol → T-031 real wrapper; T-072/T-038 |
| Model gateway and MCP proxy | GM-01–GM-12 | T-032, T-033, T-038 |
| Source/evidence gates | DQ-01–DQ-06, DQ-11, DQ-13–DQ-14 (DQ-16 `run_shared` above) | T-003/T-034; T-084 unit policy; T-068 CLI; T-072 wiring; T-073/T-038 actual SQL/checkpoint; T-044 business loop |
| Plain build/deploy, preview/stage | DQ-07–DQ-10, DQ-15 | T-076/T-035/T-062 → T-077/T-066 unit-tested binding → T-072 wiring; T-065/T-067/T-038 actual local checkpoint; T-037 CLI; blueprint expansion deferred T-047/T-069 |
| Container-first SDLC and optional managed agents (ADR 0012; pending) | PX-23/PX-24, AR-15/AR-16, VS-15; PX-A19/PX-A20, AR-A09/AR-A10, VS-A12 | T-140 checkpoint → T-142 input/secret/profile integration → T-145 assembly → T-146 actual acceptance |
| Native artifact catalogue, immutable releases and publication (ADR 0012; pending) | AF-01–AF-10; AF-A01–AF-A08 | T-140 → T-141 records/retention → T-142 consumption → T-144 publication → T-145 CLI/API → T-146 SQL/S3/registry/isolated-target evidence |
| Generic delivery, generated-output evidence and rollback (ADR 0012; pending) | DQ-17–DQ-21; DQ-A13–DQ-A16; DQ-14 explicitly scoped to required managed/project conformance | T-140 versioned contract → T-142 gates/provenance → T-143 records → T-145 legacy drain/removal → T-146 acceptance; DQ-16/DQ-A12 stay legacy until cutover |
| E2E, exploratory QA, defects | AR-12, DQ-06, DQ-10 | T-040 mapping → T-075/T-046 unit wiring → T-044 local checkpoint; browser/profile qualification deferred to T-069 |
| Git triggers and ChatOps | AC-07–AC-09 | T-036, T-041, T-075, T-046/T-044 |
| Helm and local workflow | Installation / entrypoint contracts | T-001, T-018, T-027, T-026; T-103 remaining M1 defects; O-24/O-26: T-089 component checkpoint, T-105/T-103 → T-104 live KMS, ready chart and assembled Kubernetes roundtrips → T-090 staging |
| Recovery, retention, offboarding | IT-12, VS-10, WS-12 | T-029, T-056 |
| Application guidance/templates | Blueprint guidance, AR-01, AR-14 | T-043, T-047; T-059 supplies tenant/project Markdown Git guidance, not runtime library extraction |
| Optional Platform workbench and analyst UI | Existing public contracts | Deferred T-050, T-051, T-060; separately owned upstream UI implementation |
| More harnesses and native runtime | Agent/runtime contracts | T-052–T-054, T-060 |
| Trusted-engineer baseline / later isolation / operator | O-07 regular-container scope; IT-07/IT-08 | T-021/T-026 and T-056 qualify the current trusted profile; stronger isolation T-055 and operator T-057/T-058 are deferred |

Task IDs and exact dependencies resolve through the [backlog index](tasks/README.md). Integration acceptance uses real dependencies; schema/fake validation alone does not establish Kubernetes/Git/authorization behavior.

SD-A07–SD-A09 cover explicit single-module composition, seam ownership/failure/replay and component-versus-assembly evidence. The [seam table](implementation/repository-layout.md#package-seam-contracts) maps transaction/wiring/fixture owners. Completed history and active T-021/T-025/T-028/T-032/T-033/T-034 acceptance remain intact; T-022 is done at `deff2e1`. Splits redistribute unstarted obligations explicitly rather than declaring them satisfied.

## Documentation acceptance

Check links/anchors, fenced JSON/YAML/shell syntax, example graph/output/source references, active requirement coverage, task dependency resolution/cycles, and overlapping owned paths between unordered tasks. Code claims remain planned until task evidence is merged. Open provider/release decisions remain explicit blockers.
