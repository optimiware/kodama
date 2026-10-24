---
id: T-105
milestone: production
status: done
depends_on: [T-028, T-044, T-103]
decision_blockers: []
owns: [internal/app/, internal/secrets/kms/production/config.go, internal/secrets/kms/production/README.md, deploy/helm/kodama/, go.mod, go.sum, test/e2e/m1/kit/stack.go, test/e2e/m2/kit/base_stack.go, test/integration/assembly-core/, test/integration/kms-production/README.md, README.md, dev/configuration.md, docs/operations/installation-and-configuration.md, docs/specs/variables-and-secrets.md]
check: make check-go PKGS="./internal/app/... ./internal/secrets/kms/production/... ./pkg/secretkeys/..."
resources: [go]
acceptance: [unit_secrets_config, unit_secrets_startup, unit_scw_credentials]
---
# T-105 — Secrets startup wiring

## Goal
Complete configuration-to-service startup wiring for keyfile and cloud KMS. O-26 extracts this work from T-029; live Kubernetes qualification belongs to T-104.

## Contract
Use configuration document version 2 with `secrets.adapter: keyfile | kms`. Cloud selection is `secrets.kms.provider: aws | gcp | scw`; `scw` maps to the existing persisted Scaleway identity. Reuse T-028's constructor and validation; select one client, bound discovery/probe, fail startup on error and close resources on failure or drained shutdown. Production refuses keyfile. Configuration is rollout-scoped; retained labels and installation identity remain stable.

## Baseline and conformance
Baseline: `5531f93` and its `docs/specs/variables-and-secrets.md` (VS-06–VS-10), `docs/specs/software-design.md` (SD-06–SD-12), C-18 and ADR 0006/0009. Owner approved the configuration rename, startup-wiring extraction and deployment-time validation on 2026-10-23 (O-26). Application composition owns SDK authentication and cleanup; the existing secrets service owns scope keys and encrypted values. No persistence format changes.

## Deliverables and acceptance
Typed configuration, AWS/GCP/scw clients, bounded startup probe, lifecycle cleanup, Helm projection/credentials, migrated fixtures and operator examples. `unit_secrets_config`, `unit_secrets_startup`, `unit_scw_credentials` cover selection, strict configuration, credential files, deadlines, probe refusal and cleanup. Also require `make test-chart` with Helm, `go test -race -count=1 ./test/integration/kms-production/...`, `make build`, compilation of affected integration fixtures and `make docs-check`, with independent review before merge.

## Qualification handoff
T-104 validates a ready Kubernetes deployment with actual workload credentials, secret use/restart, rotation retaining old keys, denial/outage and restoration of access. It records the installation cloud actually tested. T-029 retains all four full-recovery scenarios and measured recovery objectives.

## Completion evidence — 2026-10-23
Implementation `4771229` and reviewed fix `ac9238e` merged into `main`. Independent reviewer session `ses_e94f78c1dffeZO9qvNx52fEi4X` approved `ac9238e` against `5531f93` with no remaining blockers. The review-required fix adds a three-minute Kubernetes startup probe, with a regression asserting the maximum KMS initialization budget fits before liveness begins.

Passed commands:

- `make check-go PKGS="./internal/app/... ./internal/secrets/kms/production/... ./pkg/secretkeys/..."` — scoped formatting, vet and race/unit tests.
- `make test-chart` — race-tested configuration join plus actual Helm lint/render/schema checks; rerun after the startup-probe fix.
- `go test -race -count=1 ./test/integration/kms-production/...` — offline provider protocol/rewrap/restore fixtures.
- `make build` — static server binary.
- `go test -exec /usr/bin/true ./test/e2e/m1/... ./test/e2e/m2/... ./test/integration/assembly-core/... ./test/integration/assembly-execution/...` — compile-only verification. An initial `-run '^$'` invocation reached TestMain and reported missing scope endpoints; no live-service acceptance is claimed.
- `make docs-check` and `git diff --check` — documentation, task graph/ownership/index and patch formatting.

Live Kubernetes/cloud qualification remains pending T-104. Full recovery remains pending T-029.
