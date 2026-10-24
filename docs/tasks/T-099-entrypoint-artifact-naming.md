---
id: T-099
milestone: M2
status: done
depends_on: [T-016, T-023, T-078]
decision_blockers: []
owns: [internal/entrypoint/]
check: make check-go PKGS="./internal/entrypoint/..."
resources: [go]
acceptance: [artifact_output_name_is_declared_name, artifact_output_path_basename_free]
---
# T-099 — Artifact output naming

## Goal
Let a declared artifact output name its file anywhere under the outputs directory without the file's base name having to equal the output name.

## Contract
T-026 and T-038 both found that the entrypoint reserves an artifact under the declared file's base name (`artifactName` in `internal/entrypoint/outputs.go`), while the artifact service accepts only the declared output's name (`results/adapters` `artifacts.OutputDecl`). A step declaring `report: {type: artifact, path: /kodama/outputs/quality.json}`, as the ERP example does, therefore fails with `422 output_invalid`; only `path: /kodama/outputs/report` succeeds. Make the entrypoint reserve the artifact under the declared output name, keeping the file path as the source. Other outputs, size and count bounds, the declared-name collision rules and the retained diagnostic names must not change, and two outputs can never reserve the same name.

## Deliverables
The change in `artifactName` and its callers, tests for the examples above and for collisions, and the entrypoint README note. Change nothing outside `internal/entrypoint/`; if the artifacts or results module also needs a change, stop and report it as a blocker.

## Acceptance
`artifact_output_name_is_declared_name` proves an artifact output with a differing file name (`report` at `/kodama/outputs/quality.json`) is reserved and finalized under `report`. `artifact_output_path_basename_free` proves two outputs whose files share a base name in different directories are both accepted under their own declared names, and that a collision of declared names is still refused. Keep the T-016 and T-078 acceptance passing. Covers PX-A07 and AR-06.

## Out of scope
The artifact service and results classification, and any change to the output declaration syntax.
