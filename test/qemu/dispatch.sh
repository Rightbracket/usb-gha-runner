#!/bin/bash
# Dispatch ghr-smoke.yml in the test repository so its jobs run on the QEMU
# runner registered with <label>, and wait for the run to succeed. The two
# jobs run one after the other, so the runner has to pick up a second job
# in a fresh container.
# usage: GHR_TEST_PAT=... dispatch.sh https://github.com/<owner>/<repo> <label>
set -euo pipefail
url=${1%/}
label=$2
repo=${url#https://github.com/}
api="https://api.github.com/repos/$repo/actions"
gh_api() {
  curl -fsS --max-time 30 -H "Authorization: Bearer $GHR_TEST_PAT" \
    -H "Accept: application/vnd.github+json" -H "X-GitHub-Api-Version: 2022-11-28" "$@"
}
ref=$(gh_api "https://api.github.com/repos/$repo" | jq -r .default_branch)
since=$(date -u -d '-1 minute' +%Y-%m-%dT%H:%M:%SZ)
gh_api -X POST "$api/workflows/ghr-smoke.yml/dispatches" \
  -d "$(jq -nc --arg ref "$ref" --arg label "$label" '{ref: $ref, inputs: {label: $label}}')"
echo "  dispatched ghr-smoke.yml on $repo@$ref for $label"
run=""
for _ in $(seq 120); do
  if [ -z "$run" ]; then
    run=$(gh_api "$api/workflows/ghr-smoke.yml/runs?event=workflow_dispatch&created=>=$since&per_page=50" |
      jq -r --arg t "ghr smoke $label" '[.workflow_runs[] | select(.display_title == $t)][0].id // empty')
  fi
  if [ -n "$run" ]; then
    r=$(gh_api "$api/runs/$run")
    if [ "$(jq -r .status <<<"$r")" = completed ]; then
      echo "  run $(jq -r .html_url <<<"$r"): $(jq -r .conclusion <<<"$r")"
      gh_api "$api/runs/$run/jobs" | jq -r '.jobs[] | "    \(.name): \(.conclusion) on \(.runner_name)"'
      [ "$(jq -r .conclusion <<<"$r")" = success ]
      exit
    fi
  fi
  sleep 10
done
echo "  run ${run:-(not found)} did not finish in 20 minutes"
[ -z "$run" ] || gh_api -X POST "$api/runs/$run/cancel" >/dev/null || true
exit 1
