#!/usr/bin/env bash
# Usage: scripts/export-classic.sh [-in DIR] [-out DIR]
# Converts the schema v2 dashboards in DIR (default grafana/dashboards) to
# classic JSON for Grafana 12 and grafana.com in OUT (default dist/classic).
# A throwaway Grafana 13.3.1 container provisions the v2 files and the
# classic JSON is read back through the dashboard.grafana.app/v1 API. Each
# classic file is then imported through /api/dashboards/import into
# throwaway Grafana 12.4.15 and 13.3.1 containers. Needs Docker, curl, jq.
set -euo pipefail
cd "$(dirname "$0")/.."

in=grafana/dashboards
out=dist/classic
while [[ $# -gt 0 ]]; do
    case "$1" in
        -in) in=$2; shift 2 ;;
        -out) out=$2; shift 2 ;;
        *) echo "usage: $0 [-in DIR] [-out DIR]" >&2; exit 2 ;;
    esac
done

export_image=grafana/grafana:13.3.1@sha256:0615ec4fbd4072004b7d86b46ded63013a74b7d6eb1119273dd89e8fa25c13cf
verify_images=(
    grafana/grafana:12.4.15@sha256:2894082f6cf7081aefdc1389265c04000812309208c190d75e8c65f7a30e4b7e
    "$export_image"
)
min_grafana=12.4.15
password=$(openssl rand -hex 12)
mkdir -p .tmp
work=$(mktemp -d "$PWD/.tmp/classic.XXXXXX")
containers=()

cleanup() {
    for c in ${containers[@]+"${containers[@]}"}; do docker rm -f "$c" >/dev/null 2>&1 || true; done
    rm -rf "$work"
}
trap cleanup EXIT

