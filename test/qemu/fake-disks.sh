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

truncate -s 1G swap-gpt.img
sgdisk -q -o -n 1:0:+256M -t 1:8200 -n 2:0:0 -t 2:8300 swap-gpt.img
loop=$(losetup -f --show -P swap-gpt.img)
mkswap -q "${loop}p1"
mkfs.ext4 -q -F "${loop}p2"
losetup -d "$loop"

sha256sum ext4.img ntfs.img lvm-md.img swap-gpt.img >SHA256SUMS
cat SHA256SUMS
