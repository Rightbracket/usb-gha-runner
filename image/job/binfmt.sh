#!/bin/bash
# Since Linux 6.7, binfmt_misc is per user namespace, so a Sysbox container
# starts with its own empty table and the host's QEMU handlers don't reach
# it. Register the image's static QEMU (flag F: the kernel opens it now,
# so nested containers without QEMU can use it too). Failures only warn:
# native jobs still work without emulation.
set -u
d=/proc/sys/fs/binfmt_misc
[ -e "$d/register" ] || mount -t binfmt_misc binfmt_misc "$d" 2>/dev/null || true
if [ ! -w "$d/register" ]; then
  echo "ghr-binfmt: $d/register is not writable, no foreign-arch emulation" >&2
  exit 0
fi
shopt -s nullglob
for f in /usr/lib/binfmt.d/qemu-*.conf; do
  line=$(grep -v '^[#;]' "$f" | grep -m1 .) || continue
  name=$(cut -d: -f2 <<<"$line")
  [ -e "$d/$name" ] && continue
  echo "$line" >"$d/register" 2>/dev/null || echo "ghr-binfmt: could not register $name" >&2
done
exit 0
