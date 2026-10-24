#!/bin/sh
# Download the pinned tool binaries into deploy/tools/bin/<arch>/ and verify
# their SHA-256 (lock.env). The image build then needs no network access.
set -eu
here=$(cd "$(dirname "$0")" && pwd)
. "$here/lock.env"

sha256() { if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | cut -d' ' -f1; else shasum -a 256 "$1" | cut -d' ' -f1; fi; }

verify() { # file expected
	got=$(sha256 "$1")
	[ "$got" = "$2" ] || { echo "checksum mismatch for $1: got $got, want $2" >&2; exit 1; }
}

for arch in amd64 arm64; do
	dir="$here/bin/$arch"
	mkdir -p "$dir"
	if [ "$arch" = amd64 ]; then ko_arch=x86_64; ko_sum=$KO_SHA256_AMD64; kc_sum=$KUBECTL_SHA256_AMD64; else ko_arch=arm64; ko_sum=$KO_SHA256_ARM64; kc_sum=$KUBECTL_SHA256_ARM64; fi
	tmp=$(mktemp -d)
	curl -fsSL -o "$tmp/ko.tgz" "https://github.com/ko-build/ko/releases/download/v${KO_VERSION}/ko_${KO_VERSION}_Linux_${ko_arch}.tar.gz"
	verify "$tmp/ko.tgz" "$ko_sum"
	tar -xzf "$tmp/ko.tgz" -C "$dir" ko
	curl -fsSL -o "$dir/kubectl" "https://dl.k8s.io/release/v${KUBECTL_VERSION}/bin/linux/${arch}/kubectl"
	verify "$dir/kubectl" "$kc_sum"
	chmod 0755 "$dir/ko" "$dir/kubectl"
	rm -rf "$tmp"
done
echo "tools verified in $here/bin"
