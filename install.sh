#!/bin/sh
# Oopsy installer: curl -fsSL https://sohan-bhat.github.io/oopsy/install.sh | sh
# Installs oops and oops-db into /usr/local/bin when writable, else ~/.local/bin.
# Pin a version with OOPS_VERSION, for example:
#   curl -fsSL https://sohan-bhat.github.io/oopsy/install.sh | OOPS_VERSION=0.2.0 sh
# Set OOPSY_BIN_DIR to pick a directory.
set -eu

repo="sohan-bhat/oopsy"

say() { printf '%s\n' "$*"; }
fail() { printf 'oopsy install: %s\n' "$*" >&2; exit 1; }

need() { command -v "$1" >/dev/null 2>&1 || fail "$1 is required"; }
need uname
need tar
if command -v curl >/dev/null 2>&1; then
  fetch() { curl -fsSL "$1"; }
  fetch_to() { curl -fsSL -o "$2" "$1"; }
elif command -v wget >/dev/null 2>&1; then
  fetch() { wget -qO- "$1"; }
  fetch_to() { wget -qO "$2" "$1"; }
else
  fail "curl or wget is required"
fi

os="$(uname -s | tr '[:upper:]' '[:lower:]')"
case "$os" in
  darwin|linux) ;;
  mingw*|msys*|cygwin*) fail "on Windows, install with PowerShell: irm https://sohan-bhat.github.io/oopsy/install.ps1 | iex" ;;
  *) fail "unsupported OS: $os (Oopsy supports macOS, Linux and Windows)" ;;
esac
arch="$(uname -m)"
case "$arch" in
  x86_64|amd64) arch=amd64 ;;
  arm64|aarch64) arch=arm64 ;;
  *) fail "unsupported CPU: $arch" ;;
esac

version="${OOPS_VERSION:-${OOPSY_VERSION:-}}"
if [ -n "$version" ]; then
  version="v${version#v}"
else
  version="$(fetch "https://api.github.com/repos/$repo/releases/latest" 2>/dev/null | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -n 1)" || true
  if [ -z "$version" ] && command -v curl >/dev/null 2>&1; then
    # The API limits anonymous calls; the release page redirect has the tag too.
    url="$(curl -fsSL -o /dev/null -w '%{url_effective}' "https://github.com/$repo/releases/latest" 2>/dev/null || true)"
    version="${url##*/tag/}"
    [ "$version" = "$url" ] && version=""
  fi
  [ -n "$version" ] || fail "could not find the latest release; pin one with OOPS_VERSION=0.8.0"
fi
plain="${version#v}"
archive="oopsy_${plain}_${os}_${arch}.tar.gz"
base="https://github.com/$repo/releases/download/$version"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT INT TERM
say "Downloading Oopsy $version for $os/$arch"
fetch_to "$base/$archive" "$tmp/$archive" || fail "could not download $version for $os/$arch. Check that the version exists at https://github.com/$repo/releases"
fetch_to "$base/checksums.txt" "$tmp/checksums.txt" || fail "could not download checksums"

want="$(grep " $archive\$" "$tmp/checksums.txt" | cut -d' ' -f1)"
[ -n "$want" ] || fail "no checksum listed for $archive"
if command -v sha256sum >/dev/null 2>&1; then
  got="$(sha256sum "$tmp/$archive" | cut -d' ' -f1)"
else
  got="$(shasum -a 256 "$tmp/$archive" | cut -d' ' -f1)"
fi
[ "$want" = "$got" ] || fail "checksum mismatch for $archive"

tar -xzf "$tmp/$archive" -C "$tmp"

dir="${OOPSY_BIN_DIR:-}"
if [ -z "$dir" ]; then
  if [ -w /usr/local/bin ]; then dir=/usr/local/bin; else dir="$HOME/.local/bin"; fi
fi
mkdir -p "$dir"
for b in oops oops-db; do
  install -m 0755 "$tmp/$b" "$dir/$b" 2>/dev/null || { cp "$tmp/$b" "$dir/$b" && chmod 0755 "$dir/$b"; }
done

say "Installed oops and oops-db to $dir"
case ":$PATH:" in
  *":$dir:"*) ;;
  *) say "Add $dir to your PATH, for example: echo 'export PATH=\"$dir:\$PATH\"' >> ~/.zshrc" ;;
esac
say ""
say "Next: run  oops init  to turn on the shell and AI agent hooks."
