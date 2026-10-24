#!/usr/bin/env bash
# Builds the T-065 deployment tools image with podman, optionally pushes it to a
# registry and records what was built.
#
#   images/deployment-tools/build.sh [--tag TAG] [--push REPOSITORY] [--out DIR]
#                                    [--verify-reproducible]
#
# Inputs (environment):
#   SNAPSHOT   snapshot.debian.org timestamp for apt (default in the Containerfile)
#
# Everything the image contains is pinned in the Containerfile (base digest, apt
# snapshot, kubectl version and sha256). The build sets every timestamp to the
# epoch (`--timestamp 0`) so two builds of the same tree and snapshot should
# produce the same layers; whether they did is recorded, not assumed:
# --verify-reproducible rebuilds without cache and compares the image IDs.
#
# Output: prints `image: <repository>@sha256:<digest>` (pushed) or the local image
# ID, and writes <out>/build.json (default: evidence/build.json beside this file).
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
tag="localhost/kodama-deployment-tools:build"
push=""
out="$here/evidence"
verify=0
while [ $# -gt 0 ]; do
  case "$1" in
    --tag) tag="$2"; shift 2 ;;
    --push) push="$2"; shift 2 ;;
    --out) out="$2"; shift 2 ;;
    --verify-reproducible) verify=1; shift ;;
    *) echo "unknown argument $1" >&2; exit 2 ;;
  esac
done

args=(build -q --timestamp 0 -f "$here/Containerfile" -t "$tag")
[ -z "${SNAPSHOT:-}" ] || args+=(--build-arg "SNAPSHOT=$SNAPSHOT")
[ "$verify" = 0 ] || args+=(--no-cache)
iid="$(podman "${args[@]}" "$here")"
iid="sha256:${iid#sha256:}"

reproducible="not checked"
if [ "$verify" = 1 ]; then
  second="$(podman "${args[@]}" "$here")"
  second="sha256:${second#sha256:}"
  if [ "$second" = "$iid" ]; then reproducible="identical image id ($iid)"; else reproducible="DIFFERENT image ids: $iid and $second"; fi
fi

pkgs="$(podman run --rm --entrypoint cat "$tag" /opt/kodama/deploy/packages.txt | tr '\n' ' ')"
kubectl_version="$(podman run --rm --entrypoint cat "$tag" /opt/kodama/deploy/kubectl.version | tr -d '\n')"
scripts_sha="$(cd "$here/scripts" && sha256sum apply.sh lookup.sh delete.sh lib.sh | tr '\n' ' ')"
manifest_digest="$(podman image inspect --format '{{.Digest}}' "$tag")"

digest=""
ref=""
if [ -n "$push" ]; then
  df="$(mktemp)"
  podman push --tls-verify=false --digestfile "$df" "$tag" "docker://$push:t065-build" >/dev/null
  digest="$(cat "$df")"; rm -f "$df"
  ref="$push@$digest"
fi

mkdir -p "$out"
cat > "$out/build.json" <<JSON
{
  "task": "T-065",
  "baseImage": "docker.io/library/debian@sha256:5ae3c39ebd15e229dcedd5cee596b2497182493d41ff162e824ba13fc1b2b867",
  "kubectlVersion": "$kubectl_version",
  "scripts": "$scripts_sha",
  "localImageId": "$iid",
  "localManifestDigest": "$manifest_digest",
  "registryRef": "$ref",
  "pushedDigest": "$digest",
  "reproducibility": "$reproducible",
  "packages": "$pkgs"
}
JSON
echo "image: ${ref:-$iid}"
