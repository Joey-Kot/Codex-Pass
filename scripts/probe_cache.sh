#!/bin/sh
set -eu

usage() {
  echo "Usage: $0 --bin BIN --auth AUTH --key KEY --request REQUEST.json --model MODEL --cache-key KEY --session-id ID [--listen ADDRESS]" >&2
  exit 2
}

bin=
auth=
key=
request=
model=
cache_key=
session_id=
listen=127.0.0.1:18317

while [ "$#" -gt 0 ]; do
  case "$1" in
    --bin) bin=${2-}; shift 2 ;;
    --auth) auth=${2-}; shift 2 ;;
    --key) key=${2-}; shift 2 ;;
    --request) request=${2-}; shift 2 ;;
    --model) model=${2-}; shift 2 ;;
    --cache-key) cache_key=${2-}; shift 2 ;;
    --session-id) session_id=${2-}; shift 2 ;;
    --listen) listen=${2-}; shift 2 ;;
    --help|-h) usage ;;
    *) usage ;;
  esac
done

[ -n "$bin" ] && [ -n "$auth" ] && [ -n "$key" ] && [ -n "$request" ] && [ -n "$model" ] && [ -n "$cache_key" ] && [ -n "$session_id" ] || usage
command -v curl || exit 127
command -v jq || exit 127

workdir=$(mktemp -d "${TMPDIR:-/tmp}/codex-pass-cache.XXXXXX")
proxy_pid=
cleanup() {
  if [ -n "$proxy_pid" ]; then
    kill "$proxy_pid" || true
    wait "$proxy_pid" || true
  fi
  echo "Artifacts retained at: $workdir"
}
trap cleanup EXIT INT TERM

jq --arg model "$model" --arg cache "$cache_key" '.model = $model | .stream = false | .prompt_cache_key = $cache' "$request" | tee "$workdir/request.json" | wc -c
"$bin" --key "$key" --listen "$listen" --debug "$workdir/proxy.jsonl" "$auth" &
proxy_pid=$!

ready=false
for attempt in 1 2 3 4 5 6 7 8 9 10; do
  if curl --fail --silent --output "$workdir/healthz.json" "http://$listen/healthz"; then
    ready=true
    break
  fi
  sleep 1
done
[ "$ready" = true ] || { echo "proxy did not become ready" >&2; exit 1; }

for run in 1 2 3; do
  timing=$(curl --silent --show-error \
    --output "$workdir/response-$run.json" \
    --write-out "run=$run http=%{http_code} first=%{time_starttransfer}s total=%{time_total}s\n" \
    -H "Authorization: Bearer $key" \
    -H 'Content-Type: application/json' \
    -H "session-id: $session_id" \
    --data-binary "@$workdir/request.json" \
    "http://$listen/v1/responses")
  printf '%s\n' "$timing" | tee "$workdir/timing-$run.txt"
done

printf '%-6s %-8s %-12s %-12s %-12s %-12s\n' Case HTTP Input Cached CacheWrite Latency
for run in 1 2 3; do
  http=$(awk '{for (i = 1; i <= NF; i++) if ($i ~ /^http=/) {sub(/^http=/, "", $i); print $i}}' "$workdir/timing-$run.txt")
  latency=$(awk '{for (i = 1; i <= NF; i++) if ($i ~ /^total=/) {sub(/^total=/, "", $i); print $i}}' "$workdir/timing-$run.txt")
  jq -r --arg run "$run" --arg http "$http" --arg latency "$latency" '[
    $run,
    $http,
    (.usage.input_tokens // "absent"),
    (.usage.input_tokens_details.cached_tokens // "absent"),
    (.usage.input_tokens_details.cache_write_tokens // "absent"),
    $latency
  ] | @tsv' "$workdir/response-$run.json" | awk -F '\t' '{printf "%-6s %-8s %-12s %-12s %-12s %-12s\n", $1, $2, $3, $4, $5, $6}'
done
