#!/bin/bash
# Boot ghr-usb.img in QEMU with fake host disks attached and check that:
#   1. the data partition is created on the stick only
#   2. every host disk is read-only, unmounted, with no LVM/md/swap active
#   3. (optional, with GHR_TEST_URL + GHR_TEST_PAT) a slot registers with GitHub
#   4. after a reboot the password, certificate and data are still there
#   5. the host disks are byte-for-byte unchanged afterwards
#
# usage: run.sh <ghr-usb.img> <uefi|bios> [workdir]
set -euo pipefail
img=$(readlink -f "$1")
mode=$2
work=${3:-$(mktemp -d)}
here=$(cd "$(dirname "$0")" && pwd)
mkdir -p "$work"
cd "$work"
PASSWORD="qemu-test-password"
PORT=${PORT:-8443}
BASE="https://127.0.0.1:$PORT"
jar=$work/cookies
fails=0

log() { printf '\n[%s] %s\n' "$(date +%T)" "$*"; }
ok() { echo "  PASS: $*"; }
bad() { echo "  FAIL: $*"; fails=$((fails + 1)); }
c() { curl -sk --max-time 20 -b "$jar" -c "$jar" "$@"; }

log "fake host disks"
sudo "$here/fake-disks.sh" "$work/disks"
sudo chown -R "$(id -u)" "$work/disks"

log "stick: copy of the image, grown to 64 GB (sparse)"
cp --sparse=always "$img" stick.img
truncate -s 64G stick.img

accel=tcg cpu=max
if [ -w /dev/kvm ]; then accel=kvm cpu=host; fi
fw=()
if [ "$mode" = uefi ]; then
  first() { for f in "$@"; do [ -e "$f" ] && { echo "$f"; return 0; }; done; return 1; }
  code=$(first /usr/share/OVMF/OVMF_CODE_4M.fd /usr/share/OVMF/OVMF_CODE.fd) || { echo "OVMF not installed"; exit 1; }
  vars=$(first /usr/share/OVMF/OVMF_VARS_4M.fd /usr/share/OVMF/OVMF_VARS.fd)
  cp "$vars" vars.fd
  fw=(-drive "if=pflash,format=raw,readonly=on,file=$code" -drive "if=pflash,format=raw,file=vars.fd")
fi

boot() {
  local n=$1
  rm -f qemu.pid
  qemu-system-x86_64 -name ghr-test -machine "q35,accel=$accel" -cpu "$cpu" -smp 4 -m 8192 \
    "${fw[@]}" -display none -daemonize -pidfile qemu.pid \
    -serial "file:serial-$mode-$n.log" \
    -device qemu-xhci -drive if=none,id=stick,file=stick.img,format=raw,cache=unsafe \
    -device usb-storage,drive=stick,bootindex=0 \
    -drive if=none,id=d1,file=disks/ext4.img,format=raw -device virtio-blk-pci,drive=d1 \
    -device ahci,id=ahci -drive if=none,id=d2,file=disks/ntfs.img,format=raw -device ide-hd,drive=d2,bus=ahci.0 \
    -drive if=none,id=d3,file=disks/lvm-md.img,format=raw -device nvme,serial=ghrtest,drive=d3 \
    -drive if=none,id=d4,file=disks/swap-gpt.img,format=raw -device virtio-blk-pci,drive=d4 \
    -nic "user,model=virtio-net-pci,hostfwd=tcp:127.0.0.1:$PORT-:443"
}
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
    [ -e qemu.pid ] && kill -0 "$(cat qemu.pid)" 2>/dev/null || return 0
    sleep 5
  done
  return 1
}
status() { c -f "$BASE/api/status"; }
# After the VM has stopped, print the stick's journal from GHR-DATA (mounted
# read-only) so a failed boot can be diagnosed from the CI log.
dump_journal() {
  local n=$1 start mnt
  sgdisk -p stick.img | tail -n +8 || true
  start=$(sgdisk -p stick.img | awk '$NF == "GHR-DATA" {print $2}')
  if [ -z "$start" ]; then echo "  (no GHR-DATA partition, so no journal)"; return 0; fi
  mnt=$(mktemp -d)
  if sudo mount -o ro,noload,loop,offset=$((start * 512)) stick.img "$mnt"; then
    # shellcheck disable=SC2024 # the file is meant to be ours, not root's
    sudo journalctl -D "$mnt/log/journal" --no-pager -o short-monotonic >"journal-$mode-$n.txt" 2>&1 || true
    echo "  --- failed units and ghr services (full journal in journal-$mode-$n.txt) ---"
    grep -E 'Failed|failed|ghr|sysbox|docker|error' "journal-$mode-$n.txt" | grep -v 'systemd-ssh-generator' | tail -n 150 || true
    sudo umount "$mnt"
  else
    echo "  (could not mount GHR-DATA)"
  fi
  rmdir "$mnt"
}
stop_vm() {
  kill "$(cat qemu.pid)" 2>/dev/null || true
  wait_down || true
}

log "boot 1 ($mode, $accel)"
boot 1
if ! wait_up 180; then
  bad "web page never came up"
  tail -n 80 "serial-$mode-1.log" || true
  stop_vm
  dump_journal 1
  exit 1
fi
ok "web page is up"

