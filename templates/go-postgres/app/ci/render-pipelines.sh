#!/bin/sh
# Render the installation-specific values into pipelines/*.yaml before the
# definitions are registered. Usage: sh ci/render-pipelines.sh VALUES.env OUTDIR
#
# VALUES.env holds KEY=VALUE lines for every @@KEY@@ placeholder:
#   TOOLS_IMAGE            digest-pinned tools image (repository@sha256:...)
#   CODING_PROFILE         immutable agent profile reference, name@version
#   QA_PROFILE             immutable agent profile reference, name@version
#   PREVIEW_ENVIRONMENT_ID, STAGE_ENVIRONMENT_ID   factory environment identifiers
#   RUNTIME_URL            origin of the factory runtime API reachable from step Pods
#   REGISTRY_INSECURE      "true" only for a loopback/plain-http test registry
#   RESOURCE_PREFIX        prefix for disposable test databases created by quality checks
# Values are restricted to a safe character set; an unresolved placeholder fails.
set -eu
[ $# -eq 2 ] || { echo "usage: $0 VALUES.env OUTDIR" >&2; exit 2; }
values=$1
out=$2
here=$(cd "$(dirname "$0")/.." && pwd)
mkdir -p "$out"
for f in "$here"/pipelines/*.yaml; do cp "$f" "$out/$(basename "$f")"; done
while IFS='=' read -r key value; do
	case "$key" in '' | '#'*) continue ;; esac
	printf '%s' "$key" | grep -Eq '^[A-Z_]+$' || { echo "invalid key: $key" >&2; exit 1; }
	printf '%s' "$value" | grep -Eq '^[A-Za-z0-9@:/._,=-]+$' || { echo "invalid value for $key" >&2; exit 1; }
	for f in "$out"/*.yaml; do
		sed "s|@@$key@@|$value|g" "$f" >"$f.tmp" && mv "$f.tmp" "$f"
	done
done <"$values"
if grep -l '@@[A-Z_]*@@' "$out"/*.yaml >&2; then
	echo "unresolved placeholders remain" >&2
	exit 1
fi
