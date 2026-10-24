---
id: T-025
milestone: M1
status: done
depends_on: [T-017, T-011, T-012, T-013, T-014, T-015, T-020, T-023]
decision_blockers: []
owns: [internal/cli/work/, internal/cli/workspace/, internal/cli/pipeline/, internal/cli/run/, internal/cli/configuration/]
check: make check-go
resources: [go]
---
# T-025 — Core user CLI

## Goal
Complete the M1 workflow through public interfaces.

## Contract
Use owner-provided API fragments and [CLI examples](../examples/api/cli-and-http.md). Export command registration contributions; the integration task owns root composition.

## Deliverables
Work/history/transitions, workspace Git info/archive/restore, local pipeline validation/registration, run start/watch/log/cancel, variable/secret commands, output/exit contracts.

## Acceptance
`cli_core_journey`, `stale_edit_exit`, `watch_terminal_exit`, and `secret_stdin_no_echo` cover AC-01–AC-06, WM-02/WM-07, and VS-04.

## Out of scope
Duplicating business rules in CLI or launching remote editor sessions.
