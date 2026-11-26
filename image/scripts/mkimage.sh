#!/bin/bash
# Lay out ghr-usb.img without loop devices:
#   GPT, partition 1 = GHR-BOOT (FAT32 EFI system partition, legacy-BIOS bootable)
#   UEFI: GRUB standalone EFI/BOOT/BOOTX64.EFI
#   BIOS: syslinux gptmbr.bin in the MBR + syslinux on the FAT partition
# The GHR-DATA partition is created on the stick itself at first boot.
#
# usage: mkimage.sh <dir with vmlinuz, initrd.img, filesystem.squashfs> <images.tar.zst or ""> <out.img> <version>
set -euo pipefail
src=$1 images=$2 out=$3 version=$4
FAT_MB=${FAT_MB:-4096}
export MTOOLS_SKIP_CHECK=1

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

# Random FAT volume ID per build. The kernel gets it as live-media=, so
# live-boot goes straight to this partition instead of probing other disks.
volid=$(od -An -N4 -tx1 /dev/urandom | tr -d ' \n' | tr 'a-f' 'A-F')
uuid="${volid:0:4}-${volid:4:4}"
cmdline="boot=live live-media=/dev/disk/by-uuid/$uuid live-media-path=/live live-media-timeout=30 nopersistence noeject noprompt noresume nohibernate systemd.gpt_auto=0 systemd.swap=0 ghr.boot-uuid=$uuid console=ttyS0,115200n8 console=tty0"
title="USB GitHub Actions runner $version"

fat=$tmp/fat.img
mkfs.vfat -F 32 -n GHR-BOOT -i "$volid" -C "$fat" $((FAT_MB * 1024)) >/dev/null
mmd -i "$fat" ::/live ::/boot ::/boot/grub ::/boot/syslinux ::/EFI ::/EFI/BOOT ::/ghr
mcopy -i "$fat" "$src/vmlinuz" ::/live/vmlinuz
mcopy -i "$fat" "$src/initrd.img" ::/live/initrd.img
mcopy -i "$fat" "$src/filesystem.squashfs" ::/live/filesystem.squashfs
if [ -n "$images" ]; then
  mcopy -i "$fat" "$images" ::/live/ghr-images.tar.zst
fi
printf '%s\n' "$version" >"$tmp/VERSION"
mcopy -i "$fat" "$tmp/VERSION" ::/live/VERSION

# --- GRUB (UEFI) ---
cat >"$tmp/grub.cfg" <<CFG
set default=0
set timeout=5
insmod all_video
menuentry "$title" {
  linux /live/vmlinuz $cmdline quiet
  initrd /live/initrd.img
}
menuentry "$title (verbose boot)" {
  linux /live/vmlinuz $cmdline
  initrd /live/initrd.img
}
menuentry "$title (safe graphics)" {
  linux /live/vmlinuz $cmdline nomodeset
  initrd /live/initrd.img
}
CFG
mcopy -i "$fat" "$tmp/grub.cfg" ::/boot/grub/grub.cfg
# The EFI binary finds its own partition from the path it was loaded from,
# so it doesn't search other disks. A UUID search is only the fallback.
cat >"$tmp/embedded.cfg" <<CFG
regexp --set 1:bootdev '^\\((.*)\\)' "\$cmdpath"
if [ -n "\$bootdev" ]; then set root="\$bootdev"; else search --no-floppy --fs-uuid --set=root $uuid; fi
set prefix=(\$root)/boot/grub
configfile (\$root)/boot/grub/grub.cfg
CFG
grub-mkstandalone -O x86_64-efi -o "$tmp/BOOTX64.EFI" \
  --modules="part_gpt part_msdos fat regexp search search_fs_uuid test linux normal configfile echo all_video efi_gop efi_uga" \
  --locales="" --fonts="" --themes="" \
  "boot/grub/grub.cfg=$tmp/embedded.cfg"
mcopy -i "$fat" "$tmp/BOOTX64.EFI" ::/EFI/BOOT/BOOTX64.EFI

# --- syslinux (legacy BIOS) ---
cat >"$tmp/syslinux.cfg" <<CFG
UI menu.c32
PROMPT 0
TIMEOUT 50
MENU TITLE $title
DEFAULT ghr
LABEL ghr
  MENU LABEL $title
  LINUX /live/vmlinuz
  INITRD /live/initrd.img
  APPEND $cmdline quiet
LABEL verbose
  MENU LABEL Verbose boot
  LINUX /live/vmlinuz
  INITRD /live/initrd.img
  APPEND $cmdline
LABEL safe
  MENU LABEL Safe graphics
  LINUX /live/vmlinuz
  INITRD /live/initrd.img
  APPEND $cmdline nomodeset
CFG
mcopy -i "$fat" "$tmp/syslinux.cfg" ::/boot/syslinux/syslinux.cfg
mcopy -i "$fat" /usr/lib/syslinux/modules/bios/menu.c32 /usr/lib/syslinux/modules/bios/libutil.c32 ::/boot/syslinux/
syslinux --install --directory /boot/syslinux "$fat"

# --- user-editable ghr/ folder ---
here=$(dirname "$0")
for f in README.txt runner.env.example; do
  [ -f "$here/../stick/$f" ] && mcopy -i "$fat" "$here/../stick/$f" "::/ghr/$f"
done

# --- disk ---
total_mb=$((1 + FAT_MB + 1))
rm -f "$out"
truncate -s "${total_mb}M" "$out"
sgdisk -o "$out" >/dev/null
sgdisk -n "1:2048:+${FAT_MB}M" -t 1:EF00 -c 1:GHR-BOOT -A 1:set:2 "$out" >/dev/null
dd if="$fat" of="$out" bs=1M seek=1 conv=notrunc,sparse status=none
dd if=/usr/lib/syslinux/mbr/gptmbr.bin of="$out" bs=440 count=1 conv=notrunc status=none
# Some BIOSes only boot a disk whose protective MBR entry is marked active.
printf '\x80' | dd of="$out" bs=1 seek=446 count=1 conv=notrunc status=none

sgdisk -p "$out"
echo "boot UUID $uuid"
echo "$uuid" >"$(dirname "$out")/boot-uuid.txt"
