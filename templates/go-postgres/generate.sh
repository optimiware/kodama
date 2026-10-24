#!/bin/sh
# Generate a self-contained inventory-exception application from this template.
#
#   generate.sh --module example.com/acme/inventory --dest ./inventory [--git]
#
# The generated directory is an independent Go module: it needs nothing from the
# Kodama checkout. The script copies app/, replaces the template module path,
# preserves executable scripts, and records what it was generated from in
# GENERATED_FROM.json (template version, pins, a digest of the generated tree).
# The result is reproducible: the same template and module give the same tree
# digest, and with --git the same commit id (fixed author and date).
set -eu

here=$(cd "$(dirname "$0")" && pwd)
template_module="example.invalid/inventory"
module=""
dest=""
git_init=false

usage() { echo "usage: $0 --module MODULE_PATH --dest DIR [--git]" >&2; exit 2; }

while [ $# -gt 0 ]; do
	case "$1" in
	--module) [ $# -ge 2 ] || usage; module=$2; shift 2 ;;
	--dest) [ $# -ge 2 ] || usage; dest=$2; shift 2 ;;
	--git) git_init=true; shift ;;
	*) usage ;;
	esac
done
[ -n "$module" ] && [ -n "$dest" ] || usage

# A Go module path: dotted host first element, then path elements; no characters
# that could escape the substitution below.
printf '%s' "$module" | grep -Eq '^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+(/[A-Za-z0-9._~-]+)+$' ||
	{ echo "invalid module path: $module" >&2; exit 2; }
case "$module" in *..*) echo "invalid module path: $module" >&2; exit 2 ;; esac

if [ -e "$dest" ]; then
	[ -d "$dest" ] && [ -z "$(ls -A "$dest")" ] || { echo "destination exists and is not empty: $dest" >&2; exit 1; }
else
	mkdir -p "$dest"
fi
dest=$(cd "$dest" && pwd)

# Copy without tooling caches or downloaded binaries; keep modes (executable scripts).
(cd "$here/app" && tar --exclude='./deploy/tools/bin' --exclude='.DS_Store' -cf - .) | (cd "$dest" && tar -xf -)

# Substitute the module path in go.mod and every Go file (module validated above).
if [ "$module" != "$template_module" ]; then
	find "$dest" -type f \( -name '*.go' -o -name go.mod \) | while IFS= read -r f; do
		sed "s|example\.invalid/inventory|$module|g" "$f" >"$f.tmp" && cat "$f.tmp" >"$f" && rm -f "$f.tmp"
	done
fi

sha256() { if command -v sha256sum >/dev/null 2>&1; then sha256sum; else shasum -a 256; fi; }
tree_digest=$(cd "$dest" && find . -type f ! -name GENERATED_FROM.json | LC_ALL=C sort | while IFS= read -r f; do printf '%s  %s\n' "$(sha256 <"$f" | cut -d' ' -f1)" "$f"; done | sha256 | cut -d' ' -f1)
lock_digest=$(sha256 <"$here/template.lock.json" | cut -d' ' -f1)
version=$(sed -n 's/.*"version": *"\([^"]*\)".*/\1/p' "$here/template.lock.json" | head -n 1)

cat >"$dest/GENERATED_FROM.json" <<JSON
{
  "template": "go-postgres",
  "templateVersion": "$version",
  "templateLockSha256": "$lock_digest",
  "module": "$module",
  "templateModule": "$template_module",
  "treeSha256": "$tree_digest"
}
JSON

if [ "$git_init" = true ]; then
	(
		cd "$dest"
		git init -q
		git add -A
		GIT_AUTHOR_NAME="Kodama Blueprint" GIT_AUTHOR_EMAIL="blueprint@kodama.invalid" GIT_AUTHOR_DATE="2026-01-01T00:00:00Z" \
		GIT_COMMITTER_NAME="Kodama Blueprint" GIT_COMMITTER_EMAIL="blueprint@kodama.invalid" GIT_COMMITTER_DATE="2026-01-01T00:00:00Z" \
			git commit -q -m "Generate from the Kodama go-postgres blueprint $version"
	)
fi
echo "generated $module in $dest (tree $tree_digest)"
