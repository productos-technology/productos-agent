#!/bin/sh
# Stores the two Actions secrets the release workflow needs on Hamdan85/productos-agent:
#
#   HOMEBREW_TAP_GITHUB_TOKEN  writes the formula into Hamdan85/homebrew-tap
#   WINGET_GITHUB_TOKEN        pushes a branch to the Hamdan85/winget-pkgs fork and opens the upstream PR
#
# GitHub has no API for minting a personal access token (anything that could mint one would already hold a
# credential), so for each secret this script opens the token page pre-filled with the right name, repository
# and permissions, you press "Generate token", paste it here once, and `gh secret set` encrypts it against the
# repository's public key before upload. The value is read with the terminal echo off and is never written to
# disk or logged.
#
#   scripts/release-secrets.sh            # both secrets
#   scripts/release-secrets.sh winget     # one of: homebrew | winget
set -eu

REPO="Hamdan85/productos-agent"
OWNER="Hamdan85"

need() { command -v "$1" >/dev/null 2>&1 || { echo "missing: $1" >&2; exit 1; }; }
need gh
gh auth status >/dev/null 2>&1 || { echo "run: gh auth login" >&2; exit 1; }

open_url() {
  if command -v xdg-open >/dev/null 2>&1; then xdg-open "$1" >/dev/null 2>&1 &
  elif command -v open >/dev/null 2>&1; then open "$1"
  else echo "open this URL: $1"; fi
}

# token_page NAME TARGET_REPO DESCRIPTION: a fine-grained token restricted to one repository, 1 year.
# GitHub reads these query parameters on the "new token" page and pre-fills the form.
token_page() {
  echo "https://github.com/settings/personal-access-tokens/new?name=$1&description=$3&target_name=$OWNER&repositories=$2&expires_in=365&contents=write&pull_requests=write&workflows=write"
}

store() { # store SECRET_NAME TARGET_REPO DESCRIPTION
  name=$1; target=$2; desc=$3
  echo
  echo "== $name"
  echo "   Token for $OWNER/$target. The form opens pre-filled: check 'Repository access' shows only $target,"
  echo "   'Contents: Read and write' (and 'Pull requests: Read and write' for winget), then Generate token."
  open_url "$(token_page "$name" "$target" "$desc")"
  printf '   Paste the token (input is hidden): '
  stty -echo 2>/dev/null || true
  read -r value
  stty echo 2>/dev/null || true
  echo
  case "$value" in
    github_pat_*) ;;
    *) echo "   that does not look like a fine-grained token (expected github_pat_...); nothing stored" >&2; return 1 ;;
  esac
  printf '%s' "$value" | gh secret set "$name" --repo "$REPO" --app actions
  echo "   stored as an Actions secret on $REPO"
}

which=${1:-both}
case "$which" in
  homebrew|both) store HOMEBREW_TAP_GITHUB_TOKEN homebrew-tap "productos-agent release: writes the Homebrew formula" ;;
esac
case "$which" in
  winget|both) store WINGET_GITHUB_TOKEN winget-pkgs "productos-agent release: pushes the winget manifest branch" ;;
esac

echo
echo "Secrets now on $REPO (values are never shown):"
gh secret list --repo "$REPO"
echo
echo "Next: cut the first release ->  git tag v0.1.0 && git push origin v0.1.0"
