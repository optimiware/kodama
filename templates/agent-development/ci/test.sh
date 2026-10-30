#!/bin/sh
set -eu
out="${1:-/kodama/outputs/tests.json}"
mkdir -p "$(dirname "$out")"
# Keep the example dependency-free; a real project replaces this with its test command.
printf '{"tests":[],"result":"passed"}\n' >"$out"
