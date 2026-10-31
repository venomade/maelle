#!/usr/bin/env bash
#
# maelle — local test VM (QEMU), shaped like a Hetzner CX23:
# legacy BIOS (SeaBIOS), 1 vCPU, 2 GB RAM, one 40 GB virtio disk,
# DHCP via user networking.
#
# From inside the guest, the host is reachable as 10.0.2.2 — serve the
# repo with `python3 -m http.server` and the keys live there too.
# The guest's SSH port is forwarded to localhost:2222.
#
# Usage:
#   ISO=~/nixos-minimal-x86_64.iso ./test-vm.sh install   # boot the ISO
#   ./test-vm.sh boot                                     # boot installed system
#
# The whole session runs on the serial console in your terminal:
#   - in the installer's boot menu, pick "Serial console=ttyS0,115200n8"
#   - Ctrl+A X quits QEMU, Ctrl+A C toggles the QEMU monitor
#
set -Eeuo pipefail

MODE="${1:-}"
[[ "$MODE" == "install" || "$MODE" == "boot" ]] \
  || { echo "usage: $0 install|boot" >&2; exit 1; }
[[ "$MODE" == "boot" || -n "${ISO:-}" ]] \
  || { echo "set ISO=/path/to/nixos-minimal-x86_64.iso" >&2; exit 1; }

DISK_IMG="${DISK_IMG:-maelle-test.qcow2}"
[[ -f "$DISK_IMG" ]] || qemu-img create -f qcow2 "$DISK_IMG" 40G

# KVM only if the current user can actually use it — existence alone isn't
# enough: /dev/kvm can be root:kvm with mode 660. Otherwise fall back to
# TCG (slow but universal). Permanent fix: sudo usermod -aG kvm $USER
ACCEL="tcg"
if [[ -w /dev/kvm ]]; then
  ACCEL="kvm"
else
  echo "note: no KVM access — using TCG emulation (slow)." >&2
  [[ -e /dev/kvm ]] && echo "      hint: sudo usermod -aG kvm \$USER" >&2
fi

CDROM=()
[[ "$MODE" == "install" ]] && CDROM=(-cdrom "$ISO")

exec qemu-system-x86_64 \
  -machine pc -accel "$ACCEL" \
  -cpu max -m 2048 -smp 1 \
  -drive file="$DISK_IMG",if=virtio,format=qcow2 \
  -netdev user,id=net0,hostfwd=tcp:127.0.0.1:2222-:22 \
  -device virtio-net-pci,netdev=net0 \
  -nographic \
  "${CDROM[@]}"