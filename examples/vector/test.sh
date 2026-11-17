#!/usr/bin/env bash
# Usage: examples/vector/test.sh
# Runs both Vector examples against a local ehlo and prints the series they moved.
# Needs Go and Docker. EHLO_TEST_PORT (default 18081) and VECTOR_IMAGE override the defaults.
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
repo="$(cd "$here/../.." && pwd)"
port="${EHLO_TEST_PORT:-18081}"
image="${VECTOR_IMAGE:-timberio/vector:0.62.0-debian@sha256:6266b1d35c844918ff1c48ea2ec4f57c2b0664c46f3e17fc2418d5dda1692068}"
work="$(mktemp -d)"
pids=()
cleanup() {
  for c in ehlo-vector-test-powermta ehlo-vector-test-json; do docker rm -f "$c" >/dev/null 2>&1 || true; done
  for p in "${pids[@]}"; do kill "$p" 2>/dev/null || true; wait "$p" 2>/dev/null || true; done
  rm -rf "$work"
}
trap cleanup EXIT

(cd "$repo" && go build -o "$work/ehlo" ./cmd/ehlo)
"$work/ehlo" serve -listen "127.0.0.1:$port" -state "$work/warmup.json" >"$work/ehlo.log" 2>&1 &
pids+=("$!")
for _ in $(seq 50); do curl -fsS "http://127.0.0.1:$port/healthz" >/dev/null 2>&1 && break; sleep 0.2; done

run() {
  local name="$1" dir="$2"
  mkdir -p "$work/$name"
  sed -e 's#read_from: end#read_from: beginning#' \
      -e "s#/var/log/pmta/#/data/#; s#/var/log/mta/delivery.json#/data/sample.json#" \
      -e "s#uri: http://127.0.0.1:8080/v1/events#uri: http://host.docker.internal:$port/v1/events#" \
      "$dir/vector.yaml" >"$work/$name/vector.yaml"
  docker run -d --name "ehlo-vector-test-$name" --add-host=host.docker.internal:host-gateway \
    -v "$work/$name":/etc/vector:ro -v "$dir":/data:ro "$image" --config /etc/vector/vector.yaml >/dev/null
}

run powermta "$here/powermta"
run json "$here/json"
sleep "${VECTOR_WAIT:-10}"

docker logs ehlo-vector-test-powermta 2>&1 | grep -iE 'error|warn' | grep -v unused_expression || true
docker logs ehlo-vector-test-json 2>&1 | grep -iE 'error|warn' | grep -v unused_expression || true
metrics="$(curl -fsS "http://127.0.0.1:$port/metrics")"
grep -E '^ehlo_(events_total|smtp_attempts_total|smtp_replies_total|bounces_total|complaints_total)\{' <<<"$metrics" | grep -v ' 0$'
want='ehlo_events_total{result="accepted",type="attempt"} 11
ehlo_events_total{result="accepted",type="bounce"} 5
ehlo_events_total{result="accepted",type="complaint"} 2
ehlo_events_total{result="accepted",type="reply"} 11'
got="$(grep -E '^ehlo_events_total\{' <<<"$metrics" | grep -v ' 0$')"
if [[ "$got" != "$want" ]]; then
  printf 'FAIL: event totals differ\nwant:\n%s\ngot:\n%s\n' "$want" "$got"
  exit 1
fi
echo "PASS: both Vector examples posted the expected events"
