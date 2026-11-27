#!/bin/bash
# Boot ghr-usb.img in QEMU with fake host disks attached and check that:
#   1. the data partition is created on the stick only
#   2. every host disk is read-only, unmounted, with no LVM/md/swap active,
#      including a disk hot-plugged after boot
#   3. SSH works while the toggle is on and is refused once it is off
#   4. the firewall is loaded, and a container can reach only the mirror
#   5. the job image runs under Sysbox with its own Docker (hello-world,
#      arm64 emulation, networks, buildx, Buildah, Podman)
#   6. the agent replaces a mirror running an old image and keeps its cache
#   7. (optional, with GHR_TEST_URL + GHR_TEST_PAT) a slot registers with
#      GitHub and runs a dispatched workflow's jobs one after the other
#   8. after a reboot the password, certificate and data are still there
#   9. the host disks are byte-for-byte unchanged afterwards
#
# usage: run.sh <ghr-usb.img> <uefi|bios> [workdir]
set -euo pipefail
img=$(readlink -f "$1")
mode=$2
work=${3:-$(mktemp -d)}
here=$(cd "$(dirname "$0")" && pwd)
mkdir -p "$work"
cd "$work"
# shellcheck source=lib.sh
. "$here/lib.sh"
PASSWORD="qemu-test-password"

log "fake host disks"
sudo "$here/fake-disks.sh" "$work/disks"
sudo chown -R "$(id -u)" "$work/disks"
# A fifth disk, plugged in only after boot.
truncate -s 256M disks/hotplug.img
mkfs.ext4 -q -F -L HOST-HOTPLUG disks/hotplug.img
(cd disks && sha256sum hotplug.img >>SHA256SUMS)

log "stick: copy of the image, grown to 64 GB (sparse)"
make_stick "$img" stick.img
journal_to_serial stick.img

boot() {
  qemu_up "$1" stick.img \
    -drive if=none,id=d1,file=disks/ext4.img,format=raw -device virtio-blk-pci,drive=d1 \
    -device ahci,id=ahci -drive if=none,id=d2,file=disks/ntfs.img,format=raw -device ide-hd,drive=d2,bus=ahci.0 \
    -drive if=none,id=d3,file=disks/lvm-md.img,format=raw -device nvme,serial=ghrtest,drive=d3 \
    -drive if=none,id=d4,file=disks/swap-gpt.img,format=raw -device virtio-blk-pci,drive=d4
}

ssh-keygen -q -t ed25519 -N '' -C ghr-qemu-test -f "$work/id_test"
g() {
  ssh -i "$work/id_test" -p "$SSH_PORT" -o BatchMode=yes -o ConnectTimeout=10 \
    -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR ghr@127.0.0.1 "$@"
}
ssh_toggle() { # on|off
  local args=(--data-urlencode "authorized_keys=$(cat "$work/id_test.pub")")
  [ "$1" = on ] && args+=(-d enabled=on)
  c -o /dev/null -w '%{redirect_url}' "${args[@]}" "$BASE/config/ssh"
}

log "boot 1 ($mode, $accel)"
boot 1
if ! wait_up 180; then
  bad "web page never came up"
  echo "  --- guest log: ghr, failures and errors ---"
  serial_errors "serial-$mode-1.log"
  stop_vm
  dump_journal stick.img 1
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
boot1=$(jq -r .machine.boot_id <<<"$st")
jq -e '.stick.data_total_gb > 50' <<<"$st" >/dev/null && ok "GHR-DATA fills the stick ($(jq .stick.data_total_gb <<<"$st") GB)" || bad "data partition missing or small"
jq -e '[.disks[] | select(.boot_stick)] | length == 1' <<<"$st" >/dev/null && ok "boot stick identified" || bad "boot stick not identified"
jq -e '[.disks[] | select(.boot_stick | not)] | length == 4' <<<"$st" >/dev/null && ok "4 host disks seen" || bad "expected 4 host disks: $(jq -c '[.disks[].name]' <<<"$st")"
jq -e '.disks_ok == true' <<<"$st" >/dev/null && ok "all host disks read-only, unmounted, no holders or swap" || bad "host disk check: $(jq -c '[.disks[] | select(.ok | not) | select(.boot_stick | not)]' <<<"$st")"
jq -e '.builders.sysbox == "active"' <<<"$st" >/dev/null && ok "sysbox running" || bad "sysbox: $(jq -r .builders.sysbox <<<"$st")"
jq -e '.builders.qemu | index("aarch64")' <<<"$st" >/dev/null && ok "QEMU aarch64 binfmt registered" || bad "qemu binfmt: $(jq -c .builders.qemu <<<"$st")"

