#!/bin/bash
# Dispatch ghr-smoke.yml in the test repository so its jobs run on the QEMU
# runner registered with <label>, and wait for the run to succeed. The two
# jobs run one after the other, so the runner has to pick up a second job
# in a fresh container. [ref] is the branch to run the workflow from; it
# falls back to the default branch when the test repository doesn't have it.
# usage: GHR_TEST_PAT=... dispatch.sh https://github.com/<owner>/<repo> <label> [ref]
set -euo pipefail
url=${1%/}
label=$2
ref=${3:-}
repo=${url#https://github.com/}
api="https://api.github.com/repos/$repo/actions"
gh_api() {
  curl -fsS --max-time 30 -H "Authorization: Bearer $GHR_TEST_PAT" \
    -H "Accept: application/vnd.github+json" -H "X-GitHub-Api-Version: 2022-11-28" "$@"
}
if [ -z "$ref" ] || ! gh_api -o /dev/null "https://api.github.com/repos/$repo/branches/$ref" 2>/dev/null; then
  ref=$(gh_api "https://api.github.com/repos/$repo" | jq -r .default_branch)
fi
since=$(date -u -d '-1 minute' +%Y-%m-%dT%H:%M:%SZ)
gh_api -X POST "$api/workflows/ghr-smoke.yml/dispatches" \
  -d "$(jq -nc --arg ref "$ref" --arg label "$label" '{ref: $ref, inputs: {label: $label}}')"
echo "  dispatched ghr-smoke.yml on $repo@$ref for $label"
run=""
# The runner is removed right after this script, so never leave the run
# queued behind it: cancel it on any exit but success.
trap '[ -z "$run" ] || gh_api -X POST "$api/runs/$run/cancel" >/dev/null 2>&1 || true' EXIT
for _ in $(seq 120); do
  # API errors are retried on the next pass.
  if [ -z "$run" ]; then
    run=$(gh_api "$api/workflows/ghr-smoke.yml/runs?event=workflow_dispatch&created=>=$since&per_page=50" |
      jq -r --arg t "ghr smoke $label" '[.workflow_runs[] | select(.display_title == $t)][0].id // empty' || true)
  fi
  if [ -n "$run" ] && r=$(gh_api "$api/runs/$run"); then
    if [ "$(jq -r .status <<<"$r")" = completed ]; then
      trap - EXIT
      echo "  run $(jq -r .html_url <<<"$r"): $(jq -r .conclusion <<<"$r")"
      gh_api "$api/runs/$run/jobs" | jq -r '.jobs[] | "    \(.name): \(.conclusion) on \(.runner_name)"' || true
      [ "$(jq -r .conclusion <<<"$r")" = success ]
      exit
    fi
  fi
  sleep 10
done
echo "  run ${run:-(not found)} did not finish in 20 minutes"
exit 1
