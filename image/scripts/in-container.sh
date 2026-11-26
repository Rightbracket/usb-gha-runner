#!/bin/bash
# Runs inside a privileged debian container: live-build the root filesystem,
# then lay out the stick image. Called by image/build.sh.
#   /work/lb   staged live-build config (with the ghr binary in includes.chroot)
#   /image     this repo's image/ folder (read-only)
#   /out       results
set -euo pipefail
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
apt-get install -y -qq --no-install-recommends \
  live-build debootstrap squashfs-tools xorriso cpio ca-certificates curl \
  mtools dosfstools gdisk syslinux syslinux-common isolinux \
  grub-efi-amd64-bin grub-common grub2-common zstd >/dev/null

cd /work/lb
lb config
# lb config regenerates its default list (live-boot, live-config, ...).
# live-config would add an autologin user, so install live-boot only.
echo "live-boot" >config/package-lists/live.list.chroot
lb build 2>&1 | tee /out/live-build.log | grep -E '^(P:|E:|W:)' || true
test -f binary/live/filesystem.squashfs || { echo "live-build did not produce a squashfs; see out/live-build.log" >&2; exit 1; }

live=/out/live
mkdir -p "$live"
cp "$(ls binary/live/vmlinuz* | head -n1)" "$live/vmlinuz"
cp "$(ls binary/live/initrd.img* | head -n1)" "$live/initrd.img"
cp binary/live/filesystem.squashfs "$live/filesystem.squashfs"
cp chroot/usr/share/ghr/version "$live/VERSION"
chroot chroot dpkg-query -W -f '${Package} ${Version}\n' >/out/packages.txt

/image/scripts/mkimage.sh "$live" /out/ghr-images.tar.zst /out/ghr-usb.img "$(cat "$live/VERSION")"
