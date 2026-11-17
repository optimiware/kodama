#!/bin/sh
# install.sh: install the kodema and kodactl binaries of a Kodema release.
#
#   curl -fsSL https://raw.githubusercontent.com/optimiware/kodema/master/install.sh | sh
#
# It detects the operating system and the architecture, downloads the two
# archives of the release and its SHA256SUMS, checks both archives against it
# and installs kodema and kodactl into ~/.local/bin. It needs curl and tar, and
# never asks for root.
#
#   KODEMA_VERSION       the release to install, for example 0.2.1 (default: the latest)
#   KODEMA_INSTALL_DIR   the directory to install into (default: ~/.local/bin)
#   KODEMA_RELEASES_URL  where releases are downloaded from
#                        (default: https://github.com/optimiware/kodema/releases)
set -eu

say() { printf 'kodema install: %s\n' "$*"; }
die() {
	printf 'kodema install: %s\n' "$*" >&2
	exit 1
}

# sha256_of <file> prints the SHA-256 digest of the file in hexadecimal.
sha256_of() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | cut -d ' ' -f 1
	else
		shasum -a 256 "$1" | cut -d ' ' -f 1
	fi
}

# The whole script is one function, called on the last line, so a download
# that stops halfway runs nothing.
main() {
	releases="${KODEMA_RELEASES_URL:-https://github.com/optimiware/kodema/releases}"
	releases="${releases%/}"
	directory="${KODEMA_INSTALL_DIR:-${HOME}/.local/bin}"

	for tool in curl tar uname mktemp; do
		command -v "$tool" >/dev/null 2>&1 || die "${tool} is required"
	done
	command -v sha256sum >/dev/null 2>&1 || command -v shasum >/dev/null 2>&1 ||
		die "sha256sum or shasum is required"

	case "$(uname -s)" in
	Linux) os=linux ;;
	Darwin) os=darwin ;;
	*) die "no release is built for the operating system $(uname -s); build from a checkout with 'make build'" ;;
	esac
	case "$(uname -m)" in
	x86_64 | amd64) arch=amd64 ;;
	arm64 | aarch64) arch=arm64 ;;
	*) die "no release is built for the architecture $(uname -m); build from a checkout with 'make build'" ;;
	esac

	# The default source is reached over HTTPS only, redirects included.
	transport=""
	case "$releases" in
	https://*) transport="--proto =https --tlsv1.2" ;;
	esac
	# shellcheck disable=SC2086 # transport is two options, split on purpose
	fetch() { curl --fail --silent --show-error --location $transport "$@"; }

	version="${KODEMA_VERSION:-}"
	version="${version#v}"
	if [ -z "$version" ]; then
		# releases/latest redirects to releases/tag/v<version>.
		latest="$(fetch --output /dev/null --write-out '%{url_effective}' "${releases}/latest")" ||
			die "cannot find the latest release at ${releases}/latest"
		case "$latest" in
		*/tag/v*) version="${latest##*/tag/v}" ;;
		*) die "cannot read the latest release from ${latest}" ;;
		esac
	fi
	case "$version" in
	"" | *[!0-9A-Za-z.-]*) die "'${version}' is not a version" ;;
	esac

	work="$(mktemp -d)"
	trap 'rm -rf "$work"' EXIT

	say "downloading Kodema ${version} for ${os}/${arch}"
	fetch --output "${work}/SHA256SUMS" "${releases}/download/v${version}/SHA256SUMS" ||
		die "release ${version} was not found at ${releases}"
	for binary in kodema kodactl; do
		archive="${binary}_${version}_${os}_${arch}.tar.gz"
		fetch --output "${work}/${archive}" "${releases}/download/v${version}/${archive}" ||
			die "release ${version} has no ${binary} for ${os}/${arch}; releases before 0.2.1 carry no kodema binary"
		# The archive must be listed: a file SHA256SUMS does not name is refused.
		expected="$(awk -v name="$archive" '$2 == name || $2 == "*" name { print $1 }' "${work}/SHA256SUMS")"
		[ -n "$expected" ] || die "SHA256SUMS of release ${version} does not list ${archive}"
		[ "$(sha256_of "${work}/${archive}")" = "$expected" ] ||
			die "${archive} does not match its SHA256SUMS digest; nothing was installed"
	done

	mkdir -p "$directory"
	for binary in kodema kodactl; do
		name="${binary}_${version}_${os}_${arch}"
		tar -xzf "${work}/${name}.tar.gz" -C "$work"
		[ -f "${work}/${name}/${binary}" ] || die "${name}.tar.gz does not contain ${binary}"
		# Copy beside the destination and rename, so a binary that is running
		# is replaced and never half-written.
		cp "${work}/${name}/${binary}" "${directory}/.${binary}.install.$$"
		chmod 0755 "${directory}/.${binary}.install.$$"
		mv -f "${directory}/.${binary}.install.$$" "${directory}/${binary}"
	done
	say "installed kodema and kodactl ${version} in ${directory}"

	case ":${PATH}:" in
	*":${directory}:"*) ;;
	*) say "${directory} is not on your PATH; add it: export PATH=\"${directory}:\$PATH\"" ;;
	esac
	if ! command -v podman >/dev/null 2>&1; then
		say "'kodema up' needs rootless Podman 5 or newer: https://podman.io/docs/installation"
	fi
	say "start a local Kodema with: kodema up"
}

main "$@"