log "hot-plugged disk"
monitor "drive_add 0 if=none,id=hp,file=$work/disks/hotplug.img,format=raw" >/dev/null
monitor "device_add usb-storage,bus=xhci.0,drive=hp,id=hpdev,serial=GHRHOTPLUG" >/dev/null
for _ in $(seq 24); do
  st=$(status) && jq -e '[.disks[] | select(.boot_stick | not)] | length == 5' <<<"$st" >/dev/null && break
  sleep 5
done
if jq -e '[.disks[] | select(.boot_stick | not)] | length == 5' <<<"$st" >/dev/null; then
  ok "hot-plugged disk seen"
  jq -e '[.disks[] | select(.boot_stick | not) | select(.read_only and .ok)] | length == 5' <<<"$st" >/dev/null &&
    ok "hot-plugged disk is read-only like the others" || bad "after hot-plug: $(jq -c '[.disks[] | select(.boot_stick | not)]' <<<"$st")"
else
  bad "hot-plugged disk never appeared: $(jq -c '[.disks[].name]' <<<"$st")"
fi

log "SSH"
loc=$(ssh_toggle on)
[[ $loc != *err=* ]] && ok "SSH turned on" || bad "SSH toggle: $loc"
for _ in $(seq 12); do g true 2>/dev/null && break; sleep 5; done
if g true; then
  ok "SSH login as ghr with the key"
  [ "$(g sudo -n id -u || true)" = 0 ] && ok "ghr has sudo" || bad "ghr has no sudo"
  [ "$(g 'lsblk -ndo SERIAL,RO' | awk '$1 == "GHRHOTPLUG" {print $2}')" = 1 ] && ok "hot-plugged disk is ro in the kernel" || bad "hot-plugged disk: $(g lsblk -ndo NAME,SERIAL,RO)"
  ssh_ok=1
else
  bad "SSH login failed with the toggle on"
  ssh_ok=0
fi

