GO ?= go
CHECK := $(GO) run ./tools/check -root .
SLOT := scripts/infra-slot.sh

.DEFAULT_GOAL := check

.PHONY: check fmt fmt-check vet test race arch-check docs-check \
	build dist release-prepare \
	baseline test-e2e-hermetic suites-stack suites-kind \
	suites-parallel suite-podman suite-kind suite-stage

FMT_CHECK = files="$$(gofmt -l .)"; status=$$?; if [ $$status -ne 0 ]; then exit $$status; fi; if [ -n "$$files" ]; then echo "gofmt needed:"; echo "$$files"; exit 1; fi

check: fmt-check vet test race arch-check docs-check

fmt:
	$(GO) fmt ./...

fmt-check:
	@$(FMT_CHECK)

vet:
	$(GO) vet ./...

test:
	$(GO) test ./...

race:
	$(GO) test -race ./...

arch-check:
	$(CHECK) imports

docs-check:
	$(CHECK) docs

# ---------------------------------------------------------------------------
# Build and release artifacts (docs/runbooks/release.md).
#
#   make build   # bin/kodama and bin/kodactl for this host
#   make dist    # dist/: kodactl archives per platform, both Helm charts, SHA256SUMS
#
# Both stamp the binaries with the VERSION file and the git revision, which
# `kodama version` and `kodactl version` print. A plain `go build` reports
# version "dev".
VERSION := $(shell cat VERSION)
REVISION ?= $(shell git rev-parse HEAD 2>/dev/null)
BUILD = CGO_ENABLED=0 $(GO) build -trimpath -buildvcs=false \
	-ldflags '-s -w -X main.version=$(VERSION) -X main.revision=$(REVISION)'

build:
	$(BUILD) -o bin/kodama ./cmd/kodama
	$(BUILD) -o bin/kodactl ./cmd/kodactl

dist:
	rm -rf dist
	scripts/dist

# make release-prepare VERSION=0.2.0 sets the version of the checkout in every
# file that carries it (docs/runbooks/release.md). Nothing is pushed.
release-prepare:
	@[ "$(origin VERSION)" = "command line" ] || { echo "usage: make release-prepare VERSION=<version>"; exit 2; }
	scripts/release-prepare $(VERSION)
	$(GO) test -count=1 -run TestReleaseVersionIsConsistent ./internal/installation/

# ---------------------------------------------------------------------------
# Baseline checks.
#
# `make baseline` runs the baseline block in order and stops at the first
# failure. The race step covers CHANGED, the changed Go packages:
#
#   BASE     base ref for the change set (default: master).
#   CHANGED  default: every Go package (directory with non-test or test files
#            under the default build tags) containing a file that differs
#            between the working tree and merge-base(BASE, HEAD), committed or
#            not, plus untracked files that are not ignored. Files below a
#            testdata directory count for the package that owns it. Packages
#            whose files are all behind build tags are covered by the vet steps
#            and the real suites. An empty set skips the race step with a note.
#            Setting CHANGED explicitly (including CHANGED=) bypasses the base
#            selection entirely and prints the note when it is empty.
#
#   make baseline                          # changes since merge-base(master, HEAD)
#   make baseline BASE=<rev>               # changes since another base
#   make baseline CHANGED='./internal/x/ ./test/contracts/...'
#   make baseline CHANGED=                 # skip the race step explicitly
BASE ?=
CHANGED ?= $(shell tools/baseline/changed.sh --base '$(BASE)')

baseline:
	$(eval BASELINE_CHANGED := $(strip $(CHANGED)))
	$(if $(filter file,$(origin CHANGED)),$(if $(filter 0,$(.SHELLSTATUS)),,$(error baseline: cannot compute the changed packages; set BASE=<revision> or CHANGED=<packages> (CHANGED= skips the race step explicitly))),)
	@echo 'gofmt -l .'
	@$(FMT_CHECK)
	$(GO) vet ./...
	$(GO) vet -tags=integration ./test/...
	$(GO) vet -tags=component ./test/...
	$(GO) vet -tags=e2e ./test/e2e/...
	$(GO) vet -tags=qualification ./test/qualification/...
	$(GO) test -count=1 ./...
	$(if $(BASELINE_CHANGED),$(GO) test -race -count=1 $(BASELINE_CHANGED),@echo "baseline: no changed Go packages (BASE=$(BASE), CHANGED empty); skipping go test -race (set CHANGED= to skip explicitly, or BASE/CHANGED to select packages)")
	$(CHECK) imports docs

# `make test-e2e-hermetic` runs the e2e helper tests that need no installation,
# no Podman and no cluster.
test-e2e-hermetic:
	$(GO) test -tags=e2e -race -count=1 ./test/e2e/internal/...
	$(GO) test -tags=e2e -race -count=1 -run '^TestE2E_WorkloadScriptsKeepTokensOutOfArgv$$' ./test/e2e/

