#!/bin/sh
# Build and push the application image without a Docker daemon (ko), verify the
# pushed digest in the registry, and record source, tool, platform and digest
# evidence. Promotion consumes this digest; nothing rebuilds the image later.
#
# Required environment:
#   KO_DOCKER_REPO   registry repository to push to, for example registry.example/team/inventory
#   TOOLS_IMAGE      the digest-pinned tools image executing this build (repository@sha256:...),
#                    recorded as evidence. When the runtime context is available it must equal
#                    the step's pinned command image; without a digest-pinned identity the
#                    build fails.
# Optional:
#   BUILD_PLATFORMS  comma-separated platform set, default linux/amd64,linux/arm64. The pushed
#                    digest is an index over exactly this set and is recorded in the evidence.
#   REGISTRY_INSECURE=true   plain-http registry (loopback test registries only)
#   KODAMA_RUNTIME_URL / KODAMA_RUNTIME_TOKEN_FILE   when set, the built commit and tools
#                    image must be the pinned ones
#   $KODAMA_SECRETS_DIR/config.json   registry credentials in Docker config format
. "$(dirname "$0")/common.sh"

: "${KO_DOCKER_REPO:?KO_DOCKER_REPO is required}"
platforms="${BUILD_PLATFORMS:-linux/amd64,linux/arm64}"
export BUILD_PLATFORMS="$platforms"
if [ -z "${TOOLS_IMAGE:-}" ] && [ -z "${KODAMA_RUNTIME_URL:-}" ]; then
	fail build_tool_identity "TOOLS_IMAGE (a digest-pinned tools image) is required"
fi
case "${TOOLS_IMAGE:-@sha256:}" in
*@sha256:*) ;;
*) fail build_tool_identity "TOOLS_IMAGE must be pinned by digest" ;;
esac

[ -z "$(git status --porcelain)" ] || fail build_tree_dirty "the build requires a clean checkout of the pinned commit"
commit=$(git rev-parse HEAD)
epoch=$(git show -s --format=%ct HEAD)
export SOURCE_COMMIT="$commit" SOURCE_DATE_EPOCH="$epoch" KO_DATA_DATE_EPOCH="$epoch"
[ -z "${KODAMA_SECRETS_DIR:-}" ] || [ ! -f "$KODAMA_SECRETS_DIR/config.json" ] || export DOCKER_CONFIG="$KODAMA_SECRETS_DIR"
insecure=""
[ "${REGISTRY_INSECURE:-false}" != true ] || insecure="--insecure-registry"

# The builder's own exit status decides: never filter it through a pipeline.
ko_out="$SCRATCH/ko.out"
# shellcheck disable=SC2086
if ! ko build --bare --platform="$platforms" --sbom=none --tags="$(printf '%s' "$commit" | cut -c1-12)" $insecure ./cmd/inventory >"$ko_out"; then
	fail build_failed "ko build failed"
fi
ref=$(tail -n 1 "$ko_out")
case "$ref" in *@sha256:*) ;; *) fail build_failed "ko printed no digest reference" ;; esac

build_helper
GO_VERSION=$(go env GOVERSION | sed 's/^go//') SOURCE_COMMIT="$commit" \
	"$SCRATCH/delivery" build-verify --ref "$ref"
