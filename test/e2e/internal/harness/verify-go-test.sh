#!/usr/bin/env bash
# verify-go-test.sh: run one go test selection and fail unless every expected
# test really ran.
#
#   test/e2e/internal/harness/verify-go-test.sh \
#     --expect TestE2E_AC01InstalledPipeline --expect TestE2E_AC02ImmutableAdmission \
#     -- go test -json -tags=e2e -run '^TestE2E_AC(01|02)' ./test/e2e/
#
# The command must emit `go test -json`. The script fails on zero executed
# tests, on a missing expected test name (top-level or subtest), on any skipped
# expected test and on any failed expected test. A skipped or failed subtest
# also fails an expectation of its parent, so a scenario that never ran cannot
# hide behind a passing parent. A bare `go test` PASS with no selected tests is
# never accepted as release proof, and neither is a selected TestReleaseSmoke
# subtest that was silently removed or skipped.
#
# The wrapped command runs in its own process group. INT/TERM/HUP are forwarded
# to that group, the child is joined, the private result file is removed, and
# the script exits 130/143/129. A signal never turns into a verification pass.
set -euo pipefail

here="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"

die() {
	printf 'verify-go-test: %s\n' "$*" >&2
	exit 2
}

expected=()
while [ "$#" -gt 0 ]; do
	case "$1" in
	--expect)
		[ "$#" -ge 2 ] || die "--expect needs a test name"
		expected+=("$2")
		shift 2
		;;
	--)
		shift
		break
		;;
	*)
		die "usage: verify-go-test.sh --expect <test> [--expect <test>...] -- <command>"
		;;
	esac
done
[ "${#expected[@]}" -gt 0 ] || die "at least one --expect test name is required"
[ "$#" -gt 0 ] || die "no command to run"

command -v python3 >/dev/null 2>&1 || die "python3 is required"

result="$(mktemp "${TMPDIR:-/tmp}/verify-go-test.XXXXXX")"
status_file="$(mktemp "${TMPDIR:-/tmp}/verify-go-test-status.XXXXXX")"
child_pid=""

# group_alive reports whether any process remains in the background job's
# process group. The job-leader subshell may die on the first TERM while a
# TERM-ignoring command or grandchild survives, so leader liveness alone is not
# enough.
group_alive() {
	[ -n "$child_pid" ] && kill -0 -- "-$child_pid" 2>/dev/null
}

cleanup() {
	local status=$?
	trap - EXIT INT TERM HUP
	set +e
	if group_alive; then
		# With set -m the background job is a process-group leader whose pid is
		# the group id, so the command, tee and any children all receive the
		# signal. No ps lookup is needed (and none can race with the job start).
		kill -TERM -- "-$child_pid" 2>/dev/null || kill -TERM "$child_pid" 2>/dev/null
		local attempt
		for ((attempt = 0; attempt < 20; attempt++)); do
			group_alive || break
			sleep 0.1
		done
		if group_alive; then
			kill -KILL -- "-$child_pid" 2>/dev/null || kill -KILL "$child_pid" 2>/dev/null
		fi
		wait "$child_pid" 2>/dev/null
	fi
	rm -f -- "$result" "$status_file"
	exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
trap 'exit 129' HUP

set -m
{
	set +e
	"$@" 2>&1 | tee "$result"
	# PIPESTATUS[0] is the command; PIPESTATUS[1] is tee. A failed tee (for
	# example ENOSPC) leaves a truncated result and must never read as success.
	printf '%s\n%s\n' "${PIPESTATUS[0]}" "${PIPESTATUS[1]}" >"$status_file"
} &
child_pid=$!
set +m

# Waiting for the job group joins tee as well as the command. Verification only
# runs after tee has flushed every byte into $result.
group_status=0
wait "$child_pid" || group_status=$?
child_pid=""

command_status=1
tee_status=1
if [ -s "$status_file" ]; then
	{
		read -r command_status || true
		read -r tee_status || true
	} <"$status_file"
fi
case "$command_status" in
"" | *[!0-9]*) command_status=1 ;;
esac
case "$tee_status" in
"" | *[!0-9]*) tee_status=1 ;;
esac
if [ "$tee_status" != "0" ]; then
	printf 'verify-go-test: tee failed; the captured result is incomplete\n' >&2
	exit 1
fi
if [ "$group_status" -ne 0 ] && [ "$command_status" = "0" ]; then
	command_status="$group_status"
fi

verify=0
python3 "$here/verify-go-test.py" "$result" "${expected[@]}" || verify=$?
if [ "$command_status" -ne 0 ]; then
	exit "$command_status"
fi
exit "$verify"
