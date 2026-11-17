#!/usr/bin/env bash
# Usage: deploy/scripts/kind-e2e.sh [-keep]
# Creates a kind cluster, installs kube-prometheus-stack, the Grafana
# Operator and the ehlo chart built from this checkout, and checks that ehlo
# answers /healthz, that Prometheus scrapes it through the ServiceMonitor
# and then the PodMonitor, that every ehlo rule loads with health ok, that
# Grafana's sidecar loads every dashboard, that the Grafana Operator syncs
# every GrafanaManifest, that a lane's sent count survives a pod restart,
# and that the webhook token from webhookSecret and from
# existingConfigSecret is enforced (401 without it, 202 with it). Deletes
# the cluster at the end unless -keep is given. Needs Docker, kind, kubectl
# and jq.
set -euo pipefail
cd "$(dirname "$0")/../.."

cluster=ehlo-e2e
node_image=kindest/node:v1.37.0@sha256:a1ed56cfb0e7b93589bdf97c8cd566405a265939e3620fc4f5de89adff580ae5
kps_chart=oci://ghcr.io/prometheus-community/charts/kube-prometheus-stack
kps_version=93.3.1
kps_digest=sha256:ffe8e72492d5bad79fc6468f69afa2f0303983195993f7bd26ae86721e2d8ebe
gop_chart=oci://ghcr.io/grafana/helm-charts/grafana-operator
gop_version=5.26.0
gop_digest=sha256:ed199fff9f727777762762496c5663047d46fc208889ba568420e3f11e490539
gop_image_tag=v5.26.0@sha256:f7051c56e3e0d3581455c2755d3493d0c308e555e1b8604ce77bf6db7ed6c5df
grafana_image=docker.io/grafana/grafana:13.3.1@sha256:0615ec4fbd4072004b7d86b46ded63013a74b7d6eb1119273dd89e8fa25c13cf
image=ehlo:e2e
ns=ehlo
mon=monitoring
ehlo_port=18080
prom_port=19090
grafana_port=13000

helm=$(deploy/scripts/helm.sh path)
ctx="kind-$cluster"
k() { kubectl --context "$ctx" "$@"; }
step() { printf '\n== %s\n' "$1"; }
pids=()

cleanup() {
    for p in ${pids[@]+"${pids[@]}"}; do kill "$p" 2>/dev/null || true; wait "$p" 2>/dev/null || true; done
    if [[ "${1:-}" != -keep ]]; then
        kind delete cluster --name "$cluster"
    fi
}
trap 'cleanup "${KEEP:-}"' EXIT
[[ "${1:-}" == -keep ]] && KEEP=-keep

forward() {
    k -n "$1" port-forward "$2" "$3:$4" >/dev/null 2>&1 &
    pids+=($!)
    for _ in $(seq 30); do
        if curl -fsS -o /dev/null "http://127.0.0.1:$3$5" 2>/dev/null; then return; fi
        sleep 1
    done
    echo "port-forward to $2 did not answer on $5"
    return 1
}

retry() {
    local tries=$1 desc=$2
    shift 2
    for _ in $(seq "$tries"); do
        if "$@"; then return 0; fi
        sleep 10
    done
    echo "timed out: $desc"
    return 1
}

step "kind cluster $cluster ($node_image)"
kind create cluster --name "$cluster" --image "$node_image" --wait 120s

step "build and load $image"
docker build -q -t "$image" .
kind load docker-image "$image" --name "$cluster"

step "kube-prometheus-stack $kps_version"
"$helm" install kps "$kps_chart:$kps_version@$kps_digest" \
    --kube-context "$ctx" --namespace "$mon" --create-namespace \
    -f deploy/kind/kube-prometheus-stack.yaml \
    --set grafana.adminPassword="$(openssl rand -hex 12)" \
    --wait --timeout 10m

step "ehlo chart"
"$helm" install ehlo deploy/helm/ehlo \
    --kube-context "$ctx" --namespace "$ns" --create-namespace \
    --set image.repository=ehlo --set image.tag=e2e --set image.pullPolicy=Never \
    --set-json 'config={"sources":["mx-1","mx-2"],"targets":{"default":100000}}' \
    --set serviceMonitor.enabled=true --set serviceMonitor.labels.release=kps \
    --set prometheusRule.enabled=true --set prometheusRule.labels.release=kps \
    --set dashboards.enabled=true --set dashboards.annotations.grafana_folder=ehlo \
    --wait --timeout 5m
k -n "$ns" get pods,pvc,svc,servicemonitor,prometheusrule,configmap -l app.kubernetes.io/instance=ehlo

