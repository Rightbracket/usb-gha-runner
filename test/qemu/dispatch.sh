#!/bin/bash
# Dispatch ghr-smoke.yml in the test repository so its jobs run on the QEMU
# runner registered with <label>, and wait for the run to succeed. The two
# jobs run one after the other, so the runner has to pick up a second job
# in a fresh container. [ref] is the branch or tag to run the workflow from;
# it falls back to the default branch when the test repository doesn't have it.
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
if [ -n "$ref" ]; then
  # /commits resolves branches and tags. Only "not there" falls back.
  # Escape % and # for the URL (curl would drop a #fragment); the POST
  # body below keeps the raw ref.
  uref=${ref//%/%25}; uref=${uref//#/%23}
  code=$(gh_api -o /dev/null -w '%{http_code}' "https://api.github.com/repos/$repo/commits/$uref" 2>/dev/null || true)
  case $code in
    200) ;;
    404 | 422) echo "  ref $ref (or $repo itself) not found, trying the default branch"; ref="" ;;
    000 | "") echo "  looking up $ref in $repo failed: no response" >&2; exit 1 ;;
    *) echo "  looking up $ref in $repo failed: HTTP $code" >&2; exit 1 ;;
  esac
fi
[ -n "$ref" ] || ref=$(gh_api "https://api.github.com/repos/$repo" | jq -r .default_branch)
since=$(date -u -d '-1 minute' +%Y-%m-%dT%H:%M:%SZ)
lookup() {
  gh_api "$api/workflows/ghr-smoke.yml/runs?event=workflow_dispatch&created=>=$since&per_page=50" |
    jq -r --arg t "ghr smoke $label" '[.workflow_runs[] | select(.display_title == $t)][0].id // empty'
}
# The runner is removed right after this script, so never leave the run
# queued behind it: on any exit but a completed run, find it if need be
# and cancel it. Set before dispatching, so no exit path is missed.
run=""
cleanup() {
  [ -n "$run" ] || run=$(lookup 2>/dev/null || true)
  [ -z "$run" ] || gh_api -X POST "$api/runs/$run/cancel" >/dev/null 2>&1 || true
}
trap cleanup EXIT
# return_run_details makes GitHub answer with the new run's ID; lookup()
# is the fallback for a 204 or a response without it. A failed POST exits
# here, and the EXIT trap's own lookup cancels a run it can already see.
resp=$(gh_api -X POST "$api/workflows/ghr-smoke.yml/dispatches" \
  -d "$(jq -nc --arg ref "$ref" --arg label "$label" '{ref: $ref, inputs: {label: $label}, return_run_details: true}')")
run=$(jq -r '.workflow_run_id // empty' <<<"$resp" 2>/dev/null || true)
echo "  dispatched ghr-smoke.yml on $repo@$ref for $label"
for _ in $(seq 120); do
  # API errors are retried on the next pass.
  if [ -z "$run" ]; then
    run=$(lookup || true)
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
