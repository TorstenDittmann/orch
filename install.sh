#!/bin/sh
# Installs the orch CLI on Linux and macOS.
#
#   curl -fsSL https://raw.githubusercontent.com/TorstenDittmann/orch/main/install.sh | sh
#
# ORCH_VERSION picks a release tag such as v0.2.0 (default: the latest release).
# ORCH_INSTALL_DIR picks the directory (default: /usr/local/bin, using sudo when
# needed, or ~/.local/bin when sudo is unavailable).
set -eu

repo="TorstenDittmann/orch"
version="${ORCH_VERSION:-latest}"

say() { printf '%s\n' "$*"; }
fail() { printf 'orch install: %s\n' "$*" >&2; exit 1; }

case "$(uname -s)" in
	Linux) os=linux ;;
	Darwin) os=darwin ;;
	*) fail "unsupported system $(uname -s); on Windows run: irm https://raw.githubusercontent.com/$repo/main/install.ps1 | iex" ;;
esac
case "$(uname -m)" in
	x86_64 | amd64) arch=amd64 ;;
	aarch64 | arm64) arch=arm64 ;;
	*) fail "unsupported architecture $(uname -m)" ;;
esac
# Rosetta reports x86_64 on Apple silicon; the native build is faster.
if [ "$os" = darwin ] && [ "$arch" = amd64 ] && [ "$(sysctl -n hw.optional.arm64 2>/dev/null || true)" = 1 ]; then
	arch=arm64
fi

asset="orch-$os-$arch"
if [ "$version" = latest ]; then
	base="https://github.com/$repo/releases/latest/download"
else
	case "$version" in v*) ;; *) version="v$version" ;; esac
	base="https://github.com/$repo/releases/download/$version"
fi

if command -v curl >/dev/null 2>&1; then
	download() { curl -fsSL --retry 3 -o "$2" "$1"; }
elif command -v wget >/dev/null 2>&1; then
	download() { wget -q -O "$2" "$1"; }
else
	fail "curl or wget is required"
fi
if command -v sha256sum >/dev/null 2>&1; then
	sha256() { sha256sum "$1" | cut -d' ' -f1; }
elif command -v shasum >/dev/null 2>&1; then
	sha256() { shasum -a 256 "$1" | cut -d' ' -f1; }
else
	fail "sha256sum or shasum is required to verify the download"
fi

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT INT TERM

say "Downloading $asset ($version)"
download "$base/$asset" "$tmp/orch" || fail "could not download $base/$asset"
download "$base/SHA256SUMS" "$tmp/SHA256SUMS" || fail "could not download $base/SHA256SUMS"
expected="$(awk -v name="$asset" '$2 == name || $2 == "*" name { print $1 }' "$tmp/SHA256SUMS")"
[ -n "$expected" ] || fail "SHA256SUMS has no entry for $asset"
[ "$(sha256 "$tmp/orch")" = "$expected" ] || fail "checksum mismatch for $asset"
chmod 0755 "$tmp/orch"

dir="${ORCH_INSTALL_DIR:-}"
sudo=""
if [ -z "$dir" ]; then
	dir=/usr/local/bin
	if [ "$(id -u)" -ne 0 ] && ! { mkdir -p "$dir" 2>/dev/null && [ -w "$dir" ]; }; then
		if command -v sudo >/dev/null 2>&1; then
			say "Installing to $dir needs administrator rights; sudo may ask for your password."
			sudo="sudo"
		else
			dir="$HOME/.local/bin"
		fi
	fi
fi
$sudo mkdir -p "$dir"
# Replace rather than overwrite, so a running orch or a link that a host
# install created is swapped out instead of written through.
$sudo cp "$tmp/orch" "$dir/.orch-new"
$sudo mv -f "$dir/.orch-new" "$dir/orch"

say "Installed orch $("$dir/orch" version 2>/dev/null || true) to $dir/orch"
case ":$PATH:" in
	*":$dir:"*) ;;
	*) say "Add $dir to your PATH, for example: echo 'export PATH=\"$dir:\$PATH\"' >> ~/.profile" ;;
esac
say ""
say "Next: run 'sudo orch init' on a Linux server to start a cluster,"
say "or 'orch target add NAME ssh://root@HOST' to manage an existing one."
