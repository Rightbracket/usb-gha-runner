#!/bin/bash
# Boot sticks in states a real stick can end up in and check that the image
# fails safe:
#   1. the live medium named on the kernel command line is missing: live-boot
#      gives up instead of scanning (and mounting) the host's disks
#   2. GHR-DATA-NEW left by a first boot cut short: it is formatted again
#   3. GHR-DATA that can't be read: data-setup fails without formatting it
#   4. two copies of the image attached: data-setup refuses to pick one
#
# usage: recovery.sh <ghr-usb.img> <uefi|bios> [workdir]
set -euo pipefail
img=$(readlink -f "$1")
mode=$2
work=${3:-$(mktemp -d)}
here=$(cd "$(dirname "$0")" && pwd)
mkdir -p "$work"
cd "$work"
# shellcheck source=lib.sh
. "$here/lib.sh"

# A host disk with data on it, attached to every boot here.
truncate -s 256M host.img
mkfs.ext4 -q -F -L HOST-EXT4 host.img
host_sum=$(sha256sum <host.img)
host_disk=(-drive "if=none,id=h1,file=host.img,format=raw" -device "virtio-blk-pci,drive=h1")
host_unchanged() {
  [ "$(sha256sum <host.img)" = "$host_sum" ] && ok "$1: host disk unchanged" || bad "$1: host disk changed"
}

# add_data_part <stick> <label> adds the partition data-setup would create.
add_data_part() {
  sgdisk -e "$1" >/dev/null
  local num
  num=$(($(sgdisk -p "$1" | awk '/^ +[0-9]+ / {n=$1} END {print n+0}') + 1))
  sgdisk -n "$num:0:0" -t "$num:8300" -c "$num:$2" "$1" >/dev/null
}
# head_sum <stick> <label>: SHA-256 of the partition's first 64 MiB.
head_sum() {
  dd if="$1" bs=512 skip="$(part_start "$1" "$2")" count=$((64 * 2048)) status=none | sha256sum
}

log "1. live medium missing ($mode, $accel)"
make_stick "$img" s1.img
journal_to_serial s1.img
patch_boot s1.img 's|live-media=/dev/disk/by-uuid/[^ ]*|live-media=/dev/disk/by-uuid/0000-0000|g'
qemu_up 1 s1.img "${host_disk[@]}"
if wait_serial "serial-$mode-1.log" 'Unable to find a medium' 300; then
  ok "live-boot gave up: $(grep -am1 'Unable to find a medium' "serial-$mode-1.log" | tr -d '\r')"
else
  bad "no 'Unable to find a medium' within 5 minutes"
  tail -n 60 "serial-$mode-1.log"
fi
c -o /dev/null -f "$BASE/api/health" && bad "web page came up without the live medium" || ok "no web page"
if grep -aE 'EXT4-fs \((vd|sd|nvme)[^)]*\): mounted' "serial-$mode-1.log"; then bad "a host filesystem was mounted"; else ok "no host filesystem mounted"; fi
kill_vm
host_unchanged "live medium missing"

log "2. GHR-DATA-NEW left by an interrupted first boot"
make_stick "$img" s2.img
journal_to_serial s2.img
add_data_part s2.img GHR-DATA-NEW
# Half a mkfs: a few MiB of junk where the filesystem would start.
dd if=/dev/urandom of=s2.img bs=512 seek="$(part_start s2.img GHR-DATA-NEW)" count=8192 conv=notrunc,sparse status=none
qemu_up 2 s2.img "${host_disk[@]}"
if wait_up 180; then
  ok "web page is up"
  st=$(status || echo '{}')
  jq -e '.stick.data_total_gb > 50' <<<"$st" >/dev/null && ok "GHR-DATA mounted ($(jq .stick.data_total_gb <<<"$st") GB)" || bad "data partition after recovery: $(jq -c .stick <<<"$st")"
else
  bad "web page never came up"
  serial_errors "serial-$mode-2.log"
fi
stop_vm
[ -n "$(part_start s2.img GHR-DATA)" ] && [ -z "$(part_start s2.img GHR-DATA-NEW)" ] &&
  ok "GHR-DATA-NEW was formatted and renamed GHR-DATA" || { bad "partition table after recovery:"; sgdisk -p s2.img | tail -n +8; }
host_unchanged "GHR-DATA-NEW"

log "3. unreadable GHR-DATA"
make_stick "$img" s3.img
journal_to_serial s3.img
add_data_part s3.img GHR-DATA
dd if=/dev/urandom of=s3.img bs=512 seek="$(part_start s3.img GHR-DATA)" count=$((64 * 2048)) conv=notrunc status=none
before=$(head_sum s3.img GHR-DATA)
qemu_up 3 s3.img "${host_disk[@]}"
if wait_serial "serial-$mode-3.log" 'ghr-data: (e2fsck found errors|data-setup line)' 300; then
  ok "data-setup failed: $(grep -aEm1 'ghr-data: (e2fsck found errors|data-setup line)' "serial-$mode-3.log" | tr -d '\r')"
else
  bad "no data-setup failure on the console within 5 minutes"
  serial_errors "serial-$mode-3.log"
fi
sleep 20
c -o /dev/null -f "$BASE/api/health" && bad "web page came up without /data" || ok "no web page without /data"
stop_vm
[ "$(head_sum s3.img GHR-DATA)" = "$before" ] && ok "GHR-DATA was not formatted" || bad "GHR-DATA was written to"
[ "$(sgdisk -p s3.img | grep -c GHR-DATA)" = 1 ] && ok "no new partition" || { bad "partition table changed:"; sgdisk -p s3.img | tail -n +8; }
host_unchanged "unreadable GHR-DATA"

log "4. two copies of the image"
make_stick "$img" s4.img
journal_to_serial s4.img
cp --sparse=always s4.img s4-copy.img
qemu_up 4 s4.img "${host_disk[@]}" \
  -drive if=none,id=copy,file=s4-copy.img,format=raw -device usb-storage,bus=xhci.0,drive=copy
if wait_serial "serial-$mode-4.log" 'partitions have the boot UUID' 300; then
  ok "data-setup refused: $(grep -am1 'partitions have the boot UUID' "serial-$mode-4.log" | sed 's/.*ghr-data: //' | tr -d '\r')"
else
  bad "no duplicate-UUID refusal on the console within 5 minutes"
  serial_errors "serial-$mode-4.log"
fi
stop_vm
for s in s4.img s4-copy.img; do
  [ -z "$(part_start "$s" GHR-DATA)" ] && [ -z "$(part_start "$s" GHR-DATA-NEW)" ] && ok "$s: no data partition created" || bad "$s got a data partition"
done
host_unchanged "two copies"

finish "recovery, $mode"
