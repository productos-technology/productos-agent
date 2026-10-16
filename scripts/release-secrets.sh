#!/bin/sh
# Stores the Actions secrets the release workflow needs on productos-technology/productos-agent:
#
#   RELEASE_APP_ID, RELEASE_APP_PRIVATE_KEY   the organisation's release App: the workflow mints an installation token
#                                             from them (actions/create-github-app-token) to write the Homebrew formula
#                                             into productos-technology/homebrew-tap and the winget manifest branch into
#                                             productos-technology/winget-pkgs. An App of the organisation, never a
#                                             personal token: the release belongs to productos-technology.
#   AGENT_SIGNING_KEY                         signs checksums.txt (Ed25519 seed, base64); the agent verifies with
#                                             internal/update/sign.go
#
#   scripts/release-secrets.sh app       # the App id and private key
#   scripts/release-secrets.sh signing   # the signing key
#
# The release App, once, on GitHub (organisation settings, Developer settings, GitHub Apps, New GitHub App; the script
# opens the page): name `productos-release`, any homepage, webhook off, repository permission Contents: Read and write,
# nothing else, "Only on this account". Then install it on productos-technology for exactly `homebrew-tap` and
# `winget-pkgs`, and generate a private key (a .pem download) on the App's page. `app` reads the App id and the .pem path.
#
# `signing` reads the private key from PRODUCTOS_SIGNING_KEY_FILE (default ~/.config/productos-agent-release/signing.key,
# written by `go run ./cmd/productos-agent-sign keygen` with PRODUCTOS_SIGN_PRIVATE_OUT). Keep that file (or a copy in a
# password manager): a release signed by a key the agents do not know is refused, and a lost key means adding a new
# public key to sign.go in a release still signed by the old one.
#
# Values are read from files or the terminal and handed to `gh secret set`, which encrypts them against the
# repository's public key before upload; nothing is written to disk or logged here.
set -eu

REPO="productos-technology/productos-agent"
OWNER="productos-technology"
APP_PAGE="https://github.com/organizations/$OWNER/settings/apps"

need() { command -v "$1" >/dev/null 2>&1 || { echo "missing: $1" >&2; exit 1; }; }
need gh
gh auth status >/dev/null 2>&1 || { echo "run: gh auth login" >&2; exit 1; }

open_url() {
  if command -v xdg-open >/dev/null 2>&1; then xdg-open "$1" >/dev/null 2>&1 &
  elif command -v open >/dev/null 2>&1; then open "$1"
  else echo "open this URL: $1"; fi
}

store_app() {
  echo
  echo "== RELEASE_APP_ID and RELEASE_APP_PRIVATE_KEY"
  echo "   The organisation's release App (Contents: Read and write, installed on $OWNER for homebrew-tap and winget-pkgs)."
  open_url "$APP_PAGE"
  printf '   App id (shown on the App page): '
  read -r app_id
  case "$app_id" in
    ''|*[!0-9]*) echo "   an App id is a number; nothing stored" >&2; return 1 ;;
  esac
  printf '   Path to the private key (.pem downloaded from the App page): '
  read -r pem
  [ -s "$pem" ] || { echo "   no file at $pem; nothing stored" >&2; return 1; }
  grep -q "BEGIN RSA PRIVATE KEY\|BEGIN PRIVATE KEY" "$pem" || { echo "   $pem is not a PEM private key; nothing stored" >&2; return 1; }
  printf '%s' "$app_id" | gh secret set RELEASE_APP_ID --repo "$REPO" --app actions
  gh secret set RELEASE_APP_PRIVATE_KEY --repo "$REPO" --app actions < "$pem"
  echo "   stored as Actions secrets on $REPO"
  for old in HOMEBREW_TAP_GITHUB_TOKEN WINGET_GITHUB_TOKEN; do
    if gh secret list --repo "$REPO" --json name -q '.[].name' | grep -qx "$old"; then
      gh secret delete "$old" --repo "$REPO" --app actions && echo "   removed the personal token secret $old"
    fi
  done
}

store_signing_key() {
  file=${PRODUCTOS_SIGNING_KEY_FILE:-$HOME/.config/productos-agent-release/signing.key}
  [ -s "$file" ] || { echo "no signing key at $file (run: PRODUCTOS_SIGN_PRIVATE_OUT=$file go run ./cmd/productos-agent-sign keygen)" >&2; exit 1; }
  echo
  echo "== AGENT_SIGNING_KEY (from $file)"
  tr -d '\n' < "$file" | gh secret set AGENT_SIGNING_KEY --repo "$REPO" --app actions
  echo "   stored as an Actions secret on $REPO"
}

case "${1:-app}" in
  app) store_app ;;
  signing) store_signing_key ;;
  *) echo "usage: scripts/release-secrets.sh [app|signing]" >&2; exit 2 ;;
esac

echo
echo "Secrets now on $REPO (values are never shown):"
gh secret list --repo "$REPO"
echo
echo "Next: a release ->  git tag vX.Y.Z && git push origin vX.Y.Z   (or: gh run rerun <failed release run>)"