log "first-visitor setup"
health=$(c "$BASE/api/health")
[ "$(jq -r .password_set <<<"$health")" = false ] && ok "no password before setup" || bad "password already set: $health"
code=$(c -o /dev/null -w '%{http_code}' --data-urlencode "password=$PASSWORD" --data-urlencode "confirm=$PASSWORD" "$BASE/setup")
[ "$code" = 303 ] && ok "password set" || bad "setup returned $code"
fp1=$(echo | openssl s_client -connect "127.0.0.1:$PORT" 2>/dev/null | openssl x509 -noout -fingerprint -sha256)

log "system checks"
for _ in $(seq 60); do
  st=$(status) && [ "$(jq -r .builders.docker <<<"$st")" = active ] && break
  sleep 5
done
echo "$st" | jq . >"status-$mode-1.json"
jq -e '.stick.data_total_gb > 50' <<<"$st" >/dev/null && ok "GHR-DATA fills the stick ($(jq .stick.data_total_gb <<<"$st") GB)" || bad "data partition missing or small"
jq -e '[.disks[] | select(.boot_stick)] | length == 1' <<<"$st" >/dev/null && ok "boot stick identified" || bad "boot stick not identified"
jq -e '[.disks[] | select(.boot_stick | not)] | length == 4' <<<"$st" >/dev/null && ok "4 host disks seen" || bad "expected 4 host disks: $(jq -c '[.disks[].name]' <<<"$st")"
jq -e '.disks_ok == true' <<<"$st" >/dev/null && ok "all host disks read-only, unmounted, no holders or swap" || bad "host disk check: $(jq -c '[.disks[] | select(.ok | not) | select(.boot_stick | not)]' <<<"$st")"
jq -e '.builders.sysbox == "active"' <<<"$st" >/dev/null && ok "sysbox running" || bad "sysbox: $(jq -r .builders.sysbox <<<"$st")"
jq -e '.builders.qemu | index("aarch64")' <<<"$st" >/dev/null && ok "QEMU aarch64 binfmt registered" || bad "qemu binfmt: $(jq -c .builders.qemu <<<"$st")"

if [ -n "${GHR_TEST_URL:-}" ] && [ -n "${GHR_TEST_PAT:-}" ]; then
  log "register with $GHR_TEST_URL"
  loc=$(c -o /dev/null -w '%{redirect_url}' --data-urlencode "url=$GHR_TEST_URL" -d workers=1 -d auth_type=pat \
    --data-urlencode "pat=$GHR_TEST_PAT" --data-urlencode "labels=qemu-test-$mode" "$BASE/config")
  [[ $loc != *err=* ]] && ok "config accepted" || bad "config rejected: $loc"
  for _ in $(seq 60); do
    st=$(status)
    [ "$(jq -r '.slots[0].state // ""' <<<"$st")" = idle ] && break
    sleep 10
  done
  [ "$(jq -r '.slots[0].state // ""' <<<"$st")" = idle ] && ok "slot 1 is listening for jobs" || bad "slot 1: $(jq -c '.slots' <<<"$st")"
  c -o /dev/null -X POST "$BASE/config/remove"
else
  echo "  SKIP: registration (set GHR_TEST_URL and GHR_TEST_PAT to run it)"
fi

log "reboot"
c -o /dev/null -X POST "$BASE/actions/reboot"
sleep 20
if ! wait_up 180; then
  bad "did not come back after reboot"
  tail -n 80 "serial-$mode-2.log" || true
  stop_vm
  dump_journal 2
  exit 1
else
  ok "back after reboot"
  [ "$(c "$BASE/api/health" | jq -r .password_set)" = true ] && ok "password survived" || bad "password lost"
  rm -f "$jar"
  code=$(c -o /dev/null -w '%{http_code}' --data-urlencode "password=$PASSWORD" "$BASE/login")
  [ "$code" = 303 ] && [ "$(c -o /dev/null -w '%{http_code}' "$BASE/api/status")" = 200 ] && ok "login works" || bad "login failed"
  fp2=$(echo | openssl s_client -connect "127.0.0.1:$PORT" 2>/dev/null | openssl x509 -noout -fingerprint -sha256)
  [ -n "$fp1" ] && [ "$fp1" = "$fp2" ] && ok "same TLS certificate" || bad "certificate changed ($fp1 / $fp2)"
  st=$(status); echo "$st" | jq . >"status-$mode-2.json"
  jq -e '.disks_ok == true' <<<"$st" >/dev/null && ok "host disks still protected" || bad "host disks after reboot"
fi

log "power off"
c -o /dev/null -X POST "$BASE/actions/poweroff"
wait_down || { bad "did not power off"; kill "$(cat qemu.pid)" 2>/dev/null || true; sleep 5; }

log "host disks unchanged"
if (cd disks && sha256sum -c --quiet SHA256SUMS); then ok "all fake host disks are byte-for-byte unchanged"; else bad "a host disk changed"; fi
if sgdisk -p stick.img | grep -q GHR-DATA; then ok "GHR-DATA partition is on the stick"; else bad "no GHR-DATA on the stick"; fi

echo
if [ "$fails" -eq 0 ]; then echo "ALL CHECKS PASSED ($mode)"; else echo "$fails CHECK(S) FAILED ($mode)"; fi
[ "$fails" -eq 0 ]
