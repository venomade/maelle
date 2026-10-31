#!/usr/bin/env bash
# deploy.sh — push the repo to the maelle server and switch NixOS to it.
#
# From the repo root:
#   ./deploy.sh [user@host]           # default: maelle@maelle.sh
#
# What it does:
#   1. uploads proxy/ and site/ to a staging dir on the server, plus the
#      configuration template with the machine values (GRUB device, EFI
#      flags, SSH key) substituted in — values read from the live config,
#      never overwriting it with raw @PLACEHOLDER@s
#   2. on first run (or while /etc/maelle.env still holds the install
#      placeholder): asks for MISTRAL_API_KEY + MAELLE_PROXY_KEY and
#      installs them root-only (never in this repo)
#   3. nixos-rebuild switch FROM THE STAGING DIR — /etc/nixos only adopts
#      the new files after the build has succeeded, so a failed build
#      leaves the previous configuration fully intact
#   4. smoke-checks https://maelle.sh and https://api.maelle.sh
#
# Requires: passwordless sudo for the ssh user (the install config grants it
# to maelle) and DNS for maelle.sh + api.maelle.sh pointing at the server.
set -Eeuo pipefail

HOST="${1:-maelle@maelle.sh}"
STAGE="/home/maelle/deploy-incoming"
REPO_ROOT="$(cd "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"

log()  { printf '\033[1;32m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m ->\033[0m %s\n' "$*"; }
die()  { printf '\033[1;31mERR:\033[0m %s\n' "$*" >&2; exit 1; }
trap 'die "deploy failed — the server keeps its previous configuration"' ERR

cd "$REPO_ROOT"
for f in configuration.nix proxy/go.mod proxy/main.go proxy/completions.go \
         proxy/tools.go proxy/persona.md proxy/tools.json site/index.html; do
  [[ -f "$f" ]] || die "missing $f — run this from the repo root"
done

# A dirty tree would deploy uncommitted state — warn, don't block.
if command -v git >/dev/null 2>&1 && git rev-parse --is-inside-work-tree >/dev/null 2>&1; then
  [[ -z "$(git status --porcelain)" ]] \
    || warn "working tree has uncommitted changes — deploying them anyway"
fi

log "Uploading files to $HOST"
ssh "$HOST" "rm -rf '$STAGE' && mkdir -p '$STAGE'"
tar czf - -C "$REPO_ROOT" proxy site | ssh "$HOST" "tar xzf - -C '$STAGE'"

log "Reading machine-specific values from the live config"
# configuration.nix in the repo is a TEMPLATE (@GRUB_DEVICE@ and friends are
# substituted by install.sh at install time). The substituted values live
# only on the server — so read them from the live config and rebuild the
# substituted file, never clobber it with the raw template.
CURRENT="$(ssh "$HOST" "sudo cat /etc/nixos/configuration.nix")" \
  || die "cannot read /etc/nixos/configuration.nix on the server"