step "ehlo /healthz"
forward "$ns" svc/ehlo "$ehlo_port" 8080 /healthz
curl -fsS "http://127.0.0.1:$ehlo_port/healthz"
echo
curl -fsS "http://127.0.0.1:$ehlo_port/metrics" | grep -c '^ehlo_' | sed 's/^/ehlo_ samples: /'

step "Prometheus scrapes ehlo"
prom_ready() { k -n "$mon" wait pod -l app.kubernetes.io/name=prometheus --for=condition=Ready --timeout 10s >/dev/null 2>&1; }
retry 30 "Prometheus pod ready" prom_ready
forward "$mon" svc/kps-kube-prometheus-stack-prometheus "$prom_port" 9090 /-/ready
scraped() {
    curl -fsS "http://127.0.0.1:$prom_port/api/v1/query" --data-urlencode 'query=up{namespace="'"$ns"'",service="ehlo"}' \
        | jq -e '.data.result | length == 1 and .[0].value[1] == "1"' >/dev/null
}
retry 18 'up{service="ehlo"} == 1' scraped
curl -fsS "http://127.0.0.1:$prom_port/api/v1/query" --data-urlencode 'query=up{service="ehlo"}' | jq -c '.data.result[] | {metric: .metric | {job, namespace, service}, value: .value[1]}'

step "PrometheusRule loads with health ok"
rules_ok() {
    curl -fsS "http://127.0.0.1:$prom_port/api/v1/rules" | jq -e '
        [.data.groups[] | select(.file | contains("ehlo")) | .rules[]] as $r
        | ($r | length) > 0 and all($r[]; .health == "ok")' >/dev/null
}
retry 18 "every ehlo rule health ok" rules_ok
curl -fsS "http://127.0.0.1:$prom_port/api/v1/rules" | jq -r '
    .data.groups[] | select(.file | contains("ehlo"))
    | "\(.name): \(.rules | length) rules, health \(.rules | map(.health) | unique | join(","))"'

step "Grafana sidecar loads the dashboards"
gpass=$(k -n "$mon" get secret kps-grafana -o jsonpath='{.data.admin-password}' | base64 -d)
forward "$mon" svc/kps-grafana "$grafana_port" 80 /api/health
want=$(cd deploy/helm/ehlo/files/dashboards && ls -- *.json | sed 's/\.json$//' | sort)
loaded() {
    got=$(curl -fsS -u "admin:$gpass" "http://127.0.0.1:$grafana_port/api/search?type=dash-db&limit=5000" | jq -r '.[].uid' | sort)
    [[ "$(comm -23 <(echo "$want") <(echo "$got"))" == "" ]]
}
retry 18 "all dashboards in Grafana" loaded
curl -fsS "http://127.0.0.1:$grafana_port/api/health" | jq -c '{version}'
for uid in $want; do
    api=$(curl -fsS -u "admin:$gpass" "http://127.0.0.1:$grafana_port/apis/dashboard.grafana.app/v2/namespaces/default/dashboards/$uid" | jq -r '"\(.apiVersion) \(.spec.title) folder=\(.metadata.annotations["grafana.app/folder"] // "")"')
    echo "$uid: $api"
done

step "warmup state survives a pod restart"
now=$(date -u +%Y-%m-%dT%H:%M:%SZ)
for _ in 1 2 3; do printf '{"type":"attempt","time":"%s","source":"mx-2","domain":"gmail.com"}\n' "$now"; done \
    | curl -fsS -X POST --data-binary @- "http://127.0.0.1:$ehlo_port/v1/events" >/dev/null
sent() { curl -fsS "http://127.0.0.1:$1/v1/limits/mx-2/gmail" | jq -r .sent; }
[[ "$(sent "$ehlo_port")" == 3 ]] || { echo "mx-2/gmail sent $(sent "$ehlo_port") before the restart, want 3"; exit 1; }
k -n "$ns" rollout restart deployment/ehlo
k -n "$ns" rollout status deployment/ehlo --timeout 3m
forward "$ns" svc/ehlo "$((ehlo_port + 3))" 8080 /healthz
[[ "$(sent "$((ehlo_port + 3))")" == 3 ]] || { echo "mx-2/gmail sent $(sent "$((ehlo_port + 3))") after the restart, want 3"; exit 1; }
echo "mx-2/gmail sent 3 before and after the restart"

step "Prometheus scrapes ehlo through the PodMonitor"
"$helm" upgrade ehlo deploy/helm/ehlo --kube-context "$ctx" --namespace "$ns" --reuse-values \
    --set serviceMonitor.enabled=false --set podMonitor.enabled=true --set podMonitor.labels.release=kps --wait --timeout 5m
