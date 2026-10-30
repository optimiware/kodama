# Container-first SDLC: implementation and planning handoff

**Status:** owner-approved design on 2026-10-30; documentation prepared for the next plan-mode session. Implementation tasks are recorded as deferred for that planning pass, not dispatched. Keep using this task's worktree as the owner requested.

**Worktree:** `.work/artifact-vision-review`, branch `task/artifact-vision-review`.

**Decision:** [ADR 0012](../adr/0012-container-sdlc-and-artifact-releases.md), [O-34](../decisions.md#container-first-sdlc-and-artifacts--2026-10-30-o-34).

**Contracts:** [artifacts/releases](../specs/artifacts-and-releases.md), [wire checkpoint](../contracts/artifacts-and-releases.md), [delivery](../specs/delivery-and-quality-gates.md), [agents](../specs/agent-runtime.md).

## Review baseline and disposition

| Field | Baseline |
| --- | --- |
| Repository/source | `github.com/optimiware/kodama`, `2ff20310e4fd5005bfb5c5ba783113e9619fb076` (integrating ADR 0011) |
| Inspected behavior | `internal/artifacts/`, `internal/app/registry.go`, `internal/delivery/environment.go`, `internal/delivery/provenance.go`, `internal/app/delivery.go`, `internal/entrypoint/`; owning specs at the same commit |
| Existing invariants | C-02/C-11/C-12/C-14, ADRs 0005/0008/0010/0011, O-19/O-30/O-31; tenant authorization, exact pins, shared-workspace fencing, Git authority and unknown-effect handling |
| Intentional changes | O-34; AF-01–AF-10, PX-26/PX-27, AR-16/AR-17, VS-15, DQ-18–DQ-22; DQ-16 retained only for legacy contexts; managed conformance scoped by DQ-14 (branch-local checkpoint `da47b93c` used PX-23/24, AR-15/16, DQ-17–21 before reconciling with ADR 0011's PX-23–25, AR-15, DQ-17) |
| Adjacent source | Native Git [ADR 0011](../adr/0011-internal-tenant-git-service.md) and [implementation plan](internal-git-service.md), integrated at `2ff20310e4fd5005bfb5c5ba783113e9619fb076` |
| Disposition | Intentional owner-approved contract refinement; executable compatibility checkpoint and independent code review remain required before implementation publication. Documentation acceptance is not runtime qualification |

Pin the actual documentation candidate commit before coding; record it in the task review handoff. Never infer that the unchanged current schemas already implement these semantics.

## Desired end-to-end path

```text
task / repository event
  → container steps (optional managed agent profile)
  → shared run workspace
  → declared verified artifacts
  → immutable named release
  → ordinary publish container → target-accessible registry/bucket
  → ordinary deploy container + optional generic deployment record
  → later rollback run selecting the prior exact release
```

An internal chart/archive can be fetched by the deployment container and applied/uploaded directly. Production does not need a path to Kodama. Images/charts need a complete, retained copy at a reachable registry when the target pulls them.

## Reuse, reduce and retire

| Surface | Disposition / owner |
| --- | --- |
| `internal/artifacts` reserve/complete/seal, pins, S3, scoped reads | Reuse. T-141 adds release manifests/discovery/retention; T-144 extends publication verification. No new artifact daemon |
| `internal/artifacts/oci` manifest verification and `internal/app/registry.go` | Reuse bounded clients/policy; preserve media type and source-free/multi-source provenance. Add full-copy verification for publication; do not mistake the current manifest hash for retained image layers |
| `internal/delivery/environment.go`, `snapshot.go`, `provenance.go`, deployment records | T-143 replaces mandatory namespace/kubeconfig/single image with generic config and exact release/artifact set; retain pins, history and pre-apply action identity |
| `internal/delivery/adapters/effects.go`, `ToolQualification` in `internal/app/delivery.go`, `delivery-tools.json` settings | Retire from the new path. T-143/T-145 remove catalogue-driven automatic replay and provider interpretation after legacy obligations drain. Missing receipt stays unknown |
| Mandatory object annotations/UID/readiness callbacks, `images/deployment-tools/` | Move provider behavior into optional scripts/recipes; remove mandatory core protocol. Keep generic outcome/external references; migrate callers and delete obsolete protocol tests/settings with the old implementation |
| `internal/deliverycleanup` | Keep only optional expiry → explicitly bound ordinary cleanup run scheduling and history; no universal Kubernetes deletion engine or implied target ownership |
| `internal/agents` expansion, gateway and MCP proxy | Reuse optional managed capabilities. T-142/T-145 remove coupling that makes their installation catalogue/configuration mandatory for generic harness containers. Preserve claims for mediated profiles only |
| `internal/execution/adapters/secretroute.go`, disclosure scans | Reuse/complete explicit step + environment binding union and secret-using writer path. Test current code first: T-107 notes are older than T-113 and are not a reason to reimplement working code |
| `internal/gates`, result finalization | Keep one gate/decision mechanism and ordinary results. T-142 adds explicit artifact/release applicability and honest lineage, retaining legacy subject hashes; no new release approval service |
| `internal/workspace`, source bundle/publisher | Hosting-specific authority stays with ADR 0011 and legacy callers. Do not remove the external publisher as part of artifact simplification or rename all database/API workspace fields |
| Terraform/OpenTofu state, report rendering, registry implementation | Leave to native tools/backends. The first release layer stores outputs and receipts; no state-locking service, HTML host or custom OCI server |

Every removal must identify remaining readers, admitted runs, cleanup obligations and fixtures. Once they are migrated/drained, delete obsolete active branches/settings rather than preserving parallel delivery engines indefinitely.

## Ordered implementation tasks

```text
T-116, T-131 (run_shared assembly + native Git source integration)
  → T-140 executable contracts
  → T-141 artifact/release records
  → T-142 container inputs, secrets, profile and gate integration
  → T-143 generic deployment/cleanup records
  → T-144 publication verification and ordinary tool recipes
  → T-145 API/CLI/assembly, migration and surface removal (also after T-132)
  → T-146 real acceptance (also after T-117, T-133)
```

| Task | Main responsibility | Required evidence |
| --- | --- | --- |
| [T-140](../tasks/T-140-container-sdlc-contracts.md) | Small versioned executable checkpoint, canonical manifest/subject vectors, errors and action grants | Schema/fixture and legacy compatibility checks |
| [T-141](../tasks/T-141-artifact-release-catalog.md) | Catalogue, immutable releases, scoped operations, transactional reference retention | Memory/unit contract; SQL/RLS/pin races at T-146 |
| [T-142](../tasks/T-142-container-inputs-and-provenance.md) | Admitted input fetch, secrets union, optional profiles, artifact-aware evidence | Exact-input/authority/unknown-state and managed/direct-path checks |
| [T-143](../tasks/T-143-generic-delivery-records.md) | Tool-neutral environments/deployments, explicit cleanup/rollback, no image catalogue for new calls | Replay, unknown-effect, config/credential pin and legacy record checks |
| [T-144](../tasks/T-144-artifact-publication-recipes.md) | Verified copy receipts; ordinary image/chart/file publication recipes | Complete-content/mismatch/destination-policy checks |
| [T-145](../tasks/T-145-container-sdlc-assembly.md) | CLI/API/client/chart wiring, migration/drain, removal inventory | Composition, API compatibility, meaningful deletion checks |
| [T-146](../tasks/T-146-container-sdlc-acceptance.md) | Real build → release → isolated-target deploy → rollback and recovery | PostgreSQL/S3/registries/containers; qualified stage runtime where required |

The serial chain deliberately orders overlapping capability, migration and shared composition paths. Within this owner-requested continuation, execute one task scope at a time in the same worktree, recording task-stage checkpoints. If later handed to the factory, restore its normal one-worktree-per-task workflow rather than starting several writers here.

## Coordination with native Git and active run-resource work

ADR 0011 (`T-125–T-133`, `O-31–O-33`) is integrated from `main` at `2ff20310e4fd5005bfb5c5ba783113e9619fb076`. This work uses `T-140–T-146` and `O-34`, with requirement IDs reconciled (`PX-26–PX-27`, `AR-16–AR-17`, `DQ-18–DQ-22`). Its internal Git direction remains intact: multi-repository source sets, direct scoped agent pushes, task/default-branch triggers and project-scoped source-free runs belong to that owner.

Task dependencies explicitly order shared paths after the native Git tasks: `T-140` depends on `T-116` and `T-131` (transitively `T-126`), `T-145` depends on `T-144` and `T-132`, and `T-146` depends on `T-145`, `T-117` and `T-133`.

T-116 and T-117 retain their existing workspace/service obligations; their owners' worktrees are not edited here. ADR 0012's source/provenance changes take effect only at the later versioned cutover. Do not renumber or reinterpret their frozen schemas to avoid a migration.

## First plan-mode pass

1. Read ADR 0012 and the semantic contract, then inspect the current implementation and any newly merged T-116/native Git work. Refresh the immutable baseline and DAG as above.
2. Freeze the smallest interface changes in T-140: release manifest, scoped workload fetch, generic deployment receipts, secret-binding merge, source-set/provenance and canonical gate-subject compatibility. Prefer existing container syntax and artifact inputs; add a format/context version only where compatibility requires it.
3. Inventory legacy consumers and a bounded drain/removal plan. Reuse working source/secret/runtime pieces instead of rebuilding them. Keep source-free CI/release admission with the native Git owner rather than creating a competing run-purpose model.
4. Plan a vertical slice through one file build/report, one OCI image/chart, one release, a target-accessible copy and a generic deployment; then add rollback and failure cases. Direct harness admission can be tested with a local stand-in; live Claude/OpenCode/provider support remains separately qualified.
5. Promote the agreed task scopes for implementation and begin in this same worktree. Keep scoped checks/review before merge; retain the worktree for the owner-requested continuation.

## Documentation verification and pending evidence

Run `make docs-check`, `git diff --check`, and validate task metadata/dependencies/ordered ownership plus requirement-to-acceptance coverage. `make docs-check` currently checks links, anchors and JSON/shell fences only; task graph checking is a separate obligation. The separate OpenForge checkout is unavailable in this environment, so its `validate`/generated index cannot be claimed. Preserve its generated block and use the explicit task section/index until regeneration is available.

No product code/schema or deployment has changed in this design task. Independent review and executable tests/qualification remain pending with their owning tasks; this handoff must not be described as a delivered release service.

### Checks recorded for this documentation handoff

- `make docs-check`: passed (141 Markdown files; links, anchors, JSON/shell fences).
- `git diff --check` and `git diff --cached --check`: passed after removing Markdown hard-break trailing spaces from the new files.
- `go run /private/var/folders/4p/0wkwljg50plfl7m51cwcyrhw0000gn/T/opencode/kodama-adr0012-doccheck.go`: local read-only checker, passed: 44 task records with unique IDs/existing dependencies/no cycles; T-140–T-146 metadata/index agreement and ordered ownership against active tasks; 20 new requirements with acceptance mappings; 11 YAML fences in the 49 changed/new documentation files. The checker is a task-local temporary utility, not a new repository dependency or product test.
- OpenForge task/index validation: unavailable because `/Users/mod/Work/optimi/openforge` is absent. The local checks above are reported separately and do not claim an OpenForge verdict.

Review status: documentation is ready for the owner's planning pass; no independent review approval or merge is recorded.
