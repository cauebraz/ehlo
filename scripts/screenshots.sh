#!/usr/bin/env bash
# Usage: scripts/screenshots.sh [-scenario NAME] [-steady MINUTES] [-minutes MINUTES] [-out DIR]
# Starts a throwaway copy of the demo stack (Compose project ehlo-screenshots,
# no fixed host ports, so a running `just up` stays untouched) with the
# Grafana image renderer, runs the steady scenario for -steady minutes
# (default 10), switches to -scenario (default gmail-throttle) for -minutes
# minutes (default 20) at 300,000 messages a day, then renders every
# dashboard over that window into OUT (default docs/images/dashboards) as
# <uid>.png (light) and <uid>-dark.png. The stack and its volumes are
# removed on exit. On macOS, caffeinate keeps the host awake; the script
# fails if the run took more than 5 minutes longer than planned.
# Needs Docker with Compose v2.24 or later, curl, jq and openssl.
set -euo pipefail
cd "$(dirname "$0")/.."

scenario=gmail-throttle
steady=10
minutes=20
out=docs/images/dashboards
width=1600
while [[ $# -gt 0 ]]; do
    case "$1" in
        -scenario) scenario=$2; shift 2 ;;
        -steady) steady=$2; shift 2 ;;
        -minutes) minutes=$2; shift 2 ;;
        -out) out=$2; shift 2 ;;
        *) echo "usage: $0 [-scenario NAME] [-steady MINUTES] [-minutes MINUTES] [-out DIR]" >&2; exit 2 ;;
    esac
done
[[ "$steady" =~ ^[0-9]+$ && "$minutes" =~ ^[1-9][0-9]*$ ]] || { echo "-steady and -minutes take whole minutes" >&2; exit 2; }

export GRAFANA_ADMIN_PASSWORD RENDERER_TOKEN EHLO_SCENARIO EHLO_SPEED=60 EHLO_DAY_LENGTH=24m
GRAFANA_ADMIN_PASSWORD=$(openssl rand -hex 16)
RENDERER_TOKEN=$(openssl rand -hex 16)
compose() { docker compose -p ehlo-screenshots -f compose.yaml -f compose.screenshots.yaml "$@"; }
trap 'compose down --volumes --remove-orphans >/dev/null 2>&1 || true' EXIT
if command -v caffeinate >/dev/null; then
    caffeinate -i -w $$ &
fi

wait_minutes() {
    local n=$1 label=$2
    for ((i = 1; i <= n; i++)); do
        sleep 60
        printf '%s: %d of %d minutes\n' "$label" "$i" "$n"
    done
}

EHLO_SCENARIO=steady
if [[ "$scenario" == steady ]]; then
    steady=0
fi
echo "== start the stack"
compose up -d --build --wait --quiet-pull
compose kill -s HUP prometheus >/dev/null
from=$(($(date +%s) * 1000))
wait_minutes "$steady" steady

if [[ "$scenario" != steady ]]; then
    echo "== switch to $scenario"
    EHLO_SCENARIO=$scenario compose up -d --wait ehlo-sim
fi
EHLO_SCENARIO=$scenario
wait_minutes "$minutes" "$scenario"
to=$(($(date +%s) * 1000))
if (( (to - from) / 60000 > steady + minutes + 5 )); then
    echo "the run took $(((to - from) / 60000)) minutes instead of $((steady + minutes)); the host probably slept, run it again" >&2
    exit 1
fi

grafana="http://$(compose port grafana 3000)"
mkdir -p "$out"
echo "== render to $out"
for file in grafana/dashboards/*.json; do
    uid=$(jq -er .metadata.name "$file")
    for theme in light dark; do
        dest="$out/$uid.png"
        [[ $theme == dark ]] && dest="$out/$uid-dark.png"
        curl -fsS -u "admin:$GRAFANA_ADMIN_PASSWORD" --max-time 180 -o "$dest" \
            "$grafana/render/d/$uid?from=$from&to=$to&width=$width&height=-1&kiosk=true&theme=$theme&tz=UTC"
        [[ "$(head -c 8 "$dest" | od -An -tx1 | tr -d ' \n')" == 89504e470d0a1a0a ]] \
            || { echo "$dest is not a PNG: $(head -c 200 "$dest")" >&2; exit 1; }
        printf '%s %s KiB\n' "$dest" "$(($(wc -c < "$dest") / 1024))"
    done
done
