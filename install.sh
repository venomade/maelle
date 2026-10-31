#!/usr/bin/env bash
# Maelle Bootstrap Installer for NixOS
#
# Turns a machine booted into the NixOS minimal ISO into a running instance of Maelle
#
#   curl -fsSL https://raw.githubusercontent.com/venomade/maelle/master/install.sh \
#     | sudo bash -s -- \
#         --public-key  "ssh-ed25519 AAAA… me@host" \
#         --private-key "-----BEGIN OPENSSH PRIVATE KEY-----…"
#
# The keypair is the deploy key for github.com/venomade/maelle (master);
# the public half also authorises SSH logins as the user "maelle".
#
set -Eeuo pipefail

# Configuration Vars
REPO_RAW="${REPO_RAW:-https://raw.githubusercontent.com/venomade/maelle/master}"
GIT_REMOTE="git@github.com:venomade/maelle.git"
GIT_BRANCH="master"
USER_NAME="maelle"
DISK="${DISK:-/dev/sda}"

# Logging
log()  { printf '\033[1;32m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m ->\033[0m %s\n' "$*"; }
die()  { printf '\033[1;31mERR:\033[0m %s\n' "$*" >&2; exit 1; }
trap 'die "failed at line $LINENO — see output above"' ERR

usage() {
  cat <<'EOF'
Usage: install.sh [options]

  --public-key KEY     OpenSSH public key (file path or key material)   [required]
  --private-key KEY    matching private key (file path or material)     [required]
  -p, --prompt         ask for confirmation before wiping (default: no)
  --no-poweroff        don't power off when done
  -h, --help           print this help message
EOF
}

# Arguments
PUBKEY_ARG=""
PRIVKEY_ARG=""
PROMPT=0
DO_POWEROFF=1

while [[ $# -gt 0 ]]; do
  case "$1" in
    --public-key)   PUBKEY_ARG="$2"; shift 2 ;;
    --private-key)  PRIVKEY_ARG="$2"; shift 2 ;;
    -p|--prompt)    PROMPT=1; shift ;;
    --no-poweroff)  DO_POWEROFF=0; shift ;;
    -h|--help)      usage; exit 0 ;;
    *)              usage >&2; die "unknown option: $1" ;;
  esac
done

[[ -n "$PUBKEY_ARG" && -n "$PRIVKEY_ARG" ]] \
  || die "both --public-key and --private-key are required"

# OpenSSH Keys
load_key() {
  if [[ -f "$1" ]]; then cat "$1"; else printf '%s\n' "$1"; fi
}
PUBLIC_KEY="$(load_key "$PUBKEY_ARG")"
PRIVATE_KEY="$(load_key "$PRIVKEY_ARG")"

[[ "$PUBLIC_KEY" =~ ^(ssh|ecdsa|sk)- ]] || die "public key doesn't look like an OpenSSH public key"
[[ "$PRIVATE_KEY" == "-----BEGIN"* ]]   || die "private key doesn't look like an OpenSSH private key"
[[ "$PUBLIC_KEY" != *'"'* ]]            || die "public key contains a quote - refusing"

# Check System
[[ $EUID -eq 0 ]] || die "must run as root - pipe into 'sudo bash -s -- ...'"
command -v nixos-install >/dev/null 2>&1 \
  || die "nixos-install not found — boot the NixOS minimal ISO first"
[[ -b "$DISK" ]] || die "target disk $DISK does not exist"
nix-channel --list 2>/dev/null | grep -q '^nixos ' \
  || warn "no 'nixos' channel listed — check network if nixos-install fails"

# Prompt Mode
if (( PROMPT )); then
  lsblk -dn -o NAME,SIZE,MODEL "$DISK"
  read -r -p "This ERASES $DISK and installs NixOS. Type ERASE to continue: " answer < /dev/tty \
    || die "no terminal available for confirmation"
  [[ "$answer" == "ERASE" ]] || die "aborted"
fi

