# Design conformance and drift control

**Status:** contribution/review procedure now; agent-produced evidence is a proposed M2 contract implemented by T-031/T-034. This procedure uses existing source revisions, run context, artifacts, and gate evidence rather than introducing a design-policy service.

## One owner per fact

| Fact | Authoritative owner | Check against |
| --- | --- | --- |
| Company-core/agent-layer boundary | [Platform integration](../specs/platform-integration.md); upstream owns its contracts | Hosted adapters and supported upstream revision |
| Direction and unresolved choices | [Decision register](../decisions.md); ADRs preserve rationale | Task blockers and proposed changes |
| Domain language | [Glossary](../../CONTEXT.md) | Specifications, DTOs, examples, and agent instructions |
| Behavior and invariants | Narrowest specification and its stable requirement/acceptance IDs | Domain tests, use cases, and integration evidence |
| Wire shapes | [Contract index](../contracts/README.md); generated/maintained schemas when implemented | Handlers, clients, runtime wrappers, and fixtures |
| Physical state/configuration | Migrations, registries, and deployed configuration once implemented | Adapter queries, upgrade/restore tests, and operations |
| Task ownership/readiness | Task frontmatter and merged dependency evidence | Index, dependency graph, changed paths |
| Delivered capability | Implementation, actual CI workflow, and retained acceptance/release evidence | README, milestone status, and support claims |

Prose explains executable owners rather than duplicating their route, schema, role, or settings inventories. Until executable owners exist, Markdown contracts remain proposed and implementation tasks supply their schemas. A generated artifact is not a new source of policy.

## Review baseline

Before implementation, record the smallest relevant baseline in the task handoff or work-item specification:

| Field | Required content |
| --- | --- |
| Subject | Task/work item, tenant/project when applicable, candidate/source commit |
| Requirements | Owning specification paths and stable requirement/acceptance IDs |
| Decisions | Applicable confirmed choices, ADRs, and unresolved blockers |
| Sources | Repository, immutable revision and path for each design source; upstream platform revision for an integration; blueprint/profile version when used |
| Boundaries | Owning module, consumer ports, allowed dependencies, state/transaction owner, affected external effects |
| Verification | Named checks, command/result and retained evidence, including denial/recovery where relevant |
| Disposition | Conforms, or an intentional change with rationale, changed owner contract, compatibility/repair plan and review status |

Use paths at the pinned source revision for local design documents and immutable upstream references for external ones. Prompts must not resolve `main`, `latest`, or an unversioned blueprint again midway through a run. A new baseline starts a new run or reviewed task revision; historical evidence keeps its original baseline. The baseline contains references and safe metadata, not credentials or copied tenant responses.

For agent runs, deliver this baseline through the existing pinned specification/instruction context. T-003/T-030/T-031 freeze and test its serialization with the runtime context; no independently invented API fields are implied by this review template. Evidence is an ordinary declared artifact, included by ID and digest in the existing gate subject.

### Agent report contract

T-003 supplies the shared baseline/report schemas, Go types, `CheckConformance`, and canonical fixtures (see the [runtime contract](../../contracts/runtime/README.md#design-baseline-and-conformance-report)) before wrapper/gate consumers implement them. The baseline digest is SHA-256 over the canonical JSON baseline references in immutable run context, using the same canonicalization rules as [gate subjects](../specs/delivery-and-quality-gates.md#small-model). It excludes the later candidate/report; the report binds that baseline to the candidate explicitly. A proposed report payload is:

```json
{
  "baselineDigest": "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
  "candidateCommit": "1111111111111111111111111111111111111111",
  "requirements": ["SD-01", "SD-04"],
  "checks": [{"id": "module-boundaries", "outcome": "passed", "evidenceArtifactIds": ["art_review"]}],
  "unresolvedDeviations": [],
  "disposition": "conforms"
}
```

The digest and IDs above are placeholders. Check outcomes are `passed`, `failed`, or `not_run`; unresolved deviations identify the affected requirement and reason. Disposition is `conforms` only with no unresolved deviations and all required checks passed; otherwise it is `blocked`. Report artifacts and referenced evidence must share verified ownership/provenance. T-034 checks schema, candidate, expected baseline digest, required-check coverage, and disposition before accepting approval/progression. Required checks come from the pinned project specification/profile, not an agent-selected subset. Reviewers assess the report's claims and evidence; a syntactically valid self-report does not prove architectural correctness.

## Change and review loop

1. Identify the owner and baseline. A material conflict or unresolved dependency is a blocker, not permission for an agent to select a new architecture.
2. Implement the smallest coherent change within assigned paths. For a deliberate contract change, update its owning spec, wire fixtures, tests, and affected task dependencies/examples together. Record consequential rationale in an ADR and the register.
3. Review the diff for dependency direction, tenant/authority ownership, explicit transitions, transaction/outbox boundaries, and retry/unknown-outcome semantics. Tests prove relevant invariants through ports and real adapters, rather than mirroring directory layout.
4. Record conformance evidence for the identified candidate: baseline references, affected requirements, checks/results, and any proposed deviation. A passing functional test is not evidence that the boundary review happened.
5. An unresolved material deviation blocks review/progression. If the owner approves a design change, update the authoritative contract before dependent work proceeds and regenerate evidence for the current subject. Stale evidence cannot be relabeled as current approval.

For this repository, use this loop on every implementation task and composition checkpoint. For software produced by Kodama, the project's pinned blueprint/specification supplies its design rules; do not impose Kodama's internal Go layout on every tenant application. Human gate authority remains unchanged.

## Drift triggers and verification

| Trigger | Required response | Implementation checkpoint |
| --- | --- | --- |
| Changed spec, ADR, blueprint, profile, or upstream contract | Identify affected tasks/ports; review compatibility; refresh the baseline and candidate evidence | Owning task; T-031 context/evidence; T-061 platform compatibility |
| Changed route, enum, payload, schema, settings registration | Change the executable owner and fixtures/examples together; test compatible clients and upgrade behavior | Owning module and T-027/T-039/T-046/T-060 composition |
| Added core-like membership, secret, job, channel, or provider behavior | Apply the platform ownership table before adding another authority or retry loop | T-010/T-012/T-033; T-061 |
| New abstraction, dependency, or entity transition | Check consumer ownership and inward imports; test invariants and adapter failure semantics | Module design review, SD-A01–SD-A04 |
| Completion/support claim | Link actual merged implementation and real acceptance evidence for the declared profile | Milestone acceptance; T-056 |

Documentation checks cover local links/anchors, fenced examples, requirement-to-acceptance mappings, decision/task references, task index agreement, graph cycles, and ownership overlap. Runtime/schema fixtures and dependency-boundary checks enter CI through the scaffold/composition owners as their artifacts exist. Checks supplement architectural review; they cannot infer semantic conformance from a green build.
