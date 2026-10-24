PYTHON ?= python3
BUN ?= bun
GO ?= go
DEV_SCOPE ?= t-001-local
DEV_PROVIDER ?= podman
export DEV_PROVIDER
ENV = $(PYTHON) scripts/dev/environment.py
SCOPE_ARGS = --scope $(DEV_SCOPE) --provider $(DEV_PROVIDER)
OPENFORGE = $(BUN) --no-install scripts/openforge/src/cli.ts
CHECK_PACKAGES ?=
GO_CORE_PACKAGES = ./cmd/... ./internal/... ./api/... ./contracts/... ./pkg/... ./test/support/...
PKGS ?= $(GO_CORE_PACKAGES)

.PHONY: fmt fmt-check lint lint-openforge test test-go test-go-unit test-go-json test-python openforge-install test-openforge check-openforge check-go check-go-integration test-task check-task build tools dev-up dev-down test-integration test-assembly test-m1 test-m2 test-m3 test-chart api-generate api-check scaffold-smoke dev-teardown-check dev-diagnostics docs-check openforge-telegram

fmt:
	$(GO) fmt ./...

fmt-check:
	$(PYTHON) scripts/dev/check.py fmt $(CHECK_PACKAGES)

lint: lint-go

lint-go: fmt-check
	$(GO) vet -tags unit $(GO_CORE_PACKAGES)

config-check:
	$(GO) tool actionlint
	$(PYTHON) scripts/dev/check.py config

test: test-go-unit

test-go:
	$(GO) test -race -count=1 ./...

test-go-unit:
	$(GO) test -race -count=1 -tags unit $(GO_CORE_PACKAGES)

test-go-unit-json:
	$(GO) test -json -race -count=1 -tags unit $(GO_CORE_PACKAGES)

test-go-json:
	$(GO) test -json -race -count=1 ./...

test-python:
	$(PYTHON) scripts/dev/python_results.py

openforge-install:
	cd scripts/openforge && $(BUN) install --frozen-lockfile

openforge-telegram:
	$(OPENFORGE) telegram

test-openforge:
	cd scripts/openforge && $(BUN) --no-install test test

lint-openforge:
	cd scripts/openforge && $(BUN) --no-install run typecheck
	cd scripts/openforge && $(BUN) --no-install run lint

check-openforge: lint-openforge test-openforge

check-go:
	$(PYTHON) scripts/dev/go_results.py $(PKGS)

check-go-integration:
	$(PYTHON) scripts/dev/go_results.py --integration $(PKGS)

test-task:
	@test -n "$(TASK)" || { echo 'usage: make test-task TASK=T-080'; exit 2; }
	$(OPENFORGE) check-task $(TASK)

check-task:
	@test -n "$(TASK)" || { echo 'usage: make check-task TASK=T-080'; exit 2; }
	$(OPENFORGE) check-task $(TASK)

build:
	$(PYTHON) -c 'from pathlib import Path; Path("dev/.state/bin").mkdir(parents=True, exist_ok=True)'
	CGO_ENABLED=0 $(GO) build -mod=readonly -trimpath -o dev/.state/bin/kodama ./cmd/kodama

tools:
	$(PYTHON) scripts/dev/tools.py

dev-up:
	$(ENV) up $(SCOPE_ARGS)

dev-down:
	$(ENV) down $(SCOPE_ARGS)

dev-preflight:
	$(ENV) preflight $(SCOPE_ARGS)

test-integration:
	$(ENV) integration $(SCOPE_ARGS)

# The core assembly (T-070): unit checks plus the actual composition on a real
# listener over the services named in the environment (README, "Core assembly").
test-assembly:
	$(GO) test -race -count=1 ./internal/app/... ./api/client/... ./test/integration/assembly-core/...

# The M1 composition (T-027): the whole slice composed under KODAMA_ENV=development
# from the modules' memory adapters and the Process runtime, with no container and
# no service (README, "Development composition"): route, worker, event-consumer,
# migration and CLI registration, contract consistency, and the generated client.
test-m1:
	$(GO) test -race -count=1 -run 'TestM1' ./internal/app/...

# The M2 composition (T-039): the model gateway, the MCP proxy and the native Git push trigger
# joined to the manual agent/delivery slice under KODAMA_ENV=development, with deterministic
# provider and tool responses, no container and no service: route/capability registration,
# approvals that follow stored settings, the run budget, tool visibility, the exact-commit hook,
# the production construction guard, client compatibility with the pre-M2 baseline and
# generation drift. The real SQL, Git, deployment and Podman joins are T-073, T-067 and T-038.
test-m2:
	$(GO) test -race -count=1 -run 'TestM2' ./internal/app/...
	$(GO) test -count=1 ./internal/app/apispec/...

# The M3 composition (T-046): the business-loop slice of T-075 (the dependency graph, the SQL
# flow summaries, the QA evidence-to-defect mapping and ChatOps notifications) joined to the M1
# and M2 compositions under KODAMA_ENV=development with controlled adapters and no service.
# TestBusinessLoop* is business_loop_wiring (constructor and worker selection, the production
# construction guard, event routing, review links that grant nothing); TestM3CompositionSmoke is
# m3_composition_smoke (registration through the central owner, every new operation decided by
# the identity authority against the fixed role matrix, a forwarded gate link judged by current
# authority, the QA pipeline names, the documentation's targets) and TestM3ContractCompatibility
# is m3_contract_compatibility (the aggregate against the pre-M2, M2 and business-loop client
# baselines, reproducible generation, served and outbound payloads against their schemas).
# The real transactions, notification delivery, replay and gate join are T-044's
# (chatops_gate_integration included); browser-agent QA (T-069), the Supabase blueprint (T-047)
# and live wrapper qualification (T-085) are deferred.
test-m3:
	$(GO) test -race -count=1 -run 'TestBusinessLoop|TestM3' ./internal/app/...
	$(GO) test -count=1 ./internal/app/apispec/... ./api/client/...

# The chart join (T-027), with no cluster and no helm: the rendered chart is fed to the server's
# configuration loader and namespace preflight. The helm lint/template checks of the chart
# (deploy/helm/kodama) additionally run when the helm binary is installed.
test-chart:
	$(GO) test -race -count=1 -run 'TestChart' ./internal/app/...
	$(GO) test -count=1 ./deploy/helm/kodama/...

# api/openapi.yaml is assembled from the module fragments in api/ and the Go
# client in api/client is generated from it; both are committed artifacts.
api-generate:
	$(GO) generate ./api/client/...

api-check: api-generate
	@git diff --exit-code -- api/openapi.yaml api/client || { echo 'api/openapi.yaml or api/client is stale: run make api-generate and commit the result'; exit 1; }
	@test -z "$$(git ls-files --others --exclude-standard -- api/openapi.yaml api/client)" || { echo 'untracked generated API files: commit them'; exit 1; }

scaffold-smoke: build
	$(ENV) smoke $(SCOPE_ARGS)

dev-teardown-check:
	$(PYTHON) scripts/dev/teardown_check.py $(SCOPE_ARGS)

dev-diagnostics:
	$(ENV) diagnostics $(SCOPE_ARGS)

docs-check:
	$(PYTHON) scripts/dev/check.py docs
	$(OPENFORGE) validate

.PHONY: lint-go config-check dev-preflight test-go-unit-json