# Detect Boot
if [[ -d /sys/firmware/efi ]]; then
  BOOTMODE="uefi"
else
  BOOTMODE="bios"
fi
log "Boot mode: $BOOTMODE"

if [[ "$DISK" == *[0-9] ]]; then PART_SEP="p"; else PART_SEP=""; fi

# Partitioning
log "Unmounting leftovers from any previous run"
umount -R /mnt 2>/dev/null || true

log "Wiping $DISK"
wipefs --all --force "$DISK"
for p in $(lsblk -rno NAME "$DISK" | tail -n +2); do
  wipefs --all --force "/dev/$p" 2>/dev/null || true
done

log "Partitioning ($BOOTMODE)"
if [[ "$BOOTMODE" == "bios" ]]; then
  sfdisk --force --quiet "$DISK" <<EOF
label: dos
start=1MiB, type=83, bootable
EOF
  ROOT_PART="${DISK}${PART_SEP}1"
else
  sfdisk --force --quiet "$DISK" <<EOF
label: gpt
start=1MiB, size=512MiB, type=uefi, name=ESP
start=513MiB, type=linux, name=nixos
EOF
  ESP_PART="${DISK}${PART_SEP}1"
  ROOT_PART="${DISK}${PART_SEP}2"
fi

udevadm settle 2>/dev/null || true
for _ in $(seq 1 20); do
  [[ -b "$ROOT_PART" ]] && break
  sleep 0.5
done
[[ -b "$ROOT_PART" ]] || die "partition $ROOT_PART did not appear after partitioning"

log "Formatting"
[[ "$BOOTMODE" == "uefi" ]] && mkfs.fat -F 32 -n ESP "$ESP_PART"
mkfs.ext4 -F -L nixos "$ROOT_PART"

if ! fstype="$(blkid -o value -s TYPE "$ROOT_PART")" || [[ -z "$fstype" ]]; then
  die "no filesystem found on $ROOT_PART (expected ext4) — formatting failed"
fi
log "Found $fstype on $ROOT_PART"
sync

log "Mounting $ROOT_PART at /mnt"
mount "$ROOT_PART" /mnt
if [[ "$BOOTMODE" == "uefi" ]]; then
  mkdir -p /mnt/boot
  mount "$ESP_PART" /mnt/boot
fi

# Configuration
log "Generating hardware-configuration (detects real disks/UUIDs)"
nixos-generate-config --root /mnt

log "Fetching configuration.nix"
if [[ -n "${BASH_SOURCE[0]:-}" && -f "$(dirname -- "${BASH_SOURCE[0]}")/configuration.nix" ]]; then
  CONFIG_TEMPLATE="$(cat "$(dirname -- "${BASH_SOURCE[0]}")/configuration.nix")"
else
  CONFIG_TEMPLATE="$(curl -fsSL "$REPO_RAW/configuration.nix")"
fi

log "Substituting parameters"
if [[ "$BOOTMODE" == "bios" ]]; then
  GRUB_DEVICE="$DISK"
  EFI_SUPPORT="false"
  EFI_CAN_TOUCH="false"
else
  GRUB_DEVICE="nodev"
  EFI_SUPPORT="true"
  EFI_CAN_TOUCH="true"
fi

config="${CONFIG_TEMPLATE//@SSH_PUB@/$PUBLIC_KEY}"
config="${config//@GRUB_DEVICE@/$GRUB_DEVICE}"
config="${config//@EFI_SUPPORT@/$EFI_SUPPORT}"
config="${config//@EFI_CAN_TOUCH@/$EFI_CAN_TOUCH}"

# Only real @ALL_CAPS@ placeholders count as failures — an @ inside the SSH
# key comment (e.g. you@host) is valid and must not trip the check.
if [[ "$config" =~ @[A-Z_]+@ ]]; then
  die "unsubstituted placeholder: ${BASH_REMATCH[0]} — stale configuration.nix?"
fi

