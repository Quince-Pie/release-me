#!/usr/bin/env bash
# install-nix.sh - install one exact Nix version on a CI runner, first-party only.
#
# Fetches the official installer for NIX_VERSION from releases.nixos.org and
# checks it against the SHA-256 pinned below (the installer then verifies the
# tarball it downloads against the hash embedded in itself), then does a
# multi-user (daemon) install so builds run sandboxed. No third-party action
# is involved: the trust set stays {GitHub or Forgejo, nixos.org}, which the
# flake already relies on for nixpkgs and the binary cache.
#
# To bump: set NIX_VERSION, then update NIX_INSTALLER_SHA256 from
#   scripts/install-nix.sh --print-hash [VERSION]
set -euo pipefail

NIX_VERSION="${NIX_VERSION:-2.35.2}"
NIX_INSTALLER_SHA256="${NIX_INSTALLER_SHA256:-9adda97297d9e8ab360df95c729eabff4f4f93d6db091953c3a68f29e3fb130c}"

installer_url() {
  printf 'https://releases.nixos.org/nix/nix-%s/install\n' "$1"
}

if [ "${1:-}" = "--print-hash" ]; then
  curl -fsSL "$(installer_url "${2:-$NIX_VERSION}")" | sha256sum | cut -d' ' -f1
  exit 0
fi

if command -v nix >/dev/null 2>&1; then
  echo "install-nix: nix already installed: $(nix --version)"
  exit 0
fi

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

curl -fsSL --retry 5 --retry-delay 3 -o "$tmp/install" "$(installer_url "$NIX_VERSION")"
echo "$NIX_INSTALLER_SHA256  $tmp/install" | sha256sum --check --strict --quiet -

cat >"$tmp/nix.conf" <<CONF
experimental-features = nix-command flakes
trusted-users = root ${USER:-runner}
CONF

sh "$tmp/install" --daemon --yes --no-channel-add --no-modify-profile \
  --nix-extra-conf-file "$tmp/nix.conf"

# Make nix available to the following workflow steps.
if [ -n "${GITHUB_PATH:-}" ]; then
  echo "/nix/var/nix/profiles/default/bin" >>"$GITHUB_PATH"
fi
echo "install-nix: installed Nix $NIX_VERSION"
