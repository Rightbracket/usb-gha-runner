# shellcheck shell=bash
# shellcheck disable=SC2154 # work and mode are set by the script that sources this
# Helpers shared by the QEMU tests. Source it after setting:
#   work  the working directory (cwd)
#   mode  uefi or bios
# PORT (web page) and SSH_PORT are forwarded from 127.0.0.1 to the guest.
PORT=${PORT:-8443}
SSH_PORT=${SSH_PORT:-2222}
BASE="https://127.0.0.1:$PORT"
jar=$work/cookies
fails=0

log() { printf '\n[%s] %s\n' "$(date +%T)" "$*"; }
ok() { echo "  PASS: $*"; }
bad() { echo "  FAIL: $*"; fails=$((fails + 1)); }
c() { curl -sk --max-time 20 -b "$jar" -c "$jar" "$@"; }
status() { c -f "$BASE/api/status"; }

# make_stick <img> <out> copies the image and grows it to 64 GB (sparse).
make_stick() {
  cp --sparse=always "$1" "$2"
  truncate -s 64G "$2"
}

# patch_boot <stick> <sed expression> edits both boot loader configs on the
# stick's GHR-BOOT partition and fails if either is left unchanged.
patch_boot() {
  local stick=$1 expr=$2 f fat
  fat="$stick@@$((2048 * 512))"
  for f in /boot/grub/grub.cfg /boot/syslinux/syslinux.cfg; do
    mcopy -n -i "$fat" "::$f" boot.cfg
    cp boot.cfg boot.orig
    sed -i "$expr" boot.cfg
    if cmp -s boot.cfg boot.orig; then echo "could not patch $f with $expr"; exit 1; fi
    mcopy -o -i "$fat" boot.cfg "::$f"
  done
  rm -f boot.cfg boot.orig
}

# Send the guest's journal to the serial log, so a failed boot can be read
# from CI output even if nothing reaches GHR-DATA.
journal_to_serial() {
  patch_boot "$1" 's|console=ttyS0,115200n8 console=tty0|console=tty0 console=ttyS0,115200n8 systemd.journald.forward_to_console=1|'
}

accel=tcg cpu=max
if [ -w /dev/kvm ]; then accel=kvm cpu=host; fi
fw=()
if [ "$mode" = uefi ]; then
  first() { for f in "$@"; do [ -e "$f" ] && { echo "$f"; return 0; }; done; return 1; }
  code=$(first /usr/share/OVMF/OVMF_CODE_4M.fd /usr/share/OVMF/OVMF_CODE.fd) || { echo "OVMF not installed"; exit 1; }
  ovmf_vars=$(first /usr/share/OVMF/OVMF_VARS_4M.fd /usr/share/OVMF/OVMF_VARS.fd)
  cp "$ovmf_vars" vars.fd
  fw=(-drive "if=pflash,format=raw,readonly=on,file=$code" -drive "if=pflash,format=raw,file=vars.fd")
fi

# qemu_up <name> <stick> [extra qemu args] boots a VM from <stick> on USB
# (xhci id "xhci"), logging the serial console to serial-$mode-<name>.log.
qemu_up() {
  local name=$1 stick=$2
  shift 2
  rm -f "$work/qemu.pid"
  qemu-system-x86_64 -name ghr-test -machine "q35,accel=$accel" -cpu "$cpu" -smp 4 -m 8192 \
    "${fw[@]}" -display none -daemonize -pidfile "$work/qemu.pid" \
    -serial "file:serial-$mode-$name.log" -monitor "unix:$work/qemu.mon,server,nowait" \
    -device qemu-xhci,id=xhci -drive "if=none,id=stick,file=$stick,format=raw,cache=unsafe" \
    -device usb-storage,bus=xhci.0,drive=stick,bootindex=0 \
    "$@" \
    -nic "user,model=virtio-net-pci,hostfwd=tcp:127.0.0.1:$PORT-:443,hostfwd=tcp:127.0.0.1:$SSH_PORT-:22"
}
monitor() { echo "$*" | socat - "UNIX-CONNECT:$work/qemu.mon"; }
vm_running() { [ -e "$work/qemu.pid" ] && kill -0 "$(cat "$work/qemu.pid")" 2>/dev/null; }

wait_up() {
  local limit=$1
  for _ in $(seq "$limit"); do
    c -o /dev/null -f "$BASE/api/health" && return 0
    sleep 5
  done
  return 1
}
wait_down() {
  for _ in $(seq 120); do
    vm_running || return 0
    sleep 5
  done
  return 1
}
# wait_serial <log> <regex> <seconds> waits for a line on the serial console.
wait_serial() {
  local f=$1 re=$2 limit=$3
  for _ in $(seq $((limit / 5))); do
    grep -aqE "$re" "$f" 2>/dev/null && return 0
    sleep 5
  done
  return 1
}
# Ask the guest to shut down (ACPI power button) so its journal and GHR-DATA
# are written out cleanly; kill it only if that doesn't work.
stop_vm() {
  monitor system_powerdown >/dev/null 2>&1 || true
  for _ in $(seq 36); do
    vm_running || return 0
    sleep 5
  done
  echo "  (guest did not power off; killing QEMU)"
  kill_vm
}
kill_vm() {
  kill "$(cat "$work/qemu.pid")" 2>/dev/null || true
  for _ in $(seq 12); do vm_running || return 0; sleep 1; done
  kill -9 "$(cat "$work/qemu.pid")" 2>/dev/null || true
  sleep 1
}
serial_errors() {
  grep -aE 'ghr|[Ff]ailed|error|sysbox|docker' "$1" | grep -av ssh-generator | tail -n "${2:-200}" || true
}

# part_start <stick> <label> prints the first sector of the partition with
# that GPT name, or nothing.
part_start() { sgdisk -p "$1" | awk -v l="$2" '$NF == l {print $2}'; }

# After the VM has stopped, print the stick's journal from GHR-DATA (mounted
# read-only) so a failed boot can be diagnosed from the CI log.
dump_journal() {
  local stick=$1 n=$2 start mnt
  sgdisk -p "$stick" | tail -n +8 || true
  start=$(part_start "$stick" GHR-DATA)
  if [ -z "$start" ]; then echo "  (no GHR-DATA partition, so no journal)"; return 0; fi
  mnt=$(mktemp -d)
  if sudo mount -o ro,loop,offset=$((start * 512)) "$stick" "$mnt"; then
    # shellcheck disable=SC2024 # the file is meant to be ours, not root's
    sudo journalctl -D "$mnt/log/journal" --no-pager -o short-monotonic >"journal-$mode-$n.txt" 2>&1 || true
    sudo find "$mnt/log/journal" -ls | head -n 20
    head -n 3 "journal-$mode-$n.txt"
    echo "  --- failed units and ghr services (full journal in journal-$mode-$n.txt) ---"
    grep -E 'Failed|failed|ghr|sysbox|docker|error' "journal-$mode-$n.txt" | grep -v 'systemd-ssh-generator' | tail -n 150 || true
    sudo umount "$mnt"
  else
    echo "  (could not mount GHR-DATA)"
  fi
  rmdir "$mnt"
}

# finish prints the summary and exits non-zero if any check failed.
finish() {
  echo
  if [ "$fails" -eq 0 ]; then echo "ALL CHECKS PASSED ($1)"; else echo "$fails CHECK(S) FAILED ($1)"; fi
  [ "$fails" -eq 0 ]
}