log "Writing /mnt/etc/nixos/configuration.nix"
printf '%s\n' "$config" > /mnt/etc/nixos/configuration.nix

log "Staging proxy source + site into /mnt/etc/nixos"
# configuration.nix builds the proxy from ./maelle-proxy and serves
# ./maelle-site, so both must exist before nixos-install evaluates it.
install -d -m 755 /mnt/etc/nixos/maelle-proxy /mnt/etc/nixos/maelle-site
if [[ -n "${BASH_SOURCE[0]:-}" && -f "$(dirname -- "${BASH_SOURCE[0]}")/proxy/main.go" ]]; then
  cp "$(dirname -- "${BASH_SOURCE[0]}")"/proxy/{go.mod,main.go,completions.go,tools.go,persona.md,tools.json} \
    /mnt/etc/nixos/maelle-proxy/
  cp "$(dirname -- "${BASH_SOURCE[0]}")/site/index.html" /mnt/etc/nixos/maelle-site/
else
  for f in go.mod main.go completions.go tools.go persona.md tools.json; do
    curl -fsSL "$REPO_RAW/proxy/$f" -o "/mnt/etc/nixos/maelle-proxy/$f"
  done
  curl -fsSL "$REPO_RAW/site/index.html" -o "/mnt/etc/nixos/maelle-site/index.html"
fi

log "Creating /mnt/etc/maelle.env placeholder (deploy.sh installs the real keys)"
# The proxy refuses to start without MAELLE_PROXY_KEY, and the file must
# exist for the service to boot. A placeholder key means the API denies
# every request until deploy.sh runs — safe by default.
install -m 400 -o 0 -g 0 /dev/null /mnt/etc/maelle.env
printf 'MISTRAL_API_KEY=\nMAELLE_PROXY_KEY=placeholder-run-deploy.sh\n' > /mnt/etc/maelle.env

# Install
log "Installing NixOS — downloads and builds the whole system (incl. the Go"
log "proxy, so this takes a few minutes on first run)"
nixos-install --no-root-passwd

HOME_TARGET="/mnt/home/$USER_NAME"
log "Installing keypair into /home/$USER_NAME/.ssh"
install -d -m 700 -o 1000 -g 100 "$HOME_TARGET/.ssh"
printf '%s\n' "$PUBLIC_KEY"  > "$HOME_TARGET/.ssh/id_ed25519.pub"
printf '%s\n' "$PRIVATE_KEY" > "$HOME_TARGET/.ssh/id_ed25519"
cat > "$HOME_TARGET/.ssh/config" <<'EOF'
Host github.com
    User git
    IdentityFile ~/.ssh/id_ed25519
    IdentitiesOnly yes
    StrictHostKeyChecking accept-new
EOF
chmod 600 "$HOME_TARGET/.ssh/id_ed25519" "$HOME_TARGET/.ssh/config"
chmod 644 "$HOME_TARGET/.ssh/id_ed25519.pub"

if command -v git >/dev/null 2>&1; then
  log "Cloning venomade/maelle (branch $GIT_BRANCH)"
  GIT_SSH_COMMAND="ssh -i $HOME_TARGET/.ssh/id_ed25519 -o IdentitiesOnly=yes -o StrictHostKeyChecking=accept-new" \
    git clone --branch "$GIT_BRANCH" --single-branch "$GIT_REMOTE" "$HOME_TARGET/maelle" \
    || warn "clone failed. Is the key a deploy key for venomade/maelle? Clone again on first boot"
else
  warn "git not available in the installer. Clone venomade/maelle on first boot"
fi
chown -R 1000:100 "$HOME_TARGET" 2>/dev/null || true

# Complete
if (( DO_POWEROFF )); then
  log "Install complete. Powering off in 5 seconds."
  warn "After reboot: ssh maelle@maelle.sh, then run ./deploy.sh from the repo"
  warn "(installs the real API keys and switches to the deployed config)"
  sleep 5
  poweroff
else
  log "Install complete (left running). Unmount the ISO before next boot."
fi