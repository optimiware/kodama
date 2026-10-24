---
id: T-017
milestone: M1
status: done
depends_on: [T-004]
decision_blockers: []
owns: [internal/cli/root/, internal/cli/auth/, internal/cli/client/, api/cli-conventions.md]
---
# T-017 — CLI transport, context, and PKCE

## Goal
Give subsequent commands consistent authenticated JSON/human behavior.

## Contract
Follow [interface rules](../specs/api-cli-and-integrations.md). O-14 is resolved: use `github.com/zalando/go-keyring`, with an explicit `0600` file opt-in and no silent fallback.

## Deliverables
Standard-library `flag` root/auth/context contributions, PKCE loopback login, keyring persistence, typed HTTP client, error/exit mapping, JSON output and test server. Do not add Cobra.

## Acceptance
`pkce_state`, `context_override`, `credential_storage`, and `exit_json_contract` verify IT-01 and AC-03/AC-05/AC-10.

## Out of scope
Product commands, device flow, and edits to the central command composition.
