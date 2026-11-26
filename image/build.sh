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
(cd "$root/ghr" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o "$out/ghr" ./cmd/ghr)

log "resolve versions"
if [ "${RUNNER_VERSION}" = "latest" ] || [ -z "${RUNNER_VERSION}" ]; then
  rel=$(gh_api repos/actions/runner/releases/latest)
  RUNNER_VERSION=$(jq -r .tag_name <<<"$rel" | sed 's/^v//')
  RUNNER_SHA256=$(jq -r .body <<<"$rel" | sed -n 's/.*<!-- BEGIN SHA linux-x64 -->\([0-9a-f]\{64\}\)<!-- END SHA linux-x64 -->.*/\1/p' | head -n1)
fi
[ -n "$RUNNER_SHA256" ] || { echo "no SHA-256 for actions/runner $RUNNER_VERSION" >&2; exit 1; }
sysbox_url=$(gh_api "repos/nestybox/sysbox/releases/tags/v${SYSBOX_VERSION}" |
  jq -r '.assets[].browser_download_url | select(test("linux_amd64\\.deb$"))' | head -n1 || true)
[ -n "$sysbox_url" ] || sysbox_url="https://downloads.nestybox.com/sysbox/releases/v${SYSBOX_VERSION}/sysbox-ce_${SYSBOX_VERSION}-0.linux_amd64.deb"
echo "actions/runner $RUNNER_VERSION ($RUNNER_SHA256)"
echo "sysbox $SYSBOX_VERSION from $sysbox_url"

log "job image"
docker build --pull -t "ghr-job:$VERSION" \
  --build-arg JOB_BASE_IMAGE="$JOB_BASE_IMAGE" \
  --build-arg RUNNER_VERSION="$RUNNER_VERSION" \
  --build-arg RUNNER_SHA256="$RUNNER_SHA256" \
  --build-arg GHR_VERSION="$VERSION" \
  "$root/image/job"
docker pull -q "$REGISTRY_IMAGE"
docker save "ghr-job:$VERSION" "$REGISTRY_IMAGE" | zstd -T0 -10 -q -f -o "$out/ghr-images.tar.zst"
ls -lh "$out/ghr-images.tar.zst"

log "stage live-build config"
work=$(mktemp -d)
cleanup() { docker run --rm -v "$work:/work" debian:"$DEBIAN_SUITE" rm -rf /work/lb >/dev/null 2>&1 || true; rm -rf "$work"; }
trap cleanup EXIT
cp -a "$root/image/lb" "$work/lb"
inc="$work/lb/config/includes.chroot"
install -D -m 0755 "$out/ghr" "$inc/usr/local/bin/ghr"
install -d "$inc/usr/share/ghr" "$inc/var/cache/ghr"
echo "$VERSION" >"$inc/usr/share/ghr/version"
curl -fsSL -o "$inc/var/cache/ghr/sysbox.deb" "$sysbox_url"
sysbox_sha=$(sha256sum "$inc/var/cache/ghr/sysbox.deb" | cut -d' ' -f1)
if [ -n "${SYSBOX_SHA256}" ] && [ "$SYSBOX_SHA256" != "$sysbox_sha" ]; then
  echo "sysbox .deb SHA-256 is $sysbox_sha, expected $SYSBOX_SHA256" >&2
  exit 1
fi
{
  echo "version=$VERSION"
  echo "debian=$DEBIAN_SUITE"
  echo "actions_runner=$RUNNER_VERSION sha256=$RUNNER_SHA256"
  echo "sysbox=$SYSBOX_VERSION sha256=$sysbox_sha"
  echo "job_base=$JOB_BASE_IMAGE"
  echo "registry=$REGISTRY_IMAGE"
} | tee "$out/manifest.txt" >"$inc/usr/share/ghr/manifest.txt"

log "live-build and stick layout (this takes a while)"
docker run --rm --privileged \
  -e DEBIAN_SUITE="$DEBIAN_SUITE" -e FAT_MB="$FAT_MB" \
  -v "$work:/work" -v "$root/image:/image:ro" -v "$out:/out" \
  "debian:$DEBIAN_SUITE" /image/scripts/in-container.sh
# Files from the container are root-owned.
docker run --rm -v "$out:/out" "debian:$DEBIAN_SUITE" chown -R "$(id -u):$(id -g)" /out

log "compress"
zstd -T0 -10 -q -f "$out/ghr-usb.img" -o "$out/ghr-usb.img.zst"
(cd "$out" && sha256sum ghr-usb.img ghr-usb.img.zst >SHA256SUMS)
ls -lh "$out/ghr-usb.img" "$out/ghr-usb.img.zst"
