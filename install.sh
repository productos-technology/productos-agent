#!/bin/sh
# ProductOS Agent installer: curl -fsSL https://productos.technology/install.sh | sh
#
# Detects the OS and architecture, downloads the matching archive of the latest GitHub release of
# productos/productos-agent (or the version in PRODUCTOS_AGENT_VERSION, e.g. v1.2.3), verifies its SHA-256 against
# checksums.txt, and installs the `productos-agent` binary into PRODUCTOS_AGENT_INSTALL_DIR (default: /usr/local/bin
# when writable, else ~/.local/bin). Linux and macOS; on Windows use `winget install ProductOS.Agent`.
set -eu

REPO="productos/productos-agent"
BINARY="productos-agent"
VERSION="${PRODUCTOS_AGENT_VERSION:-latest}"
DOCS="https://productos.technology/docs/agent"

say() { printf '%s\n' "$*" >&2; }
die() { say "error: $*"; exit 1; }
need() { command -v "$1" >/dev/null 2>&1 || die "$1 is required"; }

os=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$os" in
  linux) ;;
  darwin) ;;
  mingw*|msys*|cygwin*) die "on Windows, run: winget install ProductOS.Agent" ;;
  *) die "unsupported OS: $os" ;;
esac

arch=$(uname -m)
case "$arch" in
  x86_64|amd64) arch=amd64 ;;
  aarch64|arm64) arch=arm64 ;;
  *) die "unsupported architecture: $arch" ;;
esac

need curl
need tar

if [ "$VERSION" = "latest" ]; then
  VERSION=$(curl -fsSL -H 'Accept: application/vnd.github+json' "https://api.github.com/repos/$REPO/releases/latest" \
    | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -n1)
  [ -n "$VERSION" ] || die "could not find the latest release of $REPO"
fi
version_number=${VERSION#v}

archive="${BINARY}_${version_number}_${os}_${arch}.tar.gz"
base="https://github.com/$REPO/releases/download/$VERSION"

tmp=$(mktemp -d 2>/dev/null || mktemp -d -t productos-agent)
trap 'rm -rf "$tmp"' EXIT

say "Downloading $BINARY $VERSION for $os/$arch"
curl -fsSL -o "$tmp/$archive" "$base/$archive" || die "download failed: $base/$archive"
curl -fsSL -o "$tmp/checksums.txt" "$base/checksums.txt" || die "download failed: $base/checksums.txt"

expected=$(grep " $archive\$" "$tmp/checksums.txt" | awk '{print $1}')
[ -n "$expected" ] || die "no checksum for $archive"
if command -v sha256sum >/dev/null 2>&1; then
  actual=$(sha256sum "$tmp/$archive" | awk '{print $1}')
elif command -v shasum >/dev/null 2>&1; then
  actual=$(shasum -a 256 "$tmp/$archive" | awk '{print $1}')
else
  die "sha256sum or shasum is required"
fi
[ "$expected" = "$actual" ] || die "checksum mismatch for $archive"

tar -xzf "$tmp/$archive" -C "$tmp" "$BINARY"

install_dir="${PRODUCTOS_AGENT_INSTALL_DIR:-}"
if [ -z "$install_dir" ]; then
  if [ -w /usr/local/bin ]; then
    install_dir=/usr/local/bin
  else
    install_dir="$HOME/.local/bin"
  fi
fi
mkdir -p "$install_dir"
install -m 0755 "$tmp/$BINARY" "$install_dir/$BINARY"

say "Installed $install_dir/$BINARY ($("$install_dir/$BINARY" version 2>/dev/null || echo "$VERSION"))"
case ":$PATH:" in
  *":$install_dir:"*) ;;
  *) say "Add $install_dir to your PATH." ;;
esac
say ""
say "Next:"
say "  $BINARY login https://productos.technology    # sign in and register this machine"
say "  cd <a checkout> && $BINARY link               # link a repository your workspace connects"
say "  $BINARY service install                       # keep it running in the background"
say "Documentation: $DOCS"
