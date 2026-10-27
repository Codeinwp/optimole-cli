#!/bin/sh
# Installs the optimole CLI on macOS or Linux from its GitHub release.
#
#   curl -fsSL https://raw.githubusercontent.com/Codeinwp/optimole-cli/main/install.sh | sh
#
# It downloads the build for this machine, checks it against the release's checksums.txt and puts
# `optimole` in /usr/local/bin when that is writable, else in ~/.local/bin (added to your shell's PATH).
# No sudo. Settings, all optional:
#   OPTIMOLE_VERSION=v0.1.0          a specific release instead of the latest
#   OPTIMOLE_INSTALL_DIR=/some/dir   where to put the binary
#   OPTIMOLE_NO_MODIFY_PATH=1        never edit a shell profile
#   OPTIMOLE_DOWNLOAD_URL=...        where the release files are (for testing a local build)
set -eu

repo="Codeinwp/optimole-cli"
version="${OPTIMOLE_VERSION:-latest}"

fail() {
	printf 'optimole install: %s\n' "$*" >&2
	exit 1
}

case "$(uname -s)" in
Darwin) os=darwin ;;
Linux) os=linux ;;
*) fail "unsupported system $(uname -s); on Windows use install.ps1" ;;
esac

case "$(uname -m)" in
x86_64 | amd64) arch=amd64 ;;
arm64 | aarch64) arch=arm64 ;;
*) fail "unsupported CPU $(uname -m)" ;;
esac
# A shell running under Rosetta reports x86_64 on an Apple silicon Mac; install the native build.
if [ "$os" = darwin ] && [ "$arch" = amd64 ] && [ "$(sysctl -n sysctl.proc_translated 2>/dev/null || echo 0)" = 1 ]; then
	arch=arm64
fi

if [ -n "${OPTIMOLE_DOWNLOAD_URL:-}" ]; then
	base="${OPTIMOLE_DOWNLOAD_URL%/}"
elif [ "$version" = latest ]; then
	base="https://github.com/$repo/releases/latest/download"
else
	base="https://github.com/$repo/releases/download/$version"
fi
archive="optimole_${os}_${arch}.tar.gz"

if command -v curl >/dev/null 2>&1; then
	download() {
		case "$1" in
		https://*) curl -fsSL --proto '=https' --tlsv1.2 -o "$2" "$1" ;;
		*) curl -fsSL -o "$2" "$1" ;;
		esac
	}
elif command -v wget >/dev/null 2>&1; then
	download() { wget -q -O "$2" "$1"; }
else
	fail "needs curl or wget"
fi

if command -v sha256sum >/dev/null 2>&1; then
	sha256() { sha256sum "$1" | cut -d ' ' -f 1; }
elif command -v shasum >/dev/null 2>&1; then
	sha256() { shasum -a 256 "$1" | cut -d ' ' -f 1; }
else
	fail "needs sha256sum or shasum to check the download"
fi

tmp="$(mktemp -d 2>/dev/null || mktemp -d -t optimole)"
trap 'rm -rf "$tmp"' EXIT INT TERM

printf 'Downloading %s (%s)...\n' "$archive" "$version"
download "$base/$archive" "$tmp/$archive" || fail "could not download $base/$archive"
download "$base/checksums.txt" "$tmp/checksums.txt" || fail "could not download $base/checksums.txt"

expected="$(awk -v name="$archive" '$2 == name { print $1 }' "$tmp/checksums.txt")"
[ -n "$expected" ] || fail "checksums.txt lists no $archive"
[ "$(sha256 "$tmp/$archive")" = "$expected" ] || fail "checksum mismatch for $archive; nothing was installed"

tar -xzf "$tmp/$archive" -C "$tmp" optimole || fail "could not unpack $archive"

if [ -n "${OPTIMOLE_INSTALL_DIR:-}" ]; then
	dir="$OPTIMOLE_INSTALL_DIR"
elif [ -d /usr/local/bin ] && [ -w /usr/local/bin ]; then
	dir=/usr/local/bin
else
	dir="$HOME/.local/bin"
fi
mkdir -p "$dir" || fail "could not create $dir"
cp "$tmp/optimole" "$dir/optimole.tmp" && chmod 0755 "$dir/optimole.tmp" && mv -f "$dir/optimole.tmp" "$dir/optimole" ||
	fail "could not write to $dir; set OPTIMOLE_INSTALL_DIR to a writable directory"

printf 'Installed %s\n' "$("$dir/optimole" version)"
printf '         to %s/optimole\n' "$dir"

# Make `optimole` work in new terminals when the directory is not on PATH yet.
case ":$PATH:" in
*":$dir:"*) ;;
*)
	line="export PATH=\"$dir:\$PATH\""
	case "$(basename "${SHELL:-sh}")" in
	zsh) profile="$HOME/.zshrc" ;;
	bash) if [ "$os" = darwin ]; then profile="$HOME/.bash_profile"; else profile="$HOME/.bashrc"; fi ;;
	fish)
		profile="$HOME/.config/fish/config.fish"
		line="fish_add_path \"$dir\""
		;;
	*) profile="$HOME/.profile" ;;
	esac
	if [ "${OPTIMOLE_NO_MODIFY_PATH:-}" = 1 ]; then
		printf '\n%s is not on your PATH. Add this line to %s:\n  %s\n' "$dir" "$profile" "$line"
	elif ! grep -qsF "$line" "$profile"; then
		mkdir -p "$(dirname "$profile")"
		printf '\n# Added by the optimole installer\n%s\n' "$line" >>"$profile"
		printf '\nAdded %s to your PATH in %s. Open a new terminal, or run:\n  %s\n' "$dir" "$profile" "$line"
	fi
	;;
esac

printf '\nNext: optimole login   (paste an API key from the Optimole dashboard)\n'