pod_scraped() {
    curl -fsS "http://127.0.0.1:$prom_port/api/v1/query" --data-urlencode 'query=up{namespace="'"$ns"'",job="'"$ns"'/ehlo"}' \
        | jq -e '.data.result | length == 1 and .[0].value[1] == "1"' >/dev/null
}
retry 18 'up{job="'"$ns"'/ehlo"} == 1' pod_scraped
curl -fsS "http://127.0.0.1:$prom_port/api/v1/query" --data-urlencode 'query=up{job="'"$ns"'/ehlo"}' | jq -c '.data.result[] | {metric: .metric | {job, namespace, pod}, value: .value[1]}'

step "Grafana Operator $gop_version syncs the GrafanaManifest objects"
"$helm" install grafana-operator "$gop_chart:$gop_version@$gop_digest" \
    --kube-context "$ctx" --namespace grafana-operator --create-namespace \
    --set image.tag="$gop_image_tag" --wait --timeout 5m
printf 'apiVersion: grafana.integreatly.org/v1beta1\nkind: Grafana\nmetadata:\n  name: grafana\n  labels:\n    dashboards: grafana\nspec:\n  version: %s\n' "$grafana_image" \
    | k -n "$ns" apply -f -
"$helm" upgrade ehlo deploy/helm/ehlo --kube-context "$ctx" --namespace "$ns" --reuse-values \
    --set grafanaManifests.enabled=true --wait --timeout 5m
want_n=$(echo "$want" | wc -l | tr -d ' ')
got_n=$(k -n "$ns" get grafanamanifests -l app.kubernetes.io/instance=ehlo -o name | wc -l | tr -d ' ')
[[ "$got_n" == "$want_n" ]] || { echo "$got_n GrafanaManifest objects, want $want_n"; exit 1; }
k -n "$ns" wait grafanamanifests -l app.kubernetes.io/instance=ehlo --for=condition=ManifestSynchronized --timeout 5m
k -n "$ns" get grafanamanifests -l app.kubernetes.io/instance=ehlo \
    -o custom-columns='NAME:.metadata.name,SYNCED:.status.conditions[?(@.type=="ManifestSynchronized")].status'

webhook_codes() {
    local port=$1 token=$2 code
    code=$(curl -s -o /dev/null -w '%{http_code}' -X POST "http://127.0.0.1:$port/v1/webhooks/sendgrid" -d '[]')
    [[ "$code" == 401 ]] || { echo "webhook without token: HTTP $code, want 401"; return 1; }
    code=$(curl -s -o /dev/null -w '%{http_code}' -X POST "http://127.0.0.1:$port/v1/webhooks/sendgrid?token=$token" -d '[]')
    [[ "$code" == 202 ]] || { echo "webhook with token: HTTP $code, want 202"; return 1; }
    echo "webhook: 401 without the token, 202 with it"
}

step "webhook token from webhookSecret"
token=$(openssl rand -hex 16)
k -n "$ns" create secret generic ehlo-webhooks --from-literal=token="$token"
"$helm" upgrade ehlo deploy/helm/ehlo --kube-context "$ctx" --namespace "$ns" --reuse-values \
    --set webhookSecret.name=ehlo-webhooks --wait --timeout 5m
k -n "$ns" get configmap ehlo -o jsonpath='{.data.ehlo\.yaml}' | grep -q 'token_file: /etc/ehlo-secrets/token' \
    || { echo "ConfigMap lacks webhooks.token_file"; exit 1; }
forward "$ns" svc/ehlo "$((ehlo_port + 1))" 8080 /healthz
webhook_codes "$((ehlo_port + 1))" "$token"

step "config from an existing Secret"
token=$(openssl rand -hex 16)
printf 'sources: [mx-1]\ntargets:\n  default: 100000\nwebhooks:\n  token: "%s"\n' "$token" \
    | k -n "$ns" create secret generic ehlo-config --from-file=ehlo.yaml=/dev/stdin
"$helm" upgrade ehlo deploy/helm/ehlo --kube-context "$ctx" --namespace "$ns" --reuse-values \
    --set existingConfigSecret=ehlo-config --set webhookSecret.name= --wait --timeout 5m
if k -n "$ns" get configmap ehlo >/dev/null 2>&1; then echo "ConfigMap ehlo still exists"; exit 1; fi
forward "$ns" svc/ehlo "$((ehlo_port + 2))" 8080 /healthz
webhook_codes "$((ehlo_port + 2))" "$token"

printf '\nkind e2e passed\n'