if [ "$ssh_ok" = 1 ]; then
  log "firewall"
  [ "$(g systemctl is-active nftables || true)" = active ] && ok "nftables is active" || bad "nftables: $(g systemctl status nftables --no-pager | tail -n 5)"
  g sudo nft list table inet ghr | grep -q tcp_open && ok "table inet ghr is loaded" || bad "table inet ghr missing"
  gw=$(g "docker network inspect bridge --format '{{(index .IPAM.Config 0).Gateway}}'" || true)
  job=$(g "docker images --format '{{.Repository}}:{{.Tag}}' ghr-job" | head -n 1 || true)
  mimg=$(g "docker inspect -f '{{.Config.Image}}' ghr-mirror" || true)
  echo "  docker0 gateway $gw, job image $job, mirror image $mimg"
  # Control: the ports answer from the host itself, so a timeout from a
  # container is the firewall and not a closed port.
  [ "$(g curl -sk -o /dev/null -w '%{http_code}' "https://$gw/api/health")" = 200 ] && ok "web page answers on $gw from the host" || bad "web page not listening on $gw"
  probe() { # <url>: curl's exit code from a plain container on docker0
    g "docker run --rm --entrypoint curl $job -sk -o /dev/null --max-time 5 $1; echo rc=\$?" | sed -n 's/^rc=//p'
  }
  [ "$(probe "http://$gw:5000/v2/")" = 0 ] && ok "container reaches the mirror on :5000" || bad "container can't reach the mirror"
  rc=$(probe "https://$gw/api/health"); [ "$rc" = 28 ] && ok "container times out on the web page" || bad "container got curl exit $rc from the web page (want 28, timeout)"
  rc=$(probe "telnet://$gw:22"); [ "$rc" = 28 ] && ok "container times out on SSH" || bad "container got curl exit $rc from SSH (want 28, timeout)"

  log "job image under sysbox-runc"
  out=$(g "docker run --rm -i --runtime=sysbox-runc -e GHR_REGISTRY_MIRROR=http://$gw:5000 --entrypoint bash $job -s" <"$here/job-checks.sh" 2>&1) || true
  grep '^CHECK' <<<"$out" >"job-checks-$mode.txt" || true
  for name in dockerd hello-world arm64-emulation services-network container-exec buildx buildx-arm64 buildah podman qemu-system runner-sudo runner-docker; do
    line=$(grep "^CHECK $name: " <<<"$out" || true)
    case $line in
      *": ok") ok "job: $name" ;;
      "") bad "job: $name did not run" ;;
      *) bad "job: ${line#CHECK }" ;;
    esac
  done
  if grep -q 'FAIL\|did not run' <<<"$(grep '^CHECK' <<<"$out")" || ! grep -q '^CHECK' <<<"$out"; then
    echo "  --- job container output ---"; tail -n 40 <<<"$out"
  fi

  log "mirror upgrade"
  blobs() { g "sudo find /data/registry-mirror -type f -path '*blobs*' | wc -l"; }
  before=$(blobs || echo 0)
  [ "$before" -gt 0 ] && ok "mirror cached $before blob files from the job's pulls" || bad "mirror cache is empty after the job pulled images"
  want=$(g "docker image inspect -f '{{.Id}}' $mimg" || true)
  # Stand-in for an older release: the same registry under another image ID,
  # as an upgrade from a stick with an older REGISTRY_IMAGE would leave it.
  g "printf 'FROM %s\nLABEL ghr.test=old-release\n' '$mimg' | docker build -q -t ghr-test/old-registry - >/dev/null" || bad "could not build the stand-in old registry image"
  g "docker rm -f ghr-mirror >/dev/null && docker run -d --restart=always --name ghr-mirror --label ghr.keep=true \
    -p $gw:5000:5000 -v /data/registry-mirror:/var/lib/registry -e REGISTRY_PROXY_REMOTEURL=https://registry-1.docker.io \
    ghr-test/old-registry >/dev/null" || bad "could not start the old mirror"
  old=$(g "docker inspect -f '{{.Image}}' ghr-mirror" || true)
  [ -n "$old" ] && [ "$old" != "$want" ] && ok "mirror now runs the old image" || bad "old mirror not set up ($old)"
  g sudo systemctl restart ghr-agent || bad "could not restart ghr-agent"
  for _ in $(seq 36); do
    [ "$(g docker inspect -f "'{{.State.Running}} {{.Image}}'" ghr-mirror 2>/dev/null)" = "true $want" ] && break
    sleep 5
  done
  [ "$(g docker inspect -f "'{{.State.Running}} {{.Image}}'" ghr-mirror)" = "true $want" ] && ok "agent replaced the mirror with $mimg" || bad "mirror after agent restart: $(g docker inspect -f "'{{.State.Running}} {{.Image}}'" ghr-mirror)"
  after=$(blobs || echo 0)
  [ "$after" -ge "$before" ] && ok "mirror kept its cache ($after blob files)" || bad "mirror cache shrank from $before to $after"
  g docker rmi ghr-test/old-registry >/dev/null || true

  log "SSH off"
  loc=$(ssh_toggle off)
  [[ $loc != *err=* ]] && ok "SSH turned off" || bad "SSH toggle off: $loc"
  if g true 2>/dev/null; then bad "SSH login still works with the toggle off"; else ok "SSH refused with the toggle off"; fi
fi

