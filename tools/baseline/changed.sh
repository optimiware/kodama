#!/usr/bin/env bash
# changed.sh prints the Go packages changed between a base revision and HEAD,
# one package pattern per line, for the Makefile's `baseline` target.
#
#   tools/baseline/changed.sh                 # default base selection (master)
#   tools/baseline/changed.sh --base master   # explicit base
#
# Files below a testdata directory count for the package that owns it.
# Packages whose files are all behind build tags are excluded: `go list` under
# the default tags reports no Go files for them, and the tagged vet steps plus
# the real suites cover them.
set -euo pipefail

die() {
	printf 'baseline: %s\n' "$*" >&2
	exit 2
}

base=""
while [ "$#" -gt 0 ]; do
	case "$1" in
	--base)
		[ "$#" -ge 2 ] || die "--base needs a revision"
		base="$2"
		shift 2
		;;
	*)
		die "usage: changed.sh [--base <revision>]"
		;;
	esac
done

git rev-parse --show-toplevel >/dev/null 2>&1 || die "not inside a git repository"
root="$(git rev-parse --show-toplevel)"
cd "$root"

git rev-parse -q --verify 'HEAD^{commit}' >/dev/null || die "HEAD is not a commit"
head="$(git rev-parse 'HEAD^{commit}')"

go_bin="${GO:-go}"
command -v "$go_bin" >/dev/null 2>&1 || die "the Go toolchain ($go_bin) is not available"
module="$("$go_bin" list -m 2>/dev/null)" || die "cannot read the Go module path; run changed.sh from the repository root"

# changed_since prints the packages whose files differ between the given base
# revision and the working tree (committed, staged, unstaged and untracked).
changed_since() {
	local revision="$1"
	{
		git diff --name-only "$revision" --
		git ls-files --others --exclude-standard
	} |
		awk '{ p = $0; if (p ~ /(^|\/)testdata\//) sub(/\/?testdata\/.*$/, "", p); else if (p ~ /\//) sub(/\/[^\/]*$/, "", p); else p = ""; print (p == "" ? "." : "./" p) }' |
		sort -u |
		xargs -r "$go_bin" list -e -f '{{if or .GoFiles .CgoFiles .TestGoFiles .XTestGoFiles}}{{.ImportPath}}{{end}}' |
		sed -e "s|^${module}\$|.|" -e "s|^${module}/|./|" |
		sed -e '/^$/d'
}

resolves() {
	git rev-parse -q --verify "$1^{commit}" >/dev/null 2>&1
}

merge_base_with() {
	git merge-base "$1" "$head" 2>/dev/null
}

if [ -n "$base" ]; then
	resolves "$base" || die "BASE=$base is not a commit; set BASE to a revision or CHANGED (CHANGED= skips the race step explicitly)"
	merge_base="$(merge_base_with "$base")" || die "no merge base between BASE=$base and HEAD; set BASE to a revision with common history or CHANGED (CHANGED= skips the race step explicitly)"
	packages="$(changed_since "$merge_base")"
	if [ -z "$packages" ]; then
		printf 'baseline: no changed Go packages against BASE=%s; the race step will be skipped\n' "$base" >&2
	fi
	[ -z "$packages" ] || printf '%s\n' "$packages"
	exit 0
fi

if ! resolves master; then
	die "cannot resolve base ref master; set BASE=<revision> or CHANGED=<packages> (CHANGED= skips the race step explicitly)"
fi
merge_base="$(merge_base_with master)" || die "no merge base between master and HEAD; set BASE to a revision with common history or CHANGED (CHANGED= skips the race step explicitly)"
packages="$(changed_since "$merge_base")"
if [ -n "$packages" ]; then
	printf '%s\n' "$packages"
	exit 0
fi
die "no changed Go packages against master; set BASE=<revision> or CHANGED=<packages> (CHANGED= skips the race step explicitly)"
