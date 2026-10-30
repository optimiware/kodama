#!/bin/sh
set -eu
out="${1:-/kodama/outputs/worker.tar.gz}"
root="$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)"
stage="$(mktemp -d)"
trap 'rm -rf "$stage"' EXIT
cp "$root/wrangler.jsonc" "$stage/wrangler.jsonc"
cp "$root/app/index.js" "$stage/index.js"
mkdir -p "$(dirname "$out")"
tar -czf "$out" -C "$stage" wrangler.jsonc index.js
printf 'bundle: %s\n' "$out"
