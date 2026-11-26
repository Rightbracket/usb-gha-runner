# USB GitHub Actions runner

A Debian live USB stick that turns any x86_64 PC into a self-hosted GitHub
Actions runner. Plug it in, boot from it, open the web page it shows on
screen, and connect it to a repository or organization. Everything it keeps
lives on the stick. The PC's own disks are set read-only at the kernel level
and are never mounted.

- Each job runs in its own throwaway [Sysbox](https://github.com/nestybox/sysbox)
  container with its own Docker daemon, so `services:`, `container:`, buildx,
  Buildah and Podman all work, and the job has root and passwordless sudo
  inside that container only.
- QEMU user-mode emulation is registered for arm64, arm/v7, riscv64, ppc64le
  and s390x, and `qemu-system-*` is available inside jobs.
- Several jobs at once: `workers` is 1 to 32, or `auto` = min(CPU threads ÷ 2, RAM GB ÷ 4).
- The network comes from DHCP on every wired port.

## Using it

1. Download `ghr-usb.img.zst` from a build (Actions → build → artifacts) and
   write it to a stick of **64 GB or more** (a USB SSD or high-endurance
   stick lasts much longer):
   - Linux/macOS: `zstd -dc ghr-usb.img.zst | sudo dd of=/dev/sdX bs=4M conv=fsync status=progress`
   - Windows: decompress with 7-Zip or zstd, then write with Rufus (DD mode) or balenaEtcher.
2. Optional: before first boot, open the **GHR-BOOT** drive on any computer
   and put files in its `ghr/` folder: `admin-password` (skips the first-visit
   page) and `runner.env` (see `ghr/runner.env.example`).
3. Boot the PC from the stick (UEFI or legacy BIOS; turn Secure Boot off).
   On first boot it creates the `GHR-DATA` partition in the rest of the stick.
4. The screen shows `https://ghr-XXXX.local/`, the IP address and the
   certificate fingerprint. Open it, set the admin password (the first visitor
   sets it), then enter the repo or org URL and a credential.

### Credential

Every job gets a one-time (just-in-time) runner config, which needs one of:

- **GitHub App** (recommended): create an App with *Administration: read and
  write* (repository runners) or *Self-hosted runners: read and write*
  (organization runners), install it, and give the stick the App ID, the
  installation ID and the private key.
- **Fine-grained personal access token** with the same permission.

Self-hosted runners run whatever code a workflow contains. Use them with
private repositories, or with an organization runner group limited to
trusted repositories.

## What's on the stick

| # | Partition | Filesystem | Size | Holds |
|---|-----------|------------|------|-------|
| 1 | `GHR-BOOT` | FAT32 (EFI system partition) | 4 GB | GRUB (UEFI), syslinux (BIOS), kernel, initrd, read-only `filesystem.squashfs`, job images, `ghr/` settings folder |
| 2 | `GHR-DATA` | ext4 | rest of the stick | config, Docker/Podman/Sysbox storage, registry mirror, TLS cert, logs |

The system runs from the read-only squashfs with changes in RAM, so every
boot starts clean. Only `/data` persists. To upgrade, write a newer image's
first partition, or re-flash and lose `/data`.

### Host disk protection

1. A udev rule in the initramfs runs `blockdev --setro` on every disk as it
   appears. After boot, `ghr-data.service` makes only the boot stick writable
   again, and a second udev rule makes any later (hot-plugged) disk read-only.
2. live-boot is told the boot partition's UUID (`live-media=`), so it doesn't
   probe other disks, and its persistence scan is off.
3. LVM, mdadm, multipath, ZFS, bcache, cryptsetup, udisks2, os-prober and
   SMART tools aren't installed; btrfs scanning, swap, hibernation, the GPT
   auto-generator and fstrim are disabled.
4. Jobs run in Sysbox containers, which can't see host block devices.

The status page lists every disk with its read-only flag and mounts, and the
QEMU tests check that fake host disks with ext4, NTFS, LVM on mdraid and swap
are byte-for-byte unchanged after boot, reboot and shutdown.

## Repository layout

| Path | What |
|------|------|
| `ghr/` | Go source of the `ghr` binary: `ghr agent` (root: job supervisor and a small fixed-action socket), `ghr web` (unprivileged HTTPS page), `ghr import`, `ghr prune` |
| `image/lb/` | live-build config: package lists, hook, systemd units, udev rules, scripts |
| `image/job/` | `ghr-job` image that each job runs in |
| `image/scripts/` | live-build wrapper and the stick layout (`mkimage.sh`) |
| `image/build.sh` | full build: binary, job image, live-build, stick image |
| `test/qemu/` | boot tests with fake host disks |
| `versions.env` | pinned inputs (runner, Sysbox, base images) |

## Building

Needs Linux with Docker (privileged containers), Go 1.24+, curl, jq and zstd.

```
make test          # Go unit tests
make image         # out/ghr-usb.img and out/ghr-usb.img.zst
make qemu-uefi     # boot tests (needs sudo, qemu-system-x86, ovmf, ntfs-3g, mdadm, lvm2)
```

CI (`.github/workflows/build.yml`) runs all three. To also test registration,
set the repository variable `GHR_TEST_URL` (a test repo or org URL) and the
secret `GHR_TEST_PAT`.
