#!/bin/bash
# Create fake "host disks" that already hold data: ext4, NTFS, LVM on mdraid
# and a GPT disk with a swap partition. Needs root (loop devices, mdadm, LVM).
# usage: fake-disks.sh <dir>
set -euo pipefail
dir=$1
mkdir -p "$dir"
cd "$dir"

truncate -s 1G ext4.img
mkfs.ext4 -q -F -L HOST-EXT4 ext4.img
debugfs -w -R "write /etc/hostname precious.txt" ext4.img >/dev/null 2>&1 || true

truncate -s 1G ntfs.img
mkntfs -q -F -f -L HOST-NTFS ntfs.img >/dev/null

# LVM on a degraded RAID1, so it would auto-assemble and auto-activate on a
# careless system.
truncate -s 1G lvm-md.img
loop=$(losetup -f --show lvm-md.img)
mdadm --create /dev/md/ghrtest --run --quiet --metadata=1.2 --level=1 --raid-devices=2 "$loop" missing
md=$(readlink -f /dev/md/ghrtest)
pvcreate -q "$md"
vgcreate -q ghrtestvg "$md"
lvcreate -q -y -L 200M -n data ghrtestvg
mkfs.ext4 -q /dev/ghrtestvg/data
vgchange -q -an ghrtestvg
mdadm --stop "$md" >/dev/null
losetup -d "$loop"

# GPT disk with a swap partition and an ext4 partition. The filesystems are
# made in separate files and copied in at the partition offsets, so no loop
# partition scanning is needed.
truncate -s 1G swap-gpt.img
sgdisk -o -n 1:2048:+256M -t 1:8200 -n 2:0:0 -t 2:8300 swap-gpt.img >/dev/null
part_range() { sgdisk -i "$1" swap-gpt.img | awk -v k="$2" '$0 ~ k {print $3}'; }
for n in 1 2; do
  first=$(part_range "$n" "First sector")
  last=$(part_range "$n" "Last sector")
  truncate -s $(( (last - first + 1) * 512 )) "part$n.img"
done
mkswap -q part1.img
mkfs.ext4 -q -F part2.img
dd if=part1.img of=swap-gpt.img bs=512 seek="$(part_range 1 "First sector")" conv=notrunc,sparse status=none
dd if=part2.img of=swap-gpt.img bs=512 seek="$(part_range 2 "First sector")" conv=notrunc,sparse status=none
rm -f part1.img part2.img

sha256sum ext4.img ntfs.img lvm-md.img swap-gpt.img >SHA256SUMS
cat SHA256SUMS
