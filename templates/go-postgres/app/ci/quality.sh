#!/bin/sh
# Quality gate: formatting, static checks, domain tests and the real-database
# tenancy/migration checks. Writes quality.json and quality.log under the output
# root and fails when any required check fails. A missing database is a failure,
# never a skip.
#
# Required environment: INVENTORY_TEST_ADMIN_URL (a disposable PostgreSQL
# superuser URI; the tests create and drop their own databases and roles).
. "$(dirname "$0")/common.sh"

report="$KODAMA_OUTPUTS_DIR/quality.json"
log="$KODAMA_OUTPUTS_DIR/quality.log"
: >"$log"
checks=""
failed=0

record() { # id status exitcode
	checks="${checks}${checks:+,}{\"id\":\"$1\",\"status\":\"$2\",\"exitCode\":$3}"
}

run() { # id command...
	id="$1"
	shift
	printf '== %s: %s\n' "$id" "$*" >>"$log"
	if "$@" >>"$log" 2>&1; then
		record "$id" passed 0
	else
		code=$?
		record "$id" failed "$code"
		failed=1
	fi
}

gofmt_check() {
	out=$(gofmt -l .)
	[ -z "$out" ] || {
		printf 'unformatted files:\n%s\n' "$out"
		return 1
	}
}

imports_check() {
	# The application must not import factory or Platform code.
	! go list -deps ./... 2>/dev/null | grep -E '(^|/)kodama(/|$)|optimi\.io/platform' >&2
}

scripts_check() {
	for f in ci/*.sh; do sh -n "$f" || return 1; done
}

if [ -z "${INVENTORY_TEST_ADMIN_URL:-}" ]; then
	printf 'INVENTORY_TEST_ADMIN_URL is required: database checks are mandatory\n' >>"$log"
	record database-configured failed 1
	failed=1
fi
export INVENTORY_REQUIRE_POSTGRES=1 INVENTORY_TEST_PREFIX="${INVENTORY_TEST_PREFIX:-inv}"

# The race detector needs cgo (the toolchain image carries a C compiler); the
# shipped binaries and the delivery helper are built with cgo disabled.
race_test() { CGO_ENABLED=1 go test -race -count=1 "$@"; }

run gofmt gofmt_check
run vet go vet ./...
run modules go mod verify
run scripts scripts_check
run imports imports_check
unit_pkgs=$(go list ./internal/... ./cmd/... | grep -v '/postgres$')
# shellcheck disable=SC2086
run unit race_test $unit_pkgs
if [ -n "${INVENTORY_TEST_ADMIN_URL:-}" ]; then
	run migrations race_test ./migrations/...
	run tenancy race_test ./internal/exceptions/postgres/...
	run acceptance race_test ./test/acceptance/...
fi

commit=$(git rev-parse HEAD 2>/dev/null || echo unknown)
passed=true
[ "$failed" -eq 0 ] || passed=false
printf '{"sourceCommit":"%s","goVersion":"%s","passed":%s,"checks":[%s]}\n' "$commit" "$(go env GOVERSION)" "$passed" "$checks" >"$report"

[ "$failed" -eq 0 ] || fail quality_checks_failed "quality checks failed; see quality.log"
printf '{"outcome":"completed"}\n' >"$KODAMA_RESULTS_FILE"