shopt -s nullglob
files=("$in"/*.json)
[[ ${#files[@]} -gt 0 ]] || { echo "no dashboards in $in" >&2; exit 1; }
uids=()
for f in "${files[@]}"; do
    uids+=("$(jq -er '.metadata.name' "$f")")
done

start() {
    local name=$1 image=$2 port
    shift 2
    docker run -d --quiet --name "$name" -p 127.0.0.1::3000 \
        -e GF_SECURITY_ADMIN_PASSWORD="$password" \
        -e GF_PLUGINS_PREINSTALL_DISABLED=true \
        -e GF_ANALYTICS_REPORTING_ENABLED=false \
        -e GF_ANALYTICS_CHECK_FOR_UPDATES=false \
        -e GF_NEWS_NEWS_FEED_ENABLED=false \
        -v "$work/provisioning:/etc/grafana/provisioning:ro" \
        "$@" "$image" >/dev/null
    port=$(docker port "$name" 3000/tcp | head -1 | sed 's/.*://')
    for _ in $(seq 90); do
        if curl -fsS -o /dev/null "http://127.0.0.1:$port/api/health" 2>/dev/null; then
            echo "http://127.0.0.1:$port"
            return
        fi
        sleep 1
    done
    docker logs "$name" | tail -20 >&2
    echo "$name did not start" >&2
    return 1
}

api() { curl -fsS -u "admin:$password" -H 'Content-Type: application/json' "$@"; }

mkdir -p "$work/provisioning/dashboards" "$work/provisioning/datasources" "$work/v2"
cp "${files[@]}" "$work/v2/"
cat > "$work/provisioning/datasources/prometheus.yaml" <<'EOF'
apiVersion: 1
datasources:
  - name: Prometheus
    uid: ehlo-classic-check
    type: prometheus
    access: proxy
    url: http://127.0.0.1:9090
    isDefault: true
EOF
cat > "$work/provisioning/dashboards/ehlo.yaml" <<'EOF'
apiVersion: 1
providers:
  - name: ehlo
    type: file
    options:
      path: /var/lib/ehlo-v2
EOF
chmod -R a+rX "$work"

echo "== export with $export_image"
containers+=("ehlo-classic-export-$$")
url=$(start "ehlo-classic-export-$$" "$export_image" -v "$work/v2:/var/lib/ehlo-v2:ro")
for _ in $(seq 60); do
    got=$(api "$url/api/search?type=dash-db&limit=5000" | jq -r '.[].uid')
    missing=0
    for uid in "${uids[@]}"; do grep -qx "$uid" <<<"$got" || missing=1; done
    [[ $missing == 0 ]] && break
    sleep 1
done
[[ $missing == 0 ]] || { echo "Grafana did not provision every dashboard" >&2; docker logs "ehlo-classic-export-$$" | grep -i error | tail -20 >&2; exit 1; }

mkdir -p "$out"
for uid in "${uids[@]}"; do
    api "$url/apis/dashboard.grafana.app/v1/namespaces/default/dashboards/$uid" > "$work/$uid.v1.json"
    jq -e '.status.conversion.failed != true' "$work/$uid.v1.json" >/dev/null \
        || { echo "$uid: v2 to v1 conversion failed: $(jq -c .status "$work/$uid.v1.json")" >&2; exit 1; }
    jq --arg uid "$uid" --arg min "$min_grafana" '
        def names: {bargauge: "Bar gauge", stat: "Stat", "state-timeline": "State timeline",
                    table: "Table", text: "Text", timeseries: "Time series", row: "Row"};
        .spec
        | .uid = $uid | .id = null | del(.version)
        | (.. | objects | select(.datasource? | type == "object" and .type == "prometheus")
           | .datasource) |= {type: "prometheus", uid: "${datasource}"}
        | .templating.list |= map(
            if .type == "datasource" and .query == "prometheus"
            then .current = {text: "${DS_PROMETHEUS}", value: "${DS_PROMETHEUS}"}
            else . end)
        | . as $d
        | {
            __inputs: [{name: "DS_PROMETHEUS", label: "Prometheus", description: "",
                        type: "datasource", pluginId: "prometheus", pluginName: "Prometheus"}],
            __elements: {},
            __requires: (
                [{type: "grafana", id: "grafana", name: "Grafana", version: $min},
                 {type: "datasource", id: "prometheus", name: "Prometheus", version: "1.0.0"}]
                + ([$d.panels[]?, $d.panels[]?.panels[]? | .type | select(. != "row")] | unique
                   | map({type: "panel", id: ., name: (names[.] // .), version: ""})))
          } + $d
    ' "$work/$uid.v1.json" > "$out/$uid.json"
    echo "$out/$uid.json: $(jq '[.panels[]?, .panels[]?.panels[]?] | map(select(.type != "row")) | length' "$out/$uid.json") panels"
done
docker rm -f "ehlo-classic-export-$$" >/dev/null

for image in "${verify_images[@]}"; do
    echo "== import into ${image%@*}"
    name="ehlo-classic-verify-$$-$(echo "${image%@*}" | tr ':/.' '---')"
    containers+=("$name")
    url=$(start "$name" "$image")
    echo "Grafana $(api "$url/api/health" | jq -r .version)"
    for uid in "${uids[@]}"; do
        jq '{dashboard: ., overwrite: true, folderUid: "",
             inputs: [{name: "DS_PROMETHEUS", type: "datasource", pluginId: "prometheus", value: "ehlo-classic-check"}]}' \
            "$out/$uid.json" > "$work/import.json"
        api -X POST "$url/api/dashboards/import" --data-binary "@$work/import.json" > "$work/import-result.json"
        api "$url/api/dashboards/uid/$uid" > "$work/back.json"
        want=$(jq '[.panels[]?, .panels[]?.panels[]?] | length' "$out/$uid.json")
        have=$(jq '[.dashboard.panels[]?, .dashboard.panels[]?.panels[]?] | length' "$work/back.json")
        ds=$(jq -r '.dashboard.templating.list[] | select(.type == "datasource") | .current.value' "$work/back.json")
        if [[ "$want" != "$have" ]] || [[ "$ds" != ehlo-classic-check ]] || grep -q 'DS_PROMETHEUS' "$work/back.json"; then
            echo "$uid: import check failed (panels $have of $want, datasource $ds)" >&2
            exit 1
        fi
        echo "$uid: imported, $have panels, schemaVersion $(jq .dashboard.schemaVersion "$work/back.json")"
    done
    docker rm -f "$name" >/dev/null
done

echo "classic dashboards in $out"
