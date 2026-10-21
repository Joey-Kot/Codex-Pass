#!/bin/sh
set -eu

usage() {
  echo "Usage: $0 --bin BIN --auth AUTH --key KEY --request REQUEST.json --model MODEL [--listen ADDRESS]" >&2
  exit 2
}

bin=
auth=
key=
request=
model=
listen=127.0.0.1:18318

while [ "$#" -gt 0 ]; do
  case "$1" in
    --bin) bin=${2-}; shift 2 ;;
    --auth) auth=${2-}; shift 2 ;;
    --key) key=${2-}; shift 2 ;;
    --request) request=${2-}; shift 2 ;;
    --model) model=${2-}; shift 2 ;;
    --listen) listen=${2-}; shift 2 ;;
    --help|-h) usage ;;
    *) usage ;;
  esac
done

[ -n "$bin" ] && [ -n "$auth" ] && [ -n "$key" ] && [ -n "$request" ] && [ -n "$model" ] || usage
command -v curl || exit 127
command -v jq || exit 127

workdir=$(mktemp -d "${TMPDIR:-/tmp}/codex-pass-fast.XXXXXX")
proxy_pid=
cleanup() {
  if [ -n "$proxy_pid" ]; then
    kill "$proxy_pid" || true
    wait "$proxy_pid" || true
  fi
  echo "Artifacts retained at: $workdir"
}
trap cleanup EXIT INT TERM

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

printf '%-10s %-5s %-12s %-12s %-12s %-12s\n' Tier HTTP First Total ResponseTier Input
for tier in default fast priority; do
  for run in 1 2 3; do
    output="$workdir/request-$tier-$run.json"
    if [ "$tier" = default ]; then
      jq --arg model "$model" '.model = $model | .stream = false | del(.service_tier)' "$request" | tee "$output" | wc -c
    else
      jq --arg model "$model" --arg tier "$tier" '.model = $model | .stream = false | .service_tier = $tier' "$request" | tee "$output" | wc -c
    fi
    timing=$(curl --silent --show-error \
      --output "$workdir/response-$tier-$run.json" \
      --write-out '%{http_code} %{time_starttransfer} %{time_total}' \
      -H "Authorization: Bearer $key" \
      -H 'Content-Type: application/json' \
      --data-binary "@$output" \
      "http://$listen/v1/responses")
    set -- $timing
    response_tier=$(jq -r '.service_tier // "absent"' "$workdir/response-$tier-$run.json")
    input_tokens=$(jq -r '.usage.input_tokens // "absent"' "$workdir/response-$tier-$run.json")
    printf '%-10s %-5s %-12s %-12s %-12s %-12s\n' "$tier/$run" "$1" "$2" "$3" "$response_tier" "$input_tokens"
  done
done
