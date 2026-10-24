#!/usr/bin/env bash
# Builds the T-031 OpenCode wrapper image with podman, optionally pushes it to
# a registry and records what was built.
#
#   images/harness-opencode/build.sh [--push REPOSITORY] [--out DIR]
#
# Inputs (environment):
#   OPENCODE_BIN     path of the OpenCode binary (default $HOME/.opencode/bin/opencode)
#   OPENCODE_SHA256  its expected checksum (default: the candidate measured for
#                    2.1.2); the build fails on a mismatch
#   SNAPSHOT         snapshot.debian.org timestamp for apt (default in the Containerfile)
#
# Output: prints `image: <repository>@sha256:<digest>` (pushed) or the local
# image ID, and writes <out>/build.json with the digests and checksums.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
root="$(cd "$here/../.." && pwd)"
opencode="${OPENCODE_BIN:-$HOME/.opencode/bin/opencode}"
want="${OPENCODE_SHA256:-22636c800ba6c8dc5353e264183bb3927725d824cba412f007dea9c694740f52}"
push=""
out="$here/evidence"
while [ $# -gt 0 ]; do
  case "$1" in
    --push) push="$2"; shift 2 ;;
    --out) out="$2"; shift 2 ;;
    *) echo "unknown argument $1" >&2; exit 2 ;;
  esac
done

got="$(sha256sum "$opencode" | cut -d' ' -f1)"
[ "$got" = "$want" ] || { echo "OPENCODE_BIN checksum $got does not match $want" >&2; exit 1; }
version="$("$opencode" --version | tr -d '\n')"

ctx="$(mktemp -d)"
trap 'rm -rf "$ctx"' EXIT
cp "$opencode" "$ctx/opencode"
cp "$here/Containerfile" "$ctx/Containerfile"
(cd "$root" && go run ./images/harness-opencode/wrapper/cmd/render-config) > "$ctx/opencode-system.json"
(cd "$root" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -buildvcs=false -ldflags '-s -w -buildid=' -o "$ctx/kodama-opencode-wrapper" ./images/harness-opencode/wrapper)
wrapper_sha="$(sha256sum "$ctx/kodama-opencode-wrapper" | cut -d' ' -f1)"

tag="localhost/kodama-harness-opencode:t031-build"
args=(build -q -f "$ctx/Containerfile" -t "$tag" --build-arg "OPENCODE_SHA256=$want")
[ -z "${SNAPSHOT:-}" ] || args+=(--build-arg "SNAPSHOT=$SNAPSHOT")
iid="$(podman "${args[@]}" "$ctx")"
packages="$(podman run --rm --entrypoint cat "$tag" /opt/kodama/packages.txt | tr '\n' ' ')"

digest=""
ref=""
if [ -n "$push" ]; then
  df="$(mktemp)"
  podman push --tls-verify=false --digestfile "$df" "$tag" "docker://$push:t031-build" >/dev/null
  digest="$(cat "$df")"; rm -f "$df"
  ref="$push@$digest"
fi

mkdir -p "$out"
cat > "$out/build.json" <<JSON
{
  "task": "T-031",
  "opencodeVersion": "$version",
  "opencodeSha256": "$got",
  "wrapperSha256": "$wrapper_sha",
  "localImageId": "$iid",
  "registryRef": "$ref",
  "pushedDigest": "$digest",
  "packages": "$packages"
}
JSON
echo "image: ${ref:-$iid}"