# `make suites-stack` and `make suites-kind` run the real-infrastructure suites
# of the stack and kind pools, sequentially, each through the slot launcher
# with the row's tags, packages and timeout. TMPDIR is never set here: the
# launcher gives each run a short one (Podman socket paths).
#
# A row failure stops the target unless KEEP_GOING=1, which runs the remaining
# rows and fails at the end. Each row prints one result line with its duration,
# and a summary repeats them. Under `make -n` the rows print as the bare
# commands; a real run executes the same commands, echoing each one first.
SUITE_TEST = env KODAMA_REQUIRE_INFRA=1 $(GO) test -race -count=1

SUITE_integration-factory = $(SLOT) stack -- $(SUITE_TEST) -timeout 60m -tags=integration ./test/integration/factory/
SUITE_integration-services = $(SLOT) stack -- $(SUITE_TEST) -timeout 40m -tags=integration ./test/integration/services/
SUITE_integration-composition-boundaries = $(SLOT) stack -- $(SUITE_TEST) -timeout 30m -tags=integration ./test/integration/composition/... ./test/integration/boundaries/...
SUITE_component-postgres = $(SLOT) stack -- $(SUITE_TEST) -timeout 30m -tags=component ./test/component/postgres/
SUITE_component-stack = $(SLOT) stack -- $(SUITE_TEST) -timeout 30m -tags=component ./test/component/messaging/ ./test/component/runtime/ ./test/component/recovery/ ./test/integration/composition/...
SUITE_integration-lifecycle = $(SLOT) kind -- $(SUITE_TEST) -timeout 60m -tags=integration ./test/integration/lifecycle/
SUITE_component-kubernetes-deploy = $(SLOT) kind -- $(SUITE_TEST) -timeout 40m -tags=component ./test/component/kubernetes/ ./test/component/deploy/

STACK_SUITES = integration-factory integration-services integration-composition-boundaries component-postgres component-stack
KIND_SUITES = integration-lifecycle component-kubernetes-deploy

SUITE_RUNNER = failed=0; summary=''; \
	row() { \
		name=$$1; shift; \
		echo "suites: $$name: $$*"; \
		start=$$(date +%s); "$$@"; status=$$?; elapsed=$$(( $$(date +%s) - start )); \
		if [ $$status -eq 0 ]; then result=PASS; else result="FAIL (exit $$status)"; failed=$$((failed + 1)); fi; \
		line=$$(printf 'suites: %-36s %s in %dm%02ds' "$$name" "$$result" $$((elapsed / 60)) $$((elapsed % 60))); \
		echo "$$line"; summary="$$summary$$line|"; \
		if [ $$status -ne 0 ] && [ "$(KEEP_GOING)" != 1 ]; then echo "suites: stopped at the first failure (KEEP_GOING=1 runs the remaining rows); summary:"; printf '%s' "$$summary" | tr '|' '\n'; exit $$status; fi; \
	}; \
	done_rows() { echo "suites: summary:"; printf '%s' "$$summary" | tr '|' '\n'; [ $$failed -eq 0 ] || { echo "suites: $$failed row(s) failed"; exit 1; }; }

define newline


endef

ifneq (,$(findstring n,$(firstword -$(MAKEFLAGS))))
run_suites = $(foreach s,$(1),$(SUITE_$(s))$(newline))
else
run_suites = @$(SUITE_RUNNER); $(foreach s,$(1),row $(s) $(SUITE_$(s));) done_rows
endif

suites-stack:
	$(call run_suites,$(STACK_SUITES))

suites-kind:
	$(call run_suites,$(KIND_SUITES))

# ---------------------------------------------------------------------------
# The unified suite runs the same real-infrastructure suites as concurrent
# lanes (docs/runbooks/unified-test-suite.md):
#
#   make suites-parallel   # every suites-stack and suites-kind row, and the local restore drills, at once
#   make suite-podman      # portable e2e proofs on a Podman installation, and the local restore drills
#   make suite-kind        # the kind suites, or the portable e2e proofs when INSTALLATION is set
#   make suite-stage       # the stage qualification: one concurrent wave, then the chaos probes
#
#   INSTALLATION  descriptor of an existing installation
#   KEEP_GOING=1  start the remaining waves after a failed one
#   SUITE_FLAGS   further scripts/test-suite flags, for example --dry-run
INSTALLATION ?=
SUITE_FLAGS ?=
SUITE = scripts/test-suite $(if $(strip $(INSTALLATION)),--installation=$(strip $(INSTALLATION))) $(if $(filter 1,$(KEEP_GOING)),--keep-going) $(SUITE_FLAGS)

suites-parallel:
	$(SUITE) --target=local-all

suite-podman:
	$(SUITE) --target=podman

suite-kind:
	$(SUITE) --target=kind

suite-stage:
	$(SUITE) --target=stage
