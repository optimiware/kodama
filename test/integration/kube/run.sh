#!/usr/bin/env bash
# Task-local runner for the T-021 real-cluster acceptance tests.
#
# It creates (or reuses) a kind cluster named after the task resource prefix,
# exports its kubeconfig to dev/.state/<prefix>/kubeconfig (where the tests look
# for it), and runs the tests with KODAMA_REQUIRE_KUBERNETES=1 so that a missing
# cluster or PostgreSQL fails instead of skipping (KODAMA_REQUIRE_KUBERNETES=1 and
# KODAMA_REQUIRE_POSTGRES=1). Usage:
#
#   test/integration/kube/run.sh [go test arguments...]   # default: -race ./test/integration/kube/...
#   KUBE_TEARDOWN=1 test/integration/kube/run.sh          # also deletes the cluster
#
# The tool pins (kind, kubectl, node image) come from dev/versions.lock.json; run
# `make tools` first. The container provider is DEV_PROVIDER (podman on Darwin,
# docker elsewhere), as in the Makefile, for both kind and the fixture images.
# PostgreSQL comes from KODAMA_TEST_POSTGRES_ADMIN_URL (see test/support/postgres).
# The fixture images' pins are in fixtures/pins.json. The
# default kind CNI is only a fixture: Test_restricted_job probes that it really
# enforces NetworkPolicy and fails when it does not.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
prefix="${KODAMA_RESOURCE_PREFIX:-t021}"
state="$root/dev/.state/$prefix"
kind="$root/dev/.tools/bin/kind"
kubectl="$root/dev/.tools/bin/kubectl"
image="$(cd "$root" && go run ./dev/cmd/devctl tools image node)"
[ -x "$kind" ] && [ -x "$kubectl" ] || { echo "kind/kubectl not installed; run: make tools" >&2; exit 2; }

provider="${DEV_PROVIDER:-}"
if [ -z "$provider" ]; then
  if [ "$(uname -s)" = Darwin ]; then provider=podman; else provider=docker; fi
fi
case "$provider" in
  podman|docker) ;;
  *) echo "DEV_PROVIDER must be podman or docker (got: $provider)" >&2; exit 2 ;;
esac
export DEV_PROVIDER="$provider" KIND_EXPERIMENTAL_PROVIDER="$provider"
command -v "$provider" >/dev/null 2>&1 || { echo "container provider $provider is not installed" >&2; exit 2; }
[ -n "${KODAMA_TEST_POSTGRES_ADMIN_URL:-}" ] || { echo "KODAMA_TEST_POSTGRES_ADMIN_URL is required: these tests use real PostgreSQL (see dev/README.md)" >&2; exit 2; }

mkdir -p "$state"
cat >"$state/kind.yaml" <<YAML
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
- role: control-plane
YAML

if ! "$kind" get clusters 2>/dev/null | grep -qx "$prefix"; then
  "$kind" create cluster --name "$prefix" --config "$state/kind.yaml" --image "$image" \
    --kubeconfig "$state/kubeconfig" --wait 180s >"$state/create.log" 2>&1 || { cat "$state/create.log" >&2; exit 1; }
fi
"$kind" export kubeconfig --name "$prefix" --kubeconfig "$state/kubeconfig" >/dev/null
chmod 600 "$state/kubeconfig"
export PATH="$(dirname "$kubectl"):$PATH"

cleanup() {
  if [ "${KUBE_TEARDOWN:-0}" = 1 ]; then
    "$kind" delete cluster --name "$prefix" --kubeconfig "$state/kubeconfig" || true
  fi
}
trap cleanup EXIT

args=("$@")
[ "${#args[@]}" -gt 0 ] || args=(-race ./test/integration/kube/...)
cd "$root"
KODAMA_TASK_ID=T-021 KODAMA_RESOURCE_PREFIX="$prefix" KODAMA_REQUIRE_KUBERNETES=1 KODAMA_REQUIRE_POSTGRES=1 go test -count=1 -timeout 30m "${args[@]}"
