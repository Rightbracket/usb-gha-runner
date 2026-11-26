#!/bin/bash
# Start this job's own Docker daemon, then run the Actions runner with the
# one-time (JIT) config. The runner takes exactly one job and exits, and
# ghr-agent deletes the container.
set -euo pipefail

jit="${GHR_JITCONFIG:-}"
unset GHR_JITCONFIG
if [ -z "$jit" ]; then
  echo "GHR_JITCONFIG is not set" >&2
  exit 2
fi

mkdir -p /etc/docker
if [ -n "${GHR_REGISTRY_MIRROR:-}" ]; then
  mirror_host="${GHR_REGISTRY_MIRROR#http://}"
  printf '{"registry-mirrors":["%s"],"insecure-registries":["%s"]}\n' "$GHR_REGISTRY_MIRROR" "$mirror_host" >/etc/docker/daemon.json
fi
unset GHR_REGISTRY_MIRROR

dockerd >/var/log/dockerd.log 2>&1 &
for _ in $(seq 60); do
  docker info >/dev/null 2>&1 && break
  sleep 1
done
if ! docker info >/dev/null 2>&1; then
  echo "dockerd did not start inside the job container:" >&2
  tail -n 40 /var/log/dockerd.log >&2
  exit 1
fi
chgrp docker /var/run/docker.sock || true

cd /home/runner/actions-runner
# The runner reads --jitconfig from ACTIONS_RUNNER_INPUT_JITCONFIG, which
# keeps the secret out of the process list (argv is world-readable).
export ACTIONS_RUNNER_INPUT_JITCONFIG="$jit"
unset jit
exec runuser -u runner -- ./run.sh
