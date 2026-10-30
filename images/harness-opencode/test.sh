#!/usr/bin/env bash
# Runs the OpenCode wrapper's Go tests (task T-153).
#
#   images/harness-opencode/test.sh
#
# The wrapper is Linux-only (it is a child subreaper and contains its process
# tree with Linux system calls), so its package does not build on macOS. The
# tests run natively on Linux; on any other host they run in the Linux Go image
# pinned below by digest (the one the release Containerfile builds with), through
# Podman, or Docker when DEV_PROVIDER=docker. The container mounts the repository
# read-only and keeps its Go caches in the container.
#
# Exit status:
#   0   every test passed and the required tests ran and passed
#   75  neither a Linux Go toolchain nor a container provider is available, the
#       container engine itself failed (status 125-127: no pull, no run), or the
#       Go modules could not be downloaded in the container (offline): the
#       environment is unavailable; nothing was judged
#   1   a test failed, or a required test did not pass (missing or skipped)
#   2   bad usage or configuration
#
# A required test must report `--- PASS` at the top level: a rename, a skip or a
# build constraint that removes it would otherwise leave an empty success.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
root="$(cd "$here/../.." && pwd)"

# Keep aligned with the Containerfile's build stage and dev/versions.lock.json.
image="docker.io/library/golang:1.27.2-trixie@sha256:47667934874913a449b96faa7ef4777fa285d9ec29126ee80ed61c45b7cccf0e"
pkg="./images/harness-opencode/wrapper/..."
required=(
  Test_unit_wrapper_pushes_with_attempt_grant
  Test_unit_wrapper_old_context_unchanged
)

provider="${DEV_PROVIDER:-podman}"
case "$provider" in
  podman|docker) ;;
  *) echo "DEV_PROVIDER must be podman or docker (got: $provider)" >&2; exit 2 ;;
esac

out="$(mktemp)"
trap 'rm -f "$out"' EXIT

# The native branch needs a Go at least as new as go.mod asks (an older one would
# try to fetch a toolchain, which an offline host cannot).
native_go_ok() {
  [ "$(uname -s)" = Linux ] && command -v go >/dev/null 2>&1 && command -v git >/dev/null 2>&1 || return 1
  local want have
  want="$(sed -n 's/^go \([0-9][0-9.]*\)$/\1/p' "$root/go.mod" | head -n 1)"
  have="$(go env GOVERSION 2>/dev/null | sed 's/^go//')"
  [ -n "$want" ] && [ -n "$have" ] || return 1
  [ "$(printf '%s\n%s\n' "$want" "$have" | sort -V | head -n 1)" = "$want" ]
}

status=0
if native_go_ok; then
  echo "running the wrapper tests natively on Linux" >&2
  (cd "$root" && go test -count=1 -v "$pkg") >"$out" 2>&1 || status=$?
elif command -v "$provider" >/dev/null 2>&1 && "$provider" info >/dev/null 2>&1; then
  echo "running the wrapper tests in $image through $provider" >&2
  # The tests need only Go and Git (both in the image). Caches live under /tmp in
  # the container; the module cache is downloaded on first use.
  "$provider" run --rm --pull=missing \
    -v "$root:/src:ro,z" -w /src \
    -e GOCACHE=/tmp/gocache -e GOMODCACHE=/tmp/gomod -e GOFLAGS=-buildvcs=false -e CGO_ENABLED=0 \
    "$image" go test -count=1 -v "$pkg" >"$out" 2>&1 || status=$?
else
  echo "infrastructure unavailable: no Linux Go toolchain and no usable container provider ($provider)" >&2
  exit 75
fi

cat "$out"
case "$status" in
  125|126|127)
    # The engine could not start the container (no image, no daemon): not a verdict.
    echo "infrastructure unavailable: the container engine failed (status $status)" >&2
    exit 75 ;;
esac
if [ "$status" -ne 0 ] && ! grep -q '^=== RUN' "$out" && grep -Eq '^go: .*(dial tcp|no such host|i/o timeout|network is unreachable|connection refused)|proxy\.golang\.org.*(dial tcp|no such host|timeout|unreachable)' "$out"; then
  echo "infrastructure unavailable: the Go modules could not be downloaded (offline?)" >&2
  exit 75
fi
if [ "$status" -ne 0 ]; then
  echo "the wrapper tests failed (status $status)" >&2
  exit 1
fi
if grep -Eq '^(--- FAIL|FAIL)' "$out"; then
  echo "the wrapper tests reported a failure" >&2
  exit 1
fi
for name in "${required[@]}"; do
  if ! grep -Eq "^--- PASS: ${name} " "$out"; then
    echo "required test $name did not pass (missing, skipped or renamed)" >&2
    exit 1
  fi
done
echo "wrapper tests passed, including: ${required[*]}" >&2
