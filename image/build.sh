#!/bin/bash
# Build out/ghr-usb.img. Needs Linux with Docker (privileged containers),
# Go 1.24+, curl, jq and zstd. Network access to Debian, Docker, Ubuntu and
# GitHub mirrors is required. Set GITHUB_TOKEN to avoid API rate limits.
set -euo pipefail
root=$(cd "$(dirname "$0")/.." && pwd)
out=${OUT:-$root/out}
# shellcheck source=/dev/null
. "$root/versions.env"
VERSION=${VERSION:-$(git -C "$root" describe --tags --always --dirty 2>/dev/null || echo dev)}
mkdir -p "$out"
log() { printf '\n==> %s\n' "$*"; }
gh_api() {
  local auth=()
  [ -n "${GITHUB_TOKEN:-}" ] && auth=(-H "Authorization: Bearer $GITHUB_TOKEN")
  curl -fsSL "${auth[@]}" -H "Accept: application/vnd.github+json" "https://api.github.com/$1"
}

log "ghr binary"
(cd "$root/ghr" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w -X github.com/Rightbracket/usb-gha-runner/ghr/internal/agent.MirrorImage=${REGISTRY_IMAGE%@*}" -o "$out/ghr" ./cmd/ghr)

log "resolve versions"
if [ "${RUNNER_VERSION}" = "latest" ] || [ -z "${RUNNER_VERSION}" ]; then
  rel=$(gh_api repos/actions/runner/releases/latest)
  RUNNER_VERSION=$(jq -r .tag_name <<<"$rel" | sed 's/^v//')
  RUNNER_SHA256=$(jq -r .body <<<"$rel" | sed -n 's/.*<!-- BEGIN SHA linux-x64 -->\([0-9a-f]\{64\}\)<!-- END SHA linux-x64 -->.*/\1/p' | head -n1)
fi
[ -n "$RUNNER_SHA256" ] || { echo "no SHA-256 for actions/runner $RUNNER_VERSION" >&2; exit 1; }
sysbox_url=$(gh_api "repos/nestybox/sysbox/releases/tags/v${SYSBOX_VERSION}" |
  jq -r '.assets[].browser_download_url | select(test("linux_amd64\\.deb$"))' | head -n1 || true)
[ -n "$sysbox_url" ] || sysbox_url="https://github.com/nestybox/sysbox/releases/download/v${SYSBOX_VERSION}/sysbox-ce_${SYSBOX_VERSION}.linux_amd64.deb"
echo "actions/runner $RUNNER_VERSION ($RUNNER_SHA256)"
echo "sysbox $SYSBOX_VERSION from $sysbox_url"

log "check pinned inputs"
# Sysbox runs as root on the host, so its .deb must match the pinned hash.
# The base images must be pinned by digest. On a mismatch or a missing pin
# this prints what was fetched, to review and copy into versions.env.
curl -fsSL -o "$out/sysbox.deb" "$sysbox_url"
sysbox_sha=$(sha256sum "$out/sysbox.deb" | cut -d' ' -f1)
pins_ok=true
if [ "$sysbox_sha" != "${SYSBOX_SHA256:-}" ]; then
  echo "sysbox .deb SHA-256 is $sysbox_sha, versions.env has '${SYSBOX_SHA256:-}'" >&2
  pins_ok=false
fi
for v in BUILDER_IMAGE JOB_BASE_IMAGE REGISTRY_IMAGE; do
  ref=${!v}
  if [[ $ref != *@sha256:* ]]; then
    docker pull -q "$ref" >/dev/null
    echo "$v=$ref is not pinned by digest; it currently resolves to $(docker image inspect -f '{{index .RepoDigests 0}}' "$ref")" >&2
    pins_ok=false
  fi
done
$pins_ok || exit 1

log "job image"
docker build --pull -t "ghr-job:$VERSION" \
  --build-arg JOB_BASE_IMAGE="$JOB_BASE_IMAGE" \
  --build-arg RUNNER_VERSION="$RUNNER_VERSION" \
  --build-arg RUNNER_SHA256="$RUNNER_SHA256" \
  --build-arg GHR_VERSION="$VERSION" \
  "$root/image/job"
# Pulled by digest, saved under its plain tag, which is what ghr-agent runs.
docker pull -q "$REGISTRY_IMAGE"
docker tag "$REGISTRY_IMAGE" "${REGISTRY_IMAGE%@*}"
docker save "ghr-job:$VERSION" "${REGISTRY_IMAGE%@*}" | zstd -T0 -10 -q -f -o "$out/ghr-images.tar.zst"
ls -lh "$out/ghr-images.tar.zst"

log "stage live-build config"
work=$(mktemp -d)
cleanup() { docker run --rm -v "$work:/work" "$BUILDER_IMAGE" rm -rf /work/lb >/dev/null 2>&1 || true; rm -rf "$work"; }
trap cleanup EXIT
cp -a "$root/image/lb" "$work/lb"
inc="$work/lb/config/includes.chroot"
install -D -m 0755 "$out/ghr" "$inc/usr/local/bin/ghr"
install -d "$inc/usr/share/ghr" "$inc/var/cache/ghr"
echo "$VERSION" >"$inc/usr/share/ghr/version"
mv "$out/sysbox.deb" "$inc/var/cache/ghr/sysbox.deb"
{
  echo "version=$VERSION"
  echo "debian=$DEBIAN_SUITE"
  echo "builder=$BUILDER_IMAGE"
  echo "actions_runner=$RUNNER_VERSION sha256=$RUNNER_SHA256"
  echo "sysbox=$SYSBOX_VERSION sha256=$sysbox_sha"
  echo "job_base=$JOB_BASE_IMAGE"
  echo "registry=$REGISTRY_IMAGE"
} | tee "$out/manifest.txt" >"$inc/usr/share/ghr/manifest.txt"

log "live-build and stick layout (this takes a while)"
docker run --rm --privileged \
  -e DEBIAN_SUITE="$DEBIAN_SUITE" -e FAT_MB="$FAT_MB" \
  -v "$work:/work" -v "$root/image:/image:ro" -v "$out:/out" \
  "$BUILDER_IMAGE" /image/scripts/in-container.sh
# Files from the container are root-owned.
docker run --rm -v "$out:/out" "$BUILDER_IMAGE" chown -R "$(id -u):$(id -g)" /out

log "compress"
zstd -T0 -10 -q -f "$out/ghr-usb.img" -o "$out/ghr-usb.img.zst"
(cd "$out" && sha256sum ghr-usb.img ghr-usb.img.zst >SHA256SUMS)
ls -lh "$out/ghr-usb.img" "$out/ghr-usb.img.zst"
