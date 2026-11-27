# ghr-job: the throwaway container each GitHub Actions job runs in, under
# Sysbox. It has its own Docker daemon, so services:, container: and buildx
# work as usual, and the job user has passwordless sudo inside it.
ARG JOB_BASE_IMAGE=ubuntu:24.04
FROM ${JOB_BASE_IMAGE}

ARG RUNNER_VERSION
ARG RUNNER_SHA256
ARG GHR_VERSION=dev
LABEL ghr.keep=true \
      org.opencontainers.image.title="ghr-job" \
      org.opencontainers.image.version="${GHR_VERSION}"

ENV DEBIAN_FRONTEND=noninteractive \
    LANG=C.UTF-8 \
    ImageOS=ubuntu24 \
    RUNNER_TOOL_CACHE=/opt/hostedtoolcache \
    AGENT_TOOLSDIRECTORY=/opt/hostedtoolcache

RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      ca-certificates curl wget gnupg lsb-release sudo git git-lfs openssh-client \
      jq unzip zip xz-utils zstd bzip2 pigz file rsync tzdata locales \
      build-essential pkg-config python3 python3-pip python3-venv \
      iptables iproute2 iputils-ping dnsutils kmod procps \
      buildah podman crun fuse-overlayfs uidmap \
      qemu-user-static qemu-system-x86 qemu-system-arm qemu-system-misc qemu-utils qemu-efi-aarch64 ovmf \
 && install -m 0755 -d /etc/apt/keyrings \
 && curl -fsSL https://download.docker.com/linux/ubuntu/gpg -o /etc/apt/keyrings/docker.asc \
 && echo "deb [arch=amd64 signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/ubuntu $(. /etc/os-release && echo "$VERSION_CODENAME") stable" \
      > /etc/apt/sources.list.d/docker.list \
 && apt-get update \
 && apt-get install -y --no-install-recommends \
      docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin \
 && rm -rf /var/lib/apt/lists/*

# The job user, matching GitHub-hosted runners (uid 1001, passwordless sudo).
RUN (userdel -r ubuntu 2>/dev/null || true) \
 && useradd --create-home --uid 1001 --shell /bin/bash runner \
 && usermod -aG sudo,docker runner \
 && echo 'runner ALL=(ALL) NOPASSWD:ALL' >/etc/sudoers.d/runner \
 && chmod 0440 /etc/sudoers.d/runner \
 && install -d -o runner -g runner /opt/hostedtoolcache /home/runner/actions-runner

# The Actions runner, checked against the SHA-256 from its release notes.
RUN test -n "${RUNNER_VERSION}" && test -n "${RUNNER_SHA256}" \
 && curl -fsSL -o /tmp/runner.tgz \
      "https://github.com/actions/runner/releases/download/v${RUNNER_VERSION}/actions-runner-linux-x64-${RUNNER_VERSION}.tar.gz" \
 && echo "${RUNNER_SHA256}  /tmp/runner.tgz" | sha256sum -c - \
 && tar -xzf /tmp/runner.tgz -C /home/runner/actions-runner \
 && rm /tmp/runner.tgz \
 && /home/runner/actions-runner/bin/installdependencies.sh \
 && chown -R runner:runner /home/runner \
 && rm -rf /var/lib/apt/lists/*

# Buildah and Podman use native overlay, not fuse-overlayfs (there is no
# /dev/fuse in a Sysbox container). Overlay can't sit on the container's own
# overlay root, so their storage is a volume; docker run --rm and rm -v
# delete it with the job.
RUN printf '[storage]\ndriver = "overlay"\nrunroot = "/run/containers/storage"\ngraphroot = "/var/lib/containers/storage"\n' \
      >/etc/containers/storage.conf
VOLUME /var/lib/containers

# The job registers these itself (see binfmt.sh); fail the build if the
# package stops shipping them.
RUN ls /usr/lib/binfmt.d/qemu-aarch64*.conf
COPY binfmt.sh /usr/local/bin/ghr-binfmt
COPY entrypoint.sh /usr/local/bin/ghr-job-entrypoint
STOPSIGNAL SIGINT
ENTRYPOINT ["/usr/local/bin/ghr-job-entrypoint"]
