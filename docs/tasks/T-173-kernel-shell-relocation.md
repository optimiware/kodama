---
id: T-173
milestone: kernel
status: done
depends_on: []
decision_blockers: []
owns: [internal/kernel/api/, internal/kernel/bus/, internal/kernel/dispatch/, internal/kernel/store/, internal/kernel/secret/, internal/kernel/serve/, internal/kernel/runner/, internal/shell/, internal/app/kernel.go, internal/app/git_commands.go, internal/app/git_commands_test.go, internal/app/apispec/kernel_git_test.go, dev/internal/devenv/gitworker.go, dev/internal/devenv/gitnative_test.go]
check: go run ./dev/cmd/repoctl fmt ./internal/kernel/... ./internal/shell/... ./internal/app && go vet -tags unit ./internal/kernel/... ./internal/shell/... ./internal/app && go build ./cmd/... ./internal/... ./dev/... && { go test -json -race -count=1 -tags unit ./internal/kernel/... ./internal/shell/... > "$FORGE_RESULTS"; a=$?; go test -json -race -count=1 -tags unit -run 'Test_unit_kernel_git_serve_command' ./internal/app >> "$FORGE_RESULTS"; b=$?; [ $a -eq 0 ] && [ $b -eq 0 ]; }
resources: [go]
acceptance: [unit_kernel_git_serve_command]
---
# T-173 — Relocate shell responsibilities out of the kernel

## Goal
Give the kernel and shell stable, disjoint package paths before parallel replacement work starts. This is a pure relocation: no behaviour, signature or SQL change.

## Contract
P1.0 of the [simplification plan](../implementation/pipeline-runtime-simplification.md#p10-boundary-relocation) and the layer rules in [repository layout](../implementation/repository-layout.md#layer-dependency-rules).

## Deliverables
- Move, with `git mv` so history follows:
  - `internal/kernel/api` → `internal/shell/api`
  - `internal/kernel/bus` → `internal/shell/bus`
  - `internal/kernel/dispatch` → `internal/shell/dispatch`
  - `internal/kernel/store` → `internal/shell/store`
  - `internal/kernel/secret` → `internal/shell/secrets`
  - `internal/kernel/serve` → `internal/shell/serve`
- Keep `internal/kernel/pipeline` and `internal/kernel/runner` where they are; update only their import paths. The blocking runner stays the transitional executor until the replacement engine lands.
- Update every affected consumer: `internal/app/kernel.go`, `internal/app/git_commands.go` and its test, `internal/app/apispec/kernel_git_test.go`, `dev/internal/devenv/gitworker.go` and its test, and intra-package imports.
- Record the frozen destination manifest in `internal/shell/README.md`: which package owns what today and which later task splits it.
- Package doc comments keep their current wording except for the package name and path.

## Acceptance
The declared check passes: formatting, vet, `go build` of commands and internal packages, the unit tests of the moved packages, and `Test_unit_kernel_git_serve_command` in `internal/app` (that package has unrelated pre-existing failures, so only the named test is run there).

Review confirms the diff is relocation only: no renamed exported symbol, no changed behaviour, no new dependency, and no SQL or contract edit.

## Out of scope
Splitting `shell/store` or `shell/serve`, introducing kernel ports, changing the runner's execution model, and moving `internal/identity`, `internal/gitservice`, `internal/tasks` or any other top-level package.
