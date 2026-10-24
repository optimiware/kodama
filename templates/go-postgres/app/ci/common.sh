# Shared helpers for the ordinary delivery scripts. Sourced, never executed.
# POSIX sh only: the tools image has BusyBox sh and no bash dependency.
#
# Step entrypoint contract used here:
#   KODAMA_OUTPUTS_DIR   declared file artifacts are written below it
#   KODAMA_RESULTS_FILE  scalar/OCI results manifest
#   KODAMA_SECRETS_DIR   delivered secret files
# The scripts never read a runtime token from the environment.

set -eu

: "${KODAMA_OUTPUTS_DIR:?KODAMA_OUTPUTS_DIR is required}"
: "${KODAMA_RESULTS_FILE:?KODAMA_RESULTS_FILE is required}"

SCRATCH="${TMPDIR:-/tmp}/inventory-ci.$$"
mkdir -p "$SCRATCH"
trap 'rm -rf "$SCRATCH"' EXIT INT TERM
export GOCACHE="$SCRATCH/gocache" GOFLAGS="-mod=readonly" CGO_ENABLED=0 GOTOOLCHAIN=local

fail() {
	# fail REASON MESSAGE: record a failed result and stop. REASON is a lowercase reason code.
	printf '{"outcome":"failed","reason":"%s"}\n' "$1" >"$KODAMA_RESULTS_FILE"
	printf 'ci: %s\n' "$2" >&2
	exit 1
}

build_helper() {
	go build -trimpath -o "$SCRATCH/delivery" ./cmd/delivery || fail helper_build_failed "could not build the delivery helper"
}
