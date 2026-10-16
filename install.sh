#!/bin/sh
# ProductOS Agent installer: curl -fsSL https://productos.technology/install.sh | sh
#
# Detects the OS and architecture, downloads the matching archive of the latest GitHub release of
# productos-technology/productos-agent (or the version in PRODUCTOS_AGENT_VERSION, e.g. v1.2.3), verifies the release
# signature of checksums.txt (Ed25519, with openssl) and the archive's SHA-256, and installs the `productos-agent` binary
# into PRODUCTOS_AGENT_INSTALL_DIR (default: /usr/local/bin when writable, else ~/.local/bin). Linux and macOS; on
# Windows use `winget install ProductOS.Agent`.
set -eu

REPO="productos-technology/productos-agent"
BINARY="productos-agent"
VERSION="${PRODUCTOS_AGENT_VERSION:-latest}"
DOCS="https://productos.technology/docs/agent"
# The release public key (the same one compiled into the agent, internal/update/sign.go), in the form openssl reads.
PUBLIC_KEY_PEM="-----BEGIN PUBLIC KEY-----
MCowBQYDK2VwAyEAmIF2hDoEv7MM0NWPPo1ylMgjBTfg9HYg1BxhFOqmdJA=
-----END PUBLIC KEY-----"

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

tmp=$(mktemp -d 2>/dev/null || mktemp -d -t productos-agent)
trap 'rm -rf "$tmp"' EXIT

if [ "$VERSION" = "latest" ]; then
  # Anonymous: the release has to be public. The API answers 404 for a private repository as well as for a missing
  # release, and 403 once the hourly rate limit for this IP is spent, so the failure names the status.
  api="https://api.github.com/repos/$REPO/releases/latest"
  status=$(curl -sSL -o "$tmp/release.json" -w '%{http_code}' -H 'Accept: application/vnd.github+json' "$api" 2>/dev/null || true)
  VERSION=$(sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' "$tmp/release.json" 2>/dev/null | head -n1)
  if [ -z "$VERSION" ]; then
    say "error: could not find the latest release of $REPO (GitHub answered ${status:-000})"
    say "Check https://github.com/$REPO/releases, or install a known version: PRODUCTOS_AGENT_VERSION=v1.2.3 sh install.sh"
    exit 1
  fi
fi
version_number=${VERSION#v}

archive="${BINARY}_${version_number}_${os}_${arch}.tar.gz"
base="https://github.com/$REPO/releases/download/$VERSION"


say "Downloading $BINARY $VERSION for $os/$arch"
curl -fsSL -o "$tmp/$archive" "$base/$archive" || die "download failed: $base/$archive"
curl -fsSL -o "$tmp/checksums.txt" "$base/checksums.txt" || die "download failed: $base/checksums.txt"
curl -fsSL -o "$tmp/checksums.txt.sig" "$base/checksums.txt.sig" || die "download failed: $base/checksums.txt.sig (this release is not signed)"

# The signature binds checksums.txt to the release key; checksums.txt binds the archive. Without openssl the SHA-256 is
# still checked, but nothing proves who published the release: say so.
if command -v openssl >/dev/null 2>&1; then
  printf '%s\n' "$PUBLIC_KEY_PEM" > "$tmp/release.pub"
  openssl base64 -d -A -in "$tmp/checksums.txt.sig" -out "$tmp/checksums.txt.sig.bin" 2>/dev/null || die "malformed checksums.txt.sig"
  openssl pkeyutl -verify -pubin -inkey "$tmp/release.pub" -rawin -in "$tmp/checksums.txt" -sigfile "$tmp/checksums.txt.sig.bin" >/dev/null 2>&1 \
    || die "checksums.txt is not signed by the ProductOS release key"
else
  say "warning: openssl not found, the release signature was not checked"
fi

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
say "  $BINARY login                                 # sign in and register this machine"
say "  cd <a checkout> && $BINARY link               # link a repository your workspace connects"
say "  $BINARY service install                       # keep it running in the background"
say "Documentation: $DOCS"