GRUB_DEVICE="$(grep -m1 'device = ' <<<"$CURRENT" | sed -E 's/.*= *"([^"]*)".*/\1/')"
EFI_SUPPORT="$(grep -m1 'efiSupport = ' <<<"$CURRENT" | sed -E 's/.*= *([^;]*);.*/\1/')"
EFI_CAN_TOUCH="$(grep -m1 'canTouchEfiVariables = ' <<<"$CURRENT" | sed -E 's/.*= *([^;]*);.*/\1/')"
SSH_PUB="$(grep -m1 'authorizedKeys.keys = ' <<<"$CURRENT" | sed -E 's/.*\[ *"(.*)".*/\1/')"
[[ "$GRUB_DEVICE" =~ ^/dev/ || "$GRUB_DEVICE" == "nodev" ]] || die "no GRUB device in the live config — repair /etc/nixos/configuration.nix first"
[[ "$EFI_SUPPORT" == true || "$EFI_SUPPORT" == false ]] || die "no efiSupport value in the live config"
[[ "$EFI_CAN_TOUCH" == true || "$EFI_CAN_TOUCH" == false ]] || die "no canTouchEfiVariables value in the live config"
[[ "$SSH_PUB" =~ ^(ssh|ecdsa|sk)- ]] || die "no SSH key in the live config"

log "Substituting machine values into the config template"
CONFIG="$(<configuration.nix)"
CONFIG="${CONFIG//@SSH_PUB@/$SSH_PUB}"
CONFIG="${CONFIG//@GRUB_DEVICE@/$GRUB_DEVICE}"
CONFIG="${CONFIG//@EFI_SUPPORT@/$EFI_SUPPORT}"
CONFIG="${CONFIG//@EFI_CAN_TOUCH@/$EFI_CAN_TOUCH}"
if [[ "$CONFIG" =~ @[A-Z_]+@ ]]; then
  die "unsubstituted placeholder: ${BASH_REMATCH[0]} — stale template?"
fi
printf '%s\n' "$CONFIG" | ssh "$HOST" "cat > '$STAGE'/configuration.nix"

# hardware-configuration.nix (real disk UUIDs) is generated per-machine at
# install time and never in the repo — bring it into the staging dir so the
# config can be evaluated there.
ssh "$HOST" "sudo cp /etc/nixos/hardware-configuration.nix '$STAGE/'"

log "Checking secrets (/etc/maelle.env)"
# Prompt only when the file is missing/empty or still holds the install
# placeholder. An already-configured server keeps its secrets untouched.
if ! ssh "$HOST" "sudo grep -q '^MAELLE_PROXY_KEY=' /etc/maelle.env && ! sudo grep -q 'placeholder-run-deploy' /etc/maelle.env"; then
  log "First deploy — setting MISTRAL_API_KEY and MAELLE_PROXY_KEY"
  [[ -n "${MISTRAL_API_KEY:-}" ]] || read -r -p "MISTRAL_API_KEY (console.mistral.ai): " MISTRAL_API_KEY
  [[ -n "${MAELLE_PROXY_KEY:-}" ]] || read -r -p "MAELLE_PROXY_KEY (secret your clients present): " MAELLE_PROXY_KEY
  [[ -n "${MISTRAL_API_KEY:-}" && -n "${MAELLE_PROXY_KEY:-}" ]] || die "both keys are required"
  ssh "$HOST" 'sudo sh -c "umask 077 && cat > /etc/maelle.env"' <<EOF
MISTRAL_API_KEY=$MISTRAL_API_KEY
MAELLE_PROXY_KEY=$MAELLE_PROXY_KEY
EOF
fi

log "nixos-rebuild switch on $HOST from the staging dir (first run builds the Go proxy — takes a while)"
# Switch BEFORE touching /etc/nixos: if this fails, the server keeps its
# previous configuration untouched — exactly what the error message claims.
ssh "$HOST" "sudo nixos-rebuild switch -I nixos-config='$STAGE'/configuration.nix"

log "Build succeeded — installing files into /etc/nixos"
ssh "$HOST" "
  sudo install -m 644 '$STAGE'/configuration.nix /etc/nixos/configuration.nix &&
  sudo rm -rf /etc/nixos/maelle-proxy /etc/nixos/maelle-site &&
  sudo cp -r '$STAGE'/proxy /etc/nixos/maelle-proxy &&
  sudo cp -r '$STAGE'/site /etc/nixos/maelle-site &&
  sudo chmod -R a+rX /etc/nixos/maelle-proxy /etc/nixos/maelle-site &&
  rm -rf '$STAGE'
"

log "Smoke checks"
sleep 5    # give nginx/ACME a moment on the very first deploy
if curl -fsS --max-time 15 "https://maelle.sh" | grep -qi "coming soon"; then
  log "maelle.sh: serving the coming-soon page"
else
  warn "maelle.sh not serving yet — on a first deploy Let's Encrypt may still be issuing; retry in a minute"
fi
status="$(curl -s -o /dev/null -w '%{http_code}' --max-time 15 https://api.maelle.sh/v1/models)"
if [[ "$status" == "401" ]]; then
  log "api.maelle.sh: proxy is up (401 without a key — exactly right)"
else
  warn "api.maelle.sh /v1/models returned '$status' (expected 401 — TLS may still be issuing)"
fi

log "Deployed. maelle-proxy and nginx are enabled — they survive reboots."