if [ -n "${GHR_TEST_URL:-}" ] && [ -n "${GHR_TEST_PAT:-}" ]; then
  label="qemu-test-$mode-${GITHUB_RUN_ID:-local}-$RANDOM"
  log "register with $GHR_TEST_URL as $label"
  loc=$(c -o /dev/null -w '%{redirect_url}' --data-urlencode "url=$GHR_TEST_URL" -d workers=1 -d auth_type=pat \
    --data-urlencode "pat=$GHR_TEST_PAT" --data-urlencode "labels=$label" "$BASE/config")
  [[ $loc != *err=* ]] && ok "config accepted" || bad "config rejected: $loc"
  for _ in $(seq 60); do
    st=$(status)
    [ "$(jq -r '.slots[0].state // ""' <<<"$st")" = idle ] && break
    sleep 10
  done
  [ "$(jq -r '.slots[0].state // ""' <<<"$st")" = idle ] && ok "slot 1 is listening for jobs" || bad "slot 1: $(jq -c '.slots' <<<"$st")"
  "$here/dispatch.sh" "$GHR_TEST_URL" "$label" && ok "dispatched workflow ran both jobs on this runner" || bad "dispatched workflow failed"
  c -o /dev/null -X POST "$BASE/config/remove"
else
  echo "  SKIP: registration and a real job (set GHR_TEST_URL and GHR_TEST_PAT to run them)"
fi

log "reboot"
c -o /dev/null -X POST "$BASE/actions/reboot"
# Wait for the old system to go away, so we don't check it instead.
for _ in $(seq 60); do c -o /dev/null -f "$BASE/api/health" || break; sleep 2; done
if ! wait_up 180; then
  bad "did not come back after reboot"
  serial_errors "serial-$mode-1.log"
  stop_vm
  dump_journal stick.img 2
  exit 1
else
  ok "back after reboot"
  [ "$(c "$BASE/api/health" | jq -r .password_set)" = true ] && ok "password survived" || bad "password lost"
  rm -f "$jar"
  login=$(c -o /dev/null -w '%{http_code} %{redirect_url}' --data-urlencode "password=$PASSWORD" "$BASE/login")
  # /api/status needs ghr-agent, which may still be loading images; a 401
  # means the login didn't take, anything else is the agent starting.
  for _ in $(seq 60); do
    scode=$(c -o /dev/null -w '%{http_code}' "$BASE/api/status")
    [ "$scode" = 200 ] || [ "$scode" = 401 ] && break
    sleep 5
  done
  if [ "${login%% *}" = 303 ] && [[ $login != *err=* ]] && [ "$scode" = 200 ]; then ok "login works"; else
    bad "login failed (login: $login; status: $scode)"
    echo "  cookie jar:"; grep -v '^# ' "$jar" | sed 's/\t[^\t]*$/\t<value>/' || true
    grep -aE 'ghr-web|ghr web|web\[' "serial-$mode-1.log" | tail -n 40 || true
  fi
  fp2=$(echo | openssl s_client -connect "127.0.0.1:$PORT" 2>/dev/null | openssl x509 -noout -fingerprint -sha256)
  [ -n "$fp1" ] && [ "$fp1" = "$fp2" ] && ok "same TLS certificate" || bad "certificate changed ($fp1 / $fp2)"
  st=$(status || true); echo "$st" | jq . >"status-$mode-2.json" || true
  boot2=$(jq -r .machine.boot_id <<<"$st" 2>/dev/null || true)
  [ -n "$boot1" ] && [ -n "$boot2" ] && [ "$boot1" != "$boot2" ] && ok "it really rebooted (new boot id)" || bad "boot id unchanged or missing ($boot1 / $boot2)"
  jq -e '.disks_ok == true' <<<"$st" >/dev/null && ok "host disks still protected (hot-plugged one included)" || bad "host disks after reboot"
  if g true 2>/dev/null; then bad "SSH came back on after reboot"; else ok "SSH still off after reboot"; fi
fi

log "power off"
c -o /dev/null -X POST "$BASE/actions/poweroff"
wait_down || { bad "did not power off"; kill_vm; }

log "host disks unchanged"
if (cd disks && sha256sum -c --quiet SHA256SUMS); then ok "all fake host disks (hot-plugged one included) are byte-for-byte unchanged"; else bad "a host disk changed"; fi
[ -n "$(part_start stick.img GHR-DATA)" ] && ok "GHR-DATA partition is on the stick" || bad "no GHR-DATA on the stick"
[ -z "$(part_start stick.img GHR-DATA-NEW)" ] && ok "no unfinished GHR-DATA-NEW left" || bad "GHR-DATA-NEW was never renamed"

finish "$mode"
