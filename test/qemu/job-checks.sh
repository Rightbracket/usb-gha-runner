#!/bin/bash
# Runs inside the ghr-job image under sysbox-runc, as the QEMU test's stand-in
# for a real job: it starts the job's own dockerd the way the entrypoint
# does, then checks what the README promises jobs can do. Each check prints
# "CHECK <name>: ok" or "CHECK <name>: FAIL <why>"; run.sh reads them.
set -uo pipefail
check() {
  local name=$1
  shift
  local out
  # </dev/null: the script itself arrives on stdin (bash -s), and a command
  # that reads stdin, like podman, would swallow the rest of it.
  if out=$("$@" 2>&1 </dev/null); then
    echo "CHECK $name: ok"
  else
    echo "CHECK $name: FAIL $(tail -n 5 <<<"$out" | tr '\n' ' ')"
  fi
}

/usr/local/bin/ghr-binfmt

mkdir -p /etc/docker
if [ -n "${GHR_REGISTRY_MIRROR:-}" ]; then
  printf '{"registry-mirrors":["%s"],"insecure-registries":["%s"]}\n' "$GHR_REGISTRY_MIRROR" "${GHR_REGISTRY_MIRROR#http://}" >/etc/docker/daemon.json
fi
dockerd >/var/log/dockerd.log 2>&1 &
for _ in $(seq 60); do docker info >/dev/null 2>&1 && break; sleep 1; done
check dockerd docker info

check hello-world sh -c 'docker run --rm hello-world | grep -q "Hello from Docker"'
check binfmt sh -c 'ls /proc/sys/fs/binfmt_misc | grep -q "^qemu-aarch64" || { grep binfmt /proc/mounts; ls /proc/sys/fs/binfmt_misc; false; }'
# arm64v8/alpine, not alpine: pulling alpine for arm64 would retag the
# local alpine as arm64 and break the native checks after this one.
check arm64-emulation sh -c 'm=$(docker run --rm --platform linux/arm64 arm64v8/alpine uname -m) && [ "$m" = aarch64 ] || { echo "uname -m: $m"; false; }'
# services: and container: rely on user-defined networks with DNS, and on
# docker exec into a long-running container.
check services-network sh -c 'docker network create ghr-svc >/dev/null &&
  docker run -d --name svc --network ghr-svc alpine sleep 120 >/dev/null &&
  docker run --rm --network ghr-svc alpine ping -c1 -W5 svc'
check container-exec sh -c 'mkdir -p /tmp/ws && echo hi >/tmp/ws/f &&
  docker run -d --name jobc -v /tmp/ws:/__w alpine sleep 120 >/dev/null &&
  [ "$(docker exec jobc cat /__w/f)" = hi ]'
check buildx sh -c 'printf "FROM alpine\nRUN uname -m >/m\n" | docker buildx build -q --load -t ghr-bx:amd64 - >/dev/null &&
  [ "$(docker run --rm ghr-bx:amd64 cat /m)" = x86_64 ]'
check buildx-arm64 sh -c 'printf "FROM alpine\nRUN uname -m >/m\n" | docker buildx build -q --platform linux/arm64 --load -t ghr-bx:arm64 - >/dev/null &&
  [ "$(docker run --rm --platform linux/arm64 ghr-bx:arm64 cat /m)" = aarch64 ]'
check buildah sh -c 'c=$(buildah from -q docker.io/library/alpine) && buildah run "$c" -- true && buildah commit -q "$c" ghr-buildah >/dev/null'
check podman podman run --rm docker.io/library/alpine true
check qemu-system qemu-system-aarch64 --version
check runner-sudo runuser -u runner -- sudo -n true
check runner-docker runuser -u runner -- docker info
# Job steps run as runner. Rootless Podman/Buildah aren't set up (Sysbox
# gives each container 65536 IDs, too few for a full subordinate ID range),
# so steps use sudo.
check runner-buildah runuser -u runner -- sudo -n buildah from -q docker.io/library/alpine
check runner-podman runuser -u runner -- sudo -n podman run --rm docker.io/library/alpine true
