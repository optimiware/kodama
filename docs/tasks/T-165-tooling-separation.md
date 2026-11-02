---
id: T-165
milestone: production
status: done
depends_on: [T-125]
decision_blockers: []
owns: [Makefile, AGENTS.md, CONTRIBUTING.md, openforge.json, .github/workflows/tooling.yml, dev/README.md, dev/cmd/repoctl/, dev/internal/repochk/, docs/implementation/harness.md, docs/implementation/development-and-testing.md, docs/implementation/run-workspace-services-builders.md, docs/tasks/README.md]
check: make check-devtools && make docs-check config-check validate-tasks
resources: [go]
acceptance: []
---
# T-165 — Repository tooling without OpenForge

## Goal
Make every build, check and task command work without an OpenForge checkout or Bun ([P-18](../decisions.md#implementation-baseline)). OpenForge stays an optional, separately installed integration.

## Contract
P-18. Do not reimplement OpenForge's scheduler, workers, review, publication or resource leases. Do not change `.opencode/agents/` or `.agents/skills/`.

## Deliverables
- Makefile without sibling-checkout or Bun assumptions; optional OpenForge launcher only when explicitly configured.
- `repoctl tasks` / `make validate-tasks`: frontmatter, dependency, cycle, ownership-overlap and index checks.
- `repoctl check-task` / `make check-task TASK=…`: runs a task's declared check and verifies its acceptance names from FORGE_RESULTS (aggregated or raw `go test -json`), preserving exit 75 for unavailable infrastructure.
- `openforge.json` without the install hook; CI runs `validate-tasks`; contributor instructions describe the coordinator/subagent/integration-branch workflow.
- Accurate descriptions of what `docs-check` and `validate-tasks` verify.

## Acceptance
Declared check passes; runner and validator unit tests cover passing, missing, failed and skipped acceptance, both result formats, stale results, exit 75, cycles, overlaps and index drift; the checks run with no `bun` on `PATH`.

## Out of scope
OpenForge's own code, product behavior, historical receipts.

## Completion evidence
Implemented by a subagent; independent review requested changes once (nested ownership without a trailing slash, raw results ignoring package failure, `infra_pause` ignored on a zero exit, index-row whitespace variants, a timeout classification claim) and approved `30c8bcba`. Coordinator checks on that candidate: `make check-devtools` PASS; `make docs-check config-check validate-tasks` PASS (143 files, 10 images, 46 tasks). Merged into `integration/native-git` at `277600bd`.
